package cli

import (
	"strings"
	"testing"
)

// Issue #17: CROWDVM_LABEL tags every sandbox the CLI creates, --label adds
// to it, and delete/stop refuse a sandbox without it unless overridden.
func TestLabelsFromEnvAndFlags(t *testing.T) {
	e := newEnv(t)
	e.vars["CROWDVM_LABEL"] = "agent=a, job=1"
	e.mustRun("new", "--label", "job=2", "--label", "x=y")
	got := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes"))["labels"].(map[string]any)
	if len(got) != 3 || got["agent"] != "a" || got["job"] != "2" || got["x"] != "y" {
		t.Fatalf("labels %v", got)
	}

	src := e.f.AddSandbox("running")
	e.f.Sandboxes[src]["labels"] = map[string]any{"agent": "a"}
	e.mustRun("fork", src)
	if got := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+src+"/fork"))["labels"].(map[string]any); got["agent"] != "a" {
		t.Fatalf("fork labels %v", got)
	}

	e.vars["CROWDVM_LABEL"] = "broken"
	if r := e.run("new"); r.code == 0 || !strings.Contains(r.err, "CROWDVM_LABEL wants KEY=VALUE") {
		t.Fatalf("bad CROWDVM_LABEL: %d %s", r.code, r.err)
	}
}

func TestNoLabelsWithoutEnv(t *testing.T) {
	e := newEnv(t)
	e.mustRun("new")
	if _, ok := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes"))["labels"]; ok {
		t.Fatal("no CROWDVM_LABEL and no --label must send no labels")
	}
	// No CROWDVM_LABEL: stop and delete work as before on any sandbox.
	id := e.f.AddSandbox("running")
	e.f.Sandboxes[id]["labels"] = map[string]any{"agent": "b"}
	e.mustRun("stop", id)
	e.mustRun("delete", id, "--yes")
}

func TestLabelGuard(t *testing.T) {
	e := newEnv(t)
	e.vars["CROWDVM_LABEL"] = "agent=a"
	mine, theirs, bare := e.f.AddSandbox("running"), e.f.AddSandbox("running"), e.f.AddSandbox("running")
	e.f.Sandboxes[mine]["labels"] = map[string]any{"agent": "a", "job": "7"}
	e.f.Sandboxes[theirs]["labels"] = map[string]any{"agent": "b"}

	for _, id := range []string{theirs, bare} {
		for _, args := range [][]string{{"stop", id}, {"delete", id, "--yes"}} {
			r := e.run(args...)
			if r.code == 0 || r.last(t)["code"] != "label_mismatch" {
				t.Fatalf("%v: %d %s", args, r.code, r.out)
			}
		}
	}
	if e.f.Count("POST /api/v1/sandboxes/"+theirs+"/stop") != 0 || e.f.Sandboxes[theirs] == nil {
		t.Fatal("the guard must refuse before the request")
	}
	r := e.run("delete", theirs, "--yes", "--json=false")
	if !strings.Contains(r.err, "agent=b") || !strings.Contains(r.err, "CROWDVM_LABEL= crowdvm delete") {
		t.Fatalf("message: %s", r.err)
	}

	e.mustRun("stop", mine)
	e.mustRun("delete", mine, "--yes")
	if r := e.run("snapshot", "delete", "--all", theirs, "--yes"); r.last(t)["code"] != "label_mismatch" {
		t.Fatalf("snapshot delete --all: %d %s", r.code, r.out)
	}
	e.vars["CROWDVM_LABEL"] = ""
	e.mustRun("delete", theirs, "--yes")
	e.mustRun("stop", bare)
}

func TestListLabels(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	r := e.mustRun("list", "--json=false")
	if strings.Contains(r.out, "LABELS") {
		t.Fatalf("no labels, no column: %s", r.out)
	}
	e.f.Sandboxes[id]["labels"] = map[string]any{"b": "2", "a": "1"}
	r = e.mustRun("list", "--json=false", "--label", "a=1")
	if !strings.Contains(r.out, "LABELS") || !strings.Contains(r.out, "a=1,b=2") {
		t.Fatalf("list: %s", r.out)
	}
	if q := e.f.Last("GET /api/v1/sandboxes").Query; !strings.Contains(q, "label=a%3D1") {
		t.Fatalf("server-side filter: %v", q)
	}
	if r := e.mustRun("info", id, "--json=false"); !strings.Contains(r.out, "labels:    a=1,b=2") {
		t.Fatalf("info: %s", r.out)
	}
}
