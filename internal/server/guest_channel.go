package server

// Secure layer, guest channel (docs/design/secure-layer.md §5.2–5.3).
//
// The node used to receive a sandbox's whole guest config (env, secret files,
// SSH and registry logins, AI tokens) and the bearer token of the guest API,
// write them to the config drive and proxy every API call. A node owner could
// read all of it, or run commands in the sandbox, with `cat` and `curl`.
//
// Now the node gets a boot stub: the sandbox id, a one-time ticket, the
// server's URL and the SHA-256 pin of the server's channel key, and an ops
// token for the few calls it needs (health, prepare-stop, sync, freeze, thaw).
// The guest dials GET /guest/v1/connect (Upgrade: crowdvm-guest), runs TLS
// pinned to that key inside the upgraded connection, redeems the ticket and
// gets its config. The connection then carries a yamux session over which the
// server reaches the whole guest API; the node relays ciphertext.
//
// A node that rewrites the pin and runs a man in the middle, or boots a
// modified guest, still gets in: without confidential-computing hardware the
// host controls the VM. A ticket redeemed twice is reported to the sandbox
// owner.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
	"github.com/hashicorp/yamux"
)

func init() {
	store.Register("guest_channel", store.Migration{Version: 1, SQL: `
CREATE TABLE guest_channels (
  sandbox_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  ticket_hash TEXT NOT NULL,
  session_hash TEXT NOT NULL DEFAULT '',
  config TEXT NOT NULL,
  issued_at INTEGER NOT NULL,
  redeemed_at INTEGER
);`}, store.Migration{Version: 2, SQL: `
ALTER TABLE guest_channels ADD COLUMN data_key TEXT NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN data_key TEXT NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN disk TEXT NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN data_key TEXT NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN disk TEXT NOT NULL DEFAULT '';`}, store.Migration{Version: 3, SQL: `
ALTER TABLE guest_channels ADD COLUMN hello_hash TEXT NOT NULL DEFAULT '';`})
}

// Encrypted data partition (secure-layer.md §5.4, one-disk variant). Image v4
// guests keep every write on a LUKS2 partition of the sandbox disk, opened
// before systemd starts with a key that only the server holds. The key
// follows the disk content: a snapshot records the key of the sandbox it
// came from (sandboxes.data_key and disk are copied into the row), and a
// boot from a snapshot takes that key; a boot from the base image gets a new
// one. Keys are sealed like env secrets.

// ensureDiskKey sets the sandbox's disk key for a boot from parent ("" =
// the base image) and returns it sealed.
func (s *Server) ensureDiskKey(sbID, parent string) (string, error) {
	var sealed string
	if parent != "" {
		_ = s.DB.QueryRow(`SELECT data_key FROM snapshots WHERE id=?`, parent).Scan(&sealed)
		if sealed == "" { // recorded without one (a rollback): the sandbox that made it
			_ = s.DB.QueryRow(`SELECT COALESCE(data_key,'') FROM sandboxes WHERE id=(SELECT sandbox_id FROM snapshots WHERE id=?)`, parent).Scan(&sealed)
		}
		if sealed == "" { // a snapshot from before disk keys: the sandbox's own
			_ = s.DB.QueryRow(`SELECT data_key FROM sandboxes WHERE id=?`, sbID).Scan(&sealed)
		}
	}
	if sealed == "" {
		k := make([]byte, 64)
		if _, err := rand.Read(k); err != nil {
			return "", err
		}
		var err error
		if sealed, err = s.sealSecret(base64.StdEncoding.EncodeToString(k)); err != nil {
			return "", err
		}
	}
	_, err := s.DB.Exec(`UPDATE sandboxes SET data_key=? WHERE id=?`, sealed, sbID)
	return sealed, err
}

// encryptedChain: the snapshot holds an encrypted data partition; only nodes
// with the guest channel can boot it (the key comes over the channel).
func (s *Server) encryptedChain(snapID string) bool {
	var disk string
	_ = s.DB.QueryRow(`SELECT disk FROM snapshots WHERE id=?`, snapID).Scan(&disk)
	return disk == "luks"
}

// GuestUpgradeProto is the Upgrade token of GET /guest/v1/connect.
const GuestUpgradeProto = "crowdvm-guest"

// guestTicketTTL: how long a boot ticket may wait for its first redemption
// (boots wait for image and snapshot downloads first).
var guestTicketTTL = 2 * time.Hour

// GuestConn is a connected guest channel.
type GuestConn struct {
	SandboxID, NodeID string
	sess              *yamux.Session
	transport         *http.Transport
	connectedAt       time.Time
}

func (g *GuestConn) closed() bool { return g.sess.IsClosed() }

type guestRegistry struct {
	mu      sync.Mutex
	m       map[string]*GuestConn
	reboots map[string][]time.Time // re-tickets per sandbox, last 10 minutes
	expect  map[string]time.Time   // announced reboots and server-side recovers
}

// expectReboot: the guest announced a reboot, or the server power-cycles it.
func (r *guestRegistry) expectReboot(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.expect == nil {
		r.expect = map[string]time.Time{}
	}
	r.expect[id] = time.Now()
}

// forgetExpected drops an announcement: a new boot was issued, and an
// announcement belongs to the boot that made it (bugs-r14-sonnet 1).
func (r *guestRegistry) forgetExpected(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.expect, id)
}

// takeExpected reports (and forgets) a reboot expected in the last 3 minutes.
func (r *guestRegistry) takeExpected(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.expect[id]
	delete(r.expect, id)
	return ok && time.Since(t) < 3*time.Minute
}

// rebooted records a re-ticket and returns how many happened in the last
// 10 minutes.
func (r *guestRegistry) rebooted(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reboots == nil {
		r.reboots = map[string][]time.Time{}
	}
	var keep []time.Time
	for _, t := range r.reboots[id] {
		if time.Since(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	r.reboots[id] = append(keep, time.Now())
	return len(r.reboots[id])
}

func (r *guestRegistry) get(id string) *GuestConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g := r.m[id]; g != nil && !g.closed() {
		return g
	}
	return nil
}

func (r *guestRegistry) set(g *GuestConn) *GuestConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*GuestConn{}
	}
	old := r.m[g.SandboxID]
	r.m[g.SandboxID] = g
	return old
}

func (r *guestRegistry) remove(g *GuestConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[g.SandboxID] == g {
		delete(r.m, g.SandboxID)
	}
}

// guestKey is the server's guest-channel key and self-signed certificate
// (guest-tls.pem in the data dir, made once). Its SPKI hash is the pin.
type guestKey struct {
	once sync.Once
	cert tls.Certificate
	pin  string
	err  error
}

func (s *Server) guestCert() (tls.Certificate, string, error) {
	s.gkey.once.Do(func() {
		path := filepath.Join(s.Cfg.DataDir, "guest-tls.pem")
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			b, err = newGuestCertPEM()
			if err == nil {
				err = os.WriteFile(path, b, 0o600)
			}
		}
		if err != nil {
			s.gkey.err = err
			return
		}
		cert, err := tls.X509KeyPair(b, b)
		if err != nil {
			s.gkey.err = fmt.Errorf("%s: %v", path, err)
			return
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			s.gkey.err = err
			return
		}
		cert.Leaf = leaf
		s.gkey.cert, s.gkey.pin = cert, SPKIPin(leaf)
	})
	return s.gkey.cert, s.gkey.pin, s.gkey.err
}

// SPKIPin is base64(SHA-256(SubjectPublicKeyInfo)) of a certificate.
func SPKIPin(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(h[:])
}

func newGuestCertPEM() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "crowdvm guest channel"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"crowdvm-guest-channel"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...), nil
}

// channelNode reports whether boots on node n use the guest channel.
func (s *Server) channelNode(n *Node) bool {
	return !s.Cfg.NoGuestChannel && n != nil && n.Info.GuestChannel
}

// issueGuestChannel stores the full guest config for this boot (sealed) and
// returns the stub the node gets in its place. A new boot replaces the row:
// the previous boot's ticket and session stop working.
func (s *Server) issueGuestChannel(sb *Sandbox, nodeID string, full proto.GuestConfig) (proto.GuestConfig, error) {
	_, pin, err := s.guestCert()
	if err != nil {
		return proto.GuestConfig{}, err
	}
	ops := full.Token // the node's ops token; the guest API itself is channel-only
	full.Token, full.Channel = "", nil
	b, err := json.Marshal(full)
	if err != nil {
		return proto.GuestConfig{}, err
	}
	sealed, err := s.sealSecret(string(b))
	if err != nil {
		return proto.GuestConfig{}, err
	}
	ticket := randToken(40)
	var key string
	_ = s.DB.QueryRow(`SELECT data_key FROM sandboxes WHERE id=?`, sb.ID).Scan(&key)
	s.guests.forgetExpected(sb.ID) // bugs-r14-sonnet 1
	if _, err := s.DB.Exec(`INSERT INTO guest_channels(sandbox_id,node_id,ticket_hash,session_hash,config,issued_at,redeemed_at,data_key,hello_hash) VALUES(?,?,?,'',?,?,NULL,?,'')
ON CONFLICT(sandbox_id) DO UPDATE SET node_id=excluded.node_id, ticket_hash=excluded.ticket_hash, session_hash='', config=excluded.config, issued_at=excluded.issued_at, redeemed_at=NULL, data_key=excluded.data_key, hello_hash=''`,
		sb.ID, nodeID, hashToken(ticket), sealed, store.Now(), key); err != nil {
		return proto.GuestConfig{}, err
	}
	if old := s.guests.get(sb.ID); old != nil { // the previous boot's channel
		_ = old.sess.Close()
	}
	return proto.GuestConfig{
		SandboxID: full.SandboxID, Token: ops, ListenAddr: full.ListenAddr, Hostname: full.Hostname,
		Boot: full.Boot, FirstBoot: full.FirstBoot, Desktop: full.Desktop,
		Channel: &proto.GuestChannel{URL: s.envGuestURL(), Pin: pin, Ticket: ticket},
	}, nil
}

// channelMode: the sandbox's current boot uses the guest channel (its API is
// reachable only through it).
func (s *Server) channelMode(sb *Sandbox) bool {
	var node string
	if s.DB.QueryRow(`SELECT node_id FROM guest_channels WHERE sandbox_id=?`, sb.ID).Scan(&node) != nil {
		return false
	}
	return node == sb.NodeID
}

func (s *Server) dropGuestChannel(id string) {
	_, _ = s.DB.Exec(`DELETE FROM guest_channels WHERE sandbox_id=?`, id)
	if g := s.guests.get(id); g != nil {
		_ = g.sess.Close()
	}
}

// handleGuestReboot: POST /node/v1/sandboxes/{id}/reboot. A channel guest
// keeps its session in RAM only (a session on the disk would let the node
// read it and fetch the config), so after a reboot it needs a new one-time
// ticket. Its node asks for one once the old boot's channel has closed; while
// the guest is connected the answer is 409 and the node retries. Only a
// reboot the guest announced on its channel before going down (or a recover
// the server started) gets a ticket: a node that kills its VM to get one of
// its own gets 403, and the sandbox waits for its owner.
func (s *Server) handleGuestReboot(w http.ResponseWriter, r *http.Request) {
	n := nodeFrom(r)
	sb, err := s.SandboxByID(r.PathValue("id"))
	var node, sealed string
	if err == nil {
		err = s.DB.QueryRow(`SELECT node_id,config FROM guest_channels WHERE sandbox_id=?`, sb.ID).Scan(&node, &sealed)
	}
	if err != nil || n == nil || sb.DeletedAt != nil || sb.NodeID != n.ID || node != n.ID {
		http.Error(w, "no channel sandbox of this node", 404)
		return
	}
	if s.guests.get(sb.ID) != nil {
		http.Error(w, "guest_connected", 409)
		return
	}
	if !sb.Usable() {
		// Only a running sandbox reboots: a stopped one (its guest announced
		// the shutdown) or one starting elsewhere gets no ticket (bugs-r14-sonnet 1).
		http.Error(w, "not_running: the sandbox is not running", 404) // not 409: the node gives up
		return
	}
	if !s.guests.takeExpected(sb.ID) {
		// The machine went down without shutting down: a crash, a kernel
		// panic, or its node killed it to get a ticket of its own. No
		// ticket: the sandbox stops until its owner recovers it.
		s.Log.Warn("guest channel: unannounced reboot, no new ticket", "sandbox", sb.ID, "node", n.ID)
		s.notifySandbox(sb, "security_unannounced_reboot", "warning", time.Minute, "Stopped after an unexpected restart",
			sb.Name+" went down without shutting down, so it was not started again: a crash or kernel panic does this, and so does a node that tries to read the sandbox's configuration and disk key. Run `crowdvm recover "+sb.ID+"` to start it again with its disk as it was (`crowdvm resume` would start from the last snapshot).")
		http.Error(w, "unannounced_reboot: the machine went down without shutting down; `crowdvm recover` starts it again", 403)
		return
	}
	plain, err := s.openSecret(sealed)
	var cfg proto.GuestConfig
	if err == nil {
		err = json.Unmarshal([]byte(plain), &cfg)
	}
	if err != nil {
		http.Error(w, "config_unreadable", 500)
		return
	}
	// The disk is the one that was running: nothing one-time runs again.
	cfg.Boot, cfg.FirstBoot, cfg.Scrub, cfg.Files, cfg.SetupScript = proto.BootRecover, false, nil, nil, ""
	stub, err := s.issueGuestChannel(sb, n.ID, cfg)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	count := s.guests.rebooted(sb.ID)
	s.Log.Info("guest channel: new ticket for a reboot", "sandbox", sb.ID, "node", n.ID, "last10min", count)
	if count > 3 {
		s.notifySandbox(sb, "security_reboots", "warning", time.Hour, "Frequent reboots",
			fmt.Sprintf("%s rebooted %d times in 10 minutes. Each reboot gives its node a new one-time boot ticket; if nobody in the sandbox rebooted it, the node may be trying to read the sandbox's configuration.", sb.Name, count))
	}
	writeRawJSON(w, 200, stub.Channel)
}

var errGuestChannelDown = errors.New("the sandbox's guest channel is not connected (the machine is starting, rebooting or lost its network)")

// redeemGuest checks a hello and returns the session to use from now on, the
// guest config and, on a ticket redemption, the disk key.
func (s *Server) redeemGuest(h proto.GuestHello) (string, *proto.GuestConfig, *Sandbox, string, error) {
	fail := func(code string) (string, *proto.GuestConfig, *Sandbox, string, error) {
		return "", nil, nil, "", errors.New(code)
	}
	var nodeID, ticketHash, sessionHash, sealed, sealedKey, helloHash string
	var issued int64
	var redeemed sql.NullInt64
	err := s.DB.QueryRow(`SELECT node_id,ticket_hash,session_hash,config,issued_at,redeemed_at,data_key,hello_hash FROM guest_channels WHERE sandbox_id=?`, h.SandboxID).
		Scan(&nodeID, &ticketHash, &sessionHash, &sealed, &issued, &redeemed, &sealedKey, &helloHash)
	if err != nil {
		return fail("unknown_sandbox")
	}
	sb, err := s.SandboxByID(h.SandboxID)
	if err != nil || sb.DeletedAt != nil {
		return fail("unknown_sandbox")
	}
	if sb.NodeID != nodeID {
		return fail("not_placed_here")
	}
	session, key := h.Session, ""
	switch {
	case h.Ticket != "":
		if hashToken(h.Ticket) != ticketHash {
			return fail("ticket_invalid")
		}
		// The same guest retrying after its welcome was lost: its nonce
		// travels inside TLS, so the node that relays (and can cut) the
		// connection cannot know it. A new session replaces the first
		// (bugs-r14-sonnet 3).
		retry := redeemed.Valid && h.Nonce != "" && helloHash != "" && hashToken(h.Nonce) == helloHash &&
			store.Now()-issued <= guestTicketTTL.Milliseconds()
		if retry {
			session = randToken(40)
			if _, err := s.DB.Exec(`UPDATE guest_channels SET session_hash=? WHERE sandbox_id=? AND ticket_hash=? AND hello_hash=?`,
				hashToken(session), sb.ID, ticketHash, helloHash); err != nil {
				return fail("config_unreadable")
			}
			key = sealedKey
			break
		}
		if redeemed.Valid {
			s.Log.Warn("guest channel: boot ticket redeemed twice", "sandbox", sb.ID, "node", nodeID)
			s.notifySandbox(sb, "security_ticket_reused", "warning", time.Hour, "Possible tampering on a node",
				"Someone presented the one-time boot ticket of "+sb.Name+" a second time. Only the sandbox's own machine should hold it, so the node that hosts it may be trying to read the sandbox's configuration. The second attempt was refused.")
			return fail("ticket_used")
		}
		if store.Now()-issued > guestTicketTTL.Milliseconds() {
			return fail("ticket_expired")
		}
		session = randToken(40)
		nonceHash := ""
		if h.Nonce != "" {
			nonceHash = hashToken(h.Nonce)
		}
		res, err := s.DB.Exec(`UPDATE guest_channels SET session_hash=?, redeemed_at=?, hello_hash=? WHERE sandbox_id=? AND ticket_hash=? AND redeemed_at IS NULL`,
			hashToken(session), store.Now(), nonceHash, sb.ID, ticketHash)
		if err != nil {
			return fail("config_unreadable")
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fail("ticket_used")
		}
		if h.Disk == "luks" || h.Disk == "plain" {
			_, _ = s.DB.Exec(`UPDATE sandboxes SET disk=? WHERE id=?`, h.Disk, sb.ID)
		}
		key = sealedKey
	case h.Session != "" && sessionHash != "" && hashToken(h.Session) == sessionHash:
	default:
		return fail("session_invalid")
	}
	plain, err := s.openSecret(sealed)
	if err != nil {
		return fail("config_unreadable")
	}
	var cfg proto.GuestConfig
	if err := json.Unmarshal([]byte(plain), &cfg); err != nil {
		return fail("config_unreadable")
	}
	dataKey := ""
	if key != "" {
		if dataKey, err = s.openSecret(key); err != nil {
			return fail("config_unreadable")
		}
	}
	return session, &cfg, sb, dataKey, nil
}

// handleGuestConnect: GET /guest/v1/connect (Upgrade: crowdvm-guest), from a
// guest through its node's network. Authentication happens inside TLS.
func (s *Server) handleGuestConnect(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), GuestUpgradeProto) {
		api.WriteError(w, Err(400, "upgrade_required", "Upgrade: "+GuestUpgradeProto+" required."))
		return
	}
	cert, _, err := s.guestCert()
	if err != nil {
		api.WriteError(w, Err(503, "guest_channel_unavailable", err.Error()))
		return
	}
	conn, rw, err := mux.HijackUpgrade(w)
	if err != nil {
		return
	}
	tc := tls.Server(&readerConn{Conn: conn, r: rw.Reader}, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(tc)
	line, err := br.ReadSlice('\n')
	if err != nil {
		tc.Close()
		return
	}
	var h proto.GuestHello
	welcome := proto.GuestWelcome{}
	var sb *Sandbox
	if json.Unmarshal(line, &h) != nil || h.SandboxID == "" {
		welcome.Error = "bad_hello"
	} else if welcome.Session, welcome.Config, sb, welcome.DataKey, err = s.redeemGuest(h); err != nil {
		welcome.Error = err.Error()
	}
	out, _ := json.Marshal(welcome)
	_, werr := tc.Write(append(out, '\n'))
	if welcome.Error != "" || werr != nil {
		s.Log.Info("guest channel refused", "sandbox", h.SandboxID, "err", welcome.Error, "from", s.clientIP(r))
		tc.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})
	sess, err := yamux.Server(&readerConn{Conn: tc, r: br}, mux.Config())
	if err != nil {
		tc.Close()
		return
	}
	g := &GuestConn{SandboxID: sb.ID, NodeID: sb.NodeID, sess: sess, transport: mux.Transport(sess), connectedAt: time.Now()}
	old := s.guests.set(g)
	var cur string
	if s.DB.QueryRow(`SELECT session_hash FROM guest_channels WHERE sandbox_id=?`, sb.ID).Scan(&cur) != nil || cur != hashToken(welcome.Session) {
		// A new boot was issued while this (old) session connected: it
		// must not take the new boot's place.
		s.guests.remove(g)
		_ = sess.Close()
		return
	}
	if old != nil && !old.closed() {
		// The machine rebooted or its network blipped; or someone holding
		// the session took over. The newest connection wins.
		s.Log.Info("guest channel replaced", "sandbox", sb.ID)
		_ = old.sess.Close()
	}
	s.Log.Info("guest channel connected", "sandbox", sb.ID, "node", sb.NodeID)
	go func() {
		_ = mux.ServeLimit(sess, s.guestStreams(g, welcome.Config.Egress == "server"), maxGuestStreams)
	}()
	s.Go(func(ctx context.Context) { s.followGuestChannelEvents(ctx, g) })
	s.Go(func(ctx context.Context) { s.pushGuestFirewall(ctx, g) })
	select {
	case <-sess.CloseChan():
	case <-s.ctx.Done():
		_ = sess.Close()
	}
	s.guests.remove(g)
	s.Log.Info("guest channel closed", "sandbox", sb.ID)
}

// guestLastEvent: the newest guest event time seen per sandbox, so a channel
// reconnect replays only what was missed.
var guestLastEvent sync.Map // sandbox id -> time.Time

// followGuestChannelEvents does over the channel what the node does for
// legacy guests: follow /v1/internal/events/stream and handle each event.
func (s *Server) followGuestChannelEvents(ctx context.Context, g *GuestConn) {
	// The stream ends with the channel, or early (a guest error, an agent
	// restart that kept the channel): follow again while it is up.
	for backoff := time.Second; !g.closed() && ctx.Err() == nil; backoff = min(backoff*2, 30*time.Second) {
		start := time.Now()
		s.followGuestEventsOnce(ctx, g)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-g.sess.CloseChan():
			return
		case <-time.After(backoff):
		}
	}
}

func (s *Server) followGuestEventsOnce(ctx context.Context, g *GuestConn) {
	since := time.Unix(1, 0)
	if v, ok := guestLastEvent.Load(g.SandboxID); ok {
		since = v.(time.Time)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-g.sess.CloseChan():
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://guest/v1/internal/events/stream?since="+url.QueryEscape(since.UTC().Format(time.RFC3339Nano)), nil)
	if err != nil {
		return
	}
	resp, err := (&http.Client{Transport: g.transport}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ge proto.GuestEvent
		if json.Unmarshal(line, &ge) != nil {
			continue
		}
		if ge.Time.IsZero() {
			ge.Time = time.Now().UTC()
		}
		if v, ok := guestLastEvent.Load(g.SandboxID); !ok || ge.Time.After(v.(time.Time)) {
			guestLastEvent.Store(g.SandboxID, ge.Time)
		}
		s.onGuestEvent(g.NodeID, g.SandboxID, ge)
	}
}

// pushGuestFirewall opens the sandbox's direct TCP ports in the guest
// firewall (the node does this for legacy guests; it may not for channel
// guests, or it could open any port of the guest to itself).
func (s *Server) pushGuestFirewall(ctx context.Context, g *GuestConn) {
	n, err := s.NodeByID(g.NodeID)
	if err != nil || n.Info.DirectHost == "" {
		return // no direct ports on this node: the guest firewall stays closed
	}
	open := []string{}
	for _, p := range s.directPortsOf(g.SandboxID) {
		if p.Proto == "" || p.Proto == "tcp" {
			open = append(open, fmt.Sprint(p.Port)+"/tcp")
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	b, _ := json.Marshal(map[string]any{"open": open})
	req, err := http.NewRequestWithContext(cctx, "PUT", "http://guest/v1/internal/firewall", strings.NewReader(string(b)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Transport: g.transport}).Do(req); err == nil {
		resp.Body.Close()
	}
}

// ---------------------------------------------------------------------------
// Routing: NodeConn calls for /v1/vms/{id}/guest/... and tunnels go over the
// guest channel when the sandbox uses one.
// ---------------------------------------------------------------------------

// guestRoute: for a channel sandbox, its connection (nil while it is down:
// errGuestChannelDown) and true; false for legacy sandboxes.
func (s *Server) guestRoute(sandboxID string) (*GuestConn, bool) {
	if g := s.guests.get(sandboxID); g != nil {
		return g, true
	}
	sb, err := s.SandboxByID(sandboxID)
	if err != nil || !s.channelMode(sb) {
		return nil, false
	}
	return nil, true
}

// routedTransport sends guest API requests of channel sandboxes over their
// channel and everything else to the node.
type routedTransport struct {
	node  http.RoundTripper
	route func(sandboxID string) (*GuestConn, bool)
}

func (t *routedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if id, rest, ok := guestPathOf(r.URL.Path); ok && t.route != nil {
		if g, channel := t.route(id); channel {
			if g == nil {
				if r.Body != nil {
					r.Body.Close()
				}
				return nil, errGuestChannelDown
			}
			out := r.Clone(r.Context())
			out.URL.Path, out.URL.RawPath = "/v1/"+rest, ""
			out.URL.Host, out.Host = "guest", "guest"
			return g.transport.RoundTrip(out)
		}
	}
	return t.node.RoundTrip(r)
}

// guestPathOf splits /v1/vms/{id}/guest/{rest}.
func guestPathOf(p string) (id, rest string, ok bool) {
	tail, ok := strings.CutPrefix(p, "/v1/vms/")
	if !ok {
		return "", "", false
	}
	id, rest, ok = strings.Cut(tail, "/guest/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return id, rest, true
}

// dialGuestTunnel opens /v1/tunnel?port=N over a channel.
func dialGuestTunnel(g *GuestConn, port int) (net.Conn, error) {
	st, err := g.sess.Open()
	if err != nil {
		return nil, err
	}
	return mux.DialUpgrade(st, "guest", fmt.Sprintf("/v1/tunnel?port=%d", port), nil)
}
