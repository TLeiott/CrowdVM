package guest

// Secure layer, guest side of the guest channel (server: guest_channel.go).
// With GuestConfig.Channel set, the config drive holds only a boot stub. The
// agent dials the server, runs TLS pinned to Channel.Pin inside an HTTP
// Upgrade, redeems the one-time ticket and gets its config. The same
// connection then carries a yamux session on which the server calls the
// agent's API. The local listener (what the node reaches) serves only the ops
// subset with the stub's token.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/hashicorp/yamux"
)

// channelUpgrade is the server's Upgrade token (server.GuestUpgradeProto).
const channelUpgrade = "crowdvm-guest"

// Channel is the guest's connection to the server.
type Channel struct {
	stub      proto.GuestConfig
	statePath string
	log       *log.Logger

	mu      sync.Mutex
	session string
	sess    *yamux.Session
}

type channelState struct {
	SandboxID  string `json:"sandboxId"`
	TicketHash string `json:"ticketHash"`
	Session    string `json:"session"`
}

func ticketHash(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

// ConnectChannel fetches the guest config for the boot stub over the channel,
// retrying until it succeeds or ctx ends. The returned config keeps the
// stub's ops token and channel. stateDir keeps the session for reconnects
// after agent restarts; it must be in RAM (/run): the node can read the disk.
func ConnectChannel(ctx context.Context, stub proto.GuestConfig, stateDir string, logger *log.Logger) (proto.GuestConfig, *Channel, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "crowdvm-guest: ", log.LstdFlags)
	}
	c := &Channel{stub: stub, statePath: filepath.Join(stateDir, "channel.json"), log: logger}
	if b, err := os.ReadFile(c.statePath); err == nil {
		var st channelState
		if json.Unmarshal(b, &st) == nil && st.SandboxID == stub.SandboxID && st.TicketHash == ticketHash(stub.Channel.Ticket) {
			c.session = st.Session // same boot (agent restart or reboot): the ticket is spent
		}
	}
	backoff := 500 * time.Millisecond
	for {
		cfg, err := c.connect(ctx)
		if err == nil {
			cfg.Token, cfg.Channel = stub.Token, stub.Channel
			return cfg, c, nil
		}
		logger.Printf("guest channel: %v (retrying)", err)
		select {
		case <-ctx.Done():
			return proto.GuestConfig{}, nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// connect dials, authenticates and installs the yamux session.
func (c *Channel) connect(ctx context.Context) (proto.GuestConfig, error) {
	conn, br, err := c.dial(ctx)
	if err != nil {
		return proto.GuestConfig{}, err
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	hello := proto.GuestHello{SandboxID: c.stub.SandboxID, Version: Version}
	if session != "" {
		hello.Session = session
	} else {
		hello.Ticket = c.stub.Channel.Ticket
	}
	w, err := exchange(conn, br, hello)
	if err == nil && w.Error == "session_invalid" && session != "" {
		// A new boot of this sandbox reused the disk but not the session:
		// the ticket on this config drive is the one to use.
		conn.Close()
		if conn, br, err = c.dial(ctx); err != nil {
			return proto.GuestConfig{}, err
		}
		hello.Session, hello.Ticket = "", c.stub.Channel.Ticket
		w, err = exchange(conn, br, hello)
	}
	if err != nil {
		conn.Close()
		return proto.GuestConfig{}, err
	}
	if w.Error != "" || w.Config == nil {
		conn.Close()
		return proto.GuestConfig{}, fmt.Errorf("server refused: %s", w.Error)
	}
	c.mu.Lock()
	c.session = w.Session
	c.mu.Unlock()
	c.saveState(w.Session)
	sess, err := yamux.Client(&bufReadConn{Conn: conn, r: br}, mux.Config())
	if err != nil {
		conn.Close()
		return proto.GuestConfig{}, err
	}
	c.mu.Lock()
	if c.sess != nil {
		_ = c.sess.Close()
	}
	c.sess = sess
	c.mu.Unlock()
	return *w.Config, nil
}

func (c *Channel) saveState(session string) {
	b, _ := json.Marshal(channelState{SandboxID: c.stub.SandboxID, TicketHash: ticketHash(c.stub.Channel.Ticket), Session: session})
	_ = os.MkdirAll(filepath.Dir(c.statePath), 0o700)
	tmp := c.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, c.statePath)
	}
}

// dial opens the upgraded connection and the pinned TLS inside it.
func (c *Channel) dial(ctx context.Context) (*tls.Conn, *bufio.Reader, error) {
	u, err := url.Parse(c.stub.Channel.URL)
	if err != nil {
		return nil, nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[bool]string{true: "443", false: "80"}[u.Scheme == "https"])
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{Control: markChannel}).DialContext(dctx, "tcp", host)
	if err != nil {
		return nil, nil, err
	}
	if d, ok := dctx.Deadline(); ok {
		_ = raw.SetDeadline(d)
	}
	var outer net.Conn = raw
	if u.Scheme == "https" {
		// The server's own certificate may be private; the pinned TLS
		// inside is what authenticates it.
		tc := tls.Client(raw, &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true})
		if err := tc.HandshakeContext(dctx); err != nil {
			raw.Close()
			return nil, nil, err
		}
		outer = tc
	}
	req, _ := http.NewRequest("GET", "http://"+u.Host+strings.TrimRight(u.Path, "/")+"/guest/v1/connect", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", channelUpgrade)
	if err := req.Write(outer); err != nil {
		outer.Close()
		return nil, nil, err
	}
	obr := bufio.NewReader(outer)
	resp, err := http.ReadResponse(obr, req)
	if err != nil {
		outer.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		outer.Close()
		return nil, nil, fmt.Errorf("upgrade: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	pin := c.stub.Channel.Pin
	tc := tls.Client(&bufReadConn{Conn: outer, r: obr}, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by the pin check
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			h := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare([]byte(base64.StdEncoding.EncodeToString(h[:])), []byte(pin)) != 1 {
				return errors.New("server key does not match the pin")
			}
			return nil
		},
	})
	if err := tc.HandshakeContext(dctx); err != nil {
		outer.Close()
		return nil, nil, fmt.Errorf("channel TLS: %w", err)
	}
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	return tc, bufio.NewReader(tc), nil
}

func exchange(conn *tls.Conn, br *bufio.Reader, hello proto.GuestHello) (proto.GuestWelcome, error) {
	b, _ := json.Marshal(hello)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return proto.GuestWelcome{}, err
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		return proto.GuestWelcome{}, err
	}
	var w proto.GuestWelcome
	if err := json.Unmarshal(line, &w); err != nil {
		return proto.GuestWelcome{}, err
	}
	if netConn := conn.NetConn(); netConn != nil {
		_ = netConn.SetDeadline(time.Time{})
	}
	return w, nil
}

// Serve serves h on the streams the server opens, reconnecting with the
// session whenever the connection drops, until ctx ends.
func (c *Channel) Serve(ctx context.Context, h http.Handler) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		c.mu.Lock()
		sess := c.sess
		c.mu.Unlock()
		if sess != nil {
			start := time.Now()
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					_ = sess.Close()
				case <-done:
				}
			}()
			_ = mux.Serve(sess, h)
			close(done)
			_ = sess.Close()
			if time.Since(start) > 30*time.Second {
				backoff = 250 * time.Millisecond
			}
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := c.connect(ctx); err != nil {
			c.log.Printf("guest channel: reconnect: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 5*time.Second)
			c.mu.Lock()
			c.sess = nil
			c.mu.Unlock()
		}
	}
}

// opsPaths: what the node may call on a channel guest. Nothing here returns
// sandbox content or changes it beyond what pausing or stopping the VM
// already can.
var opsPaths = map[string]bool{
	"GET /v1/health":                    true,
	"POST /v1/internal/prepare-stop":    true,
	"POST /v1/internal/sync":            true,
	"POST /v1/internal/freeze":          true,
	"POST /v1/internal/thaw":            true,
	"POST /v1/internal/playbook/finish": true,
}

type bufReadConn struct {
	net.Conn
	r io.Reader
}

func (c *bufReadConn) Read(p []byte) (int, error) { return c.r.Read(p) }
