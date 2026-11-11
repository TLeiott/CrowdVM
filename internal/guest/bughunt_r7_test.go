package guest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// capOutput cuts the log file below an offset a live client may already have
// read. When that client reconnects (the attach stream asks it to every
// attachMaxDuration) with its offset, the guest sees "offset > file size",
// treats it as "log replaced" and rewinds to 0: the client gets the capped
// output a second time. Same in GET /commands/{pid}?stdoutOffset=.
func TestBughuntR7AttachAfterCapReplaysFromZero(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.a.outputCap = 4 << 10
	m := e.mustOK("POST", "/v1/commands", map[string]any{
		"command": "head -c 100000 /dev/zero | tr '\\0' x", "detached": true})
	pid := num(m["processId"])
	cm := e.a.hist.byProcessID(pid)
	path := e.a.hist.path(cm.CommandID, "stdout")
	end := e.a.outputCap
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fileSize(path) == end && e.a.hist.record(cm).Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fileSize(path) != end {
		t.Fatalf("setup: stdout is %d bytes, want %d", fileSize(path), end)
	}
	// A client that streamed live read 100000 bytes before the cut.
	frames := e.ndjson("GET", fmt.Sprintf("/v1/commands/%d/stream?stdoutOffset=100000", pid), nil)
	if got := joinFrames(frames, "stdout"); got != "" {
		t.Fatalf("attach with offset 100000 replayed %d bytes of old output (starts %.20q)", len(got), got)
	}
	s := e.mustOK("GET", fmt.Sprintf("/v1/commands/%d?stdoutOffset=100000", pid), nil)
	if d, _ := s["stdout"].(string); strings.Contains(d, "x") {
		t.Fatalf("status with offset 100000 replayed %d bytes of old output", len(d))
	}
}
