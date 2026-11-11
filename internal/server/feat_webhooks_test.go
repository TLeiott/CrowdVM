package server_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

type hookHit struct {
	Header http.Header
	Body   []byte
}

// hookReceiver records deliveries; fail decides (per request) to answer 500.
type hookReceiver struct {
	mu   sync.Mutex
	hits []hookHit
	fail func(n int) bool
	srv  *httptest.Server
}

func newHookReceiver(t *testing.T) *hookReceiver {
	h := &hookReceiver{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.hits = append(h.hits, hookHit{r.Header.Clone(), b})
		n := len(h.hits)
		f := h.fail
		h.mu.Unlock()
		if f != nil && f(n) {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookReceiver) all() []hookHit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookHit(nil), h.hits...)
}

// verify is the SDK verification algorithm (sdk/python/src/crowdvm/webhooks.py).
func verifyHook(hit hookHit, secret, prefix string) bool {
	d, ts := hit.Header.Get(prefix+"Delivery"), hit.Header.Get(prefix+"Timestamp")
	sig := strings.TrimPrefix(hit.Header.Get(prefix+"Signature"), "v1=")
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(d + "." + ts + "."))
	m.Write(hit.Body)
	return hmac.Equal([]byte(sig), []byte(hex.EncodeToString(m.Sum(nil))))
}

func TestWebhooks(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.WebhookBackoff = []time.Duration{50 * time.Millisecond} })
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	recv := newHookReceiver(t)

	// Validation.
	if r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"url": recv.srv.URL}); r.Status != 400 || r.Code() != "invalid_events" {
		t.Fatalf("no events: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"url": "ftp://x", "events": []string{"sandbox.ready"}}); r.Status != 400 {
		t.Fatalf("bad url: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"url": recv.srv.URL, "events": []string{"ready"}}); r.Status != 400 {
		t.Fatalf("short event name: %s", r)
	}
	// Scope: account.admin.
	devKey := env.Key(env.Admin, "ci")
	if r := env.Do(devKey, "GET", "/api/v1/webhooks", nil); r.Status != 403 {
		t.Fatalf("scope: %s", r)
	}

	r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"name": "prod", "url": recv.srv.URL,
		"events": []string{"sandbox.ready", "sandbox.archived", "sandbox.hydrated"}})
	if r.Status != 201 || r.Str("type") != "webhook.created" || !strings.HasPrefix(r.Str("secret"), "whsec_") || len(r.Str("secret")) != 70 {
		t.Fatalf("create: %s", r)
	}
	whID, secret := r.Str("webhook.id"), r.Str("secret")
	if len(whID) != 27 || r.Str("webhook.name") != "prod" {
		t.Fatalf("webhook: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/webhooks/"+whID, nil); r.Status != 200 || strings.Contains(string(r.Body), "whsec_") {
		t.Fatalf("get: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/webhooks", nil); r.Status != 200 || len(r.Get("webhooks").([]any)) != 1 {
		t.Fatalf("list: %s", r)
	}

	// Lifecycle: ready, then stop -> archived; resume -> ready + hydrated (a restore).
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Eventually("ready delivery", func() bool { return len(recv.all()) >= 1 })
	hit := recv.all()[0]
	if !verifyHook(hit, secret, "X-Ascii-") || !verifyHook(hit, secret, "X-CrowdVM-") {
		t.Fatalf("signature mismatch: %v", hit.Header)
	}
	if hit.Header.Get("X-Ascii-Event") != "sandbox.ready" || hit.Header.Get("X-Ascii-Attempt") != "1" || !strings.HasPrefix(hit.Header.Get("X-Ascii-Delivery"), "evt_") {
		t.Fatalf("headers: %v", hit.Header)
	}
	var ev map[string]any
	_ = json.Unmarshal(hit.Body, &ev)
	data := ev["data"].(map[string]any)
	if ev["id"] != hit.Header.Get("X-Ascii-Delivery") || ev["type"] != "sandbox.ready" || data["sandbox"].(map[string]any)["id"] != id ||
		data["state"] != "ready" || data["previousState"] == nil || len(ev["id"].(string)) != 36 {
		t.Fatalf("payload: %s", hit.Body)
	}

	// Rotate, then fail the first attempts of the next deliveries: retried with the new secret.
	r = env.Do(key, "POST", "/api/v1/webhooks/"+whID+"/rotate", nil)
	if r.Status != 200 || r.Str("secret") == secret {
		t.Fatalf("rotate: %s", r)
	}
	secret = r.Str("secret")
	recv.mu.Lock()
	recv.fail = func(n int) bool { return n == 2 }
	recv.mu.Unlock()
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status/100 != 2 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	env.Eventually("archived delivered", func() bool {
		for _, h := range recv.all() {
			if h.Header.Get("X-Ascii-Event") == "sandbox.archived" {
				return true
			}
		}
		return false
	})
	count := func(ev string) (n int) {
		for _, h := range recv.all() {
			if h.Header.Get("X-Ascii-Event") == ev {
				n++
			}
		}
		return n
	}
	if n := count("sandbox.hydrated"); n != 0 {
		t.Fatalf("hydrated on stop: %d", n)
	}
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	env.Eventually("hydrated after resume", func() bool { return count("sandbox.hydrated") == 1 })
	for _, h := range recv.all()[1:] {
		if !verifyHook(h, secret, "X-Ascii-") {
			t.Fatalf("signature after rotate: %v", h.Header)
		}
	}

	// Delivery log shows attempts.
	defer func() {
		if t.Failed() {
			t.Logf("deliveries: %s", env.Do(key, "GET", "/api/v1/webhooks/"+whID+"/deliveries", nil))
			for i, h := range recv.all() {
				t.Logf("hit %d: %s %s", i+1, h.Header.Get("X-Ascii-Event"), h.Body)
			}
		}
	}()
	env.Eventually("a delivery retried to success", func() bool { // hit #2 failed once
		r = env.Do(key, "GET", "/api/v1/webhooks/"+whID+"/deliveries", nil)
		ds, _ := r.Get("deliveries").([]any)
		for _, d := range ds {
			m := d.(map[string]any)
			if m["attempts"].(float64) == 2 && m["status"] == "delivered" && m["error"] == nil {
				return true
			}
		}
		return false
	})

	// Test delivery.
	if r := env.Do(key, "POST", "/api/v1/webhooks/"+whID+"/test", nil); r.Status != 200 || r.Get("test.ok") != true {
		t.Fatalf("test: %s", r)
	}

	// Patch: events only, then delete.
	r = env.Do(key, "PATCH", "/api/v1/webhooks/"+whID, map[string]any{"events": []string{"sandbox.error"}, "name": nil})
	if r.Status != 200 || r.Get("webhook.name") != nil || r.Get("webhook.events.0") != "sandbox.error" {
		t.Fatalf("patch: %s", r)
	}
	if r := env.Do(key, "PATCH", "/api/v1/webhooks/"+whID, map[string]any{"bogus": 1}); r.Status != 400 {
		t.Fatalf("patch unknown: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/webhooks/"+whID, nil); r.Status != 200 || r.Str("id") != whID {
		t.Fatalf("delete: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/webhooks/"+whID, nil); r.Status != 404 {
		t.Fatalf("after delete: %s", r)
	}
}

func TestWebhookURLLimit(t *testing.T) {
	env := servertest.New(t)
	for i := 0; i < 10; i++ {
		if r := env.Do(env.AdminKey, "POST", "/api/v1/webhooks", map[string]any{"url": "http://10.0.0.1/h" + string(rune('a'+i)), "events": []string{"sandbox.ready"}}); r.Status != 201 {
			t.Fatalf("create %d: %s", i, r)
		}
	}
	// Same URL again is fine; an 11th URL is not.
	if r := env.Do(env.AdminKey, "POST", "/api/v1/webhooks", map[string]any{"url": "http://10.0.0.1/ha", "events": []string{"sandbox.error"}}); r.Status != 201 {
		t.Fatalf("dup url: %s", r)
	}
	if r := env.Do(env.AdminKey, "POST", "/api/v1/webhooks", map[string]any{"url": "http://10.0.0.1/zz", "events": []string{"sandbox.ready"}}); r.Status != 409 {
		t.Fatalf("limit: %s", r)
	}
}
