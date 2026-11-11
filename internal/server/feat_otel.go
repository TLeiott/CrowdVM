package server

// OpenTelemetry export (Boat parity OTEL-001..010, API-022..024).
//
// One OTLP/HTTP JSON destination per account. Lifecycle events (from the
// webhook outbox, feat_webhooks.go) become log records on <endpoint>/v1/logs,
// retried like webhook deliveries. Every OTelMetricsInterval the account and
// sandbox gauges go to <endpoint>/v1/metrics; unsent windows are kept for at
// most 10 minutes and sent in time order. No traces, no agent/AI data.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// OTelMetricsInterval is the default metrics export period (Boat: 60 s);
// Config.OTelMetricsInterval overrides it per server (tests).
var OTelMetricsInterval = 60 * time.Second

// OTelPrefix prefixes metric and attribute names ("boat" keeps Boat dashboards working).
var OTelPrefix = "boat"

const (
	otelMaxHeaders  = 20
	otelMaxBacklog  = 10 * time.Minute
	otelMaxAttempts = 8
	otelServiceName = "crowdvm"
)

func init() {
	store.Register("otel", store.Migration{Version: 1, SQL: `
CREATE TABLE telemetry (
  owner_id TEXT PRIMARY KEY,
  endpoint TEXT NOT NULL,
  headers TEXT NOT NULL,
  header_names TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_export_at INTEGER,
  last_error TEXT
);
CREATE TABLE otel_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  record TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (owner_id, event_id)
);
CREATE INDEX otel_logs_due ON otel_logs(owner_id, next_at);
`})
	outboxConsumers = append(outboxConsumers, otelFanout)
	RegisterFeature("otel", func(s *Server) error {
		o := &otelState{s: s, kick: make(chan struct{}, 1), cpu: map[string]otelCPU{}, backlog: map[string][]otelWindow{}, holdUntil: map[string]time.Time{}}
		s.API("GET /telemetry", "account.admin", o.get)
		s.API("PUT /telemetry", "account.admin", o.put)
		s.API("DELETE /telemetry", "account.admin", o.del)
		s.Bus.Subscribe(func(e Event) {
			if isHookEvent(e.Type) {
				select { // logs follow the outbox fan-out; check soon
				case o.kick <- struct{}{}:
				default:
				}
			}
		})
		s.Go(o.loop)
		return nil
	})
}

type otelState struct {
	s    *Server
	kick chan struct{}

	mu        sync.Mutex
	cpu       map[string]otelCPU      // sandbox -> last /proc/stat sample
	flushing  map[string]bool         // owner -> a log flush runs
	backlog   map[string][]otelWindow // owner -> unsent metric exports
	holdUntil map[string]time.Time    // owner -> Retry-After / backoff
}

type otelCPU struct{ busy, total uint64 }

type otelWindow struct {
	at   time.Time
	body []byte
}

// ---------------------------------------------------------------------------
// OTLP JSON helpers
// ---------------------------------------------------------------------------

func otelAttr(k string, v any) map[string]any {
	var val map[string]any
	switch x := v.(type) {
	case string:
		val = map[string]any{"stringValue": x}
	case int:
		val = map[string]any{"intValue": strconv.Itoa(x)}
	case int64:
		val = map[string]any{"intValue": strconv.FormatInt(x, 10)}
	case float64:
		val = map[string]any{"doubleValue": x}
	case bool:
		val = map[string]any{"boolValue": x}
	default:
		val = map[string]any{"stringValue": fmt.Sprint(x)}
	}
	return map[string]any{"key": k, "value": val}
}

func otelName(n string) string { return OTelPrefix + "." + n }

// sandboxResource is the OTLP resource of one sandbox (OTEL-006).
func (s *Server) otelSandboxResource(id, name, size, owner string) map[string]any {
	attrs := []any{otelAttr("service.name", otelServiceName), otelAttr("service.instance.id", id),
		otelAttr(otelName("sandbox.id"), id), otelAttr(otelName("sandbox.name"), name), otelAttr(otelName("sandbox.size"), size),
		otelAttr(otelName("account.id"), owner)}
	if sp, ok := api.SizeByName(size); ok {
		attrs = append(attrs, otelAttr(otelName("sandbox.memory.size"), int64(sp.MemoryMiB)<<20), otelAttr(otelName("sandbox.disk.size"), int64(sp.DiskGiB)<<30))
	}
	return map[string]any{"attributes": attrs}
}

var otelSeverity = map[string][2]any{
	EvSandboxReady: {9, "INFO"}, EvSandboxHydrated: {9, "INFO"}, EvSandboxArchived: {9, "INFO"}, EvSandboxRecovered: {9, "INFO"},
	EvSandboxError: {17, "ERROR"}, EvSandboxDegraded: {13, "WARN"},
}

// otelFanout queues one OTLP log record per lifecycle event (OTEL-007).
func otelFanout(s *Server, tx *sql.Tx, ev *hookEvent) error {
	var n int
	if tx.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE owner_id=?`, ev.OwnerID).Scan(&n) != nil || n == 0 {
		return nil
	}
	var size string
	_ = tx.QueryRow(`SELECT size FROM sandboxes WHERE id=?`, ev.SandboxID).Scan(&size)
	sev := otelSeverity[ev.Type]
	nano := strconv.FormatInt(ev.CreatedAt*1e6, 10)
	attrs := []any{otelAttr("event.name", ev.Type), otelAttr(otelName("sandbox.id"), ev.SandboxID), otelAttr(otelName("sandbox.name"), ev.SandboxName),
		otelAttr(otelName("event.id"), ev.ID)}
	if ev.Type != EvSandboxHydrated && ev.State != "" {
		attrs = append(attrs, otelAttr(otelName("sandbox.state"), ev.State))
	}
	switch ev.Type {
	case EvSandboxReady, EvSandboxError, EvSandboxArchived:
		attrs = append(attrs, otelAttr(otelName("sandbox.previous_state"), ev.PrevState))
	case EvSandboxDegraded, EvSandboxRecovered:
		if ev.Reason.Valid {
			attrs = append(attrs, otelAttr(otelName("sandbox.reason"), ev.Reason.String))
		}
	}
	rec := map[string]any{
		"resource": s.otelSandboxResource(ev.SandboxID, ev.SandboxName, size, ev.OwnerID),
		"record": map[string]any{"timeUnixNano": nano, "observedTimeUnixNano": nano, "severityNumber": sev[0], "severityText": sev[1],
			"eventName": ev.Type, "body": map[string]any{"stringValue": ev.Type}, "attributes": attrs},
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO otel_logs(owner_id,event_id,record,next_at,created_at) VALUES(?,?,?,?,?)`,
		ev.OwnerID, ev.ID, mustJSON(rec), store.Now(), store.Now())
	return err
}

// ---------------------------------------------------------------------------
// Export loop
// ---------------------------------------------------------------------------

type otelDest struct {
	Owner, Endpoint string
	Headers         map[string]string
}

func (o *otelState) dests() []otelDest {
	rows, err := o.s.DB.Query(`SELECT owner_id, endpoint, headers FROM telemetry`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []otelDest
	for rows.Next() {
		var d otelDest
		var sealed string
		if rows.Scan(&d.Owner, &d.Endpoint, &sealed) != nil {
			continue
		}
		plain, err := o.s.openSecret(sealed)
		if err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(plain), &d.Headers)
		out = append(out, d)
	}
	return out
}

func (o *otelState) dest(owner string) (otelDest, bool) {
	for _, d := range o.dests() {
		if d.Owner == owner {
			return d, true
		}
	}
	return otelDest{}, false
}

// otelParallel bounds work without starting a goroutine for every item.
func otelParallel[T any](items []T, fn func(T)) {
	jobs := make(chan T)
	var wg sync.WaitGroup
	for range min(8, len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				fn(item)
			}
		}()
	}
	for _, item := range items {
		jobs <- item
	}
	close(jobs)
	wg.Wait()
}

func (o *otelState) loop(ctx context.Context) {
	ctx = withOutboundPolicy(ctx, o.s.Cfg.WebhookAllowLocal)
	every := o.s.Cfg.OTelMetricsInterval
	// Sampling and metrics delivery must never hold up lifecycle logs.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				otelParallel(o.dests(), func(d otelDest) {
					cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					o.collectMetrics(cctx, d)
					if !o.held(d.Owner) {
						o.flushMetrics(cctx, d)
					}
				})
				o.pruneCPU()
			}
		}
	}()
	defer wg.Wait()
	flush := time.NewTicker(min(time.Second, every))
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.kick:
		case <-flush.C:
		}
		// Each account's logs go out on their own: a slow collector holds up
		// only its own account, never the next round of the others
		// (bugs-r5-opus 5).
		for _, d := range o.dests() {
			if o.held(d.Owner) || !o.beginFlush(d.Owner) {
				continue
			}
			wg.Add(1)
			go func(d otelDest) {
				defer wg.Done()
				defer o.endFlush(d.Owner)
				o.flushLogs(ctx, d)
			}(d)
		}
	}
}

// beginFlush marks owner's log flush as running (false: one already is).
func (o *otelState) beginFlush(owner string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.flushing == nil {
		o.flushing = map[string]bool{}
	}
	if o.flushing[owner] {
		return false
	}
	o.flushing[owner] = true
	return true
}

func (o *otelState) endFlush(owner string) {
	o.mu.Lock()
	delete(o.flushing, owner)
	o.mu.Unlock()
}

// pruneCPU forgets the CPU samples of sandboxes that no longer run, so the
// map does not grow for the life of the server (bugs-r5-sonnet).
func (o *otelState) pruneCPU() {
	live := map[string]bool{}
	if rows, err := o.s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND state IN ('ready','idle','running')`); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				live[id] = true
			}
		}
		rows.Close()
	} else {
		return
	}
	o.mu.Lock()
	for id := range o.cpu {
		if !live[id] {
			delete(o.cpu, id)
		}
	}
	o.mu.Unlock()
}

func (o *otelState) held(owner string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Now().Before(o.holdUntil[owner])
}

func (o *otelState) hold(owner string, d time.Duration) {
	o.mu.Lock()
	o.holdUntil[owner] = time.Now().Add(d)
	o.mu.Unlock()
}

// otelPost sends one OTLP/HTTP JSON request. retry is true for 429, 502,
// 503, 504 and network errors; wait is the Retry-After delay.
func otelPost(ctx context.Context, d otelDest, path string, body []byte) (status int, msg string, retry bool, wait time.Duration) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", d.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error(), false, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CrowdVM-OTel/1")
	for k, v := range d.Headers {
		req.Header.Set(k, v)
	}
	resp, err := whClient.Do(req)
	if err != nil {
		return 0, "connection failed: " + err.Error(), true, 0
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		var ps struct {
			PartialSuccess struct {
				ErrorMessage string `json:"errorMessage"`
			} `json:"partialSuccess"`
		}
		_ = json.Unmarshal(raw, &ps)
		return resp.StatusCode, ps.PartialSuccess.ErrorMessage, false, 0
	}
	msg = "HTTP " + strconv.Itoa(resp.StatusCode)
	if t := strings.TrimSpace(string(raw)); t != "" {
		if len(t) > 300 {
			t = t[:300]
		}
		msg += ": " + t
	}
	switch resp.StatusCode {
	case 429, 502, 503, 504:
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			wait = time.Duration(min(n, 600)) * time.Second
		} else if t, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			wait = min(time.Until(t), 10*time.Minute)
		}
		return resp.StatusCode, msg, true, wait
	}
	return resp.StatusCode, msg, false, 0
}

func (o *otelState) setError(owner string, status int, msg string) {
	var st any
	if status != 0 {
		st = status
	}
	_, _ = o.s.DB.Exec(`UPDATE telemetry SET last_error=? WHERE owner_id=?`,
		mustJSON(map[string]any{"message": msg, "status": st, "at": ts(time.Now())}), owner)
}

func (o *otelState) flushLogs(ctx context.Context, d otelDest) {
	rows, err := o.s.DB.Query(`SELECT id, record, attempts FROM otel_logs WHERE owner_id=? AND next_at<=? ORDER BY id LIMIT 200`, d.Owner, store.Now())
	if err != nil {
		return
	}
	type item struct {
		id       int64
		rec      map[string]any
		attempts int
	}
	var items []item
	for rows.Next() {
		var it item
		var raw string
		if rows.Scan(&it.id, &raw, &it.attempts) == nil && json.Unmarshal([]byte(raw), &it.rec) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	if len(items) == 0 {
		return
	}
	// One resourceLogs per sandbox resource.
	byRes := map[string]map[string]any{}
	var order []string
	for _, it := range items {
		k := mustJSON(it.rec["resource"])
		rl := byRes[k]
		if rl == nil {
			rl = map[string]any{"resource": it.rec["resource"], "scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "crowdvm.lifecycle"}, "logRecords": []any{}}}}
			byRes[k] = rl
			order = append(order, k)
		}
		sl := rl["scopeLogs"].([]any)[0].(map[string]any)
		sl["logRecords"] = append(sl["logRecords"].([]any), it.rec["record"])
	}
	var rls []any
	for _, k := range order {
		rls = append(rls, byRes[k])
	}
	body, _ := json.Marshal(map[string]any{"resourceLogs": rls})
	status, msg, retry, wait := otelPost(ctx, d, "/v1/logs", body)
	if status/100 == 2 {
		for _, it := range items {
			_, _ = o.s.DB.Exec(`DELETE FROM otel_logs WHERE id=?`, it.id)
		}
		if msg != "" {
			o.setError(d.Owner, status, msg)
		}
		return
	}
	o.setError(d.Owner, status, msg)
	for _, it := range items {
		n := it.attempts + 1
		if !retry || n >= otelMaxAttempts {
			_, _ = o.s.DB.Exec(`DELETE FROM otel_logs WHERE id=?`, it.id)
			continue
		}
		delay := o.s.backoff(n)
		if wait > delay {
			delay = wait
		}
		_, _ = o.s.DB.Exec(`UPDATE otel_logs SET attempts=?, next_at=? WHERE id=?`, n, store.Now()+delay.Milliseconds(), it.id)
	}
	if wait > 0 {
		o.hold(d.Owner, wait)
	}
}

// flushMetrics sends queued windows in time order.
func (o *otelState) flushMetrics(ctx context.Context, d otelDest) {
	for {
		o.mu.Lock()
		q := o.backlog[d.Owner]
		cut := time.Now().Add(-otelMaxBacklog)
		for len(q) > 0 && q[0].at.Before(cut) {
			q = q[1:]
		}
		o.backlog[d.Owner] = q
		if len(q) == 0 {
			o.mu.Unlock()
			return
		}
		w := q[0]
		o.mu.Unlock()
		status, msg, retry, wait := otelPost(ctx, d, "/v1/metrics", w.body)
		if status/100 == 2 {
			_, _ = o.s.DB.Exec(`UPDATE telemetry SET last_export_at=? WHERE owner_id=?`, store.Now(), d.Owner)
			if msg != "" {
				o.setError(d.Owner, status, msg)
			}
		} else {
			o.setError(d.Owner, status, msg)
			if retry {
				if wait <= 0 {
					wait = 5 * time.Second
				}
				o.hold(d.Owner, wait)
				return
			}
		}
		o.mu.Lock()
		if q := o.backlog[d.Owner]; len(q) > 0 && q[0].at.Equal(w.at) {
			o.backlog[d.Owner] = q[1:]
		}
		o.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Metrics (OTEL-005, OTEL-009)
// ---------------------------------------------------------------------------

type otelGauge struct {
	name, unit string
	value      float64
}

func (o *otelState) collectMetrics(ctx context.Context, d otelDest) {
	now := time.Now()
	nano := strconv.FormatInt(now.UnixNano(), 10)
	gaugeMetric := func(g otelGauge) map[string]any {
		pt := map[string]any{"timeUnixNano": nano}
		if g.value == float64(int64(g.value)) && g.unit != "1" {
			pt["asInt"] = strconv.FormatInt(int64(g.value), 10)
		} else {
			pt["asDouble"] = g.value
		}
		return map[string]any{"name": otelName(g.name), "unit": g.unit, "gauge": map[string]any{"dataPoints": []any{pt}}}
	}
	var rms []any
	scope := map[string]any{"name": "crowdvm.metrics"}

	// Account gauges.
	acct := []any{gaugeMetric(otelGauge{"account.sandboxes.active", "{sandbox}", float64(o.s.activeCount(d.Owner))})}
	if lim := o.s.maxActiveFor(d.Owner); lim > 0 { // the wallet's policy, else the server default
		acct = append(acct, gaugeMetric(otelGauge{"account.sandboxes.limit", "{sandbox}", float64(lim)}))
	}
	if o.s.Cfg.EnforceCredits {
		// credits are machine hours of a default sandbox
		acct = append(acct, gaugeMetric(otelGauge{"account.credit.balance", "s", float64(int64(o.s.Balance(d.Owner) * 3600))}))
	}
	rms = append(rms, map[string]any{
		"resource":     map[string]any{"attributes": []any{otelAttr("service.name", otelServiceName), otelAttr(otelName("account.id"), d.Owner)}},
		"scopeMetrics": []any{map[string]any{"scope": scope, "metrics": acct}},
	})

	// Sandbox gauges: running sandboxes with monitoring on.
	rows, err := o.s.DB.Query(`SELECT `+sbCols+` FROM sandboxes WHERE owner_id=? AND deleted_at IS NULL AND monitoring=1 AND state IN ('ready','idle','running')`, d.Owner)
	var sbs []*Sandbox
	if err == nil {
		for rows.Next() {
			if sb, err := scanSandbox(rows); err == nil {
				sbs = append(sbs, sb)
			}
		}
		rows.Close()
	}
	var rmsMu sync.Mutex
	otelParallel(sbs, func(sb *Sandbox) {
		gs := o.sample(ctx, sb)
		if len(gs) == 0 {
			return
		}
		var ms []any
		for _, g := range gs {
			m := gaugeMetric(g)
			// Every point carries the sandbox id and name (series per sandbox).
			pt := m["gauge"].(map[string]any)["dataPoints"].([]any)[0].(map[string]any)
			pt["attributes"] = []any{otelAttr(otelName("sandbox.id"), sb.ID), otelAttr(otelName("sandbox.name"), sb.Name)}
			ms = append(ms, m)
		}
		rmsMu.Lock()
		rms = append(rms, map[string]any{"resource": o.s.otelSandboxResource(sb.ID, sb.Name, sb.Size, sb.OwnerID),
			"scopeMetrics": []any{map[string]any{"scope": scope, "metrics": ms}}})
		rmsMu.Unlock()
	})
	body, _ := json.Marshal(map[string]any{"resourceMetrics": rms})
	o.mu.Lock()
	o.backlog[d.Owner] = append(o.backlog[d.Owner], otelWindow{at: now, body: body})
	o.mu.Unlock()
}

// sample reads /proc in the guest (through the guest file API, which keeps
// no history) and returns the sandbox gauges it could compute.
func (o *otelState) sample(ctx context.Context, sb *Sandbox) []otelGauge {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := o.s.NodeForSandbox(sb)
	if err != nil {
		return nil
	}
	read := func(p string) string {
		var out struct {
			Content string `json:"content"`
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if conn.Do(cctx, "GET", "/v1/vms/"+sb.ID+"/guest/files?path="+url.QueryEscape(p), nil, &out) != nil {
			return ""
		}
		return out.Content
	}
	var gs []otelGauge
	if st := read("/proc/stat"); st != "" {
		if f := strings.Fields(strings.SplitN(st, "\n", 2)[0]); len(f) > 4 && f[0] == "cpu" {
			var total, idle uint64
			for i, v := range f[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				total += n
				if i == 3 || i == 4 { // idle, iowait
					idle += n
				}
			}
			cur := otelCPU{busy: total - idle, total: total}
			o.mu.Lock()
			prev, ok := o.cpu[sb.ID]
			o.cpu[sb.ID] = cur
			o.mu.Unlock()
			if ok && cur.total > prev.total {
				gs = append(gs, otelGauge{"sandbox.cpu.utilization", "1", float64(cur.busy-prev.busy) / float64(cur.total-prev.total)})
			} else if total > 0 {
				gs = append(gs, otelGauge{"sandbox.cpu.utilization", "1", float64(cur.busy) / float64(cur.total)})
			}
		}
	}
	if mi := read("/proc/meminfo"); mi != "" {
		kv := map[string]float64{}
		for _, line := range strings.Split(mi, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			f := strings.Fields(v)
			if len(f) > 0 {
				n, _ := strconv.ParseFloat(f[0], 64)
				kv[k] = n * 1024
			}
		}
		if tot := kv["MemTotal"]; tot > 0 {
			used := tot - kv["MemAvailable"]
			gs = append(gs, otelGauge{"sandbox.memory.usage", "By", used}, otelGauge{"sandbox.memory.utilization", "1", used / tot})
		}
	}
	if pr := read("/proc/pressure/io"); pr != "" {
		for _, f := range strings.Fields(strings.SplitN(pr, "\n", 2)[0]) {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					gs = append(gs, otelGauge{"sandbox.io.pressure", "1", n / 100})
				}
			}
		}
	}
	// Filesystem usage: the guest exposes it as /run/crowdvm/df ("<used> <total>"
	// bytes) when it can; otherwise the filesystem gauges are omitted.
	if df := strings.Fields(read("/run/crowdvm/df")); len(df) == 2 {
		used, _ := strconv.ParseFloat(df[0], 64)
		tot, _ := strconv.ParseFloat(df[1], 64)
		if tot > 0 {
			gs = append(gs, otelGauge{"sandbox.filesystem.usage", "By", used}, otelGauge{"sandbox.filesystem.utilization", "1", used / tot})
		}
	}
	sort.SliceStable(gs, func(i, j int) bool { return gs[i].name < gs[j].name })
	return gs
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

var otelHeaderName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]{1,128}$`)

// otelNormalize strips a trailing /v1/metrics, /v1/logs or /v1/traces.
// OTEL-004 (ADAPTED): http and LAN/Tailscale addresses are allowed.
func otelNormalize(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", Err(400, "invalid_endpoint", "endpoint must be an absolute http:// or https:// URL without credentials.")
	}
	p := strings.TrimRight(u.Path, "/")
	for _, suf := range []string{"/v1/metrics", "/v1/logs", "/v1/traces"} {
		p = strings.TrimSuffix(p, suf)
	}
	u.Path, u.RawQuery, u.Fragment = p, "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (o *otelState) json(owner string) (any, error) {
	var endpoint, names string
	var created int64
	var last sql.NullInt64
	var lastErr sql.NullString
	err := o.s.DB.QueryRow(`SELECT endpoint, header_names, created_at, last_export_at, last_error FROM telemetry WHERE owner_id=?`, owner).
		Scan(&endpoint, &names, &created, &last, &lastErr)
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var hn []string
	_ = json.Unmarshal([]byte(names), &hn)
	if hn == nil {
		hn = []string{}
	}
	var le any
	if lastErr.Valid {
		le = json.RawMessage(lastErr.String)
	}
	return map[string]any{"endpoint": endpoint, "headers": hn, "lastExportAt": tsNull(last), "lastError": le, "createdAt": tsMs(created)}, nil
}

func (o *otelState) get(c *Ctx) error {
	t, err := o.json(c.P.User.ID)
	if err != nil {
		return err
	}
	return c.OK(200, "telemetry.info", map[string]any{"telemetry": t})
}

func (o *otelState) put(c *Ctx) error {
	var req struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if len(req.Endpoint) > 2048 {
		return Err(400, "invalid_endpoint", "endpoint must be at most 2048 bytes.")
	}
	ep, err := otelNormalize(req.Endpoint)
	if err != nil {
		return err
	}
	if err := o.s.checkOutboundURL(c.R.Context(), ep, "invalid_endpoint"); err != nil {
		return err
	}
	if len(req.Headers) > otelMaxHeaders {
		return Err(400, "too_many_headers", fmt.Sprintf("At most %d headers.", otelMaxHeaders))
	}
	names := []string{}
	for k, v := range req.Headers {
		if !otelHeaderName.MatchString(k) {
			return Err(400, "invalid_header", "Invalid header name "+strconv.Quote(k)+".")
		}
		if len(v) > 8192 || strings.ContainsAny(v, "\r\n") {
			return Err(400, "invalid_header", "Header "+k+" has an invalid value (8192 characters max, one line).")
		}
		names = append(names, k)
	}
	sort.Strings(names)
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	sealed, err := o.s.sealSecret(mustJSON(req.Headers))
	if err != nil {
		return err
	}
	owner := c.P.User.ID
	now := store.Now()
	if _, err := o.s.DB.Exec(`INSERT INTO telemetry(owner_id,endpoint,headers,header_names,created_at) VALUES(?,?,?,?,?)
ON CONFLICT(owner_id) DO UPDATE SET endpoint=excluded.endpoint, headers=excluded.headers, header_names=excluded.header_names, last_error=NULL`,
		owner, ep, sealed, mustJSON(names), now); err != nil {
		return err
	}
	o.mu.Lock()
	delete(o.holdUntil, owner)
	o.mu.Unlock()
	// Test export: one empty metrics request (OTEL-002).
	status, msg, _, _ := otelPost(withOutboundPolicy(c.R.Context(), o.s.Cfg.WebhookAllowLocal), otelDest{Owner: owner, Endpoint: ep, Headers: req.Headers}, "/v1/metrics", []byte(`{"resourceMetrics":[]}`))
	ok := status/100 == 2
	if ok {
		_, _ = o.s.DB.Exec(`UPDATE telemetry SET last_export_at=? WHERE owner_id=?`, store.Now(), owner)
	}
	if !ok || msg != "" {
		o.setError(owner, status, msg)
	}
	t, err := o.json(owner)
	if err != nil {
		return err
	}
	var st any
	if status != 0 {
		st = status
	}
	return c.OK(200, "telemetry.updated", map[string]any{"telemetry": t, "test": map[string]any{"ok": ok, "status": st, "message": nz(msg)}})
}

func (o *otelState) del(c *Ctx) error {
	owner := c.P.User.ID
	res, err := o.s.DB.Exec(`DELETE FROM telemetry WHERE owner_id=?`, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("Telemetry export")
	}
	_, _ = o.s.DB.Exec(`DELETE FROM otel_logs WHERE owner_id=?`, owner)
	o.mu.Lock()
	delete(o.backlog, owner)
	delete(o.holdUntil, owner)
	o.mu.Unlock()
	return c.OK(200, "telemetry.deleted", map[string]any{"telemetry": nil})
}
