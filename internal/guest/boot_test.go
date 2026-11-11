package guest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func waitSetup(t *testing.T, e *env, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		m := e.mustOK("GET", "/v1/internal/setup-status", nil)
		if m["setupStatus"] == want {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup status: %v (want %s)", m, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBootFilesEnvAndSetup(t *testing.T) {
	cfg := proto.GuestConfig{
		Boot: proto.BootCreate, FirstBoot: true,
		Env:         map[string]string{"GREETING": "it's here", "bad-name": "x"},
		SecretFiles: []proto.File{{Path: ".config/app/secret", Content: []byte("s3cret")}},
		Files:       []proto.File{{Path: "/home/user/notes/readme.md", Content: []byte("hi"), Mode: 0o640}},
		SetupScript: "echo setup-out; echo \"$GREETING\" > setup-ran.txt",
	}
	root := t.TempDir()
	home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("# user stuff\nalias ll='ls -l'\n"), 0o644)
	e := newEnvAt(t, cfg, home, data)
	if m := e.mustOK("GET", "/v1/internal/setup-status", nil); m["setupStatus"] != SetupPending {
		t.Fatalf("before background: %v", m)
	}
	go e.a.BootBackground(context.Background())
	waitSetup(t, e, SetupDone)
	if b, _ := os.ReadFile(filepath.Join(home, "setup-ran.txt")); string(b) != "it's here\n" {
		t.Fatalf("setup env %q", b)
	}
	if fi, err := os.Stat(filepath.Join(home, ".config/app/secret")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(home, "notes/readme.md")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("file %v %v", fi, err)
	}
	rc, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
	if !strings.HasPrefix(string(rc), bashrcBegin+"\nexport GREETING='it'\\''s here'\n"+bashrcEnd+"\n# user stuff\n") || strings.Contains(string(rc), "bad-name") {
		t.Fatalf("bashrc:\n%s", rc)
	}
	// the managed block makes the env visible in a non-login bash too
	out, err := exec.Command("bash", "-c", "source "+filepath.Join(home, ".bashrc")+"; echo \"$GREETING\"").Output()
	if err != nil || string(out) != "it's here\n" {
		t.Fatalf("bashrc source: %q %v", out, err)
	}

	// agent restart in the same boot: first-boot work does not run again
	os.Remove(filepath.Join(home, "setup-ran.txt"))
	os.WriteFile(filepath.Join(home, "notes/readme.md"), []byte("user edit"), 0o644)
	e.srv.Close()
	e2 := newEnvAt(t, cfg, home, data)
	e2.a.BootBackground(context.Background())
	if m := e2.mustOK("GET", "/v1/internal/setup-status", nil); m["setupStatus"] != SetupDone {
		t.Fatalf("after restart %v", m)
	}
	if _, err := os.Stat(filepath.Join(home, "setup-ran.txt")); err == nil {
		t.Fatal("setup script ran twice")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "notes/readme.md")); string(b) != "user edit" {
		t.Fatal("first-boot file rewritten")
	}
	// env change rewrites the block in place
	cfg.Env = map[string]string{"OTHER": "1"}
	e3 := newEnvAt(t, cfg, home, data)
	_ = e3
	rc, _ = os.ReadFile(filepath.Join(home, ".bashrc"))
	if strings.Count(string(rc), bashrcBegin) != 1 || strings.Contains(string(rc), "GREETING") || !strings.Contains(string(rc), "export OTHER='1'") {
		t.Fatalf("bashrc rewrite:\n%s", rc)
	}
}

func TestSetupScriptFailure(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{FirstBoot: true, SetupScript: "echo working; echo boom >&2; exit 3"})
	e.a.BootBackground(context.Background())
	m := waitSetup(t, e, SetupFailed)
	if msg, _ := m["setupError"].(string); !strings.Contains(msg, "exit code 3") || !strings.Contains(msg, "boom") {
		t.Fatalf("setup error %v", m)
	}
	if h := e.mustOK("GET", "/v1/health", nil); h["setupStatus"] != SetupFailed {
		t.Fatalf("health %v", h)
	}
}

func TestNoSetupScript(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{FirstBoot: true})
	e.a.BootBackground(context.Background())
	if m := e.mustOK("GET", "/v1/internal/setup-status", nil); m["setupStatus"] != nil {
		t.Fatalf("no script: %v", m)
	}
}

func TestRepoCloneAndSetup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := filepath.Join(t.TempDir(), "proj")
	for _, c := range [][]string{{"init", "-q", "-b", "main", src}, {"-C", src, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", c, out)
		}
	}
	cfg := proto.GuestConfig{FirstBoot: true, Repos: []proto.Repo{
		{URL: src, SetupScript: "touch setup-done; false; touch never"},
		{URL: src, Path: "other", SetupScript: "touch ok"},
	}}
	e := newEnv(t, cfg)
	e.a.BootBackground(context.Background())
	st := e.mustOK("GET", "/v1/internal/setup-status", nil)
	repos := st["repos"].([]any)
	if len(repos) != 2 {
		t.Fatalf("repos %v", st)
	}
	r0, r1 := repos[0].(map[string]any), repos[1].(map[string]any)
	if r0["path"] != "/home/user/proj" || r0["status"] != "failed" || !strings.Contains(r0["error"].(string), "exit code 1") {
		t.Fatalf("repo0 %v", r0)
	}
	if r1["path"] != "/home/user/other" || r1["status"] != "done" {
		t.Fatalf("repo1 %v", r1)
	}
	if _, err := os.Stat(filepath.Join(e.home, "proj/.git")); err != nil {
		t.Fatal("not cloned")
	}
	if _, err := os.Stat(filepath.Join(e.home, "proj/never")); err == nil {
		t.Fatal("set -e not applied")
	}
	if _, err := os.Stat(filepath.Join(e.a.tmp, "routine-logs/proj.log")); err != nil {
		t.Fatal("failure log missing")
	}
}

func TestDesktop(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	if _, err := exec.LookPath("Xvnc"); err != nil {
		if st, m := e.do("POST", "/v1/internal/desktop/start", nil); st != 409 || m["code"] != "desktop_not_installed" {
			t.Fatalf("start without Xvnc: %d %v", st, m)
		}
	}
	s := e.mustOK("GET", "/v1/internal/desktop/status", nil)
	if num(s["port"]) != 5900 || num(s["width"]) != 1920 {
		t.Fatalf("status %v", s)
	}
	if st, m := e.do("POST", "/v1/internal/desktop/input", map[string]any{"actions": []any{map[string]any{"type": "fly"}}}); st != 400 || m["code"] != "invalid_action" {
		t.Fatalf("bad action %d %v", st, m)
	}
	if st, _ := e.do("POST", "/v1/internal/desktop/recording/stop", nil); st != 409 {
		t.Fatal("stop without recording")
	}
	if st, m := e.do("POST", "/v1/internal/desktop/wallpaper", map[string]any{"content": "aGVsbG8="}); st != 400 || m["code"] != "invalid_wallpaper" {
		t.Fatalf("bad wallpaper %d %v", st, m)
	}
	// a valid 1x1 PNG is stored at ~/.config/crowdvm/wallpaper.png
	png1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	w := e.mustOK("POST", "/v1/internal/desktop/wallpaper", map[string]any{"content": png1})
	if w["path"] != "/home/user/.config/crowdvm/wallpaper.png" {
		t.Fatalf("wallpaper %v", w)
	}
	e.mustOK("POST", "/v1/internal/desktop/wallpaper", map[string]any{"reset": true})
	if _, err := os.Stat(filepath.Join(e.home, ".config/crowdvm/wallpaper.png")); err == nil {
		t.Fatal("reset kept the file")
	}
}

func TestXdotoolArgs(t *testing.T) {
	x, y, tx, ty := 10, 20, 30, 40
	cases := []struct {
		a    InputAction
		want string
	}{
		{InputAction{Type: "click", X: &x, Y: &y, Button: "right"}, "mousemove --sync 10 20 click --repeat 1 3"},
		{InputAction{Type: "double_click"}, "click --repeat 2 1"},
		{InputAction{Type: "drag", X: &x, Y: &y, ToX: &tx, ToY: &ty}, "mousemove --sync 10 20 mousedown 1 mousemove --sync 30 40 mouseup 1"},
		{InputAction{Type: "scroll", Direction: "down"}, "click --repeat 3 5"},
		{InputAction{Type: "type", Text: "-hi"}, "type --delay 12 -- -hi"},
		{InputAction{Type: "key", Keys: "ctrl+c Return"}, "key -- ctrl+c Return"},
		{InputAction{Type: "mouse_down", Button: "middle"}, "mousedown 2"},
	}
	for _, c := range cases {
		got, err := xdotoolArgs(c.a)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%+v: %q %v", c.a, got, err)
		}
	}
	for _, bad := range []InputAction{{Type: "move"}, {Type: "drag"}, {Type: "scroll"}, {Type: "click", Button: "x"}, {Type: "type"}} {
		if _, err := xdotoolArgs(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// TestReplaceMachineIDAtomic: systemd-networkd reads /etc/machine-id (its
// DHCP client ID) while the guest gives a create/fork a new one at boot. A
// moment without the file failed the NIC for good: no address, boot
// timeout. Readers must always see a complete ID.
func TestReplaceMachineIDAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "machine-id")
	const first = "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(p, []byte(first+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan string)
	go func() {
		bad := ""
		for {
			select {
			case <-stop:
				done <- bad
				return
			default:
			}
			b, err := os.ReadFile(p)
			if id := strings.TrimSpace(string(b)); bad == "" && (err != nil || len(id) != 32) {
				bad = fmt.Sprintf("%q, %v", b, err)
			}
		}
	}()
	v4 := regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
	a := &Agent{}
	prev := first
	for range 300 {
		old, cur, err := a.replaceMachineID(p)
		if err != nil {
			t.Fatal(err)
		}
		if old != prev || cur == old || !v4.MatchString(cur) {
			t.Fatalf("replaceMachineID: old %q (want %q), new %q", old, prev, cur)
		}
		prev = cur
	}
	close(stop)
	if bad := <-done; bad != "" {
		t.Fatalf("a reader saw no machine ID while it was replaced: %s", bad)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	ents, _ := os.ReadDir(dir)
	if string(b) != prev+"\n" || fi.Mode().Perm() != 0o444 || len(ents) != 1 {
		t.Fatalf("machine-id %q mode %v, %d files in its folder", b, fi.Mode().Perm(), len(ents))
	}
}
