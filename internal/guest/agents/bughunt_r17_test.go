package agents

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The API documents attachments as "paths in the sandbox (~/attachments/...)",
// and the event code resolves "~/x" against the home. The pre-flight does not:
// it joins "~/x" below the home as a directory named "~", the stat fails, and
// the file is skipped, so the 25 MB / 64 MB limits do not apply to it.
func TestBughuntR17AttachmentLimitsApplyToTildePaths(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(home, "attachments", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxAttachFile + 1); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	if err := checkAttachments([]string{filepath.Join(home, "attachments", "big.bin")}, home); err == nil {
		t.Fatal("control: the absolute path of a 25 MB + 1 file must be refused")
	}
	if err := checkAttachments([]string{"~/attachments/big.bin"}, home); err == nil {
		t.Fatal("~/attachments/big.bin is over the 25 MB per-file limit but was accepted")
	}
}

// Zero data retention: a disk that held prompts before the account turned it
// on (fork, resume, a changed setting) keeps their text in state.json when the
// agent starts with retention on and nothing else rewrites the file.
func TestBughuntR17ZeroRetentionDropsEarlierPromptTextAtStart(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	p, _ := e.prompt(map[string]any{"provider": "fake", "prompt": "earlier-secret-prompt"})
	e.waitStatus(p, "finished")
	if b, _ := os.ReadFile(filepath.Join(dir, "state.json")); !strings.Contains(string(b), "earlier-secret-prompt") {
		t.Fatalf("setup: state.json should hold the prompt before retention is on: %s", b)
	}
	// The next boot has zero data retention on.
	m := New(Options{Home: t.TempDir(), Unprivileged: true, DataDir: dir, SandboxID: "bx_test",
		ZeroDataRetention: func() bool { return true }})
	_ = m
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(b), "earlier-secret-prompt") {
		t.Fatalf("state.json still holds a prompt text with zero data retention on: %s", b)
	}
}

// eventAttachments reads every attachment path with os.ReadFile while
// handlePrompt holds m.mu (newPromptLocked). checkAttachments only stats it, and
// a FIFO has size 0, so it passes. ReadFile on a FIFO nobody writes to blocks
// for ever, and with it the lock: every other call of the agent API (events,
// conversations, interrupt, other prompts) hangs too.
func TestBughuntR17FifoAttachmentDoesNotWedgeTheAgent(t *testing.T) {
	e := newEnv(t, t.TempDir())
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no mkfifo:", err)
	}
	t.Cleanup(func() { // free the blocked reader when the test is over
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	done := make(chan int, 1)
	go func() {
		code, _ := e.do("POST", "/v1/prompt", map[string]any{"provider": "fake", "prompt": "look", "attachments": []string{fifo}})
		done <- code
	}()
	select {
	case code := <-done:
		if code != 202 && code != 400 {
			t.Fatalf("prompt with a FIFO attachment answered %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("POST /v1/prompt with a FIFO as attachment never answers: the agent is stuck in os.ReadFile holding its lock")
	}
}
