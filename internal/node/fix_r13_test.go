package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bugs-r13-opus 5 and 8 regression tests.

// signedTransport signs each request as the page's JS does: header
// "<nonce>.<unix-ms>.<sig>", sig keyed with the session's hmac part.
type signedTransport struct {
	sess string
	at   func() time.Time
	mut  func(sig string) string // optional: corrupt the signature
}

func (s signedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	nonce, key, _ := strings.Cut(s.sess, ".")
	ms := strconv.FormatInt(s.at().UnixMilli(), 10)
	sig := uiReqSig(key, r.Method, r.URL.RequestURI(), ms)
	if s.mut != nil {
		sig = s.mut(sig)
	}
	r.Header.Set(UISessionHeader, nonce+"."+ms+"."+sig)
	return http.DefaultTransport.RoundTrip(r)
}

func TestUIPageSignsRequests(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()
	sess := loginSession(t, newLogin(t, srv.URL, "tok"))
	now := time.Now

	ok := &http.Client{Transport: signedTransport{sess: sess, at: now}}
	if !getStatus(t, ok, srv.URL).Authorized {
		t.Fatal("correctly signed status is not authorized")
	}
	if r := uiPost(t, ok, srv.URL+"/api/pause", "", `{"paused":true}`); r.StatusCode != 200 {
		t.Fatalf("signed pause: %d", r.StatusCode)
	}

	// The bare session (what a squatter would have captured before) is refused.
	if getStatus(t, &http.Client{Transport: sessionBare(sess)}, srv.URL).Authorized {
		t.Fatal("bare session header is accepted")
	}
	if r := uiPost(t, &http.Client{Transport: sessionBare(sess)}, srv.URL+"/api/pause", "", `{"paused":false}`); r.StatusCode != 401 {
		t.Fatalf("pause with the bare session: %d", r.StatusCode)
	}
	// A wrong signature, a stale and a far-future timestamp are refused.
	bad := &http.Client{Transport: signedTransport{sess: sess, at: now, mut: func(s string) string {
		if s[0] == '0' {
			return "1" + s[1:]
		}
		return "0" + s[1:]
	}}}
	if getStatus(t, bad, srv.URL).Authorized {
		t.Fatal("wrong signature is accepted")
	}
	stale := &http.Client{Transport: signedTransport{sess: sess, at: func() time.Time { return time.Now().Add(-2 * time.Minute) }}}
	if getStatus(t, stale, srv.URL).Authorized {
		t.Fatal("stale timestamp is accepted")
	}
	future := &http.Client{Transport: signedTransport{sess: sess, at: func() time.Time { return time.Now().Add(2 * time.Minute) }}}
	if getStatus(t, future, srv.URL).Authorized {
		t.Fatal("future timestamp is accepted")
	}
	// A signature for another path or method does not carry over.
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	nonce, key, _ := strings.Cut(sess, ".")
	ms := strconv.FormatInt(time.Now().UnixMilli(), 10)
	req.Header.Set(UISessionHeader, nonce+"."+ms+"."+uiReqSig(key, "GET", "/api/other", ms))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Authorized {
		t.Fatal("signature for another path is accepted")
	}
	// A signed request still ends its session on logout.
	if r := uiPost(t, ok, srv.URL+"/api/logout", "", ""); r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if getStatus(t, ok, srv.URL).Authorized {
		t.Fatal("signed request authorized after logout")
	}
}

// sessionBare sends the session value itself, as the page did before.
type sessionBare string

func (s sessionBare) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(UISessionHeader, string(s))
	return http.DefaultTransport.RoundTrip(r)
}

// bugs-r13-opus 8: lowering cpus, memory or max-sandboxes drops pool VMs.
func TestWarmPoolDropsOverLoweredCapacity(t *testing.T) {
	for _, tc := range []struct{ key, val string }{{"cpus", "4"}, {"memory", "8G"}, {"max-sandboxes", "2"}} {
		t.Run(tc.key, func(t *testing.T) {
			h := start(t, func(c *Config) { c.WarmPool = 2; c.MaxSandboxes = 6 })
			h.srv.putImage("img1", []byte("base"))
			call(t, h.sess, "POST", "/v1/vms", spec("bx_o1")) // 2 vCPU / 4 GiB; teaches the pool its shape
			h.waitState(t, "bx_o1", "running")
			waitFor(t, "two ready pool vms", func() bool {
				h.node.mu.Lock()
				defer h.node.mu.Unlock()
				ready := 0
				for _, w := range h.node.warm {
					if w.ready {
						ready++
					}
				}
				return ready == 2
			})
			// 6 vCPU / 12 GiB / 3 vms in use; the new limit is below that.
			if _, err := h.node.SetConfig(tc.key, tc.val); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "pool vm dropped", func() bool {
				h.node.mu.Lock()
				defer h.node.mu.Unlock()
				return len(h.node.warm) < 2
			})
			time.Sleep(10 * warmTick) // and it does not refill over the limit
			h.node.mu.Lock()
			n := len(h.node.warm)
			over := func() bool {
				u, cp := h.node.usageWithWarmLocked(), h.node.config().Capacity()
				return u.VCPU > cp.VCPU || u.MemoryMiB > cp.MemoryMiB || u.VMs > cp.MaxSandboxes
			}()
			h.node.mu.Unlock()
			if n != 1 || over {
				t.Fatalf("pool %d vms, over capacity %v", n, over)
			}
		})
	}
}
