package guest

import (
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func lowSpace(t *testing.T, share *float64) {
	old := fsFreeShare
	fsFreeShare = func(string) float64 { return *share }
	t.Cleanup(func() { fsFreeShare = old })
}

// bugs-r10-opus 6: with monitoring off and /run nearly full, makeRoom dropped
// a short command's record the moment it finished, before the CLI attached
// to read its output and exit (process_not_found, or empty output).
func TestR10MakeRoomKeepsUnfollowedRecord(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{NoMonitoring: true})
	free := 0.1
	lowSpace(t, &free)
	pid := num(e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo probe", "detached": true})["processId"])
	deadline := time.Now().Add(5 * time.Second)
	for e.a.hist.record(e.a.hist.byProcessID(pid)).Status == "running" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	e.a.hist.makeRoom() // another command's capOutput under pressure
	frames := e.ndjson("GET", fmt.Sprintf("/v1/commands/%d/stream", pid), nil)
	if last := frames[len(frames)-1]; last["type"] != "exit" || frames[1]["data"] != "probe\n" {
		t.Fatalf("frames: %v", frames)
	}
	e.a.hist.makeRoom() // the exit was read: now it may go
	if e.a.hist.byProcessID(pid) != nil {
		t.Fatal("a followed, finished record is kept under pressure")
	}
}

// bugs-r10-opus 6: running unmonitored commands could fill /run together
// (each stream was capped at an eighth of it). Below 10% free, their output
// stops growing and the record says it was truncated.
func TestR10OutputFreezesWhenRunNearlyFull(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{NoMonitoring: true})
	free := 0.05
	lowSpace(t, &free)
	pid := num(e.mustOK("POST", "/v1/commands", map[string]any{"command": "sleep 0.3; yes", "detached": true})["processId"])
	defer syscall.Kill(-pid, syscall.SIGKILL)
	m := e.a.hist.byProcessID(pid)
	path := e.a.hist.path(m.CommandID, "stdout")
	time.Sleep(time.Second)
	e.mustOK("DELETE", fmt.Sprintf("/v1/commands/%d", pid), nil)
	deadline := time.Now().Add(5 * time.Second)
	for e.a.hist.record(e.a.hist.byProcessID(pid)).Status == "running" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(3 * capOutputInterval) // capOutput's last pass
	if sz := fileSize(path); sz > 1<<20 {
		t.Fatalf("stdout kept %d bytes with /run nearly full", sz)
	}
	if !e.a.hist.record(e.a.hist.byProcessID(pid)).OutputTruncated {
		t.Fatal("output was dropped but the record does not say so")
	}
}
