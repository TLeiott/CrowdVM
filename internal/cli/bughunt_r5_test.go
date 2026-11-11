package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// `crowdvm logout` in one terminal while `crowdvm new` runs in another: new
// saves the whole config it loaded at its start, writing the signed-out key
// back.
func TestBughuntR5LogoutUndoneByConcurrentCommand(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "CROWDVM_API_KEY")
	delete(e.vars, "CROWDVM_API_BASE")
	target, _ := url.Parse(e.f.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	hold, arrived := make(chan struct{}), make(chan struct{})
	var once sync.Once
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/sandboxes" {
			once.Do(func() { close(arrived) })
			<-hold
		}
		rp.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	e.mustRun("login", "--server", proxy.URL, "--api-key", clienttest.Key)
	done := make(chan result, 1)
	go func() { done <- e.run("new", "--json") }()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("create not sent")
	}
	e.mustRun("logout")
	close(hold)
	if r := <-done; r.code != 0 {
		t.Fatalf("new: %d %s", r.code, r.err)
	}
	cfg, err := loadConfig(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["default"]; p != nil && p.APIKey != "" {
		t.Fatalf("logout was undone by a command that started before it: apiKey %q is back in config.json", p.APIKey)
	}
}

// Two CLI processes saving at once (e.g. `new` in two shells) share one
// fixed temp file name: one rename fails or the result is corrupt JSON.
func TestBughuntR5ConcurrentSaveConfig(t *testing.T) {
	dir := t.TempDir()
	var failures []string
	var mu sync.Mutex
	for iter := 0; iter < 200; iter++ {
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				cfg := &Config{Profiles: map[string]*Profile{"default": {Server: "http://s", APIKey: "cvm_key", Current: strings.Repeat("x", 50+g*400)}}}
				if err := saveConfig(dir, cfg); err != nil {
					mu.Lock()
					failures = append(failures, "save: "+err.Error())
					mu.Unlock()
				}
			}(g)
		}
		wg.Wait()
		if _, err := loadConfig(dir); err != nil {
			failures = append(failures, fmt.Sprintf("iteration %d: config.json unreadable: %v", iter, err))
		}
	}
	if len(failures) > 0 {
		corrupt := 0
		for _, f := range failures {
			if strings.Contains(f, "unreadable") {
				corrupt++
			}
		}
		t.Fatalf("%d failures (%d corrupt config.json), e.g. %s", len(failures), corrupt, failures[len(failures)-1])
	}
}

// `config set server` drops the saved key because "a key belongs to one
// server", but --api-url (or CROWDVM_API_BASE) sends that key to any server.
func TestBughuntR5APIURLOverrideSendsSavedKeyElsewhere(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "CROWDVM_API_KEY")
	delete(e.vars, "CROWDVM_API_BASE")
	e.mustRun("login", "--server", e.f.URL, "--api-key", clienttest.Key)
	var mu sync.Mutex
	var got []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "nope", 401)
	}))
	defer other.Close()
	e.run("--api-url", other.URL, "list")
	mu.Lock()
	defer mu.Unlock()
	for _, h := range got {
		if strings.Contains(h, clienttest.Key) {
			t.Fatalf("the key saved for %s was sent to %s: %q", e.f.URL, other.URL, h)
		}
	}
}
