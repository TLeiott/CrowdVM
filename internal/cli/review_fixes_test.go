package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Cobra turns `backticked` text in a flag's usage into the value placeholder
// ("--environment crowdvm env list"), so flag usages must not contain backticks.
func TestFlagUsageHasNoBackticks(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s --%s: usage has backticks: %q", c.CommandPath(), f.Name, f.Usage)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(a.rootCmd())
}

func TestDataRetentionEnableSendsBoatPhrase(t *testing.T) {
	e := newEnv(t)
	e.f.On("PATCH /api/v1/account/data-retention", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "account.data_retention", map[string]any{"enabled": true})
	})
	e.mustRun("data-retention", "enable", "--yes")
	if b := reqBody(t, e.f.Last("PATCH /api/v1/account/data-retention")); b["confirmation"] != "delete archived sandbox data" || b["enabled"] != true {
		t.Fatalf("body %v", b)
	}
}

func TestOrgDefaultVisibilityShowsWithoutPatch(t *testing.T) {
	e := newEnv(t)
	e.f.On("GET /api/v1/orgs", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "org.list", map[string]any{"orgs": []map[string]any{
			{"id": "usr_1", "type": "personal", "active": false},
			{"id": "org_1", "name": "Acme", "type": "org", "active": true, "newSandboxesPrivate": true}}})
	})
	e.f.On("PATCH /api/v1/orgs/{org}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "org.updated", map[string]any{"newSandboxesPrivate": false})
	})
	r := e.mustRun("org", "default-visibility")
	if r.last(t)["defaultVisibility"] != "private" || e.f.Count("PATCH ") != 0 {
		t.Fatalf("out %q, patches %d", r.out, e.f.Count("PATCH "))
	}
	e.mustRun("org", "default-visibility", "shared")
	if b := reqBody(t, e.f.Last("PATCH /api/v1/orgs/org_1")); b["newSandboxesPrivate"] != false {
		t.Fatalf("body %v", b)
	}
}

func TestEnvNewPrintsTheCreatedEnvironment(t *testing.T) {
	e := newEnv(t)
	e.f.On("POST /api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "environments.list", map[string]any{"environments": []map[string]any{
			{"id": "env_default", "name": "default"}, {"id": "env_2", "name": "web"}}})
	})
	r := e.mustRun("--json", "env", "new", "web")
	if l := r.last(t); l["id"] != "env_2" || l["name"] != "web" {
		t.Fatalf("out %s", r.out)
	}
}

func TestSnapshotPullAcceptsSandboxID(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("archived")
	e.f.On("GET /api/v1/sandboxes/{id}/snapshots/latest", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "snapshot", map[string]any{"snapshot": map[string]any{"id": "snap_9", "status": "completed"}})
	})
	e.f.On("GET /api/v1/snapshots/{sid}/files", func(w http.ResponseWriter, r *http.Request) {
		clienttest.Error(w, 404, "not_found", "none")
	})
	e.run("snapshot", "pull", id, "-o", e.dir+"/out")
	if e.f.Last("GET /api/v1/snapshots/snap_9/files") == nil {
		t.Fatalf("did not resolve %s to its newest snapshot", id)
	}
}

func TestSkillInstallAndPath(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.vars["HOME"] = home
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := e.mustRun("skill", "install") // only the agents found: codex
	if l := r.lines(t); len(l) != 1 || l[0]["path"] != filepath.Join(home, ".codex/skills/crowdvm") {
		t.Fatalf("install: %s", r.out)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex/skills/crowdvm/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	e.mustRun("skill", "install", "--agent", "claude", "--agent", "opencode")
	for _, d := range []string{".claude/skills/crowdvm/SKILL.md", ".config/opencode/skills/crowdvm/references/cli.md"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Fatal(err)
		}
	}
	installed := 0
	for _, l := range e.mustRun("skill", "path").lines(t) {
		if l["installed"] == true {
			installed++
		}
	}
	if installed != 3 {
		t.Fatalf("path: %d installed", installed)
	}
	if r := e.run("skill", "install", "--agent", "vim"); r.code == 0 {
		t.Fatal("unknown agent accepted")
	}
}

// bugs-r2-opus 4: a blocked deletion ends the wait at once with a
// non-"deleted" event and a non-zero exit, not a 10-minute wait and
// "deleted".
func TestDeleteBlockedIsNotDeleted(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	e.f.On("DELETE /api/v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		clienttest.OK(w, "sandbox.deleting", map[string]any{"operation": map[string]any{"id": "bdop_1", "kind": "sandbox", "targetId": id, "status": "processing"}})
	})
	e.f.On("GET /api/v1/deletion-operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "deletion_operation.info", map[string]any{"operation": map[string]any{"id": "bdop_1", "kind": "sandbox", "targetId": id,
			"status": "blocked", "lastError": "node_removed: 5 snapshot copy(ies) stayed on a removed node"}})
	})
	start := time.Now()
	r := e.run("delete", id, "--yes")
	if r.last(t)["code"] != "deletion_blocked" || r.last(t)["status"] != float64(409) || r.code == 0 || time.Since(start) > 30*time.Second || !strings.Contains(r.out, `"deletion_blocked"`) || strings.Contains(r.out, `"event":"deleted"`) {
		t.Fatalf("blocked deletion: exit %d after %s\n%s%s", r.code, time.Since(start), r.out, r.err)
	}
}
