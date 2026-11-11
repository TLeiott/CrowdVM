package cli

import (
	"fmt"
	"github.com/crowdvm/crowdvm/internal/client/clienttest"
	"net/http"
	"strings"
	"testing"
)

func TestR3EnvAccess(t *testing.T) {
	for _, flag := range []string{"--remove", "--only"} {
		t.Run(flag, func(t *testing.T) {
			e := newEnv(t)
			e.f.On("GET /api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
				clienttest.OK(w, "environments.list", map[string]any{"environments": []any{map[string]any{"id": "env_1", "name": "web", "access": map[string]any{"restricted": true, "members": []string{"usr_bob", "usr_other"}}}}})
			})
			e.f.On("GET /api/v1/orgs/acme/members", func(w http.ResponseWriter, r *http.Request) {
				clienttest.OK(w, "org.members", map[string]any{"members": []any{map[string]any{"id": "usr_bob", "login": "bob", "email": "bob@example.test"}}})
			})
			e.f.On("PUT /api/v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
				clienttest.OK(w, "environment.updated", map[string]any{})
			})
			value := "bob"
			if flag == "--only" {
				value = ""
			}
			e.mustRun("--org", "acme", "env", "access", "web", flag, value)
			b := reqBody(t, e.f.Last("PUT /api/v1/environments/"))
			acc := b["access"].(map[string]any)
			wantEmpty := flag == "--only"
			if acc["restricted"] != true || (wantEmpty && acc["members"] != nil) || (!wantEmpty && fmt.Sprint(acc["members"]) != "[usr_other]") {
				t.Fatalf("access %v", acc)
			}
		})
	}
}

func TestR3ExecSignals(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "attach"} {
		for _, sig := range []string{"SIGKILL", "SIGTERM"} {
			t.Run(mode+sig, func(t *testing.T) {
				e := newEnv(t)
				id := e.f.AddSandbox("ready")
				e.f.On("POST /api/v1/sandboxes/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
					if mode == "stream" {
						fmt.Fprintf(w, "{\"type\":\"exit\",\"signal\":%q}\n", sig)
						return
					}
					clienttest.OK(w, "command", map[string]any{"exitCode": nil, "signal": sig})
				})
				e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, "{\"type\":\"exit\",\"signal\":%q}\n", sig)
				})
				e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}", func(w http.ResponseWriter, r *http.Request) {
					clienttest.OK(w, "command", map[string]any{"known": true, "status": "killed", "exitCode": nil, "signal": sig})
				})
				args := []string{"exec", id, "--json", "true"} // text mode streams; --json is the sync path
				if mode == "stream" {
					args = []string{"exec", id, "--stream", "true"}
				}
				if mode == "attach" {
					args = []string{"exec", id, "--attach", "12"}
				}
				r := e.run(args...)
				want := 137
				if sig == "SIGTERM" {
					want = 143
				}
				if r.code != want {
					t.Fatalf("exit %d want %d: %s %s", r.code, want, r.out, r.err)
				}
			})
		}
	}
}
func TestR3InfoFields(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	sb := e.f.Sandboxes[id]
	sb["directEndpoints"] = map[string]string{"tcp/80": "host:80"}
	sb["storage"] = map[string]any{"diskGB": 20}
	sb["imageId"] = "img_1"
	r := e.mustRun("info", id)
	for _, field := range []string{"directEndpoints", "storage", "imageId"} {
		if !strings.Contains(r.out, `"`+field+`"`) {
			t.Fatalf("missing %s: %s", field, r.out)
		}
	}
}
func TestR3UndrainLocalPause(t *testing.T) {
	e := newEnv(t)
	e.f.On("PATCH /api/v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "node.updated", map[string]any{"node": map[string]any{"name": "node", "info": map[string]any{"paused": true}}})
	})
	r := e.mustRun("nodes", "undrain", "node")
	if !strings.Contains(r.out+r.err, "crowdvm-node config set paused false") {
		t.Fatalf("%s %s", r.out, r.err)
	}
}
func TestR3KeyPresetAndCredits(t *testing.T) {
	e := newEnv(t)
	e.f.On("GET /api/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "keys", map[string]any{"apiKeys": []any{map[string]any{"id": "key_1", "preset": "read-only"}}})
	})
	if r := e.mustRun("api-key", "list"); !strings.Contains(r.out, `"preset":"read-only"`) {
		t.Fatalf("%s", r.out)
	}
	e.f.On("GET /api/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "credits", map[string]any{"wallet": map[string]any{"balanceCredits": 12}, "rates": map[string]any{"creditsPerDefaultHour": 1, "multipliers": map[string]any{"small": 0.5}}})
	})
	r := e.mustRun("--json=false", "credits")
	if strings.Contains(r.out, "map[") || !strings.Contains(r.out, "small: 0.5") {
		t.Fatalf("%s", r.out)
	}
}
