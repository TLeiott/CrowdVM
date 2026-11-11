// Package mux carries HTTP in both directions over one yamux session that runs
// on a websocket (node dials server). It also has helpers for raw TCP tunnels
// via HTTP Upgrade.
package mux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Config returns the yamux config used by both sides (large windows for
// snapshot transfer throughput on LAN).
func Config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.MaxStreamWindowSize = 16 << 20
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 30 * time.Second
	c.LogOutput = io.Discard
	return c
}

// Accept upgrades an HTTP request to a websocket and returns the yamux server
// session (server side of the node connection).
func Accept(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(-1)
	nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return yamux.Server(nc, Config())
}

// Dial dials the websocket URL (ws:// or wss://) with headers and returns the
// yamux client session (node side).
func Dial(ctx context.Context, url string, header http.Header) (*yamux.Session, error) {
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(-1)
	nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return yamux.Client(nc, Config())
}

// Serve serves h on every stream the peer opens, until the session closes.
func Serve(s *yamux.Session, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	err := srv.Serve(s)
	if errors.Is(err, http.ErrServerClosed) || s.IsClosed() {
		return nil
	}
	return err
}

// Client returns an http.Client whose connections are new streams on s.
// Use any host in URLs, e.g. "http://peer/v1/info".
func Client(s *yamux.Session) *http.Client {
	return &http.Client{Transport: Transport(s)}
}

func Transport(s *yamux.Session) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.Open()
		},
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}
}

// ---------------------------------------------------------------------------
// Upgrade tunnels
// ---------------------------------------------------------------------------

// UpgradeProto is the Upgrade token for raw TCP tunnels.
const UpgradeProto = "crowdvm-tunnel"

// HijackUpgrade answers an Upgrade request with 101 and returns the raw conn
// (server side of a tunnel endpoint).
func HijackUpgrade(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeProto + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}

// DialUpgrade performs an Upgrade request for path over conn (already
// connected to the peer) and returns the tunnel conn on success.
func DialUpgrade(conn net.Conn, host, path string, header http.Header) (net.Conn, error) {
	req, _ := http.NewRequest("GET", "http://"+host+path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", UpgradeProto)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("tunnel upgrade: %s: %s", resp.Status, b)
	}
	return &bufConn{Conn: conn, r: br}, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// halfCloseIdle is how long the other direction may stay silent after one
// direction ended with a half-close. A full close looks the same as a
// half-close, so without it an abandoned session would live for ever. It is
// long enough for a backend that thinks before answering a half-closed
// request (tests shorten it).
var halfCloseIdle = func() *atomic.Int64 { var v atomic.Int64; v.Store(int64(time.Minute)); return &v }()

// Join copies in both directions and closes both ends once both are done. When
// one direction ends, the other end's write side is half-closed (if it can be),
// so the reply to a request that ended with a half-close still arrives, as long
// as the other direction keeps moving or answers within halfCloseIdle. An end
// without CloseWrite closes both at once.
func Join(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { a.Close(); b.Close() }) }
	var mu sync.Mutex
	var idle *time.Timer
	touch := func() { // data moved: push the idle deadline back
		mu.Lock()
		if idle != nil {
			idle.Reset(time.Duration(halfCloseIdle.Load()))
		}
		mu.Unlock()
	}
	copyDir := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(touchWriter{dst, touch}, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
			mu.Lock()
			if idle == nil {
				idle = time.AfterFunc(time.Duration(halfCloseIdle.Load()), closeBoth)
			}
			mu.Unlock()
			return
		}
		closeBoth()
	}
	done := make(chan struct{})
	go func() { copyDir(a, b); close(done) }()
	copyDir(b, a)
	<-done
	mu.Lock()
	if idle != nil {
		idle.Stop()
	}
	mu.Unlock()
	closeBoth()
}

// touchWriter calls touch after every write.
type touchWriter struct {
	io.Writer
	touch func()
}

func (w touchWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.touch()
	return n, err
}
