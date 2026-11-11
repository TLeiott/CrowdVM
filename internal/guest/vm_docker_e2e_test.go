package guest_test

// Real-VM checks of Docker across stop/resume and fork, of what a fork of a
// running sandbox does not keep, of direct-port firewall rules and of the
// in-image Chrome first-run policy (full image):
//
//	CROWDVM_E2E=1 CROWDVM_E2E_IMAGE=~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v3.qcow2 \
//	  go test -v -run VMDocker -timeout 30m ./internal/guest/
//
// Two 3 GiB VMs at most. Data lives under ~/.local/share/crowdvm-dev/guest-e2e
// (never /tmp: the image is copied there).
//
// CROWDVM_E2E_HOLD=<file> (TestVMHold) boots one VM, writes "addr token" to
// the file and keeps it until the file is deleted (manual probing).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm/qemu"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// e2eDir is a fresh directory under ~/.local/share/crowdvm-dev/guest-e2e,
// removed after the test.
func e2eDir(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".local/share/crowdvm-dev/guest-e2e")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, strings.ReplaceAll(t.Name(), "/", "_")+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newVMEnv builds the guest (+ CLI), imports the image and returns a driver
// that destroys every VM when the test ends.
func newVMEnv(t *testing.T, opts ...func(*qemu.Options)) *vmEnv {
	t.Helper()
	img := os.Getenv("CROWDVM_E2E_IMAGE")
	if os.Getenv("CROWDVM_E2E") != "1" || img == "" || runtime.GOOS != "linux" {
		t.Skip("set CROWDVM_E2E=1 and CROWDVM_E2E_IMAGE=<crowdvm qcow2>")
	}
	dir := e2eDir(t)
	bin := filepath.Join(dir, "crowdvm-guest")
	if err := buildStatic(t, "./cmd/crowdvm-guest", bin); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "crowdvm")
	if buildStatic(t, "./cmd/crowdvm", cli) != nil {
		cli = ""
	}
	o := qemu.Options{DataDir: filepath.Join(dir, "node"), GuestBinary: bin, CLIBinary: cli,
		BootTimeout: 3 * time.Minute, Logf: t.Logf}
	for _, f := range opts {
		f(&o)
	}
	d, err := qemu.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, st := range d.List() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_ = d.Destroy(ctx, st.SandboxID)
			cancel()
		}
		d.Close()
	})
	imp := filepath.Join(dir, "img.qcow2")
	if out, err := exec.Command("cp", "--reflink=auto", img, imp).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	if err := d.Store().ImportImage("img", imp); err != nil {
		t.Fatal(err)
	}
	return &vmEnv{t: t, d: d, img: img}
}

func (e *vmEnv) stop(id, snap string, force bool) proto.SnapshotRef {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ref, err := e.d.Stop(ctx, id, snap, force)
	if err != nil {
		e.t.Fatalf("stop %s: %v", id, err)
	}
	return ref
}

// waitFor polls cmd until its output equals want.
func (g guestAPI) waitFor(cmd, want string, max time.Duration) string {
	g.t.Helper()
	var got string
	for deadline := time.Now().Add(max); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		m := g.ok("POST", "/v1/commands", map[string]any{"command": cmd, "timeoutSeconds": 60})
		if got, _ = m["stdout"].(string); strings.TrimSpace(got) == want {
			return want
		}
	}
	return strings.TrimSpace(got)
}

func TestVMDocker(t *testing.T) {
	e := newVMEnv(t)
	g := e.start("dkA", "", proto.BootCreate, proto.GuestConfig{})
	if g.sh("command -v docker >/dev/null && echo y || echo n") != "y" {
		t.Skip("image has no Docker (use the full image)")
	}
	// alpine from Docker Hub; a container per restart policy; one never ran
	g.sh(`docker pull -q alpine:3 >/dev/null && docker pull -q busybox:1 >/dev/null
docker run -d --name plain alpine:3 sleep infinity >/dev/null
docker run -d --name unless --restart unless-stopped alpine:3 sleep infinity >/dev/null
docker create --name never alpine:3 true >/dev/null
docker volume create vol1 >/dev/null && docker run --rm -v vol1:/v alpine:3 sh -c 'echo kept > /v/f'
printf 'FROM alpine:3\nRUN echo built > /built\n' | docker build -q -t local/built - >/dev/null
docker create --name usesbuilt local/built true >/dev/null
printf 'FROM alpine:3\nRUN echo tmp > /tmp1\n' | docker build -q -t local/unused - >/dev/null
mkdir -p ~/.cache/tool && echo c > ~/.cache/tool/x
mkdir -p ~/proj/dist && echo d > ~/proj/dist/out && printf 'dist/\n' > ~/proj/.crowdvmignore`)
	states := `for c in plain unless never usesbuilt; do printf '%s=%s ' $c "$(docker inspect -f '{{.State.Running}}' $c)"; done`
	if got := g.sh(states); got != "plain=true unless=true never=false usesbuilt=false" {
		t.Fatalf("before: %q", got)
	}
	// direct-port firewall: opened, and the user's own rule is not ours
	fw := g.ok("PUT", "/v1/internal/firewall", map[string]any{"open": []string{"8000/tcp"}})
	if fw["active"] != true || !strings.Contains(g.sh("sudo ufw status"), "8000/tcp") {
		t.Errorf("firewall %v / %s", fw, g.sh("sudo ufw status"))
	}
	g.ok("PUT", "/v1/internal/firewall", map[string]any{"open": []string{}})
	if strings.Contains(g.sh("sudo ufw status"), "8000/tcp") {
		t.Errorf("firewall rule not removed: %s", g.sh("sudo ufw status"))
	}

	// ---- fork of the running sandbox (live snapshot) ---------------------
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	snap, err := e.d.Snapshot(ctx, "dkA", "dkLive")
	cancel()
	if err != nil {
		t.Fatalf("live snapshot: %v", err)
	}
	if got := g.sh(states); got != "plain=true unless=true never=false usesbuilt=false" {
		t.Errorf("source after live snapshot: %q", got)
	}
	f := e.start("dkB", snap.ID, proto.BootFork, proto.GuestConfig{})
	if got := f.waitFor(states, "plain=true unless=true never=false usesbuilt=false", 2*time.Minute); got != "plain=true unless=true never=false usesbuilt=false" {
		t.Errorf("fork containers: %q", got)
	}
	if got := f.sh("ls ~/.cache/tool 2>/dev/null | wc -l; test -e ~/proj/dist && echo dist || echo nodist; docker run --rm -v vol1:/v alpine:3 cat /v/f"); got != "0\nnodist\nkept" {
		t.Errorf("fork unsaved paths / volume: %q", got)
	}
	img := `for i in alpine:3 busybox:1 local/built local/unused; do docker image inspect $i >/dev/null 2>&1 && printf '%s ' $i; done`
	if got := f.waitFor(img, "alpine:3 local/built", 2*time.Minute); got != "alpine:3 local/built" {
		t.Errorf("fork images: %q", got)
	}
	e.stop("dkB", "dkBstop", true)

	// ---- stop + resume ---------------------------------------------------
	ref := e.stop("dkA", "dkStop", false)
	g = e.start("dkA", ref.ID, proto.BootResume, proto.GuestConfig{})
	if got := g.waitFor(states, "plain=true unless=true never=false usesbuilt=false", 2*time.Minute); got != "plain=true unless=true never=false usesbuilt=false" {
		t.Errorf("resume containers: %q", got)
	}
	if got := g.sh(img + "; echo; docker run --rm -v vol1:/v alpine:3 cat /v/f; docker buildx du 2>/dev/null | tail -1"); !strings.HasPrefix(got, "alpine:3 local/built \nkept") || !strings.HasSuffix(got, "0B") {
		t.Errorf("resume images / volume: %q", got)
	} else {
		t.Logf("resume: %s", got)
	}
	e.stop("dkA", "dkStop2", true)
}

// TestVMHold boots one VM from CROWDVM_E2E_IMAGE and keeps it for manual
// probing: "addr token" goes to $CROWDVM_E2E_HOLD; delete the file to end.
func TestVMHold(t *testing.T) {
	hold := os.Getenv("CROWDVM_E2E_HOLD")
	if hold == "" {
		t.Skip("set CROWDVM_E2E_HOLD=<file>")
	}
	e := newVMEnv(t)
	g := e.start("hold1", "", proto.BootCreate, proto.GuestConfig{})
	if err := os.WriteFile(hold, []byte(g.addr+" "+g.tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(hold); err != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// CROWDVM_E2E_VIRGL=1: node option gpu.virgl gives the guest an OpenGL
// virtio-gpu rendered on this machine's GPU.
func TestVMVirGL(t *testing.T) {
	if os.Getenv("CROWDVM_E2E_VIRGL") != "1" {
		t.Skip("set CROWDVM_E2E_VIRGL=1 (needs a render node and QEMU with virglrenderer)")
	}
	e := newVMEnv(t, func(o *qemu.Options) { o.VirGL = "on" })
	g := e.start("gl1", "", proto.BootCreate, proto.GuestConfig{})
	out := g.sh("ls /dev/dri; basename $(readlink /sys/class/drm/renderD128/device/driver); sudo dmesg | grep -i -m3 -e virgl -e 'virtio-gpu\\|virtio_gpu' || true")
	t.Logf("guest DRM:\n%s", out)
	if !strings.Contains(out, "renderD128") || !strings.Contains(out, "virtio") {
		t.Fatalf("no virtio-gpu render node in the guest: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "virgl 3d acceleration enabled") && !strings.Contains(out, "+virgl") {
		t.Errorf("virtio-gpu without virgl: %s", out)
	}
	e.stop("gl1", "gl1s", true)
}
