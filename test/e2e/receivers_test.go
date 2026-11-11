package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// receivers are host-side HTTP servers the CrowdVM server talks to: a webhook
// receiver, an OTLP/HTTP collector and a fake AI vendor (Anthropic-shaped).
type receivers struct {
	mu       sync.Mutex
	webhooks []hookDelivery
	otlp     []otlpRequest
	vendor   []vendorRequest
	secret   string // webhook signing secret (set after create)

	hookURL, otlpURL, vendorURL string
	srvs                        []*http.Server
}

type hookDelivery struct {
	at       time.Time
	header   http.Header
	body     []byte
	event    map[string]any
	sigValid bool
}

type otlpRequest struct {
	path string
	body []byte
}

type vendorRequest struct {
	path   string
	header http.Header
	body   []byte
}

const fakeVendorKey = "sk-ant-e2e-real-key-0123456789"

func startReceivers(t *testing.T) *receivers {
	r := &receivers{}
	r.hookURL = r.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		d := hookDelivery{at: time.Now(), header: req.Header.Clone(), body: b}
		_ = json.Unmarshal(b, &d.event)
		r.mu.Lock()
		d.sigValid = verifySig(r.secret, req.Header, b)
		r.webhooks = append(r.webhooks, d)
		r.mu.Unlock()
		w.WriteHeader(204)
	}))
	r.otlpURL = r.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.otlp = append(r.otlp, otlpRequest{path: req.URL.Path, body: b})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	r.vendorURL = r.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.vendor = append(r.vendor, vendorRequest{path: req.URL.Path, header: req.Header.Clone(), body: b})
		r.mu.Unlock()
		if req.Header.Get("X-Api-Key") != fakeVendorKey && req.Header.Get("Authorization") != "Bearer "+fakeVendorKey {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-e2e-fake",`+
			`"content":[{"type":"text","text":"pong from fake vendor"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	return r
}

// serve listens on 127.0.0.1:0 and returns the base URL.
func (r *receivers) serve(t *testing.T, h http.Handler) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	s := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.Serve(l) }()
	r.srvs = append(r.srvs, s)
	return "http://" + l.Addr().String()
}

func (r *receivers) close() {
	for _, s := range r.srvs {
		_ = s.Close()
	}
}

// serverEnv points the server's AI proxy at the fake vendor.
func (r *receivers) serverEnv() []string {
	return []string{"CROWDVM_AI_UPSTREAM_ANTHROPIC=" + r.vendorURL}
}

func (r *receivers) setSecret(s string) {
	r.mu.Lock()
	r.secret = s
	r.mu.Unlock()
}

// verifySig checks X-CrowdVM-Signature: v1=hex(HMAC_SHA256(secret, delivery.timestamp.body)).
func verifySig(secret string, h http.Header, body []byte) bool {
	if secret == "" {
		return false
	}
	sig := h.Get("X-CrowdVM-Signature")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h.Get("X-CrowdVM-Delivery") + "." + h.Get("X-CrowdVM-Timestamp") + "."))
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	for _, s := range strings.Split(sig, ",") {
		if hmac.Equal([]byte(strings.TrimSpace(s)), []byte(want)) {
			return true
		}
	}
	return false
}

// hooksFor returns webhook deliveries of the given type for a sandbox id ("" = any).
func (r *receivers) hooksFor(typ, sandboxID string) []hookDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hookDelivery
	for _, d := range r.webhooks {
		if typ != "" && d.event["type"] != typ {
			continue
		}
		if sandboxID != "" && gets(d.event, "data.sandbox.id") != sandboxID {
			continue
		}
		out = append(out, d)
	}
	return out
}

// waitHook waits for a webhook event of type typ for the sandbox.
func (c *cluster) waitHook(t testing.TB, typ, sandboxID string, timeout time.Duration) hookDelivery {
	t.Helper()
	var got []hookDelivery
	waitFor(t, timeout, 500*time.Millisecond, "webhook "+typ+" for "+sandboxID, func() (bool, string) {
		got = c.hooks.hooksFor(typ, sandboxID)
		return len(got) > 0, fmt.Sprintf("%d deliveries total", len(c.hooks.hooksFor("", "")))
	})
	d := got[0]
	if !d.sigValid {
		t.Errorf("webhook %s for %s: signature did not verify (headers %v)", typ, sandboxID, d.header)
	}
	return d
}

func (r *receivers) otlpReqs(path string) []otlpRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []otlpRequest
	for _, o := range r.otlp {
		if o.path == path {
			out = append(out, o)
		}
	}
	return out
}

func (r *receivers) vendorReqs() []vendorRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]vendorRequest(nil), r.vendor...)
}
