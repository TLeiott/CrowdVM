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
	"crypto/rand"
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
	"os/exec"
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

	disk string // reported on ticket redemption (preinit): "luks" | "plain"

	mu      sync.Mutex
	session string
	sess    *yamux.Session
	dataKey []byte          // from the welcome of a ticket redemption
	nonce   string          // sent with the ticket; a retry with it survives a lost welcome (bugs-r14-sonnet 3)
	dnsSess *yamux.Session  // the session dnsTr dials on
	dnsTr   *http.Transport // DNS forwards (bugs-r14-opus 3)
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
		// Same boot (the state is in /run): the ticket is spent. A drive
		// without the ticket (TicketFrom) cannot compare its hash.
		if json.Unmarshal(b, &st) == nil && st.SandboxID == stub.SandboxID && (stub.Channel.Ticket == "" || st.TicketHash == ticketHash(stub.Channel.Ticket)) {
			c.session = st.Session
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

// fetchTicket reads the boot ticket from the node's network service when the
// config drive does not carry it (GuestChannel.TicketFrom), retrying until
// ctx ends.
func fetchTicket(ctx context.Context, stub *proto.GuestConfig) error {
	if stub.Channel == nil || stub.Channel.Ticket != "" || stub.Channel.TicketFrom == "" {
		return nil
	}
	ch := *stub.Channel
	for {
		c, err := (&net.Dialer{Timeout: 3 * time.Second, Control: markChannel}).DialContext(ctx, "tcp", ch.TicketFrom)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			var v struct {
				Ticket string `json:"ticket"`
			}
			err = json.NewDecoder(c).Decode(&v)
			c.Close()
			if err == nil && v.Ticket != "" {
				ch.Ticket = v.Ticket
				stub.Channel = &ch
				return nil
			}
			if err == nil {
				err = errors.New("the node holds no ticket for this boot")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("boot ticket from %s: %v", ch.TicketFrom, err)
		case <-time.After(time.Second):
		}
	}
}

// helloNonce is this process's random redemption nonce (RAM only).
func (c *Channel) helloNonce() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nonce == "" {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		c.nonce = hex.EncodeToString(b)
	}
	return c.nonce
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
		if err := fetchTicket(ctx, &c.stub); err != nil {
			conn.Close()
			return proto.GuestConfig{}, err
		}
		hello.Ticket, hello.Disk, hello.Nonce = c.stub.Channel.Ticket, c.disk, c.helloNonce()
	}
	w, err := exchange(conn, br, hello)
	if err == nil && w.Error == "session_invalid" && session != "" {
		// A new boot of this sandbox reused the disk but not the session:
		// the ticket on this config drive is the one to use.
		conn.Close()
		if err := fetchTicket(ctx, &c.stub); err != nil {
			return proto.GuestConfig{}, err
		}
		if conn, br, err = c.dial(ctx); err != nil {
			return proto.GuestConfig{}, err
		}
		hello.Session, hello.Ticket, hello.Disk, hello.Nonce = "", c.stub.Channel.Ticket, c.disk, c.helloNonce()
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
	if w.DataKey != "" {
		c.dataKey, _ = base64.StdEncoding.DecodeString(w.DataKey)
	}
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

// TakeDataKey returns the disk key of the last ticket redemption and forgets
// it (nil after a session reconnect, or when the server sent none).
func (c *Channel) TakeDataKey() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.dataKey
	c.dataKey = nil
	return k
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
	if a := c.stub.Channel.Addr; a != "" {
		host = a // the node's gateway alias of the server (no DNS needed)
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
					if systemStopping() {
						announceReboot(sess)
					}
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

// systemStopping: the machine is shutting down or rebooting (not just the
// agent restarting).
func systemStopping() bool {
	out, _ := exec.Command("systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out)) == "stopping"
}

// announceReboot tells the server that this machine is going down by its own
// choice, so the new boot ticket its node asks for next is expected. A node
// that kills the VM to get a ticket of its own cannot announce.
func announceReboot(sess *yamux.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://server/v1/rebooting", nil)
	if resp, err := (&http.Client{Transport: mux.Transport(sess)}).Do(req); err == nil {
		resp.Body.Close()
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
