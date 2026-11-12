package server

// Webhooks (Boat parity HOOK-001..013, API-016..021, API-123).
//
// Lifecycle events go through an outbox (hook_events): ready/error/archived
// and the other transition events are written in the same transaction as the
// state change (Hooks.EventTx); events published outside a transition
// (hydrated) are written by a Bus subscriber. A dispatcher fans each event
// out to its consumers (webhook deliveries here, OTel log records in
// feat_otel.go) and delivers webhooks with retries.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

// WebhookEvents are the six Boat lifecycle events a webhook can subscribe to.
var WebhookEvents = []string{EvSandboxReady, EvSandboxHydrated, EvSandboxError, EvSandboxArchived, EvSandboxDegraded, EvSandboxRecovered}

// WebhookBackoff is the default wait before attempt n+1 (index n-1); 8
// attempts max. Config.WebhookBackoff overrides it per server (tests).
var WebhookBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute,
	30 * time.Minute, time.Hour, 2 * time.Hour}

// WebhookTimeout is how long an endpoint has to answer (Boat: 5 s).
var WebhookTimeout = 5 * time.Second

const (
	whMaxAttempts = 8
	whMaxURLs     = 10
	whKeepRecords = 30 * 24 * time.Hour
	whRotateGrace = 10 * time.Minute
)

func init() {
	store.Register("webhooks", store.Migration{Version: 1, SQL: `
CREATE TABLE hook_events (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  sandbox_id TEXT NOT NULL,
  sandbox_name TEXT NOT NULL DEFAULT '',
  previous_state TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '',
  reason TEXT,
  created_at INTEGER NOT NULL,
  fanned INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX hook_events_fan ON hook_events(fanned, created_at);

CREATE TABLE webhooks (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  name TEXT,
  url TEXT NOT NULL,
  events TEXT NOT NULL,
  secret TEXT NOT NULL,
  prev_secret TEXT,
  prev_until INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX webhooks_owner ON webhooks(owner_id);

CREATE TABLE webhook_deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  webhook_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  payload TEXT NOT NULL,
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER,
  last_status INTEGER,
  last_error TEXT,
  last_attempt_at INTEGER,
  created_at INTEGER NOT NULL,
  completed_at INTEGER,
  UNIQUE (webhook_id, event_id)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at);
CREATE INDEX webhook_deliveries_hook ON webhook_deliveries(webhook_id, created_at);
`})
	RegisterFeature("webhooks", func(s *Server) error {
		h := &whState{s: s, kick: make(chan struct{}, 1), inflight: map[int64]string{}}
		s.Hooks.EventTx = append(s.Hooks.EventTx, h.eventTx)
		s.API("GET /webhooks", "account.admin", h.list)
		s.API("POST /webhooks", "account.admin", h.create)
		s.API("GET /webhooks/{webhookId}", "account.admin", h.get)
		s.API("PATCH /webhooks/{webhookId}", "account.admin", h.update)
		s.API("DELETE /webhooks/{webhookId}", "account.admin", h.del)
		s.API("POST /webhooks/{webhookId}/rotate", "account.admin", h.rotate)
		// CrowdVM extensions for the dashboard: delivery log and test delivery.
		s.API("GET /webhooks/{webhookId}/deliveries", "account.admin", h.deliveries)
		s.API("POST /webhooks/{webhookId}/test", "account.admin", h.test)
		s.Bus.Subscribe(h.onEvent)
		s.Go(h.loop)
		return nil
	})
}

// outboxConsumers fan one outbox event out (called by the dispatcher, in a
// transaction). feat_otel.go adds itself.
var outboxConsumers []func(s *Server, tx *sql.Tx, ev *hookEvent) error

type hookEvent struct {
	ID, Type, OwnerID, SandboxID, SandboxName, PrevState, State string
	Reason                                                      sql.NullString
	CreatedAt                                                   int64
}

// payload is the Boat WebhookEvent body.
func (ev *hookEvent) payload() []byte {
	data := map[string]any{"sandbox": map[string]any{"id": ev.SandboxID, "name": ev.SandboxName}}
	switch ev.Type {
	case EvSandboxReady, EvSandboxError, EvSandboxArchived:
		data["previousState"], data["state"] = ev.PrevState, ev.State
	case EvSandboxDegraded, EvSandboxRecovered:
		data["state"] = ev.State
		if ev.Reason.Valid && ev.Type == EvSandboxDegraded {
			data["reason"] = ev.Reason.String
		} else {
			data["reason"] = nil
		}
	}
	b, _ := json.Marshal(map[string]any{"id": ev.ID, "type": ev.Type, "createdAt": tsMs(ev.CreatedAt), "data": data})
	return b
}

type whState struct {
	s    *Server
	kick chan struct{}

	mu       sync.Mutex
	inflight map[int64]string // delivery id -> owner id
}

func (h *whState) poke() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// isHookEvent accepts the six events and their legacy Box names (box.ready
// ...), whose webhooks get box.* events with "box" payloads (API-137).
func isHookEvent(t string) bool {
	if rest, ok := strings.CutPrefix(t, "box."); ok {
		t = "sandbox." + rest
	}
	return slices.Contains(WebhookEvents, t)
}

// boxPayload renames a delivery body to the legacy Box shape.
func boxPayload(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if t, ok := m["type"].(string); ok {
		m["type"] = "box." + strings.TrimPrefix(t, "sandbox.")
	}
	if d, ok := m["data"].(map[string]any); ok {
		if sb, ok := d["sandbox"]; ok {
			delete(d, "sandbox")
			d["box"] = sb
		}
	}
	b, _ := json.Marshal(m)
	return b
}

// eventTx writes the outbox row in the state-change transaction.
func (h *whState) eventTx(tx *sql.Tx, e Event) error {
	if !isHookEvent(e.Type) {
		return nil
	}
	var name, owner string
	if err := tx.QueryRow(`SELECT name, owner_id FROM sandboxes WHERE id=?`, e.SandboxID).Scan(&name, &owner); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO hook_events(id,type,owner_id,sandbox_id,sandbox_name,previous_state,state,reason,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Type, owner, e.SandboxID, name, e.PreviousState, e.State, nz(e.Reason), store.Now())
	return err
}

// onEvent records events published outside a transition (hydrated) and
// wakes the dispatcher. It never blocks the bus for long.
func (h *whState) onEvent(e Event) {
	if !isHookEvent(e.Type) {
		return
	}
	if e.ID == "" && e.SandboxID != "" {
		owner := e.OwnerID
		if owner == "" {
			_ = h.s.DB.QueryRow(`SELECT owner_id FROM sandboxes WHERE id=?`, e.SandboxID).Scan(&owner)
		}
		_, _ = h.s.DB.Exec(`INSERT OR IGNORE INTO hook_events(id,type,owner_id,sandbox_id,sandbox_name,previous_state,state,reason,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			"evt_"+randHex(16), e.Type, owner, e.SandboxID, e.SandboxName, e.PreviousState, e.State, nz(e.Reason), store.T(e.Time))
	}
	h.poke()
}

func (h *whState) loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastGC := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
		case <-t.C:
		}
		h.fanOut()
		h.deliverDue(ctx)
		if time.Since(lastGC) > 10*time.Minute {
			lastGC = time.Now()
			cut := store.T(time.Now().Add(-whKeepRecords))
			_, _ = h.s.DB.Exec(`DELETE FROM webhook_deliveries WHERE status!='pending' AND created_at<?`, cut)
			_, _ = h.s.DB.Exec(`DELETE FROM hook_events WHERE fanned=1 AND created_at<?`, cut)
		}
	}
}

// fanOut turns new outbox events into per-consumer work.
func (h *whState) fanOut() {
	rows, err := h.s.DB.Query(`SELECT id,type,owner_id,sandbox_id,sandbox_name,previous_state,state,reason,created_at FROM hook_events WHERE fanned=0 ORDER BY created_at, id LIMIT 200`)
	if err != nil {
		return
	}
	var evs []*hookEvent
	for rows.Next() {
		ev := &hookEvent{}
		if rows.Scan(&ev.ID, &ev.Type, &ev.OwnerID, &ev.SandboxID, &ev.SandboxName, &ev.PrevState, &ev.State, &ev.Reason, &ev.CreatedAt) == nil {
			evs = append(evs, ev)
		}
	}
	rows.Close()
	for _, ev := range evs {
		err := h.s.DB.Tx(h.s.ctx, func(tx *sql.Tx) error {
			if err := whFanout(h.s, tx, ev); err != nil {
				return err
			}
			for _, fn := range outboxConsumers {
				if err := fn(h.s, tx, ev); err != nil {
					return err
				}
			}
			_, err := tx.Exec(`UPDATE hook_events SET fanned=1 WHERE id=?`, ev.ID)
			return err
		})
		if err != nil {
			h.s.Log.Warn("webhook fan-out failed", "event", ev.ID, "err", err)
		}
	}
}

func whFanout(s *Server, tx *sql.Tx, ev *hookEvent) error {
	rows, err := tx.Query(`SELECT id, events FROM webhooks WHERE owner_id=?`, ev.OwnerID)
	if err != nil {
		return err
	}
	var ids, boxIDs []string
	boxType := "box." + strings.TrimPrefix(ev.Type, "sandbox.")
	for rows.Next() {
		var id, evJSON string
		if rows.Scan(&id, &evJSON) == nil {
			var list []string
			_ = json.Unmarshal([]byte(evJSON), &list)
			if slices.Contains(list, ev.Type) {
				ids = append(ids, id)
			} else if slices.Contains(list, boxType) {
				boxIDs = append(boxIDs, id)
			}
		}
	}
	rows.Close()
	if len(ids)+len(boxIDs) == 0 {
		return nil
	}
	body := ev.payload()
	now := store.Now()
	for i, list := range [][]string{ids, boxIDs} {
		typ, b := ev.Type, string(body)
		if i == 1 {
			typ, b = boxType, string(boxPayload(body))
		}
		for _, id := range list {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO webhook_deliveries(webhook_id,event_id,event_type,payload,status,next_attempt_at,created_at) VALUES(?,?,?,?,?,?,?)`,
				id, ev.ID, typ, b, "pending", now, now); err != nil {
				return err
			}
		}
	}
	return nil
}

type whDelivery struct {
	ID                       int64
	WebhookID, EventID, Type string
	Payload                  string
	Attempts                 int
	Owner                    string
}

// whParallel bounds all concurrent attempts; whPerOwner keeps one account's
// hanging endpoints from taking every slot.
const (
	whParallel = 16
	whPerOwner = 4
)

func (h *whState) deliverDue(ctx context.Context) {
	// At most whPerOwner due rows per account (oldest first): in-flight rows
	// hold a lease, so one account's backlog cannot crowd out the others.
	rows, err := h.s.DB.Query(`SELECT id,webhook_id,event_id,event_type,payload,attempts,owner FROM (
SELECT d.id,d.webhook_id,d.event_id,d.event_type,d.payload,d.attempts,d.next_attempt_at,COALESCE(w.owner_id,'') AS owner,
ROW_NUMBER() OVER (PARTITION BY COALESCE(w.owner_id,'') ORDER BY d.next_attempt_at, d.id) AS rn
FROM webhook_deliveries d LEFT JOIN webhooks w ON w.id=d.webhook_id WHERE d.status='pending' AND d.next_attempt_at<=?)
WHERE rn<=? ORDER BY next_attempt_at, id LIMIT 100`, store.Now(), whPerOwner)
	if err != nil {
		return
	}
	var due []*whDelivery
	for rows.Next() {
		d := &whDelivery{}
		if rows.Scan(&d.ID, &d.WebhookID, &d.EventID, &d.Type, &d.Payload, &d.Attempts, &d.Owner) == nil {
			due = append(due, d)
		}
	}
	rows.Close()
	for _, d := range due {
		h.mu.Lock()
		_, busy := h.inflight[d.ID]
		own := 0
		for _, o := range h.inflight {
			if o == d.Owner {
				own++
			}
		}
		busy = busy || len(h.inflight) >= whParallel || own >= whPerOwner
		h.mu.Unlock()
		if busy {
			continue
		}
		// Claim the row with a lease, then use its current attempt count: a row
		// read before a concurrent attempt finished must not be sent again.
		now := store.Now()
		lease := now + (WebhookTimeout + 10*time.Second).Milliseconds()
		res, err := h.s.DB.Exec(`UPDATE webhook_deliveries SET next_attempt_at=? WHERE id=? AND status='pending' AND next_attempt_at<=?`, lease, d.ID, now)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if h.s.DB.QueryRow(`SELECT attempts FROM webhook_deliveries WHERE id=?`, d.ID).Scan(&d.Attempts) != nil {
			continue
		}
		h.mu.Lock()
		h.inflight[d.ID] = d.Owner
		h.mu.Unlock()
		d := d
		h.s.Go(func(ctx context.Context) {
			defer func() {
				h.mu.Lock()
				delete(h.inflight, d.ID)
				h.mu.Unlock()
			}()
			h.attempt(ctx, d)
		})
	}
}

// attempt sends one delivery attempt and records the result.
func (h *whState) attempt(ctx context.Context, d *whDelivery) {
	var urlStr, sealed string
	if err := h.s.DB.QueryRow(`SELECT url, secret FROM webhooks WHERE id=?`, d.WebhookID).Scan(&urlStr, &sealed); err != nil {
		_, _ = h.s.DB.Exec(`DELETE FROM webhook_deliveries WHERE id=?`, d.ID) // endpoint deleted
		return
	}
	secret, err := h.s.openSecret(sealed)
	if err != nil {
		h.s.Log.Error("webhook secret unreadable", "webhook", d.WebhookID, "err", err)
		now := store.Now()
		_, _ = h.s.DB.Exec(`UPDATE webhook_deliveries SET status='failed', attempts=?, last_error=?, last_attempt_at=?, next_attempt_at=NULL, completed_at=? WHERE id=?`,
			d.Attempts+1, "webhook secret is unreadable; rotate the secret to fix this endpoint", now, now, d.ID)
		return
	}
	n := d.Attempts + 1
	status, msg := whSend(withOutboundPolicy(ctx, h.s.Cfg.WebhookAllowLocal), urlStr, secret, d.EventID, d.Type, []byte(d.Payload), n)
	now := store.Now()
	switch {
	case status/100 == 2:
		_, _ = h.s.DB.Exec(`UPDATE webhook_deliveries SET status='delivered', attempts=?, last_status=?, last_error=NULL, last_attempt_at=?, next_attempt_at=NULL, completed_at=? WHERE id=?`,
			n, status, now, now, d.ID)
	case n >= whMaxAttempts:
		_, _ = h.s.DB.Exec(`UPDATE webhook_deliveries SET status='failed', attempts=?, last_status=?, last_error=?, last_attempt_at=?, next_attempt_at=NULL, completed_at=? WHERE id=?`,
			n, nullInt(status), msg, now, now, d.ID)
	default:
		wait := h.s.backoff(n)
		_, _ = h.s.DB.Exec(`UPDATE webhook_deliveries SET attempts=?, last_status=?, last_error=?, last_attempt_at=?, next_attempt_at=? WHERE id=? AND status='pending'`,
			n, nullInt(status), msg, now, now+wait.Milliseconds(), d.ID)
	}
}

// backoff is the wait after failed attempt n (1-based).
func (s *Server) backoff(n int) time.Duration {
	b := s.Cfg.WebhookBackoff
	return b[min(n-1, len(b)-1)]
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// WebhookSignature returns the X-Ascii-Signature value for a delivery:
// v1=hex(HMAC_SHA256(secret, delivery + "." + timestamp + "." + body)).
func WebhookSignature(secret, delivery, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(delivery + "." + timestamp + "."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// whSend posts one attempt. It returns the HTTP status (0 = no answer) and a
// diagnostic for anything but 2xx.
func whSend(ctx context.Context, urlStr, secret, eventID, typ string, body []byte, attempt int) (int, string) {
	cctx, cancel := context.WithTimeout(ctx, WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", urlStr, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	sig := WebhookSignature(secret, eventID, tsStr, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CrowdVM-Webhooks/1")
	for _, p := range []string{"X-Ascii-", "X-CrowdVM-"} {
		req.Header.Set(p+"Event", typ)
		req.Header.Set(p+"Delivery", eventID)
		req.Header.Set(p+"Timestamp", tsStr)
		req.Header.Set(p+"Signature", sig)
		req.Header.Set(p+"Attempt", strconv.Itoa(attempt))
	}
	resp, err := whClient.Do(req)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return 0, fmt.Sprintf("timeout: no answer within %s", WebhookTimeout)
		}
		return 0, "connection failed: " + err.Error()
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, ""
	}
	msg := "HTTP " + strconv.Itoa(resp.StatusCode)
	if resp.StatusCode/100 == 3 {
		msg += " (redirects are not followed)"
	}
	if s := strings.TrimSpace(string(snippet)); s != "" {
		msg += ": " + s
	}
	return resp.StatusCode, msg
}

// ---------------------------------------------------------------------------
// Secrets at rest (AES-256-GCM with a key file in the data dir)
// ---------------------------------------------------------------------------

var (
	secretKeyMu  sync.Mutex
	secretKeyMem = map[string][]byte{}
)

// secretKey returns the master key of this data dir (<DataDir>/secret.key).
func (s *Server) secretKey() ([]byte, error) {
	secretKeyMu.Lock()
	defer secretKeyMu.Unlock()
	path := filepath.Join(s.Cfg.DataDir, "secret.key")
	if k, ok := secretKeyMem[path]; ok {
		return k, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// A new key cannot open what the lost one sealed. With encrypted
		// sandbox disks that is their data: refuse instead of hiding it.
		var sealed int
		if s.DB != nil && s.DB.QueryRow(`SELECT 1 FROM snapshots WHERE data_key != '' UNION SELECT 1 FROM sandboxes WHERE data_key != '' LIMIT 1`).Scan(&sealed) == nil {
			return nil, fmt.Errorf("%s is missing, but encrypted sandbox disks are sealed with it: restore it from your backup (a new key cannot open them)", path)
		}
		b = []byte(randHex(32))
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	k := sha256.Sum256(bytes.TrimSpace(b))
	secretKeyMem[path] = k[:]
	return k[:], nil
}

// sealSecret encrypts a secret for storage ("v1:" + base64(nonce|ciphertext)).
func (s *Server) sealSecret(plain string) (string, error) {
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return "v1:" + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *Server) openSecret(sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, "v1:")
	if !ok {
		return "", errors.New("unknown secret format")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	if len(b) < gcm.NonceSize() {
		return "", errors.New("secret too short")
	}
	out, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
	return string(out), err
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

type whRow struct {
	ID, OwnerID, URL string
	Name             sql.NullString
	Events           []string
	CreatedAt        int64
	UpdatedAt        int64
	PrevUntil        sql.NullInt64
}

func (h *whState) load(c *Ctx) (*whRow, error) {
	w := &whRow{}
	var evs string
	err := h.s.DB.QueryRow(`SELECT id,owner_id,name,url,events,created_at,updated_at,prev_until FROM webhooks WHERE id=?`, c.Param("webhookId")).
		Scan(&w.ID, &w.OwnerID, &w.Name, &w.URL, &evs, &w.CreatedAt, &w.UpdatedAt, &w.PrevUntil)
	if err != nil || w.OwnerID != c.P.User.ID {
		return nil, errNotFound("Webhook")
	}
	_ = json.Unmarshal([]byte(evs), &w.Events)
	return w, nil
}

func (w *whRow) json() map[string]any {
	return map[string]any{"id": w.ID, "name": nullStr(w.Name), "url": w.URL, "events": w.Events,
		"createdAt": tsMs(w.CreatedAt), "updatedAt": tsMs(w.UpdatedAt)}
}

func (h *whState) list(c *Ctx) error {
	rows, err := h.s.DB.Query(`SELECT id,owner_id,name,url,events,created_at,updated_at,prev_until FROM webhooks WHERE owner_id=? ORDER BY created_at, id`, c.P.User.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		w := &whRow{}
		var evs string
		if err := rows.Scan(&w.ID, &w.OwnerID, &w.Name, &w.URL, &evs, &w.CreatedAt, &w.UpdatedAt, &w.PrevUntil); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(evs), &w.Events)
		out = append(out, w.json())
	}
	return c.OK(200, "webhook.list", map[string]any{"webhooks": out})
}

// whCheckURL validates an endpoint. HOOK-004 (ADAPTED): http and private/LAN
// addresses are allowed; loopback, link-local and metadata targets are not
// unless the admin allows them (outbound_guard.go); redirects are never
// followed.
func whCheckURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return "", Err(400, "invalid_url", "url must be an absolute http:// or https:// URL.")
	}
	if u.User != nil {
		return "", Err(400, "invalid_url", "url must not contain credentials.")
	}
	if len(raw) > 2048 {
		return "", Err(400, "invalid_url", "url is too long (2048 characters max).")
	}
	u.Fragment = ""
	return u.String(), nil
}

func whCheckEvents(evs []string) ([]string, error) {
	if len(evs) == 0 {
		return nil, Err(400, "invalid_events", "events needs at least one event, e.g. [\"sandbox.ready\"].")
	}
	var out []string
	for _, e := range evs {
		if !isHookEvent(e) {
			return nil, ErrD(400, "invalid_events", "Unknown event "+strconv.Quote(e)+".", map[string]any{"allowed": WebhookEvents})
		}
		if slices.Contains(out, e) {
			return nil, Err(400, "invalid_events", "events must not repeat "+e+".")
		}
		out = append(out, e)
	}
	return out, nil
}

func whCheckName(n *string, create bool) (any, error) {
	if n == nil {
		return nil, nil
	}
	v := strings.TrimSpace(*n)
	if len(v) > 100 || (create && v == "") {
		return nil, Err(400, "invalid_name", "name must be 1-100 characters.")
	}
	return nz(v), nil
}

// urlLimit enforces at most 10 unique endpoint URLs per account.
func (h *whState) urlLimit(owner, newURL, exceptID string) error {
	rows, err := h.s.DB.Query(`SELECT DISTINCT url FROM webhooks WHERE owner_id=? AND id!=?`, owner, exceptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var u string
		_ = rows.Scan(&u)
		if u == newURL {
			return nil
		}
		n++
	}
	if n >= whMaxURLs {
		return ErrD(409, "webhook_limit_reached", fmt.Sprintf("An account can register at most %d endpoint URLs.", whMaxURLs), map[string]any{"limit": whMaxURLs})
	}
	return nil
}

func newWebhookSecret() string { return "whsec_" + randHex(32) }

func (h *whState) create(c *Ctx) error {
	var req struct {
		Name   *string  `json:"name"`
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	u, err := whCheckURL(req.URL)
	if err != nil {
		return err
	}
	if err := h.s.checkOutboundURL(c.R.Context(), u, "invalid_url"); err != nil {
		return err
	}
	evs, err := whCheckEvents(req.Events)
	if err != nil {
		return err
	}
	name, err := whCheckName(req.Name, true)
	if err != nil {
		return err
	}
	if err := h.urlLimit(c.P.User.ID, u, ""); err != nil {
		return err
	}
	secret := newWebhookSecret()
	sealed, err := h.s.sealSecret(secret)
	if err != nil {
		return err
	}
	id := "wh_" + randHex(12)
	now := store.Now()
	if _, err := h.s.DB.Exec(`INSERT INTO webhooks(id,owner_id,name,url,events,secret,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, c.P.User.ID, name, u, mustJSON(evs), sealed, now, now); err != nil {
		return err
	}
	c.W.Header().Set("Cache-Control", "no-store")
	w := &whRow{ID: id, URL: u, Events: evs, CreatedAt: now, UpdatedAt: now}
	if name != nil {
		w.Name = sql.NullString{String: name.(string), Valid: true}
	}
	return c.OK(201, "webhook.created", map[string]any{"webhook": w.json(), "secret": secret})
}

func (h *whState) get(c *Ctx) error {
	w, err := h.load(c)
	if err != nil {
		return err
	}
	return c.OK(200, "webhook.info", map[string]any{"webhook": w.json()})
}

func (h *whState) update(c *Ctx) error {
	w, err := h.load(c)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	body, err := readBody(c)
	if err != nil {
		return err
	}
	if json.Unmarshal(body, &raw) != nil || len(raw) == 0 {
		return Err(400, "invalid_json", "Send at least one of name, url, events.")
	}
	var req struct {
		Name   *string  `json:"name"`
		URL    *string  `json:"url"`
		Events []string `json:"events"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Err(400, "unknown_field", "Request has an unknown field: "+strings.TrimPrefix(err.Error(), "json: "))
	}
	sets := map[string]any{}
	if _, ok := raw["name"]; ok {
		if req.Name == nil {
			sets["name"] = nil
		} else {
			n, err := whCheckName(req.Name, false)
			if err != nil {
				return err
			}
			sets["name"] = n
		}
	}
	if req.URL != nil {
		u, err := whCheckURL(*req.URL)
		if err != nil {
			return err
		}
		if err := h.s.checkOutboundURL(c.R.Context(), u, "invalid_url"); err != nil {
			return err
		}
		if err := h.urlLimit(c.P.User.ID, u, w.ID); err != nil {
			return err
		}
		sets["url"] = u
	}
	if _, ok := raw["events"]; ok {
		evs, err := whCheckEvents(req.Events)
		if err != nil {
			return err
		}
		sets["events"] = mustJSON(evs)
	}
	sets["updated_at"] = store.Now()
	parts, args := []string{}, []any{}
	for k, v := range sets {
		parts = append(parts, k+"=?")
		args = append(args, v)
	}
	if _, err := h.s.DB.Exec(`UPDATE webhooks SET `+strings.Join(parts, ",")+` WHERE id=?`, append(args, w.ID)...); err != nil {
		return err
	}
	w, _ = h.load(c)
	return c.OK(200, "webhook.updated", map[string]any{"webhook": w.json()})
}

func (h *whState) del(c *Ctx) error {
	w, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.s.DB.Tx(c.R.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM webhook_deliveries WHERE webhook_id=?`, w.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM webhooks WHERE id=?`, w.ID)
		return err
	}); err != nil {
		return err
	}
	return c.OK(200, "webhook.deleted", map[string]any{"id": w.ID})
}

// rotate replaces the secret. Attempts already in flight keep the old
// signature; receivers should accept both for whRotateGrace
// (previousSecretExpiresAt, a CrowdVM extension).
func (h *whState) rotate(c *Ctx) error {
	w, err := h.load(c)
	if err != nil {
		return err
	}
	secret := newWebhookSecret()
	sealed, err := h.s.sealSecret(secret)
	if err != nil {
		return err
	}
	now := store.Now()
	until := now + whRotateGrace.Milliseconds()
	if _, err := h.s.DB.Exec(`UPDATE webhooks SET prev_secret=secret, prev_until=?, secret=?, updated_at=? WHERE id=?`, until, sealed, now, w.ID); err != nil {
		return err
	}
	w, _ = h.load(c)
	c.W.Header().Set("Cache-Control", "no-store")
	return c.OK(200, "webhook.rotated", map[string]any{"webhook": w.json(), "secret": secret, "previousSecretExpiresAt": tsMs(until)})
}

// deliveries is the dashboard's delivery log (newest first, 30 days).
func (h *whState) deliveries(c *Ctx) error {
	w, err := h.load(c)
	if err != nil {
		return err
	}
	limit := 100
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Err(400, "invalid_limit", "limit must be an integer from 1 to 500.")
		}
		limit = min(n, 500)
	}
	rows, err := h.s.DB.Query(`SELECT id,event_id,event_type,payload,status,attempts,next_attempt_at,last_status,last_error,last_attempt_at,created_at,completed_at
FROM webhook_deliveries WHERE webhook_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, w.ID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var evID, typ, payload, status string
		var attempts int
		var next, lastStatus, lastAt, done sql.NullInt64
		var lastErr sql.NullString
		var created int64
		if err := rows.Scan(&id, &evID, &typ, &payload, &status, &attempts, &next, &lastStatus, &lastErr, &lastAt, &created, &done); err != nil {
			return err
		}
		var ls any
		if lastStatus.Valid {
			ls = lastStatus.Int64
		}
		out = append(out, map[string]any{"id": id, "eventId": evID, "event": typ, "payload": json.RawMessage(payload), "status": status,
			"attempts": attempts, "maxAttempts": whMaxAttempts, "nextRetryAt": tsNull(next), "httpStatus": ls, "error": nullStr(lastErr),
			"lastAttemptAt": tsNull(lastAt), "createdAt": tsMs(created), "completedAt": tsNull(done)})
	}
	return c.OK(200, "webhook.deliveries", map[string]any{"webhookId": w.ID, "deliveries": out})
}

// test sends one signed "webhook.test" event now and reports the answer.
func (h *whState) test(c *Ctx) error {
	w, err := h.load(c)
	if err != nil {
		return err
	}
	var sealed string
	if err := h.s.DB.QueryRow(`SELECT secret FROM webhooks WHERE id=?`, w.ID).Scan(&sealed); err != nil {
		return err
	}
	secret, err := h.s.openSecret(sealed)
	if err != nil {
		return err
	}
	ev := "evt_" + randHex(16)
	now := store.Now()
	body, _ := json.Marshal(map[string]any{"id": ev, "type": "webhook.test", "createdAt": tsMs(now),
		"data": map[string]any{"webhook": map[string]any{"id": w.ID, "name": nullStr(w.Name)}}})
	status, msg := whSend(withOutboundPolicy(c.R.Context(), h.s.Cfg.WebhookAllowLocal), w.URL, secret, ev, "webhook.test", body, 1)
	st := "delivered"
	if status/100 != 2 {
		st = "failed"
	}
	_, _ = h.s.DB.Exec(`INSERT INTO webhook_deliveries(webhook_id,event_id,event_type,payload,status,attempts,last_status,last_error,last_attempt_at,created_at,completed_at) VALUES(?,?,?,?,?,1,?,?,?,?,?)`,
		w.ID, ev, "webhook.test", string(body), st, nullInt(status), nz(msg), now, now, store.Now())
	var sv any
	if status != 0 {
		sv = status
	}
	return c.OK(200, "webhook.test", map[string]any{"test": map[string]any{"ok": status/100 == 2, "status": sv, "message": nz(msg), "eventId": ev}})
}
