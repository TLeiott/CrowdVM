package guest

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// A runaway command's stdout file stops at the cap and ends with the marker.
func TestCommandOutputCapped(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.a.outputCap = 64 << 10
	m := e.mustOK("POST", "/v1/commands", map[string]any{"command": "yes", "detached": true})
	pid := num(m["processId"])
	defer syscall.Kill(-pid, syscall.SIGKILL)
	cm := e.a.hist.byProcessID(pid)
	path := e.a.hist.path(cm.CommandID, "stdout")
	limit := e.a.outputCap
	deadline := time.Now().Add(5 * time.Second)
	for fileSize(path) != limit && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	e.mustOK("DELETE", fmt.Sprintf("/v1/commands/%d", pid), nil)
	b, _ := os.ReadFile(path)
	if int64(len(b)) != limit || strings.Contains(string(b), "crowdvm: output truncated") {
		t.Fatalf("stdout is %d bytes, want exactly %d with no marker in the data", len(b), limit)
	}
	if !e.a.hist.byProcessID(pid).OutputTruncated { // bugs-r7-opus 2
		t.Fatal("history does not say the output was truncated")
	}
}
