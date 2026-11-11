package guest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r9-sonnet 1: the History scrub of a foreign deploy wipes the command
// history and the detached logs, but not the coding agents' conversations,
// prompts and events (a.data/agents): the previous owner's prompts, with
// whatever secrets they pasted, stay readable by the new owner.
func TestBughuntR9ScrubHistoryKeepsAgentConversations(t *testing.T) {
	t.Setenv("CROWDVM_FAKE_HARNESS", "1")
	e := newEnv(t, proto.GuestConfig{})
	code, out := e.do("POST", "/v1/prompt", map[string]any{"provider": "fake", "prompt": "use AWS_SECRET=TOPSECRET-R9 to deploy"})
	if code != 202 {
		t.Fatalf("prompt: %d %v", code, out)
	}
	pid, _ := out["promptId"].(string)
	for i := 0; i < 300; i++ {
		_, o := e.do("GET", "/v1/prompts/"+pid, nil)
		if run, _ := o["promptRun"].(map[string]any); run != nil && run["status"] != "running" && run["status"] != "queued" && run["status"] != "pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, o := e.do("GET", "/v1/conversations", nil); !strings.Contains(fmt.Sprint(o), "TOPSECRET-R9") {
		_, ev := e.do("GET", "/v1/events", nil)
		if !strings.Contains(fmt.Sprint(ev), "TOPSECRET-R9") {
			t.Fatalf("setup: prompt not stored: %v / %v", o, ev)
		}
	}
	e.a.runScrub(&proto.GuestScrub{ID: "sc-r9", History: true})
	for _, p := range []string{"/v1/prompts/" + pid, "/v1/conversations", "/v1/events"} {
		_, o := e.do("GET", p, nil)
		if strings.Contains(fmt.Sprint(o), "TOPSECRET-R9") {
			t.Errorf("GET %s after a History scrub still returns the previous owner's prompt: %v", p, o)
		}
	}
}

// bugs-r9-sonnet 5: history.list (history.go:207) answers an unknown cursor
// with an empty last page: start = len(all). A cursor whose record was
// pruned or wiped between two page requests, or a mistyped one, silently
// ends the pagination (the older commands vanish from the client's view).
// The events list answers 400 invalid_cursor, and so does the server's own
// copy of the command history (feat_history.go:461).
func TestBughuntR9HistoryUnknownCursorIsSilentEnd(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	for i := 0; i < 3; i++ {
		e.mustOK("POST", "/v1/commands", map[string]any{"command": "true"})
	}
	st, out := e.do("GET", "/v1/command-history?cursor=cmd_pruned_away&limit=2", nil)
	if st == 200 {
		t.Errorf("GET /v1/command-history with a cursor that names no record: %d %v; want 400 invalid_cursor (the events list and the server's stored history answer so)", st, out)
	}
}
