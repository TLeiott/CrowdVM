package cli

import (
	"strings"
	"testing"
)

// Field test: `crowdvm ssh desk1 -- hostname` said "no sandbox id given",
// and `crowdvm exec lat1 -- ...` ran `lat1` as a command in the current
// sandbox. A sandbox's name works where its id does.
func TestSandboxNameInPlaceOfID(t *testing.T) {
	e := newEnv(t)
	attachFrames(e, map[string]any{"type": "exit", "exitCode": 0})
	cur := e.f.AddSandbox("ready")
	id := e.f.AddSandbox("ready")
	e.f.Sandboxes[id]["name"] = "desk1"
	e.vars["CROWDVM_CURRENT"] = cur

	e.mustRun("exec", "desk1", "--", "hostname")
	if body := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/commands")); body["command"] != "hostname" {
		t.Fatalf("exec by name: %v", body)
	}
	// Not a name: still the command, run in the current sandbox.
	e.mustRun("exec", "uname", "-a")
	if body := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+cur+"/commands")); body["command"] != "uname -a" {
		t.Fatalf("exec without id: %v", body)
	}
	if r := e.mustRun("info", "desk1"); !strings.Contains(r.out, id) {
		t.Fatalf("info by name: %s", r.out)
	}
	// Two with that name: ask for the id instead of guessing.
	other := e.f.AddSandbox("ready")
	e.f.Sandboxes[other]["name"] = "desk1"
	if r := e.run("info", "desk1", "--json=false"); r.code == 0 || !strings.Contains(r.err, "2 sandboxes are named") {
		t.Fatalf("ambiguous name: %d %q", r.code, r.err)
	}
}

// Field test: a sandbox in the error state vanished from `crowdvm ls`. The
// default stays "running" (Boat), but text mode says what else is there.
func TestListMentionsHiddenSandboxes(t *testing.T) {
	e := newEnv(t)
	e.f.AddSandbox("running")
	e.f.AddSandbox("error")
	e.f.AddSandbox("provisioning")
	e.f.AddSandbox("archived")
	r := e.mustRun("list", "--json=false")
	if !strings.Contains(r.err, "also 1 starting, 1 with an error: crowdvm ls --filter pe") {
		t.Fatalf("hint: %q", r.err)
	}
	if r := e.mustRun("list", "--all", "--json=false"); strings.Contains(r.err, "also") {
		t.Fatalf("hint with --all: %q", r.err)
	}
}
