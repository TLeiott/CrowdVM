package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
)

// bugs-r12-opus 2: the CLI sends the control token only to a listener that
// proved it holds it.

// impostor stands on the UI port like a daemon, answers 200 to everything and
// records the Authorization header of every request.
type impostor struct {
	mu   sync.Mutex
	auth []string
	hits []string
}

// handler answers /api/hello with proof(nonce) (nothing when proof is nil).
func (i *impostor) handler(proof func(nonce string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		i.mu.Lock()
		i.auth = append(i.auth, r.Header.Get("Authorization"))
		i.hits = append(i.hits, r.Method+" "+r.URL.Path)
		i.mu.Unlock()
		if r.URL.Path == "/api/hello" && proof != nil {
			w.Write([]byte(`{"proof":"` + proof(r.URL.Query().Get("nonce")) + `"}`))
			return
		}
		w.Write([]byte(`{"applied":true,"code":"c0de"}`))
	}
}

func (i *impostor) sawToken() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, a := range i.auth {
		if a != "" {
			return true
		}
	}
	return false
}

// impostorDir makes a data dir with a control token whose UI address is h.
func impostorDir(t *testing.T, h http.HandlerFunc) (dir, token string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	token, err := node.UIToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := node.LoadConfig(dir)
	cfg.UIAddr = strings.TrimPrefix(srv.URL, "http://")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return dir, token
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestCLIRefusesListenerWithWrongProof(t *testing.T) {
	for name, proof := range map[string]func(string) string{
		"no proof":    nil,
		"wrong proof": func(n string) string { return node.HelloProof("not-the-token", n) },
		"fixed proof": func(string) string { return "00" },
	} {
		t.Run(name, func(t *testing.T) {
			var im impostor
			dir, token := impostorDir(t, im.handler(proof))
			out := captureStdout(t, func() {
				if err := run(t, "--data-dir", dir, "config", "set", "cpus", "2"); err != nil {
					t.Fatal(err)
				}
			})
			if strings.Contains(out, "Applied.") || !strings.Contains(out, "next start") {
				t.Fatalf("config set output: %q", out)
			}
			cfg, _ := node.LoadConfig(dir)
			if got, _ := cfg.Get("cpus"); got != "2" {
				t.Fatalf("config set did not write config.json itself: cpus=%q", got)
			}
			captureStdout(t, func() {
				if err := run(t, "--data-dir", dir, "status"); err != nil {
					t.Fatal(err)
				}
			})
			if err := run(t, "--data-dir", dir, "open", "--print"); err == nil || !strings.Contains(err.Error(), "not running") {
				t.Fatalf("open with an unverified listener: %v", err)
			}
			if im.sawToken() {
				t.Fatalf("the CLI sent the token to an unverified listener: %q (token %s)", im.auth, token)
			}
			for _, h := range im.hits {
				if h != "GET /api/hello" {
					t.Fatalf("the CLI called more than hello: %v", im.hits)
				}
			}
		})
	}
}

func TestCLITrustsDaemonWithRightProof(t *testing.T) {
	var im impostor
	var token string
	dir, tk := impostorDir(t, func(w http.ResponseWriter, r *http.Request) {
		im.handler(func(n string) string { return node.HelloProof(token, n) })(w, r)
	})
	token = tk
	out := captureStdout(t, func() {
		if err := run(t, "--data-dir", dir, "config", "set", "cpus", "2"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Applied.") || !im.sawToken() {
		t.Fatalf("verified daemon: output %q, token sent %v", out, im.sawToken())
	}
}
