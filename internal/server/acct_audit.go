package server

// Audit log (DATA-014): every successful state-changing control-plane API
// call (not guest pass-through traffic) and the sandbox lifecycle events are
// recorded with actor, key, org, route and target. Records outlive the
// sandboxes and accounts they name. GET /audit-events lists the caller's own
// records; GET /admin/audit-events lists all.

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("audit", store.Migration{Version: 1, SQL: `
CREATE TABLE audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  actor_id TEXT,
  key_id TEXT,
  org_id TEXT,
  action TEXT NOT NULL,
  target TEXT,
  status INTEGER,
  ip TEXT,
  request_id TEXT,
  data TEXT
);
CREATE INDEX audit_events_actor ON audit_events(actor_id, at);
CREATE INDEX audit_events_target ON audit_events(target, at);
`})
	RegisterFeature("audit", func(s *Server) error {
		s.API("GET /audit-events", "account.read", s.handleAuditEvents)
		s.API("GET /admin/audit-events", ActionAdmin, s.handleAuditEvents)
		s.Hooks.Guard = append(s.Hooks.Guard, s.auditGuard)
		s.Bus.Subscribe(s.auditOnEvent)
		return nil
	})
}

// audit records one event for the request's principal.
func (s *Server) audit(c *Ctx, action, target string, data map[string]any) {
	var actor, key any
	if c != nil && c.P != nil {
		actor = c.P.User.ID
		if c.P.Key != nil {
			key = c.P.Key.ID
		}
	}
	var ip, rid any
	if c != nil {
		ip, rid = s.clientIP(c.R), c.RequestID
	}
	_, _ = s.DB.Exec(`INSERT INTO audit_events(at,actor_id,key_id,org_id,action,target,status,ip,request_id,data) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		store.Now(), actor, key, nil, action, nz(target), nil, ip, rid, nullJSON(data))
}

func nullJSON(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return mustJSON(m)
}

// auditGuard wraps the response writer of mutating control-plane calls so
// the call is recorded with its status once the handler answers.
func (s *Server) auditGuard(c *Ctx) error {
	switch c.R.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	pat := normPattern(c.R.Pattern)
	_, path, ok := strings.Cut(pat, " ")
	if !ok {
		path = pat
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/sandboxes/{}/"); ok {
		first, _, _ := strings.Cut(rest, "/")
		for _, g := range guestPrefixes {
			if first == g {
				return nil // in-machine traffic, not control plane
			}
		}
		if first == "tunnel" || first == "desktop" {
			return nil
		}
	}
	if c.R.Header.Get("Upgrade") != "" {
		return nil
	}
	target := firstNonEmpty(c.R.PathValue("sandboxId"), c.R.PathValue("snapshotId"), c.R.PathValue("apiKeyId"), c.R.PathValue("org"),
		c.R.PathValue("userId"), c.R.PathValue("profile"), c.R.PathValue("name"), c.R.PathValue("nodeId"))
	var keyID any
	if c.P.Key != nil {
		keyID = c.P.Key.ID
	}
	aw := &auditWriter{ResponseWriter: c.W}
	aw.done = func(status int) {
		if status >= 400 {
			return
		}
		_, _ = s.DB.Exec(`INSERT INTO audit_events(at,actor_id,key_id,org_id,action,target,status,ip,request_id,data) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			store.Now(), c.P.User.ID, keyID, nz(orgHeader(c.R)), c.R.Method+" "+path, nz(target), status, s.clientIP(c.R), c.RequestID, nil)
	}
	c.W = aw
	return nil
}

// auditWriter calls done with the status on the first header write.
type auditWriter struct {
	http.ResponseWriter
	done  func(int)
	wrote bool
}

func (w *auditWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.done(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *auditWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// auditOnEvent records lifecycle events that have no API call of their own
// (TTL stops, credit stops, node failures, deletions by retention).
func (s *Server) auditOnEvent(e Event) {
	switch e.Type {
	case EvSandboxCreated, EvSandboxDeleted, EvSandboxError, EvSandboxArchived, EvSandboxDegraded, EvSandboxRecovered:
	default:
		return
	}
	data := map[string]any{"state": e.State, "reason": e.Reason, "nodeId": e.NodeID}
	_, _ = s.DB.Exec(`INSERT INTO audit_events(at,actor_id,org_id,action,target,data) VALUES(?,?,?,?,?,?)`,
		store.Now(), nz(e.OwnerID), nz(e.OrgID), e.Type, e.SandboxID, mustJSON(data))
}

func (s *Server) handleAuditEvents(c *Ctx) error {
	pg, err := ParsePage(c.R)
	if err != nil {
		return err
	}
	where, args := "1=1", []any{}
	if !c.P.IsAdmin() || !strings.HasPrefix(c.R.URL.Path, "/api/v1/admin/") {
		// A key limited to some sandboxes sees no account-wide log
		// (bugs-r13-sonnet 4).
		if k := c.P.Key; k != nil && k.Restricted() {
			return Err(403, "api_key_restricted", "Restricted keys cannot read the audit log.")
		}
		where, args = "actor_id=?", []any{c.P.User.ID}
	}
	for _, f := range []struct{ q, col string }{{"actor", "actor_id"}, {"target", "target"}, {"action", "action"}} {
		if v := c.Query(f.q); v != "" {
			where += " AND " + f.col + "=?"
			args = append(args, v)
		}
	}
	w, a, tail := pg.SQL("at", "id")
	rows, err := s.DB.Query(`SELECT id,at,COALESCE(actor_id,''),COALESCE(key_id,''),COALESCE(org_id,''),action,COALESCE(target,''),status,COALESCE(ip,''),COALESCE(request_id,''),COALESCE(data,'')
		FROM audit_events WHERE `+where+w+tail, append(args, a...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type ev struct {
		id, at int64
		m      map[string]any
	}
	var list []ev
	for rows.Next() {
		var id, at int64
		var actor, key, org, action, target, ip, rid, data string
		var status sql.NullInt64
		if rows.Scan(&id, &at, &actor, &key, &org, &action, &target, &status, &ip, &rid, &data) != nil {
			continue
		}
		m := map[string]any{"id": id, "at": tsMs(at), "actorId": nz(actor), "apiKeyId": nz(key), "org": nz(org), "action": action,
			"target": nz(target), "ip": nz(ip), "requestId": nz(rid)}
		if status.Valid {
			m["status"] = status.Int64
		}
		if data != "" {
			m["data"] = rawJSON(data)
		}
		list = append(list, ev{id, at, m})
	}
	items, info := PageResult(pg, list, func(e ev) (int64, string) { return e.at, itoa64(e.id) })
	out := make([]map[string]any, len(items))
	for i, e := range items {
		out[i] = e.m
	}
	return c.OK(200, "audit.list", map[string]any{"events": out, "pageInfo": pageJSON(info)})
}

func rawJSON(s string) json.RawMessage {
	if !json.Valid([]byte(s)) {
		b, _ := json.Marshal(s)
		return b
	}
	return json.RawMessage(s)
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
