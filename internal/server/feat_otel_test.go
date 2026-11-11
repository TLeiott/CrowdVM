package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

type otlpReq struct {
	Path, Key string
	Body      map[string]any
	Raw       string
	Status    int
}

type fakeCollector struct {
	mu     sync.Mutex
	reqs   []otlpReq
	status func(path string, n int) int
	srv    *httptest.Server
}

func newFakeCollector(t *testing.T) *fakeCollector {
	f := &fakeCollector{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		code := 200
		if f.status != nil {
			if c := f.status(r.URL.Path, len(f.reqs)+1); c != 0 {
				code = c
			}
		}
		f.reqs = append(f.reqs, otlpReq{r.URL.Path, r.Header.Get("X-Api-Key"), m, string(b), code})
		f.mu.Unlock()
		if code != 200 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCollector) byPath(p string) []otlpReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []otlpReq
	for _, r := range f.reqs {
		if r.Path == p {
			out = append(out, r)
		}
	}
	return out
}

func TestOpenTelemetry(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) {
		c.OTelMetricsInterval = 200 * time.Millisecond
		c.WebhookBackoff = []time.Duration{50 * time.Millisecond}
	})
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	col := newFakeCollector(t)

	if r := env.Do(key, "GET", "/api/v1/telemetry", nil); r.Status != 200 || r.Get("telemetry") != nil {
		t.Fatalf("get empty: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/telemetry", nil); r.Status != 404 {
		t.Fatalf("delete empty: %s", r)
	}
	r := env.Do(key, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": col.srv.URL + "/v1/metrics/", "headers": map[string]string{"X-Api-Key": "s3cret"}})
	if r.Status != 200 || r.Str("type") != "telemetry.updated" || r.Get("test.ok") != true || r.Get("test.status").(float64) != 200 {
		t.Fatalf("put: %s", r)
	}
	if r.Str("telemetry.endpoint") != col.srv.URL || r.Get("telemetry.headers.0") != "X-Api-Key" || strings.Contains(string(r.Body), "s3cret") {
		t.Fatalf("telemetry: %s", r)
	}
	if ms := col.byPath("/v1/metrics"); len(ms) != 1 || ms[0].Key != "s3cret" || ms[0].Raw != `{"resourceMetrics":[]}` {
		t.Fatalf("test export: %+v", ms)
	}

	// A sandbox with /proc files (fake guest serves files written through the API).
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	for p, c := range map[string]string{
		"/proc/stat":        "cpu  100 0 100 700 100 0 0 0 0 0\n",
		"/proc/meminfo":     "MemTotal:       8000 kB\nMemAvailable:   2000 kB\n",
		"/proc/pressure/io": "some avg10=1.00 avg60=5.00 avg300=0.00 total=1\nfull avg10=0 avg60=0 avg300=0 total=0\n",
	} {
		if r := env.Do(key, "PUT", "/api/v1/sandboxes/"+id+"/files", map[string]any{"path": p, "content": c}); r.Status != 200 {
			t.Fatalf("write %s: %s", p, r)
		}
	}
	monOff := createSandbox(t, env, key, map[string]any{"monitoring": false})
	env.WaitState(key, monOff, "ready")

	// Lifecycle event as an OTLP log record.
	var rec map[string]any
	env.Eventually("ready log record", func() bool {
		for _, lr := range col.byPath("/v1/logs") {
			for _, rl := range lr.Body["resourceLogs"].([]any) {
				for _, x := range rl.(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any) {
					m := x.(map[string]any)
					if m["eventName"] == "sandbox.ready" && strings.Contains(otelJSON(m), id) {
						rec = m
						return true
					}
				}
			}
		}
		return false
	})
	if rec["severityText"] != "INFO" || rec["severityNumber"].(float64) != 9 {
		t.Fatalf("log record: %v", rec)
	}
	attrs := otelJSON(rec["attributes"])
	for _, want := range []string{`"event.name"`, `"boat.sandbox.state"`, `"boat.sandbox.previous_state"`, `"boat.event.id"`, `"boat.sandbox.name"`} {
		if !strings.Contains(attrs, want) {
			t.Fatalf("missing %s in %s", want, attrs)
		}
	}

	// Metrics: sandbox gauges for the monitored sandbox only. A window
	// collected while the /proc files were being written lacks some gauges,
	// so wait for any window that has them all.
	env.Eventually("sandbox metrics", func() bool {
		for _, m := range col.byPath("/v1/metrics") {
			if strings.Contains(m.Raw, monOff) {
				t.Fatalf("monitoring-off sandbox exported metrics: %s", m.Raw)
			}
			if strings.Contains(m.Raw, "boat.account.sandboxes.active") && strings.Contains(m.Raw, "boat.sandbox.memory.utilization") &&
				strings.Contains(m.Raw, `"asDouble":0.75`) && strings.Contains(m.Raw, `"boat.sandbox.io.pressure"`) && strings.Contains(m.Raw, `"service.instance.id"`) {
				return true
			}
		}
		return false
	})

	// 503 with Retry-After: logs are retried, then lastError is visible.
	col.mu.Lock()
	failing := true // guarded by col.mu (status runs under it)
	col.status = func(p string, n int) int {
		if failing && p == "/v1/logs" {
			return 503
		}
		return 0
	}
	col.mu.Unlock()
	before := len(col.byPath("/v1/logs"))
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	// Wait for the archived record itself to fail (another record, e.g. the
	// second sandbox's ready, may fail first).
	env.Eventually("a failed archived export", func() bool {
		for _, lr := range col.byPath("/v1/logs")[before:] {
			if lr.Status != 200 && strings.Contains(lr.Raw, "sandbox.archived") {
				return true
			}
		}
		return false
	})
	env.WaitFor(key, "/api/v1/telemetry", "telemetry.lastError.status", 30*time.Second, "503")
	col.mu.Lock()
	failing = false
	col.mu.Unlock()
	env.Eventually("archived log retried", func() bool {
		failed := false
		for _, lr := range col.byPath("/v1/logs")[before:] {
			if strings.Contains(lr.Raw, "sandbox.archived") {
				if lr.Status != 200 {
					failed = true
				} else if failed {
					return true // delivered after a failed attempt
				}
			}
		}
		return false
	})
	if r := env.Do(key, "GET", "/api/v1/telemetry", nil); r.Get("telemetry.lastExportAt") == nil {
		t.Fatalf("lastExportAt: %s", r)
	}

	if r := env.Do(key, "DELETE", "/api/v1/telemetry", nil); r.Status != 200 || r.Get("telemetry") != nil {
		t.Fatalf("delete: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": "ftp://x"}); r.Status != 400 {
		t.Fatalf("bad endpoint: %s", r)
	}
}

func otelJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
