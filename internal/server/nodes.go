package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Node is a registered contributor machine.
type Node struct {
	ID, Name, OwnerID, JoinTokenID, RemoteAddr string
	Info                                       proto.NodeInfo
	Usage                                      proto.Usage
	Labels                                     map[string]string // admin labels (override Info.Labels)
	Disabled                                   bool
	Trusted                                    bool // set by an administrator (placement.trust "trusted", SEC-014)
	CreatedAt                                  time.Time
	LastSeenAt, ConnectedAt                    *time.Time
}

// AllLabels merges the node's own labels with admin labels.
func (n *Node) AllLabels() map[string]string {
	m := map[string]string{}
	for k, v := range n.Info.Labels {
		m[k] = v
	}
	for k, v := range n.Labels {
		m[k] = v
	}
	return m
}

const nodeCols = `id,name,COALESCE(owner_id,''),COALESCE(join_token_id,''),remote_addr,info,usage,labels,disabled,created_at,last_seen_at,connected_at,trusted`

func scanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	n := &Node{}
	var info, usage, labels string
	var created int64
	var seen, conn sql.NullInt64
	if err := sc.Scan(&n.ID, &n.Name, &n.OwnerID, &n.JoinTokenID, &n.RemoteAddr, &info, &usage, &labels, &n.Disabled, &created, &seen, &conn, &n.Trusted); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(info), &n.Info)
	_ = json.Unmarshal([]byte(usage), &n.Usage)
	_ = json.Unmarshal([]byte(labels), &n.Labels)
	n.CreatedAt = store.Time(created)
	n.LastSeenAt, n.ConnectedAt = store.NullTime(seen), store.NullTime(conn)
	return n, nil
}

func init() {
	store.Register("nodetrust", store.Migration{Version: 1, SQL: `ALTER TABLE nodes ADD COLUMN trusted INTEGER NOT NULL DEFAULT 0`})
}

// NodeByID loads a node.
func (s *Server) NodeByID(id string) (*Node, error) {
	return scanNode(s.DB.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
}

// nodeByRef finds a node by id, or else by its (unique) name, so
// `crowdvm nodes info|drain|remove <name>` work like the id.
func (s *Server) nodeByRef(ref string) (*Node, error) {
	n, err := s.NodeByID(ref)
	if err == nil || !store.IsNoRows(err) {
		return n, err
	}
	var id string
	if s.DB.QueryRow(`SELECT id FROM nodes WHERE name=? ORDER BY created_at LIMIT 1`, ref).Scan(&id) != nil {
		return nil, err
	}
	return s.NodeByID(id)
}

// Nodes lists all nodes.
func (s *Server) Nodes() ([]*Node, error) {
	rows, err := s.DB.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// NodeConn is the live yamux session to a node. Use Do for JSON calls and
// Open/HTTP for raw streams.
type NodeConn struct {
	ID          string
	sess        *yamux.Session
	transport   *routedTransport // guest calls of channel sandboxes go over their channel
	client      *http.Client
	connectedAt time.Time
	lastSeen    time.Time
	mu          sync.Mutex
	vms         map[string]proto.VMStatus
}

func newNodeConn(id string, sess *yamux.Session) *NodeConn {
	tr := &routedTransport{node: mux.Transport(sess)}
	return &NodeConn{ID: id, sess: sess, transport: tr, client: &http.Client{Transport: tr}, connectedAt: time.Now(), lastSeen: time.Now(), vms: map[string]proto.VMStatus{}}
}

// NodeError is a non-2xx answer from a node.
type NodeError struct {
	Status int
	Body   string
}

func (e *NodeError) Error() string {
	return fmt.Sprintf("node: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// HTTP returns an http.Client whose requests travel over the node session
// (use any host, e.g. "http://node/v1/info").
func (n *NodeConn) HTTP() *http.Client { return n.client }

// Transport is the node session's RoundTripper (for reverse proxies).
func (n *NodeConn) Transport() http.RoundTripper { return n.transport }

// Open opens a raw stream to the node.
func (n *NodeConn) Open() (net.Conn, error) { return n.sess.Open() }

// Closed reports whether the session ended.
func (n *NodeConn) Closed() bool { return n.sess.IsClosed() }

// Do sends a JSON request to the node API and decodes a JSON answer into out.
func (n *NodeConn) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &NodeError{Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// DialTunnel opens a raw TCP tunnel to port inside the sandbox's VM (over
// its guest channel when it has one).
func (n *NodeConn) DialTunnel(sandboxID string, port int) (net.Conn, error) {
	if n.transport.route != nil {
		if g, channel := n.transport.route(sandboxID); channel {
			if g == nil {
				return nil, errGuestChannelDown
			}
			return dialGuestTunnel(g, port)
		}
	}
	st, err := n.Open()
	if err != nil {
		return nil, err
	}
	return mux.DialUpgrade(st, "node", fmt.Sprintf("/v1/vms/%s/tunnel?port=%d", sandboxID, port), nil)
}

type nodeRegistry struct {
	mu    sync.RWMutex
	conns map[string]*NodeConn
}

func newNodeRegistry() *nodeRegistry { return &nodeRegistry{conns: map[string]*NodeConn{}} }

func (r *nodeRegistry) get(id string) *NodeConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.conns[id]
	if c == nil || c.Closed() {
		return nil
	}
	return c
}

func (r *nodeRegistry) set(id string, c *NodeConn) *NodeConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.conns[id]
	r.conns[id] = c
	return old
}

// remove deletes id only if it still maps to c.
func (r *nodeRegistry) remove(id string, c *NodeConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[id] == c {
		delete(r.conns, id)
		return true
	}
	return false
}

func (r *nodeRegistry) online() map[string]*NodeConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m := map[string]*NodeConn{}
	for id, c := range r.conns {
		if !c.Closed() {
			m[id] = c
		}
	}
	return m
}

func (r *nodeRegistry) closeAll() {
	r.mu.Lock()
	cs := r.conns
	r.conns = map[string]*NodeConn{}
	r.mu.Unlock()
	for _, c := range cs {
		_ = c.sess.Close()
	}
}

func (r *nodeRegistry) expireStale(s *Server) {
	for id, c := range r.online() {
		c.mu.Lock()
		stale := time.Since(c.lastSeen) > s.Cfg.NodeTimeout
		c.mu.Unlock()
		if stale {
			s.Log.Warn("node heartbeat timeout", "node", id)
			_ = c.sess.Close()
		}
	}
}

// NodeConn returns the live connection to a node (nil when offline).
func (s *Server) NodeConn(id string) *NodeConn { return s.nodes.get(id) }

// NodeForSandbox returns the connection to the node running sb.
func (s *Server) NodeForSandbox(sb *Sandbox) (*NodeConn, error) {
	if sb.NodeID == "" {
		return nil, Err(409, "machine_not_running", "The sandbox has no machine. Resume it first.")
	}
	c := s.nodes.get(sb.NodeID)
	if c == nil {
		return nil, ErrD(502, "sandbox_direct_failed", "The node hosting this sandbox is offline; the call did not run.", map[string]any{"retryable": true, "message": "sandbox_restoring", "retryAfterSeconds": 2})
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Join tokens and registration
// ---------------------------------------------------------------------------

// JoinTokenParams creates a join token.
type JoinTokenParams struct {
	Name      string
	CreatedBy string // user id
	OwnerID   string // who earns credits for the node (default CreatedBy)
	TTL       time.Duration
	MaxUses   int // 0 = unlimited
	Labels    map[string]string
}

// CreateJoinToken returns the id and the secret token ("cvj_...").
func (s *Server) CreateJoinToken(p JoinTokenParams) (string, string, error) {
	tok := "cvj_" + randToken(40)
	id := "jt_" + randToken(12)
	if p.OwnerID == "" {
		p.OwnerID = p.CreatedBy
	}
	var exp any
	if p.TTL > 0 {
		exp = store.Now() + p.TTL.Milliseconds()
	}
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	_, err := s.DB.Exec(`INSERT INTO join_tokens(id,token_hash,prefix,name,created_by,owner_id,labels,created_at,expires_at,max_uses) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, hashToken(tok), tok[:12], p.Name, nz(p.CreatedBy), nz(p.OwnerID), mustJSON(p.Labels), store.Now(), exp, p.MaxUses)
	return id, tok, err
}

// JoinTokens lists join tokens.
func (s *Server) JoinTokens() ([]map[string]any, error) {
	rows, err := s.DB.Query(`SELECT id,prefix,name,COALESCE(created_by,''),COALESCE(owner_id,''),labels,created_at,expires_at,max_uses,uses,revoked_at FROM join_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, prefix, name, by, owner, labels string
		var created int64
		var exp, rev sql.NullInt64
		var maxU, uses int
		if err := rows.Scan(&id, &prefix, &name, &by, &owner, &labels, &created, &exp, &maxU, &uses, &rev); err != nil {
			return nil, err
		}
		status := "active"
		switch {
		case rev.Valid:
			status = "revoked"
		case exp.Valid && exp.Int64 < store.Now():
			status = "expired"
		case maxU > 0 && uses >= maxU:
			status = "used"
		}
		var lm map[string]string
		_ = json.Unmarshal([]byte(labels), &lm)
		out = append(out, map[string]any{"id": id, "prefix": prefix, "name": name, "createdBy": nz(by), "ownerId": nz(owner), "labels": lm,
			"createdAt": tsMs(created), "expiresAt": tsNull(exp), "maxUses": maxU, "uses": uses, "status": status, "revokedAt": tsNull(rev)})
	}
	return out, rows.Err()
}

// RevokeJoinToken revokes by id or prefix.
func (s *Server) RevokeJoinToken(id string) error {
	res, err := s.DB.Exec(`UPDATE join_tokens SET revoked_at=? WHERE (id=? OR prefix=?) AND revoked_at IS NULL`, store.Now(), id, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("Join token")
	}
	return nil
}

func (s *Server) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var req proto.RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		api.WriteError(w, Err(400, "invalid_json", "Body must be a RegisterRequest."))
		return
	}
	var jtID, owner, labels string
	var exp sql.NullInt64
	var maxU, uses int
	var rev sql.NullInt64
	err := s.DB.QueryRow(`SELECT id,COALESCE(owner_id,''),labels,expires_at,max_uses,uses,revoked_at FROM join_tokens WHERE token_hash=?`, hashToken(strings.TrimSpace(req.JoinToken))).
		Scan(&jtID, &owner, &labels, &exp, &maxU, &uses, &rev)
	refused := Err(401, "invalid_join_token", "The join token is wrong, expired, used up or revoked.")
	if err != nil || rev.Valid || (exp.Valid && exp.Int64 < store.Now()) || (maxU > 0 && uses >= maxU) {
		api.WriteError(w, refused)
		return
	}
	// Take the use first, in one statement: parallel registrations each
	// passed the check above (bugs-r11-sonnet 2).
	if res, err := s.DB.Exec(`UPDATE join_tokens SET uses=uses+1 WHERE id=? AND (max_uses=0 OR uses<max_uses)`, jtID); err != nil {
		api.WriteError(w, Err(500, "internal_error", err.Error()))
		return
	} else if n, _ := res.RowsAffected(); n == 0 {
		api.WriteError(w, refused)
		return
	}
	tok := "cvn_" + randToken(48)
	nodeID := ""
	if req.Info.NodeID != "" {
		// A node keeps its id only when it proves it is that node with its
		// current token: the same owner is not enough, as a team's nodes
		// often share one (bugs-r11-sonnet 11).
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id=? AND COALESCE(owner_id,'')=? AND token_hash=?`,
			req.Info.NodeID, owner, hashToken(strings.TrimSpace(req.NodeToken))).Scan(&n)
		if n > 0 && req.NodeToken != "" {
			nodeID = req.Info.NodeID
		}
	}
	name := req.Info.Name
	if name == "" {
		name = "node-" + randToken(4)
	}
	nodeNameMu.Lock() // the name check and its write are one step (bugs-r12-sonnet 2)
	name = s.uniqueNodeName(name, nodeID)
	info := req.Info
	if nodeID == "" {
		nodeID = api.NewID(api.PrefixNode, 12)
		info.NodeID = nodeID
		_, err = s.DB.Exec(`INSERT INTO nodes(id,name,token_hash,owner_id,join_token_id,info,labels,created_at,remote_addr) VALUES(?,?,?,?,?,?,?,?,?)`,
			nodeID, name, hashToken(tok), nz(owner), jtID, mustJSON(info), labels, store.Now(), s.clientIP(r))
	} else {
		info.NodeID = nodeID
		_, err = s.DB.Exec(`UPDATE nodes SET name=?, token_hash=?, info=?, remote_addr=? WHERE id=?`, name, hashToken(tok), mustJSON(info), s.clientIP(r), nodeID)
	}
	nodeNameMu.Unlock()
	if err != nil {
		_, _ = s.DB.Exec(`UPDATE join_tokens SET uses=uses-1 WHERE id=?`, jtID) // the use was not taken
		api.WriteError(w, Err(500, "internal_error", err.Error()))
		return
	}
	s.Log.Info("node registered", "node", nodeID, "name", name, "owner", owner)
	writeRawJSON(w, 200, proto.RegisterResponse{NodeID: nodeID, NodeToken: tok})
}

// nodeNameMu holds uniqueNodeName and the write of its result together.
var nodeNameMu sync.Mutex

// reportedNodeName is the name to store for a node that reports name in its
// info, or "" to keep the stored one: only a change of the node's own name
// renames it, so an administrator's rename stays (bugs-r12-sonnet 4). Call
// with nodeNameMu held.
func (s *Server) reportedNodeName(id, name string) string {
	if name == "" {
		return ""
	}
	var old string
	_ = s.DB.QueryRow(`SELECT COALESCE(json_extract(info,'$.name'),'') FROM nodes WHERE id=?`, id).Scan(&old)
	if old == name {
		return ""
	}
	return s.uniqueNodeName(name, id)
}

// uniqueNodeName: want, or want-2, want-3, ... when another node has that
// name (case-insensitive). Pins and lookups match nodes by name, so a node of
// another owner must not share it (bugs-r11-sonnet 3). The first node keeps
// the name.
func (s *Server) uniqueNodeName(want, selfID string) string {
	name := want
	for i := 2; ; i++ {
		var n int
		if s.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE lower(name)=lower(?) AND id<>?`, name, selfID).Scan(&n) != nil || n == 0 {
			return name
		}
		name = fmt.Sprintf("%s-%d", want, i)
	}
}

func (s *Server) nodeByToken(r *http.Request) (*Node, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, errUnauthorized()
	}
	n, err := scanNode(s.DB.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE token_hash=?`, hashToken(strings.TrimSpace(tok))))
	if err != nil {
		return nil, errUnauthorized()
	}
	return n, nil
}

func (s *Server) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	n, err := s.nodeByToken(r)
	if err != nil {
		api.WriteError(w, err.(*api.Error))
		return
	}
	if s.refuseIncompatibleNode(w, r, n) {
		return
	}
	// A drained (disabled) node still connects: its running sandboxes keep
	// running, only new ones go elsewhere (bugs-r7-sonnet 6). Removing a
	// node is how an administrator shuts it out.
	sess, err := mux.Accept(w, r)
	if err != nil {
		s.Log.Warn("node connect: upgrade failed", "node", n.ID, "err", err)
		return
	}
	conn := newNodeConn(n.ID, sess)
	conn.transport.route = s.guestRoute
	if old := s.nodes.set(n.ID, conn); old != nil {
		_ = old.sess.Close()
	}
	now := store.Now()
	_, _ = s.DB.Exec(`UPDATE nodes SET connected_at=?, last_seen_at=?, remote_addr=? WHERE id=?`, now, now, s.clientIP(r), n.ID)
	s.Log.Info("node connected", "node", n.ID, "name", n.Name)
	s.Bus.Publish(Event{Type: EvNodeOnline, NodeID: n.ID})
	s.Go(func(ctx context.Context) { s.afterNodeConnect(ctx, conn) })
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.nodeMux().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeCtxKey{}, conn)))
	})
	done := make(chan struct{})
	go func() {
		select {
		case <-s.ctx.Done():
			_ = sess.Close()
		case <-done:
		}
	}()
	_ = mux.Serve(sess, h)
	close(done)
	_ = sess.Close()
	if s.nodes.remove(n.ID, conn) {
		s.Log.Info("node disconnected", "node", n.ID)
		_, _ = s.DB.Exec(`UPDATE nodes SET connected_at=NULL, last_seen_at=? WHERE id=?`, store.Now(), n.ID)
		s.Bus.Publish(Event{Type: EvNodeOffline, NodeID: n.ID})
		if s.ctx.Err() == nil {
			s.onNodeOffline(n.ID)
		}
	}
}

type nodeCtxKey struct{}

func nodeFrom(r *http.Request) *NodeConn {
	c, _ := r.Context().Value(nodeCtxKey{}).(*NodeConn)
	return c
}

// nodeMux serves the node -> server API (on the yamux session, and on the
// main listener with the node token as Bearer).
func (s *Server) nodeMux() http.Handler { return s.nodeH }

func (s *Server) buildNodeMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /node/v1/heartbeat", s.handleHeartbeat)
	m.HandleFunc("POST /node/v1/events", s.handleNodeEvents)
	m.HandleFunc("GET /node/v1/blobs/{kind}/{id}", s.handleBlobGet)
	m.HandleFunc("HEAD /node/v1/blobs/{kind}/{id}", s.handleBlobGet)
	m.HandleFunc("PUT /node/v1/blobs/{kind}/{id}", s.handleBlobPut)
	m.HandleFunc("GET /node/v1/snapshots/{id}", s.handleSnapshotMeta)
	m.HandleFunc("POST /node/v1/snapshots", s.handleSnapshotTaken)
	m.HandleFunc("POST /node/v1/sandboxes/{id}/reboot", s.handleGuestReboot)
	m.HandleFunc("GET /node/v1/images", s.handleNodeImages)
	m.HandleFunc("GET /node/v1/bench/download", s.handleBenchDownload)
	m.HandleFunc("POST /node/v1/bench/upload", s.handleBenchUpload)
	m.HandleFunc("GET /node/v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeRawJSON(w, 200, map[string]any{"nodeId": nodeFrom(r).ID, "serverUrl": s.Cfg.PublicURL})
	})
	return m
}

// handleNodeHTTP serves the node API on the main listener (Bearer node token).
func (s *Server) handleNodeHTTP(w http.ResponseWriter, r *http.Request) {
	n, err := s.nodeByToken(r)
	if err != nil {
		api.WriteError(w, err.(*api.Error))
		return
	}
	c := s.nodes.get(n.ID)
	if c == nil {
		c = &NodeConn{ID: n.ID, vms: map[string]proto.VMStatus{}, lastSeen: time.Now()}
	}
	s.nodeMux().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeCtxKey{}, c)))
}

func (s *Server) nodeRoutes() {
	s.nodeH = s.buildNodeMux()
	s.router.HandleFunc("POST /node/v1/register", s.handleNodeRegister)
	s.router.HandleFunc("GET /node/v1/connect", s.handleNodeConnect)
	s.router.HandleFunc("GET /guest/v1/connect", s.handleGuestConnect)
	s.router.HandleFunc("/node/v1/", s.handleNodeHTTP)
	s.API("GET /nodes", ActionAny, s.handleListNodes)
	s.API("GET /nodes/{nodeId}", ActionAny, s.handleGetNode)
	s.API("PATCH /nodes/{nodeId}", ActionAny, s.handleUpdateNode)
	s.API("DELETE /nodes/{nodeId}", ActionAny, s.handleDeleteNode)
	s.API("POST /nodes/{nodeId}/evacuate", ActionAny, s.handleEvacuateNode)
	s.API("DELETE /nodes/{nodeId}/evacuate", ActionAny, s.handleCancelEvacuation)
	s.API("GET /join-tokens", ActionAny, s.handleListJoinTokens)
	s.API("POST /join-tokens", ActionAny, s.handleCreateJoinToken)
	s.API("DELETE /join-tokens/{tokenId}", ActionAny, s.handleRevokeJoinToken)
}

// ---------------------------------------------------------------------------
// node -> server handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	c := nodeFrom(r)
	var hb proto.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&hb); err != nil {
		http.Error(w, "bad heartbeat", 400)
		return
	}
	now := store.Now()
	c.mu.Lock()
	c.lastSeen = time.Now()
	for _, vm := range hb.VMs {
		c.vms[vm.SandboxID] = vm
	}
	c.mu.Unlock()
	if hb.Info != nil {
		info := *hb.Info
		info.NodeID = c.ID
		nodeNameMu.Lock()
		name := s.reportedNodeName(c.ID, info.Name)
		_, _ = s.DB.Exec(`UPDATE nodes SET usage=?, info=?, name=CASE WHEN ?<>'' THEN ? ELSE name END, last_seen_at=? WHERE id=?`, mustJSON(hb.Usage), mustJSON(info), name, name, now, c.ID)
		nodeNameMu.Unlock()
		s.syncOutdated(c.ID)
	} else {
		_, _ = s.DB.Exec(`UPDATE nodes SET usage=?, last_seen_at=? WHERE id=?`, mustJSON(hb.Usage), now, c.ID)
	}
	for _, vm := range hb.VMs {
		s.reconcileVM(c.ID, vm, false)
	}
	resp := proto.HeartbeatResponse{}
	_ = s.DB.QueryRow(`SELECT disabled FROM nodes WHERE id=?`, c.ID).Scan(&resp.Drained)
	resp.EarnedCredits, resp.Earned24h = s.nodeEarned(c.ID)
	resp.Evacuation = s.evacuation(c.ID)
	resp.ServerGen = compat.ServerGen
	if n, err := s.NodeByID(c.ID); err == nil {
		resp.GuestChannel = s.channelNode(n)
		resp.Update = s.nodeUpdate(n)
	}
	switch {
	case s.Cfg.SnapshotInterval < 0:
		resp.SnapshotIntervalSec = -1
	case s.Cfg.SnapshotInterval > 0:
		resp.SnapshotIntervalSec = int(s.Cfg.SnapshotInterval.Seconds())
	default: // Boat: a live snapshot every minute (SNAP-001)
		resp.SnapshotIntervalSec = int(DefaultSnapshotInterval.Seconds())
	}
	writeRawJSON(w, 200, resp)
}

func (s *Server) handleNodeEvents(w http.ResponseWriter, r *http.Request) {
	c := nodeFrom(r)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		http.Error(w, "bad events", 400)
		return
	}
	var evs []proto.Event
	if err := json.Unmarshal(body, &evs); err != nil {
		var one proto.Event
		if err2 := json.Unmarshal(body, &one); err2 != nil {
			http.Error(w, "bad events", 400)
			return
		}
		evs = []proto.Event{one}
	}
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
	for _, ev := range evs {
		s.handleNodeEvent(c.ID, ev)
	}
	writeRawJSON(w, 200, map[string]any{"ok": true, "accepted": len(evs)})
}

func (s *Server) handleNodeEvent(nodeID string, ev proto.Event) {
	switch ev.Type {
	case proto.EvVMState:
		var st proto.VMStatus
		if json.Unmarshal(ev.Data, &st) != nil {
			return
		}
		if st.SandboxID == "" {
			st.SandboxID = ev.SandboxID
		}
		if c := s.nodes.get(nodeID); c != nil {
			c.mu.Lock()
			c.vms[st.SandboxID] = st
			c.mu.Unlock()
		}
		s.reconcileVM(nodeID, st, true)
	case proto.EvSnapshotUploaded:
		var ref proto.SnapshotRef
		if json.Unmarshal(ev.Data, &ref) != nil {
			return
		}
		s.onSnapshotUploaded(nodeID, ev.SandboxID, ref)
	case proto.EvSnapshotFailed:
		var d struct {
			SnapshotID string `json:"snapshotId"`
			Error      string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		s.Log.Warn("snapshot failed on node", "node", nodeID, "sandbox", ev.SandboxID, "snapshot", d.SnapshotID, "err", d.Error)
		// Only the node that runs the sandbox, or holds the layer, reports
		// its failure (bugs-r8-sonnet 4).
		if ev.SandboxID != "" {
			_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_status='failed', last_snapshot_at=? WHERE id=? AND node_id=?`, store.Now(), ev.SandboxID, nodeID)
		}
		if d.SnapshotID != "" && (s.hasLocation(d.SnapshotID, nodeID) || s.snapshotTakenOn(d.SnapshotID, nodeID)) {
			s.savedSnapshotFailed(d.SnapshotID, d.Error)
			s.kickDeletions() // a deletion may have waited for this upload
		}
	case proto.EvTransfer:
		var tp proto.TransferProgress
		if json.Unmarshal(ev.Data, &tp) == nil && ev.SandboxID != "" && tp.Direction == "download" {
			// Only the node the sandbox boots on reports its hydration
			// (bugs-r9-sonnet).
			if sb, err := s.SandboxByID(ev.SandboxID); err == nil && sb.NodeID == nodeID {
				hydrationProgress.Store(ev.SandboxID, tp)
			}
		}
	case proto.EvGuest:
		var ge proto.GuestEvent
		if json.Unmarshal(ev.Data, &ge) != nil {
			return
		}
		s.onGuestEvent(nodeID, ev.SandboxID, ge)
	}
}

func blobSHA(r *http.Request) string {
	for _, h := range []string{"X-Crowdvm-Sha256", "X-Content-SHA256", "X-Checksum-SHA256", "X-SHA256"} {
		if v := r.Header.Get(h); v != "" {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	if v := r.URL.Query().Get("sha256"); v != "" {
		return strings.ToLower(v)
	}
	for _, h := range []string{"Content-Digest", "Digest"} {
		for _, part := range strings.Split(r.Header.Get(h), ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || !strings.EqualFold(k, "sha-256") {
				continue
			}
			v = strings.Trim(v, ":")
			if b, err := base64.StdEncoding.DecodeString(v); err == nil {
				return hex.EncodeToString(b)
			}
		}
	}
	return ""
}

func (s *Server) handleBlobPut(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !store.ValidKind(kind) {
		http.Error(w, "unknown blob kind", 404)
		return
	}
	// A snapshot of deleted data is refused (410): the node then erases its
	// copy. Checked again after the upload, which may race the deletion.
	var ref proto.SnapshotRef
	if kind == store.BlobSnapshot {
		_ = json.Unmarshal([]byte(r.Header.Get("X-Crowdvm-Snapshot")), &ref)
		if s.snapshotGone(id, ref.SandboxID) {
			writeRawJSON(w, 410, map[string]any{"ok": false, "error": "snapshot_deleted"})
			return
		}
	}
	// Only a node that has the data may store it, and a stored blob is
	// never replaced: later sandboxes of every tenant boot from it
	// (bugs-r11-sonnet 1, 14).
	if code, msg := s.blobPutRefusal(nodeFrom(r).ID, kind, id); code != 0 {
		writeRawJSON(w, code, map[string]any{"ok": false, "error": msg})
		return
	}
	if old, err := s.Blobs.Stat(kind, id); err == nil {
		if sha := blobSHA(r); sha == "" || !strings.EqualFold(sha, old.SHA256) {
			writeRawJSON(w, 409, map[string]any{"ok": false, "error": "blob_exists: a stored " + kind + " is never replaced"})
			return
		}
		writeRawJSON(w, 200, old) // a retry of the same upload
		return
	}
	bi, err := s.Blobs.Put(kind, id, r.Body, blobSHA(r))
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrChecksum) {
			status = 422
		}
		writeRawJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	now := store.Now()
	if kind == store.BlobImage {
		_, _ = s.DB.Exec(`INSERT INTO images(id,size_bytes,sha256,uploaded_by,created_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET size_bytes=excluded.size_bytes, sha256=excluded.sha256, uploaded_by=excluded.uploaded_by, created_at=excluded.created_at`,
			id, bi.Size, bi.SHA256, nodeFrom(r).ID, now)
	} else {
		if s.snapshotGone(id, ref.SandboxID) {
			_ = s.Blobs.Delete(kind, id)
			writeRawJSON(w, 410, map[string]any{"ok": false, "error": "snapshot_deleted"})
			return
		}
		res, _ := s.DB.Exec(`UPDATE snapshots SET uploaded_at=?, sha256=?, size_bytes=CASE WHEN size_bytes=0 THEN ? ELSE size_bytes END WHERE id=? AND uploaded_at IS NULL`, now, bi.SHA256, bi.Size, id)
		if blobSHA(r) != "" { // Put checked the node's checksum (SNAP-002)
			_, _ = s.DB.Exec(`UPDATE snapshots SET verified_at=COALESCE(verified_at,?) WHERE id=?`, now, id)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.afterSnapshotStored(id)
		}
	}
	writeRawJSON(w, 200, bi)
}

// blobPutRefusal: why nodeID may not upload blob kind/id (0 = it may). A
// recorded snapshot layer comes from the node that took it or holds it; a
// layer not recorded yet is taken (deletions and GC erase what nothing
// records). An image comes from a trusted node or one of an administrator:
// the newest image is the default of every sandbox (bugs-r11-sonnet 1, 14).
func (s *Server) blobPutRefusal(nodeID, kind, id string) (int, string) {
	if kind == store.BlobImage {
		n, err := s.NodeByID(nodeID)
		if err != nil {
			return 403, "unknown node"
		}
		if !n.Trusted {
			if u, err := s.UserByID(n.OwnerID); err != nil || !u.IsAdmin {
				return 403, "images are published by trusted nodes and nodes of an administrator only"
			}
		}
		return 0, ""
	}
	if kind != store.BlobSnapshot {
		return 0, ""
	}
	var row, mine int
	_ = s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(node_id=? OR id IN (SELECT snapshot_id FROM snapshot_locations WHERE node_id=?)),0) FROM snapshots WHERE id=?`,
		nodeID, nodeID, id).Scan(&row, &mine)
	if row == 0 || mine > 0 {
		return 0, ""
	}
	return 403, "this node does not hold snapshot " + id
}

func (s *Server) handleBlobGet(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if n := nodeFrom(r); n == nil || !s.mayFetch(n.ID, kind, id) {
		http.Error(w, "blob not found", 404) // not this node's to read (secure layer)
		return
	}
	bi, err := s.Blobs.Stat(kind, id)
	if err != nil {
		http.Error(w, "blob not found", 404)
		return
	}
	f, err := os.Open(bi.Path)
	if err != nil {
		http.Error(w, "blob not found", 404)
		return
	}
	defer f.Close()
	w.Header().Set("X-Crowdvm-Sha256", bi.SHA256)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) handleSnapshotMeta(w http.ResponseWriter, r *http.Request) {
	snap, err := s.SnapshotByID(r.PathValue("id"))
	if n := nodeFrom(r); err == nil && (n == nil || !s.mayFetch(n.ID, store.BlobSnapshot, snap.ID)) {
		err = errors.New("not this node's")
	}
	if err != nil {
		http.Error(w, "snapshot not found", 404)
		return
	}
	writeRawJSON(w, 200, map[string]any{
		"id": snap.ID, "parentId": snap.ParentID, "imageId": snap.ImageID, "sizeBytes": snap.SizeBytes,
		"createdAt": snap.CreatedAt, "sha256": snap.SHA256, "uploaded": snap.UploadedAt != nil, "chain": s.snapshotChain(snap.ID),
	})
}

// handleSnapshotTaken: a node reports a snapshot it took on its own,
// before uploading it (see proto.SnapshotRef).
func (s *Server) handleSnapshotTaken(w http.ResponseWriter, r *http.Request) {
	var ref proto.SnapshotRef
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ref); err != nil || ref.ID == "" {
		http.Error(w, "bad snapshot ref", 400)
		return
	}
	s.recordTaken(nodeFrom(r).ID, ref)
	writeRawJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleNodeImages(w http.ResponseWriter, r *http.Request) {
	list, _ := s.Blobs.List(store.BlobImage)
	if list == nil {
		list = []store.BlobInfo{}
	}
	writeRawJSON(w, 200, map[string]any{"images": list, "default": s.defaultImage()})
}

func (s *Server) handleBenchDownload(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
	if n <= 0 {
		n = 1
	}
	if n > 8<<30 {
		n = 8 << 30
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	buf := make([]byte, 256<<10)
	_, _ = rand.Read(buf)
	for n > 0 {
		k := int64(len(buf))
		if n < k {
			k = n
		}
		if _, err := w.Write(buf[:k]); err != nil {
			return
		}
		n -= k
	}
}

func (s *Server) handleBenchUpload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	n, _ := io.Copy(io.Discard, r.Body)
	d := time.Since(start)
	mbps := 0.0
	if d > 0 {
		mbps = float64(n) * 8 / d.Seconds() / 1e6
	}
	writeRawJSON(w, 200, map[string]any{"bytes": n, "ms": d.Milliseconds(), "mbps": mbps})
}

// afterNodeConnect refreshes node info and recovers sandboxes whose VM
// survived a disconnect.
func (s *Server) afterNodeConnect(ctx context.Context, c *NodeConn) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	var info proto.NodeInfo
	if err := c.Do(cctx, "GET", "/v1/info", nil, &info); err == nil {
		info.NodeID = c.ID
		nodeNameMu.Lock()
		name := s.reportedNodeName(c.ID, info.Name)
		_, _ = s.DB.Exec(`UPDATE nodes SET info=?, name=CASE WHEN ?<>'' THEN ? ELSE name END WHERE id=?`, mustJSON(info), name, name, c.ID)
		nodeNameMu.Unlock()
		s.syncOutdated(c.ID)
	}
	cancel()
	s.reconcileNodeSnapshots(ctx, c)                             // copies of deleted data it kept
	s.Go(func(ctx context.Context) { s.uploadStranded(ctx, c) }) // layers whose upload a restart cut off
	s.Go(s.processDeletionOps)                                   // deletions that waited for this node
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND state='error' AND error='node_offline'`, c.ID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		var st proto.VMStatus
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.Do(cctx, "GET", "/v1/vms/"+id, nil, &st)
		cancel()
		var ne *NodeError
		switch {
		case err == nil && st.State == proto.VMError:
			// The machine did not survive the node's restart: say so instead
			// of node_offline (opus-22 m3); recover keeps its disk.
			_, _ = s.DB.Exec(`UPDATE sandboxes SET error=?, health_reason='machine_error' WHERE id=? AND state='error' AND error='node_offline'`,
				"machine_error: "+firstNonEmpty(st.Error, "the machine stopped while its node was offline"), id)
		case err == nil:
			if st.SandboxID == "" {
				st.SandboxID = id
			}
			s.reconcileVM(c.ID, st, true)
		case errors.As(err, &ne) && ne.Status == 404:
			s.restartLostMachine(id, c.ID)
		}
	}
}

// chainTip: the newest live layer of sb that builds on its head (a layer
// the node took but has not uploaded yet), else the head.
func (s *Server) chainTip(sb *Sandbox) string {
	rows, err := s.DB.Query(`SELECT id FROM snapshots WHERE sandbox_id=? AND deleted_at IS NULL AND purged_at IS NULL ORDER BY created_at DESC LIMIT 50`, sb.ID)
	if err != nil {
		return sb.HeadSnapshotID
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if slices.Contains(s.snapshotChain(id), sb.HeadSnapshotID) {
			return id
		}
	}
	return sb.HeadSnapshotID
}

// restartLostMachine (opus-22 m3): a node_offline sandbox whose node came
// back without its machine (it restarted before the machine started, or
// lost it) boots again from its last snapshot, on that node first: it
// holds the layers.
func (s *Server) restartLostMachine(id, nodeID string) {
	sb, err := s.SandboxByID(id)
	if err != nil || sb.State != api.StateError || sb.Error != "node_offline" {
		return
	}
	if sb.HeadSnapshotID == "" || !sb.FirstBootDone {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET error=?, health_reason='machine_lost' WHERE id=? AND state='error' AND error='node_offline'`,
			"machine_lost: the node came back without this sandbox's machine; resume it to start it again", id)
		return
	}
	if _, ok := s.transition(id, []string{api.StateError}, api.StateProvisioning, map[string]any{"error": nil, "health": "degraded", "health_reason": "rebuilding"}); !ok {
		return
	}
	s.queueBoot(id, pendingBoot{Kind: proto.BootResume, Parent: s.chainTip(sb), Prefer: nodeID, Cleanup: nodeID, Reason: "machine_lost"})
	s.Log.Info("restarting a sandbox whose node came back without its machine", "sandbox", id, "node", nodeID)
}

// ---------------------------------------------------------------------------
// Public node API (CrowdVM extension for the dashboard)
// ---------------------------------------------------------------------------

func (s *Server) nodeJSON(n *Node, detail bool) map[string]any {
	c := s.nodes.get(n.ID)
	reserved := s.reservedOn(n.ID)
	m := map[string]any{
		"id": n.ID, "name": n.Name, "ownerId": nz(n.OwnerID), "online": c != nil, "disabled": n.Disabled,
		"trust": map[bool]string{true: "trusted", false: "community"}[n.Trusted],
		"os":    n.Info.OS, "arch": n.Info.Arch, "driver": n.Info.Driver, "version": n.Info.Version,
		"labels": n.AllLabels(), "capacity": n.Info.Capacity, "usage": n.Usage, "reserved": reserved,
		// Never below 0: limits lowered under what runs (bugs-r12-opus 5).
		"free": proto.Capacity{VCPU: max(n.Info.Capacity.VCPU-reserved.VCPU, 0), MemoryMiB: max(n.Info.Capacity.MemoryMiB-reserved.MemoryMiB, 0),
			DiskGiB: max(n.Info.Capacity.DiskGiB-reserved.DiskGiB, 0), MaxSandboxes: max(n.Info.Capacity.MaxSandboxes-reserved.VMs, 0)},
		"tier": n.Info.Bench.Tier, "score": n.Info.Bench.Score, "categories": n.Info.Bench.Categories,
		"gpus": len(n.Info.Bench.GPUs), "nested": n.Info.Bench.Virtualization.Nested, "accel": n.Info.Bench.Virtualization.Accel,
		"directHost": nz(n.Info.DirectHost), "images": n.Info.Images,
		"createdAt": ts(n.CreatedAt), "lastSeenAt": nil, "connectedAt": nil, "sandboxes": reserved.VMs,
		"info": n.Info, "owner": nil, "paused": n.Info.Paused, "pauseReason": nil, "evacuation": nil,
		"gen": n.Info.Gen, "update": s.nodeUpdate(n),
	}
	if n.Info.Paused {
		m["pauseReason"] = nz(n.Info.PauseReason)
	}
	if ev := s.evacuation(n.ID); ev != nil {
		m["evacuation"] = map[string]any{"startedAt": ts(ev.StartedAt), "deadline": ts(ev.Deadline), "reason": s.evacuationReason(n.ID)}
	}
	if n.OwnerID != "" {
		if u, err := s.UserByID(n.OwnerID); err == nil {
			m["owner"] = map[string]any{"id": u.ID, "login": u.Username, "displayName": u.DisplayName}
		}
	}
	if n.LastSeenAt != nil {
		m["lastSeenAt"] = ts(*n.LastSeenAt)
	}
	if n.ConnectedAt != nil && c != nil {
		m["connectedAt"] = ts(*n.ConnectedAt)
	}
	if detail {
		m["benchmark"] = n.Info.Bench
		m["remoteAddr"] = n.RemoteAddr
		var earned float64
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM credit_ledger WHERE node_id=? AND kind='earn'`, n.ID).Scan(&earned)
		m["earnedCredits"] = round6(earned)
		var sbs []string
		rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND state NOT IN ('archived','error','cancelled')`, n.ID)
		if err == nil {
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				sbs = append(sbs, id)
			}
			rows.Close()
		}
		if sbs == nil {
			sbs = []string{}
		}
		m["sandboxIds"] = sbs
		var noSnap int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND snapshots_enabled=0 AND `+onNodeStates, n.ID).Scan(&noSnap)
		m["noSnapshotSandboxes"] = noSnap
	}
	return m
}

func (s *Server) handleListNodes(c *Ctx) error {
	nodes, err := s.Nodes()
	if err != nil {
		return err
	}
	out := []map[string]any{}
	var cap, free proto.Capacity
	online := 0
	for _, n := range nodes {
		m := s.nodeJSON(n, false)
		out = append(out, m)
		if m["online"] == true && !n.Disabled {
			online++
			cap.VCPU += n.Info.Capacity.VCPU
			cap.MemoryMiB += n.Info.Capacity.MemoryMiB
			cap.DiskGiB += n.Info.Capacity.DiskGiB
			cap.MaxSandboxes += n.Info.Capacity.MaxSandboxes
			f := m["free"].(proto.Capacity)
			free.VCPU += max(f.VCPU, 0)
			free.MemoryMiB += max(f.MemoryMiB, 0)
			free.DiskGiB += max(f.DiskGiB, 0)
			free.MaxSandboxes += max(f.MaxSandboxes, 0)
		}
	}
	return c.OK(200, "node.list", map[string]any{"nodes": out, "summary": map[string]any{"total": len(nodes), "online": online, "capacity": cap, "free": free}})
}

func (s *Server) handleGetNode(c *Ctx) error {
	n, err := s.nodeByRef(c.Param("nodeId"))
	if err != nil {
		return errNotFound("Node")
	}
	m := s.nodeJSON(n, true)
	if !s.canManageNode(c.P, n) {
		// The owner's address, earnings and other tenants' sandboxes are
		// the owner's business; a user sees only their own sandboxes there
		// (bugs-r7-sonnet 3).
		delete(m, "remoteAddr")
		delete(m, "earnedCredits")
		delete(m, "noSnapshotSandboxes")
		own := []string{}
		if rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND owner_id=? AND deleted_at IS NULL AND state NOT IN ('archived','error','cancelled')`, n.ID, c.P.User.ID); err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					own = append(own, id)
				}
			}
			rows.Close()
		}
		m["sandboxIds"] = own
	}
	return c.OK(200, "node.info", map[string]any{"node": m})
}

func (s *Server) canManageNode(p *Principal, n *Node) bool {
	return p.IsAdmin() || (n.OwnerID == p.User.ID && (p.Key == nil || p.Key.Allows("account.admin")))
}

func (s *Server) handleUpdateNode(c *Ctx) error {
	n, err := s.nodeByRef(c.Param("nodeId"))
	if err != nil {
		return errNotFound("Node")
	}
	if !s.canManageNode(c.P, n) {
		return Err(403, "forbidden", "Only the node owner or an administrator can change it.")
	}
	var req struct {
		Name     *string            `json:"name"`
		Disabled *bool              `json:"disabled"`
		Labels   *map[string]string `json:"labels"`
		Trust    *string            `json:"trust"` // "trusted" | "community" (admins only)
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Disabled != nil && !*req.Disabled && s.evacuation(n.ID) != nil {
		return Err(409, "node_evacuating", "The node is being evacuated: cancel the evacuation first.")
	}
	if req.Trust != nil {
		if !c.P.IsAdmin() {
			return Err(403, "admin_required", "Only an administrator can mark a node trusted (its owner can read the disks it hosts).")
		}
		if *req.Trust != "trusted" && *req.Trust != "community" {
			return Err(400, "invalid_trust", "trust must be trusted or community.")
		}
		_, _ = s.DB.Exec(`UPDATE nodes SET trusted=? WHERE id=?`, boolInt(*req.Trust == "trusted"), n.ID)
		s.audit(c, "node.trust", n.ID, map[string]any{"trust": *req.Trust})
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		nm := strings.TrimSpace(*req.Name)
		nodeNameMu.Lock()
		if s.uniqueNodeName(nm, n.ID) != nm {
			nodeNameMu.Unlock()
			return Err(409, "node_name_taken", "Another node is named "+nm+".") // bugs-r11-sonnet 3
		}
		_, _ = s.DB.Exec(`UPDATE nodes SET name=? WHERE id=?`, nm, n.ID)
		nodeNameMu.Unlock()
	}
	if req.Disabled != nil {
		_, _ = s.DB.Exec(`UPDATE nodes SET disabled=? WHERE id=?`, boolInt(*req.Disabled), n.ID)
	}
	if req.Labels != nil {
		_, _ = s.DB.Exec(`UPDATE nodes SET labels=? WHERE id=?`, mustJSON(*req.Labels), n.ID)
	}
	n, _ = s.NodeByID(n.ID)
	return c.OK(200, "node.updated", map[string]any{"node": s.nodeJSON(n, true)})
}

// snapshotTakenOn: snapshot id was taken by node (or is still pending on
// it: a saved snapshot's layer before the server recorded it).
func (s *Server) snapshotTakenOn(id, nodeID string) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE id=? AND node_id=?`, id, nodeID).Scan(&n)
	if n == 0 {
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM saved_snapshots ss JOIN sandboxes b ON b.id=ss.source_sandbox_id WHERE ss.pending_snapshot_id=? AND b.node_id=?`, id, nodeID).Scan(&n)
	}
	return n > 0
}

func (s *Server) handleDeleteNode(c *Ctx) error {
	n, err := s.nodeByRef(c.Param("nodeId"))
	if err != nil {
		return errNotFound("Node")
	}
	if !s.canManageNode(c.P, n) {
		return Err(403, "forbidden", "Only the node owner or an administrator can remove it.")
	}
	if r := s.reservedOn(n.ID); r.VMs > 0 && c.Query("force") != "true" {
		return Err(409, "node_busy", "The node still runs sandboxes. Stop them first or pass force=true.")
	}
	if only := s.onlyCopiesOn(n.ID); len(only) > 0 && c.Query("force") != "true" {
		return ErrD(409, "node_holds_snapshots", fmt.Sprintf("%d snapshot(s) exist only on this node; wait for them to upload, or pass force=true (they are lost).", len(only)),
			map[string]any{"snapshots": len(only)})
	}
	// m5: a removed node can no longer be reached. When online it erases
	// every snapshot copy the server can do without (deleted ones, and ones
	// stored on the server) first. Copies it keeps stay recorded as its
	// locations: deletions of that data then report "blocked" (node_removed)
	// instead of claiming everything is erased.
	kept := 0
	if conn := s.nodes.get(n.ID); conn != nil {
		// m-a: its machines go first (force), so it takes no more snapshots
		// it could never upload; their sandboxes then turn node_offline and
		// are rebuilt elsewhere from their last stored snapshot.
		s.destroyNodeVMs(c.R.Context(), conn)
		kept = s.eraseNodeCopies(c.R.Context(), conn)
		_ = conn.sess.Close()
	} else {
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM snapshot_locations WHERE node_id=?`, n.ID).Scan(&kept)
	}
	_, _ = s.DB.Exec(`DELETE FROM nodes WHERE id=?`, n.ID)
	_, _ = s.DB.Exec(`DELETE FROM node_evacuations WHERE node_id=?`, n.ID)
	s.earnMu.Lock()
	delete(s.earned, n.ID)
	s.earnMu.Unlock()
	s.fallBackFromRemovedNode()
	// m-b: pins to the removed node would keep its sandboxes from ever
	// being placed again.
	_, _ = s.DB.Exec(`UPDATE sandboxes SET placement=json_remove(placement,'$.node')
		WHERE json_valid(placement) AND lower(json_extract(placement,'$.node')) IN (lower(?),lower(?))`, n.ID, n.Name)
	s.kickDeletions()
	return c.OK(200, "node.deleted", map[string]any{"id": n.ID, "snapshotCopiesLeft": kept})
}

// destroyNodeVMs destroys the machines of every sandbox the server placed
// on a node that is being removed.
func (s *Server) destroyNodeVMs(ctx context.Context, c *NodeConn) {
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND deleted_at IS NULL`, c.ID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		_ = c.Do(cctx, "DELETE", "/v1/vms/"+id, nil, nil)
		cancel()
	}
}

// eraseNodeCopies asks a node that is being removed to erase its snapshot
// copies that are deleted or also stored on the server, and drops those
// locations. It returns how many copies stay (the only copy of a snapshot
// not uploaded yet, or the node failed).
func (s *Server) eraseNodeCopies(ctx context.Context, c *NodeConn) int {
	rows, err := s.DB.Query(`SELECT l.snapshot_id, (s.deleted_at IS NOT NULL OR s.purged_at IS NOT NULL OR s.uploaded_at IS NOT NULL OR s.id IS NULL)
		FROM snapshot_locations l LEFT JOIN snapshots s ON s.id=l.snapshot_id WHERE l.node_id=?`, c.ID)
	if err != nil {
		return 0
	}
	var erase []string
	kept := 0
	for rows.Next() {
		var id string
		var ok bool
		if rows.Scan(&id, &ok) == nil {
			if ok {
				erase = append(erase, id)
			} else {
				kept++
			}
		}
	}
	rows.Close()
	for _, id := range erase {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.Do(cctx, "DELETE", "/v1/snapshots/"+id, nil, nil)
		cancel()
		var ne *NodeError
		if err != nil && !(errors.As(err, &ne) && ne.Status == 404) {
			kept++
			continue
		}
		_, _ = s.DB.Exec(`DELETE FROM snapshot_locations WHERE snapshot_id=? AND node_id=?`, id, c.ID)
	}
	return kept
}

func (s *Server) handleListJoinTokens(c *Ctx) error {
	if !c.P.IsAdmin() {
		return Err(403, "admin_required", "Only administrators list join tokens.")
	}
	list, err := s.JoinTokens()
	if err != nil {
		return err
	}
	return c.OK(200, "join_token.list", map[string]any{"joinTokens": list})
}

func (s *Server) handleCreateJoinToken(c *Ctx) error {
	if !c.P.IsAdmin() && !(s.Cfg.OpenNodes && (c.P.Key == nil || c.P.Key.Allows("account.admin"))) {
		return Err(403, "admin_required", "Only administrators create join tokens on this server.")
	}
	var req struct {
		Name       string            `json:"name"`
		TTLSeconds int               `json:"ttlSeconds"`
		MaxUses    *int              `json:"maxUses"`
		Owner      string            `json:"owner"`
		Labels     map[string]string `json:"labels"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	owner := c.P.User.ID
	if req.Owner != "" && c.P.IsAdmin() {
		u, err := s.UserByLogin(req.Owner)
		if err != nil {
			return Err(400, "unknown_user", "No user "+req.Owner+".")
		}
		owner = u.ID
	}
	ttl := 24 * time.Hour
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	maxU := 1
	if req.MaxUses != nil {
		maxU = *req.MaxUses
	}
	id, tok, err := s.CreateJoinToken(JoinTokenParams{Name: req.Name, CreatedBy: c.P.User.ID, OwnerID: owner, TTL: ttl, MaxUses: maxU, Labels: req.Labels})
	if err != nil {
		return err
	}
	return c.OK(201, "join_token.created", map[string]any{"id": id, "token": tok, "serverUrl": s.Cfg.PublicURL,
		"command": "crowdvm-node join --server " + s.Cfg.PublicURL + " --token " + tok, "expiresAt": ts(time.Now().Add(ttl)), "maxUses": maxU})
}

func (s *Server) handleRevokeJoinToken(c *Ctx) error {
	if !c.P.IsAdmin() {
		return Err(403, "admin_required", "Only administrators revoke join tokens.")
	}
	if err := s.RevokeJoinToken(c.Param("tokenId")); err != nil {
		return err
	}
	return c.OK(200, "join_token.revoked", map[string]any{"id": c.Param("tokenId")})
}

// blobGrants: snapshot layers a node may download because the server sent
// it a boot or an inspector that needs them (node id + "/" + snapshot id ->
// expiry). Any node token used to read any sandbox's disk (secure layer).
var blobGrants sync.Map

func (s *Server) grantChain(nodeID, snapID string) {
	now := time.Now()
	blobGrants.Range(func(k, v any) bool { // expired ones go
		if now.After(v.(time.Time)) {
			blobGrants.Delete(k)
		}
		return true
	})
	exp := now.Add(24 * time.Hour)
	for _, id := range s.snapshotChain(snapID) {
		blobGrants.Store(nodeID+"/"+id, exp)
	}
}

// mayFetch: node nodeID may read blob kind/id. Images are public to nodes;
// a snapshot only when the node was granted its chain, holds it already, or
// it is in the chain of a sandbox placed on the node (grants are in memory:
// this covers a server restart).
func (s *Server) mayFetch(nodeID, kind, id string) bool {
	if kind == store.BlobImage {
		return true
	}
	if v, ok := blobGrants.Load(nodeID + "/" + id); ok && time.Now().Before(v.(time.Time)) {
		return true
	}
	if s.hasLocation(id, nodeID) {
		return true
	}
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND deleted_at IS NULL`, nodeID)
	if err != nil {
		return false
	}
	var ids []string
	for rows.Next() {
		var sid string
		if rows.Scan(&sid) == nil {
			ids = append(ids, sid)
		}
	}
	rows.Close()
	for _, sid := range ids {
		sb, err := s.SandboxByID(sid)
		if err != nil {
			continue
		}
		heads := []string{sb.HeadSnapshotID}
		if v, _ := sb.Extra["bootParent"].(string); v != "" {
			heads = append(heads, v)
		}
		if pb := pendingBootOf(sb); pb != nil && pb.Parent != "" {
			heads = append(heads, pb.Parent)
		}
		for _, h := range heads {
			if h != "" && slices.Contains(s.snapshotChain(h), id) {
				s.grantChain(nodeID, h)
				return true
			}
		}
	}
	return false
}
