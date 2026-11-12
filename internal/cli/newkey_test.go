package cli

import "testing"

// bugs-r14-opus 1: --new-key asks the server for a copy with a disk key of
// its own, on `new --from` and on `fork`; without --from it is refused.
func TestNewKeyFlagIsSent(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	e.mustRun("fork", id, "--new-key")
	if reqBody(t, e.f.Find("POST", "/api/v1/sandboxes/"+id+"/fork"))["newDiskKey"] != true {
		t.Fatal("fork --new-key did not send newDiskKey")
	}
	e.mustRun("fork", id)
	if _, ok := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes/"+id+"/fork"))["newDiskKey"]; ok {
		t.Fatal("a plain fork sent newDiskKey")
	}
	e.run("new", "--from", "tpl", "--new-key", "--no-wait")
	if b := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes")); b["newDiskKey"] != true || b["from"] != "tpl" {
		t.Fatalf("new --from --new-key sent %v", b)
	}
	if r := e.run("new", "--new-key"); r.code == 0 {
		t.Fatal("--new-key without --from was accepted")
	}
}
