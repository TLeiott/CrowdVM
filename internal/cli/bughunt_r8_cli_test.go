package cli

import (
	"net/http"
	"os/signal"
	"syscall"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// SIGQUIT (Ctrl-\) must cancel the CLI's context like SIGINT, so exec stops
// the remote command; a SIGQUIT inherited as ignored stays ignored
// (bugs-r8-opus 5).
func TestBughuntR8CancelSignals(t *testing.T) {
	has := func(want syscall.Signal) bool {
		for _, s := range cancelSignals() {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(syscall.SIGQUIT) || !has(syscall.SIGHUP) {
		t.Fatalf("SIGQUIT and SIGHUP must cancel: %v", cancelSignals())
	}
	signal.Ignore(syscall.SIGQUIT)
	defer signal.Reset(syscall.SIGQUIT)
	if has(syscall.SIGQUIT) {
		t.Fatal("an ignored SIGQUIT must stay ignored")
	}
}

// Any end of the follow loop other than the command's exit stops the command,
// not only Ctrl-C and the timeout (bugs-r8-opus 5).
func TestBughuntR8ExecStopsCommandWhenFollowFails(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		clienttest.Error(w, 404, "not_found", "no such process")
	})
	stop := "DELETE /api/v1/sandboxes/" + id + "/commands/4242"
	r := e.run("exec", id, "--timeout", "700", "sleep 1000")
	if r.code == 0 {
		t.Fatalf("expected failure, got %+v", r)
	}
	if e.f.Count(stop) != 1 {
		t.Fatalf("command not stopped after the follow loop failed (stop calls: %d)", e.f.Count(stop))
	}
}

// `nodes update --label ""` removes all labels (bugs-r8-opus 10).
func TestBughuntR8NodesUpdateClearLabels(t *testing.T) {
	e := newEnv(t)
	e.f.On("PATCH /api/v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "node", map[string]any{"node": map[string]any{"id": "n1", "name": "n1"}})
	})
	e.mustRun("nodes", "update", "n1", "--label", "")
	b := reqBody(t, e.f.Find("PATCH", "/api/v1/nodes/n1"))
	m, ok := b["labels"].(map[string]any)
	if !ok || len(m) != 0 {
		t.Fatalf("want labels {}, got %v", b)
	}
	e.mustRun("nodes", "update", "n1", "--label", "a=b")
	b = reqBody(t, e.f.Find("PATCH", "/api/v1/nodes/n1"))
	if m, _ := b["labels"].(map[string]any); m["a"] != "b" {
		t.Fatalf("labels: %v", b)
	}
}
