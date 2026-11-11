package cli

import (
	"net/http"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

func TestNodesEvacuate(t *testing.T) {
	e := newEnv(t)
	e.f.On("POST /api/v1/nodes/{id}/evacuate", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "node.evacuating", map[string]any{"node": map[string]any{"id": "n_1", "name": "lab",
			"evacuation": map[string]any{"startedAt": "2026-11-11T10:00:00.000Z", "deadline": "2026-11-11T10:10:00.000Z"}, "noSnapshotSandboxes": 1}})
	})
	e.f.On("DELETE /api/v1/nodes/{id}/evacuate", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "node.evacuation_cancelled", map[string]any{"node": map[string]any{"id": "n_1", "name": "lab"}})
	})
	if r := e.mustRun("nodes", "evacuate", "n_1").last(t); mapOf(r["node"])["id"] != "n_1" {
		t.Fatalf("output %v", r)
	}
	if b := reqBody(t, e.f.Find("POST", "/api/v1/nodes/n_1/evacuate")); b["graceSec"] != float64(600) {
		t.Fatalf("default grace %v", b)
	}
	e.mustRun("nodes", "evacuate", "n_1", "--grace", "0")
	if b := reqBody(t, e.f.Find("POST", "/api/v1/nodes/n_1/evacuate")); b["graceSec"] != float64(0) {
		t.Fatalf("grace 0 %v", b)
	}
	e.mustRun("nodes", "evacuate", "n_1", "--cancel")
	if e.f.Find("DELETE", "/api/v1/nodes/n_1/evacuate") == nil {
		t.Fatal("cancel did not call DELETE")
	}
	if r := e.runIn("", "nodes", "evacuate", "n_1", "--grace", "-1m"); r.code == 0 {
		t.Fatal("negative grace accepted")
	}
}
