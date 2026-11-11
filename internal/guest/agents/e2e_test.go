package agents

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestE2EOpencode runs the real opencode CLI on a free model.
// CROWDVM_E2E=1 go test -run E2EOpencode -v ./internal/guest/agents/
// Model override: CROWDVM_E2E_OPENCODE_MODEL (default opencode/beam-free).
func TestE2EOpencode(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not installed")
	}
	model := os.Getenv("CROWDVM_E2E_OPENCODE_MODEL")
	if model == "" {
		model = "opencode/beam-free"
	}
	home, _ := os.UserHomeDir() // real home: opencode config and auth
	e := newEnv(t, t.TempDir())
	e.m.o.Home = home
	cwd := t.TempDir()

	p1, c := e.prompt(map[string]any{"provider": "opencode", "model": model, "cwd": cwd,
		"prompt": "Run the shell command `echo crowdvm-e2e` with your bash tool, then reply with just DONE."})
	run := e.waitStatusLong(p1)
	if run["status"] != "finished" {
		t.Fatalf("run: %v", run)
	}
	texts, tools := responses(e.events("sort=asc"), p1)
	t.Logf("texts=%q tools=%d", texts, len(tools))
	if len(texts) == 0 || len(tools) == 0 {
		t.Fatalf("expected text and a tool call, got %v / %v", texts, tools)
	}
	session := e.m.convs[c].Session
	if !strings.HasPrefix(session, "ses_") {
		t.Fatalf("session %q", session)
	}

	p2, c2 := e.prompt(map[string]any{"prompt": "What exactly did that command print? Answer with the printed text only.", "cwd": cwd})
	if run := e.waitStatusLong(p2); run["status"] != "finished" || c2 != c {
		t.Fatalf("run 2: %v", run)
	}
	texts, _ = responses(e.events("sort=asc"), p2)
	t.Logf("follow-up texts=%q", texts)
	if e.m.convs[c].Session != session || !strings.Contains(strings.Join(texts, " "), "crowdvm-e2e") {
		t.Fatalf("session continuation lost: %q %v", e.m.convs[c].Session, texts)
	}
}

func (e *env) waitStatusLong(id string) map[string]any {
	e.t.Helper()
	for i := 0; i < 8; i++ { // up to 2 minutes
		_, out := e.do("GET", "/v1/prompts/"+id, nil)
		if run := out["promptRun"].(map[string]any); run["done"] == true {
			return run
		}
		e.do("GET", "/v1/events?sort=asc&wait=15&cursor="+lastCursor(e), nil)
	}
	e.t.Fatal("prompt did not finish in time")
	return nil
}

func lastCursor(e *env) string {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if len(e.m.events) == 0 {
		return ""
	}
	return encodeCursor(e.m.events[len(e.m.events)-1])
}

// e2eSteer runs a real harness: a tool call, a native/fallback steer during a
// long tool call, and a follow-up on the same session.
// CROWDVM_E2E=1 CROWDVM_E2E_HARNESS=pi CROWDVM_E2E_MODEL=openrouter/... go test -run E2ESteer -v ./internal/guest/agents/
func TestE2ESteer(t *testing.T) {
	provider := os.Getenv("CROWDVM_E2E_HARNESS")
	if os.Getenv("CROWDVM_E2E") != "1" || provider == "" {
		t.Skip("set CROWDVM_E2E=1 and CROWDVM_E2E_HARNESS")
	}
	model := os.Getenv("CROWDVM_E2E_MODEL")
	e := newEnv(t, t.TempDir())
	if os.Getenv("CROWDVM_E2E_REAL_HOME") == "1" {
		e.m.o.Home, _ = os.UserHomeDir()
	}
	cwd := t.TempDir()
	body := map[string]any{"provider": provider, "cwd": cwd, "reasoningEffort": os.Getenv("CROWDVM_E2E_EFFORT"),
		"prompt": "Use your shell tool to run exactly `sleep 12 && echo first-done`. After it finishes, reply with one short sentence."}
	if model != "" {
		body["model"] = model
	}
	if body["reasoningEffort"] == "" {
		delete(body, "reasoningEffort")
	}
	if f := os.Getenv("CROWDVM_E2E_FAST"); f != "" {
		body["fast"] = f == "1"
	}
	p1, c := e.prompt(body)
	e.waitStatus(p1, "running")
	time.Sleep(6 * time.Second) // inside the sleep tool call
	out := e.steer(c, "Also include the word PINEAPPLE in your final reply.")
	t.Logf("steer: %v", out)
	run := e.waitStatusLong(p1)
	texts, tools := responses(e.events("sort=asc&limit=200"), p1)
	t.Logf("run=%v texts=%q tools=%d session=%q", run["status"], texts, len(tools), e.session(c))
	if run["status"] != "finished" || len(tools) == 0 || e.session(c) == "" {
		t.Fatalf("turn 1: %v", run)
	}
	if !strings.Contains(strings.ToUpper(strings.Join(texts, " ")), "PINEAPPLE") {
		t.Errorf("steer not acted on: %q", texts)
	}
	for _, ev := range e.events("sort=asc&type=steer") {
		t.Logf("steer event mode=%v", ev["data"].(map[string]any)["mode"])
	}
	partials := 0
	for _, ev := range e.events("sort=asc&type=response&limit=200") {
		if ev["data"].(map[string]any)["is_streaming"] == true {
			partials++
		}
	}
	t.Logf("streaming partials: %d", partials)

	session := e.session(c)
	p2, _ := e.prompt(map[string]any{"conversationId": c, "cwd": cwd, "prompt": "Which fruit did I ask you to mention? One word."})
	run = e.waitStatusLong(p2)
	texts, _ = responses(e.events("sort=asc&limit=200"), p2)
	t.Logf("follow-up: %v %q session=%q", run["status"], texts, e.session(c))
	if run["status"] != "finished" || e.session(c) != session || !strings.Contains(strings.ToUpper(strings.Join(texts, " ")), "PINEAPPLE") {
		t.Fatalf("session continuation lost")
	}
}
