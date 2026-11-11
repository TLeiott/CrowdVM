package guest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestBughuntR3SolMoveOntoSelfPreservesData(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "valuable.txt", "content": "irreplaceable"})
	status, response := e.do("POST", "/v1/files/move", map[string]any{"source": "valuable.txt", "destination": "./valuable.txt", "overwrite": true})
	b, err := os.ReadFile(filepath.Join(e.home, "valuable.txt"))
	if err != nil || string(b) != "irreplaceable" {
		t.Fatalf("move onto self lost source: status=%d response=%v content=%q err=%v", status, response, b, err)
	}
}

// Moving onto a directory that holds the source, or into the source, never
// removes the source first.
func TestMoveNestedPreservesData(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "a/b/keep.txt", "content": "keep"})
	for _, mv := range [][2]string{{"a/b", "a"}, {"a", "a/b/c"}} {
		if status, _ := e.do("POST", "/v1/files/move", map[string]any{"source": mv[0], "destination": mv[1], "overwrite": true}); status != 400 {
			t.Fatalf("move %s onto %s: status %d, want 400", mv[0], mv[1], status)
		}
		if b, err := os.ReadFile(filepath.Join(e.home, "a/b/keep.txt")); err != nil || string(b) != "keep" {
			t.Fatalf("move %s onto %s lost data: %q %v", mv[0], mv[1], b, err)
		}
	}
}

// bugs-r4-opus 1: the desktop check sees the VNC listener without
// connecting to it (Xvnc blacklists 127.0.0.1 after five early closes).
func TestListeningDoesNotConnect(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	for i := 0; i < 10; i++ {
		if !listening(port) {
			t.Fatal("listener not seen")
		}
	}
	l.Close()
	if listening(port) {
		t.Fatal("closed listener still seen")
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the check connected %d times", n)
	}
}

// bugs-r6-opus 1: the session script gets -BlacklistTimeout 0 (off), never
// -BlacklistThreshold 0 (block after one failure).
func TestPatchSessionScript(t *testing.T) {
	v3 := "Xvnc :0 -geometry \"$g\" -depth 24 -rfbport 5900 -localhost -SecurityTypes None \\\n\t-AlwaysShared -AcceptSetDesktopSize -nolisten tcp &\n"
	r5 := "Xvnc :0 -rfbport 5900 -localhost -SecurityTypes None \\\n\t-AlwaysShared -AcceptSetDesktopSize -BlacklistThreshold 0 -nolisten tcp &\n"
	for name, in := range map[string]string{"v3": v3, "round5": r5} {
		out, ok := patchSessionScript([]byte(in))
		if !ok || !strings.Contains(string(out), "-SecurityTypes None -BlacklistTimeout 0") || strings.Contains(string(out), "BlacklistThreshold") {
			t.Fatalf("%s: %v %q", name, ok, out)
		}
		if _, again := patchSessionScript(out); again {
			t.Fatalf("%s: patched twice", name)
		}
	}
}

// bugs-r6-opus 4: a secret file whose path is an existing folder leaves the
// folder alone and reports file.skipped instead of failing silently.
func TestSecretFileOnFolderReported(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	dir := filepath.Join(e.home, ".cargo", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "cargo"), []byte("tool"), 0o755)
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/internal/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	e.a.writeConfigFile(proto.File{Path: ".cargo/bin", Content: []byte("secret")}, 0o600)
	sc := bufio.NewScanner(resp.Body)
	seen := false
	for !seen && sc.Scan() {
		var ev proto.GuestEvent
		json.Unmarshal(sc.Bytes(), &ev)
		seen = ev.Type == EvFileSkipped
	}
	if !seen {
		t.Fatal("no file.skipped event")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cargo")); err != nil || string(b) != "tool" {
		t.Fatalf("the folder was touched: %q %v", b, err)
	}
}

// bugs-r8-opus 4: a foreign deploy's scrub wipes the saver's command
// history and its output.
func TestScrubWipesHistory(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	r := e.mustOK("POST", "/v1/commands", map[string]any{"command": "echo aws_secret=TOPSECRET"})
	id, _ := r["commandId"].(string)
	if e.a.hist.get(id) == nil {
		t.Fatal("setup: no record")
	}
	e.a.runScrub(&proto.GuestScrub{ID: "sc1", History: true})
	if e.a.hist.get(id) != nil {
		t.Fatal("record kept")
	}
	if _, err := os.Stat(e.a.hist.path(id, "stdout")); !os.IsNotExist(err) {
		t.Fatal("output kept")
	}
	if l := e.mustOK("GET", "/v1/command-history", nil); strings.Contains(fmt.Sprint(l), "TOPSECRET") {
		t.Fatalf("history still lists it: %v", l)
	}
}

// bugs-r9-opus 5: a detached command with killAfterSeconds ends on its own
// when nobody stops it (the CLI that followed it died).
func TestDetachedKillAfter(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	m := e.mustOK("POST", "/v1/commands", map[string]any{"command": "sleep 300", "detached": true, "killAfterSeconds": 1})
	pid := num(m["processId"])
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	t.Fatal("the command outlived killAfterSeconds")
}
