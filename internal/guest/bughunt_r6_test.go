package guest

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Concurrent history.update calls must leave meta.json parseable and equal to
// the final in-memory record. save() marshals under the lock but writes outside
// it, to one shared "<meta.json>.tmp" path.
func TestBughuntR6HistorySaveRace(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	bad := 0
	for round := 0; round < 500 && bad == 0; round++ {
		m := &meta{CommandRecord: CommandRecord{CommandID: fmt.Sprintf("cmd_race%d", round), Mode: "detached", Status: "running", StartedAt: time.Now()}}
		if err := e.a.hist.create(m); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					e.a.hist.update(m, func(m *meta) { m.Command = strings.Repeat("x", (g*7+i)%50+1) })
				}
			}(g)
		}
		wg.Wait()
		b, _ := os.ReadFile(e.a.hist.path(m.CommandID, "meta.json"))
		var got meta
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("round %d: meta.json corrupt: %v (%d bytes)", round, err, len(b))
			bad++
		} else if got.Rev != m.Rev || got.Command != m.Command {
			t.Errorf("round %d: meta.json stale: rev %d want %d", round, got.Rev, m.Rev)
			bad++
		}
	}
}

// After history pruning removes a finished detached command, its process id
// must be unknown (404), not served from the never-pruned a.procs table.
func TestBughuntR6PrunedDetachedStillServed(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	m := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo hi", "detached": true})
	pid := fmt.Sprint(num(m["processId"]))
	e.ndjson("GET", "/v1/commands/"+pid+"/stream", nil) // wait until it exited
	if n := e.a.hist.prune(time.Now().Add(100 * 24 * time.Hour)); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	e.a.mu.Lock()
	n := len(e.a.procs)
	e.a.mu.Unlock()
	st, body := e.do("GET", "/v1/commands/"+pid, nil)
	if st != 200 {
		return
	}
	t.Fatalf("pruned command still answered 200 (procs table has %d entries, never removed): %v", n, body)
}

// DELETE on a detached command must not leave its process group running: the
// SIGKILL escalation is skipped once the leader exits on SIGTERM, so a child
// that ignores SIGTERM survives the "kill".
func TestBughuntR6KillLeavesTermIgnoringChild(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	m := e.mustOK("POST", "/v1/commands", map[string]any{
		"command": `(trap '' TERM; exec sleep 300) & wait`, "detached": true})
	pid := num(m["processId"])
	defer syscall.Kill(-pid, syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	k := e.mustOK("DELETE", fmt.Sprintf("/v1/commands/%d", pid), nil)
	if k["signaled"] != true {
		t.Fatalf("kill %v", k)
	}
	time.Sleep(200 * time.Millisecond)
	for _, p := range listPIDs() {
		if st, err := readStat(p); err == nil && st.pgrp == pid && st.state != "Z" {
			t.Fatalf("process group %d still has live member pid %d (state %s) after DELETE", pid, p, st.state)
		}
	}
}
