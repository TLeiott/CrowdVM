package node

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Field test: a laptop node idle-suspended under its sandbox. While it hosts
// sandboxes the node holds a sleep inhibitor, and lets go when it hosts none.
func TestKeepAwakeWhileHosting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("logind inhibitors are Linux only")
	}
	bin := t.TempDir()
	mark := filepath.Join(t.TempDir(), "held")
	// A fake systemd-inhibit: records its arguments while it runs.
	script := "#!/bin/sh\necho \"$@\" > " + mark + "\ntrap 'rm -f " + mark + "; exit 0' TERM\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(bin, "systemd-inhibit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := awakeTick
	awakeTick = 20 * time.Millisecond
	defer func() { awakeTick = old }()

	n := &Node{cfg: &Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vms: map[string]*vmRec{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.keepAwakeLoop(ctx); close(done) }()
	held := func() bool { _, err := os.Stat(mark); return err == nil }
	wait := func(what string, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal(what)
			}
		}
	}
	time.Sleep(100 * time.Millisecond)
	if held() {
		t.Fatal("inhibitor held with no sandboxes")
	}
	n.mu.Lock()
	n.vms["bx_a"] = &vmRec{}
	n.mu.Unlock()
	wait("inhibitor not taken while hosting", held)
	if b, _ := os.ReadFile(mark); !strings.Contains(string(b), "--what=sleep") || !strings.Contains(string(b), "--mode=block") {
		t.Fatalf("inhibitor args: %s", b)
	}
	n.mu.Lock()
	delete(n.vms, "bx_a")
	n.mu.Unlock()
	// SIGKILL skips the trap: the process is gone, the mark stays. Check the
	// process instead by hosting again: a second inhibitor starts.
	_ = os.Remove(mark)
	time.Sleep(100 * time.Millisecond)
	n.mu.Lock()
	n.vms["bx_b"] = &vmRec{}
	n.mu.Unlock()
	wait("inhibitor not taken again", held)
	cancel()
	<-done
}

func TestKeepAwakeOff(t *testing.T) {
	c := Config{}
	if v, _ := c.Get("keep-awake"); v != "true" {
		t.Fatalf("default keep-awake = %s", v)
	}
	if err := c.Set("keep-awake", "false"); err != nil || !c.NoKeepAwake {
		t.Fatalf("set false: %v %v", err, c.NoKeepAwake)
	}
	if err := c.Set("keep-awake", ""); err != nil || c.NoKeepAwake {
		t.Fatalf("reset: %v %v", err, c.NoKeepAwake)
	}
}
