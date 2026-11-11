package process

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

var guestOnce sync.Once
var guestBin string

func TestMain(m *testing.M) {
	code := m.Run()
	if guestBin != "" {
		os.RemoveAll(filepath.Dir(guestBin))
	}
	os.Exit(code)
}

// buildGuest builds cmd/crowdvm-guest for the host, or the stand-in guest
// when CROWDVM_E2E_GUEST=fake or the real one does not build.
func buildGuest(t *testing.T) string {
	guestOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "crowdvm-guest-host")
		out := filepath.Join(dir, "crowdvm-guest")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		pkgs := []string{"../../../../cmd/crowdvm-guest", "../qemu/testdata/fakeguest"}
		if os.Getenv("CROWDVM_E2E_GUEST") == "fake" {
			pkgs = pkgs[1:]
		}
		for _, pkg := range pkgs {
			if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
				t.Logf("build %s: %v\n%s", pkg, err, b)
				continue
			}
			t.Logf("guest agent: %s", pkg)
			guestBin = out
			return
		}
	})
	if guestBin == "" {
		t.Skip("no guest binary builds")
	}
	return guestBin
}

type env struct {
	t *testing.T
	d *Driver
}

func newEnv(t *testing.T, dir string) *env {
	d, err := New(Options{DataDir: dir, GuestBinary: buildGuest(t), StopTimeout: 3 * time.Second, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t, d}
}

func spec(id, parent string, boot proto.BootKind) proto.VMSpec {
	return proto.VMSpec{SandboxID: id, ImageID: "img", ParentID: parent,
		Guest: proto.GuestConfig{SandboxID: id, Token: "tok-" + id, Boot: boot, FirstBoot: boot == proto.BootCreate}}
}

func (e *env) start(s proto.VMSpec) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := e.d.Start(ctx, s); err != nil {
		e.t.Fatal(err)
	}
	if err := e.d.Wait(ctx, s.SandboxID); err != nil {
		e.t.Fatal(err)
	}
}

// call does a guest API request and returns the decoded JSON reply.
func (e *env) call(id, method, path string, body any) map[string]any {
	e.t.Helper()
	addr, err := e.d.GuestAddr(id)
	if err != nil {
		e.t.Fatal(err)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+addr+path, rd)
	req.Header.Set("Authorization", "Bearer tok-"+id)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		e.t.Fatalf("%s %s: %s %s", method, path, resp.Status, b)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func (e *env) write(id, file, content string) {
	e.call(id, "PUT", "/v1/files", map[string]string{"path": proto.GuestHome + "/" + file, "content": content})
}

func (e *env) read(id, file string) string {
	c, _ := e.call(id, "GET", "/v1/files?path="+proto.GuestHome+"/"+file, nil)["content"].(string)
	return c
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := newEnv(t, dir)
	defer func() {
		for _, s := range e.d.List() {
			e.d.Destroy(ctx, s.SandboxID)
		}
	}()

	t0 := time.Now()
	e.start(spec("a", "", proto.BootCreate))
	t.Logf("create -> healthy: %s", time.Since(t0))
	e.write("a", "notes/a.txt", "one")
	if got := e.read("a", "notes/a.txt"); got != "one" {
		t.Fatalf("read %q", got)
	}

	// Tunnel: no isolation, so guest 127.0.0.1 is the host's.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	c, err := e.d.DialGuest(ctx, "a", l.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	io.ReadFull(c, buf)
	c.Close()
	if string(buf) != "ping" {
		t.Fatalf("tunnel echo %q", buf)
	}

	// Live snapshot + fork alongside.
	s1, err := e.d.Snapshot(ctx, "a", "s1")
	if err != nil || s1.SizeBytes == 0 {
		t.Fatalf("snapshot %+v %v", s1, err)
	}
	idle, _ := e.d.DiskChanged("a")
	e.write("a", "y", "changed")
	if ch, err := e.d.DiskChanged("a"); err != nil || !ch {
		t.Fatalf("DiskChanged after a write: %v %v", ch, err)
	}
	t.Logf("DiskChanged right after the snapshot: %v", idle)
	e.start(spec("b", "s1", proto.BootFork))
	e.write("a", "x", "A")
	e.write("b", "x", "B")
	if e.read("b", "notes/a.txt") != "one" || e.read("a", "x") != "A" || e.read("b", "x") != "B" {
		t.Fatal("fork did not diverge")
	}

	// Stop -> snapshot; resume.
	s2, err := e.d.Stop(ctx, "a", "s2", false)
	if err != nil || s2.ParentID != "s1" {
		t.Fatalf("stop %+v %v", s2, err)
	}
	if _, ok := e.d.Status("a"); ok {
		t.Fatal("stopped sandbox still listed")
	}
	e.start(spec("a", "s2", proto.BootResume))
	if e.read("a", "x") != "A" {
		t.Fatal("file lost across stop/resume")
	}

	// Recover.
	if err := e.d.Restart(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Wait(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if boot := e.call("a", "GET", "/v1/health", nil)["boot"]; boot != "recover" {
		t.Fatalf("boot after restart: %v", boot)
	}

	// Export s2 as a tar and import it on another node.
	tarPath, err := e.d.Store().SnapshotPath("s2")
	if err != nil {
		t.Fatal(err)
	}
	other := newEnv(t, t.TempDir())
	dl := filepath.Join(t.TempDir(), "dl.tar")
	b, _ := os.ReadFile(tarPath)
	os.WriteFile(dl, b, 0o600)
	if err := other.d.Store().ImportSnapshot(s2, dl); err != nil {
		t.Fatal(err)
	}
	other.start(spec("c", "s2", proto.BootFork))
	if other.read("c", "x") != "A" {
		t.Fatal("imported snapshot lacks file")
	}
	other.d.Destroy(ctx, "c")

	// Node restart: running guests are adopted; dead ones become error.
	e.d.Close()
	e = newEnv(t, dir)
	if err := e.d.Wait(ctx, "a"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if e.read("a", "x") != "A" {
		t.Fatal("adopted guest unreachable")
	}
	e.d.mu.Lock()
	pid := e.d.vms["b"].rec.PID
	e.d.mu.Unlock()
	e.d.Close()
	p, _ := os.FindProcess(pid)
	killGroup(p)
	time.Sleep(300 * time.Millisecond)
	e = newEnv(t, dir)
	if st, _ := e.d.Status("b"); st.State != proto.VMError {
		t.Fatalf("dead guest after restart: %+v", st)
	}
	if _, err := e.d.Stop(ctx, "b", "s3", false); err != nil {
		t.Fatal(err)
	}
	if !e.d.Store().HasSnapshot("s3") {
		t.Fatal("s3 missing")
	}

	if err := e.d.Destroy(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Snapshot(ctx, "a", "s9"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("snapshot destroyed: %v", err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		e.d.Store().DeleteSnapshot(id)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "snapshots")); len(ents) != 0 {
		t.Fatalf("snapshots left: %v", ents)
	}
}

func TestExtractTarRejectsEscapes(t *testing.T) {
	for name, entries := range map[string][]tar.Header{
		"dotdot":  {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"abs":     {{Name: "/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"symlink": {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "l/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
	} {
		p := filepath.Join(t.TempDir(), "x.tar")
		f, _ := os.Create(p)
		tw := tar.NewWriter(f)
		for _, h := range entries {
			tw.WriteHeader(&h)
		}
		tw.Close()
		f.Close()
		if err := extractTar(p, filepath.Join(t.TempDir(), "out")); err == nil || !strings.Contains(err.Error(), "tar:") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCopyTreeReadOnlyDirs(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "mod", "pkg"), 0o755)
	os.WriteFile(filepath.Join(src, "mod", "pkg", "f"), []byte("x"), 0o444)
	os.Symlink("pkg/f", filepath.Join(src, "mod", "link"))
	os.Chmod(filepath.Join(src, "mod", "pkg"), 0o555)
	defer os.Chmod(filepath.Join(src, "mod", "pkg"), 0o755)
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "mod", "link")); string(b) != "x" {
		t.Fatalf("copy content %q", b)
	}
	if err := removeAll(dst); err != nil {
		t.Fatal(err)
	}
}
