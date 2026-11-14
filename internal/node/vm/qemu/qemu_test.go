package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func needQemuIO(t *testing.T) {
	t.Helper()
	for _, b := range []string{"qemu-img", "qemu-io", "qemu-system-x86_64"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
}

func qimg(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("qemu-img", args...).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img %v: %v %s", args, err, out)
	}
}

// qio runs a qemu-io command (e.g. "write -P 0x11 0 64k") on an image chain.
func qio(t *testing.T, img, cmd string) {
	t.Helper()
	args := []string{"-f", "qcow2", "-c", cmd, img}
	if strings.HasPrefix(cmd, "read") {
		args = append([]string{"-r"}, args...)
	}
	out, err := exec.Command("qemu-io", args...).CombinedOutput()
	if err != nil || strings.Contains(string(out), "Pattern verification failed") || strings.Contains(string(out), "failed") {
		t.Fatalf("qemu-io %s %q: %v %s", img, cmd, err, out)
	}
}

// TestStoreFlattenImportGC exercises the disk model without KVM: a dead VM
// (as after a node crash) is adopted as error, Stop freezes its overlay and
// flattens the over-long chain, ImportSnapshot rebases a foreign file, and
// deleted snapshots are collected once nothing builds on them.
func TestStoreFlattenImportGC(t *testing.T) {
	needQemuIO(t)
	dir := t.TempDir()
	src := t.TempDir()
	img := filepath.Join(src, "base.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", img, "64M")
	qio(t, img, "write -P 0x11 0 64k")

	d, err := New(Options{DataDir: dir, MaxChain: 2})
	if err != nil {
		t.Fatal(err)
	}
	st := d.Store()
	if err := st.ImportImage("img1", img); err != nil {
		t.Fatal(err)
	}
	if !st.HasImage("img1") || !slices.Equal(st.Images(), []string{"img1"}) {
		t.Fatal("image not imported")
	}
	// An image with a backing file is stored standalone.
	ovl := filepath.Join(src, "ovl.qcow2")
	imgPath, _ := st.ImagePath("img1")
	qimg(t, "create", "-q", "-f", "qcow2", "-b", imgPath, "-F", "qcow2", ovl, "64M")
	if err := st.ImportImage("img2", ovl); err != nil {
		t.Fatal(err)
	}
	p2, _ := st.ImagePath("img2")
	qio(t, p2, "read -P 0x11 0 64k")
	if info, _ := d.store.info(p2); info.BackingFile != "" {
		t.Fatalf("img2 still has backing %s", info.BackingFile)
	}
	// s1 <- s2 built by hand like frozen overlays.
	layerOn := func(name, parent, parentFmt, pattern string, off string) string {
		rel := "disks/" + name + ".qcow2"
		p := d.store.abs(rel)
		qimg(t, "create", "-q", "-f", "qcow2", "-b", parent, "-F", parentFmt, p, "64M")
		qio(t, p, "write -P "+pattern+" "+off+" 64k")
		return rel
	}
	s1 := layerOn("s1", imgPath, "qcow2", "0x22", "1M")
	if _, err := d.addLayer("bx_t", "s1", s1, "", "img1", 0, nil); err != nil {
		t.Fatal(err)
	}
	s2 := layerOn("s2", d.store.abs(s1), "qcow2", "0x33", "2M")
	if _, err := d.addLayer("bx_t", "s2", s2, "s1", "img1", 0, nil); err != nil {
		t.Fatal(err)
	}
	// A VM whose qemu died with the node: overlay on s2, QMP gone.
	ov := layerOn("sb1-x", d.store.abs(s2), "qcow2", "0x44", "3M")
	vmDir := filepath.Join(dir, "vms", "sb1")
	os.MkdirAll(vmDir, 0o700)
	writeJSON(filepath.Join(vmDir, "vm.json"), vmRecord{
		Spec: proto.VMSpec{SandboxID: "sb1", ImageID: "img1", ParentID: "s2"}, ImageID: "img1",
		Disk: ov, Parent: "s2", QMPNet: "unix", QMPAddr: filepath.Join(vmDir, "nope.sock"),
	})
	d.Close()

	d, err = New(Options{DataDir: dir, MaxChain: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st = d.Store()
	if s, ok := d.Status("sb1"); !ok || s.State != proto.VMError {
		t.Fatalf("dead VM not reported as error: %+v", s)
	}
	if _, err := d.GuestAddr("sb1"); !errors.Is(err, vm.ErrNotRunning) {
		t.Fatalf("GuestAddr: %v", err)
	}
	ref, err := d.Stop(context.Background(), "sb1", "s3", false)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ParentID != "" || ref.ImageID != "img1" || ref.SizeBytes == 0 || !slices.Equal(ref.Supersedes, []string{"s2", "s1"}) {
		t.Fatalf("depth 3 > MaxChain 2 must flatten onto the image: %+v", ref)
	}
	if _, ok := d.Status("sb1"); ok {
		t.Fatal("stopped VM still listed")
	}
	s3, _ := st.SnapshotPath("s3")
	for _, c := range []string{"read -P 0x11 0 64k", "read -P 0x22 1M 64k", "read -P 0x33 2M 64k", "read -P 0x44 3M 64k"} {
		qio(t, s3, c)
	}

	// A snapshot uploaded by another node names that node's parent path.
	foreign := filepath.Join(src, "s4.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", "-b", s3, "-F", "qcow2", foreign, "64M")
	qio(t, foreign, "write -P 0x55 4M 64k")
	qimg(t, "rebase", "-u", "-b", "/elsewhere/s3.qcow2", "-F", "qcow2", foreign)
	if err := st.ImportSnapshot(proto.SnapshotRef{ID: "s4", ParentID: "nope", ImageID: "img1"}, foreign); !errors.Is(err, vm.ErrNoParent) {
		t.Fatalf("import without parent: %v", err)
	}
	if err := st.ImportSnapshot(proto.SnapshotRef{ID: "s4", ParentID: "s3", ImageID: "img1"}, foreign); err != nil {
		t.Fatal(err)
	}
	s4, _ := st.SnapshotPath("s4")
	qio(t, s4, "read -P 0x11 0 64k")
	qio(t, s4, "read -P 0x55 4M 64k")
	if r, ok := st.SnapshotRef("s4"); !ok || r.ParentID != "s3" || r.SizeBytes == 0 {
		t.Fatalf("s4 ref %+v", r)
	}

	// Deleting s1 keeps its file while s2 builds on it; deleting s2 frees both.
	s1Path := d.store.abs(s1)
	st.DeleteSnapshot("s1")
	if st.HasSnapshot("s1") || fileSize(s1Path) == 0 {
		t.Fatal("s1 must be hidden but kept")
	}
	st.DeleteSnapshot("s2")
	if fileSize(s1Path) != 0 || fileSize(d.store.abs(s2)) != 0 {
		t.Fatal("s1/s2 files not collected")
	}
	// s3 is a parent of s4: hidden, kept.
	st.DeleteSnapshot("s3")
	qio(t, s4, "read -P 0x44 3M 64k")
	st.DeleteSnapshot("s4")
	ents, _ := os.ReadDir(filepath.Join(dir, "disks"))
	if len(ents) != 2 { // only the images
		t.Fatalf("disks left: %v", ents)
	}
}

func TestStartErrors(t *testing.T) {
	needQemuIO(t)
	bin := filepath.Join(t.TempDir(), "guest")
	os.WriteFile(bin, []byte("x"), 0o755)
	d, err := New(Options{DataDir: t.TempDir(), GuestBinary: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := d.Start(ctx, proto.VMSpec{SandboxID: "a", ImageID: "missing"}); !errors.Is(err, vm.ErrNoParent) {
		t.Fatalf("missing image: %v", err)
	}
	if err := d.Start(ctx, proto.VMSpec{SandboxID: "../x", ImageID: "missing"}); err == nil {
		t.Fatal("bad id accepted")
	}
	if len(d.List()) != 0 {
		t.Fatal("failed start left a VM")
	}
	if _, err := d.Stop(ctx, "a", "s", false); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("stop unknown: %v", err)
	}
	if err := d.Destroy(ctx, "a"); err != nil {
		t.Fatalf("destroy unknown: %v", err)
	}
}

// TestMergePlan runs the compaction policy over a day of 60 s snapshots:
// the chain stays short and every layer is re-merged only a few times.
func TestMergePlan(t *testing.T) {
	type lay struct{ level, merges int }
	var chain []lay // top first
	maxDepth, maxMerges := 0, 0
	for i := 0; i < 1440; i++ {
		levels := make([]int, len(chain))
		for j, l := range chain {
			levels[j] = l.level
		}
		n, level := mergePlan(levels, 4)
		merges := 0
		for _, l := range chain[:n] {
			merges = max(merges, l.merges+1)
		}
		chain = append([]lay{{level, merges}}, chain[n:]...)
		maxDepth, maxMerges = max(maxDepth, len(chain)), max(maxMerges, merges)
	}
	if maxDepth > 18 || maxMerges > 6 {
		t.Fatalf("depth %d, merges %d", maxDepth, maxMerges)
	}
	t.Logf("1440 snapshots: max depth %d, a byte is re-uploaded at most %d times", maxDepth, maxMerges)
	if n, l := mergePlan([]int{0, 0, 0, 1, 1, 1, 2}, 4); n != 6 || l != 2 {
		t.Fatalf("got %d %d", n, l)
	}
	if n, _ := mergePlan([]int{0, 0, levelUnknown, levelUnknown}, 3); n != 2 {
		t.Fatalf("unknown levels must stop the merge: %d", n)
	}
	if n, _ := mergePlan([]int{0, 0, 0}, -1); n != 0 {
		t.Fatal("fanout < 2 disables merging")
	}
}

func TestArgs(t *testing.T) {
	d := &Driver{store: &store{dir: "/data"}, accel: "kvm", opt: Options{NoNested: true}}
	rec := vmRecord{Spec: proto.VMSpec{SandboxID: "sb", Size: proto.Size{VCPU: 4, MemoryMiB: 8192}}, Disk: "disks/a,b.qcow2"}
	a := strings.Join(mustArgs(t, d, rec, "/data/vms/sb", 4242, "", "unix:/q,server=on,wait=off"), " ")
	for _, want := range []string{"-accel kvm", "-cpu host,-vmx,-svm", "-smp 4", "-m 8192",
		"file=/data/disks/a,,b.qcow2,if=none,id=disk0,format=qcow2,discard=unmap",
		"hostfwd=tcp:127.0.0.1:4242-:7070", "-qmp unix:/q,server=on,wait=off", "readonly=on"} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q:\n%s", want, a)
		}
	}
	a = strings.Join(mustArgs(t, d, rec, "/data/vms/sb", 4242, "/data/vms/sb/net,x.sock", "q"), " ")
	for _, want := range []string{"-netdev stream,id=net0,server=on,addr.type=unix,addr.path=/data/vms/sb/net,,x.sock",
		"virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56,host_mtu=65520"} {
		if !strings.Contains(a, want) {
			t.Errorf("netstack args lack %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "hostfwd") || strings.Contains(a, "user,") {
		t.Errorf("netstack args use slirp: %s", a)
	}
	rec.Spec.Nested = true
	if a := strings.Join(mustArgs(t, d, rec, "/d", 1, "", "q"), " "); !strings.Contains(a, "-cpu host ") {
		t.Errorf("nested must use plain -cpu host: %s", a)
	}
	rec.Spec.Nested, d.opt.NoNested = false, false
	if a := strings.Join(mustArgs(t, d, rec, "/d", 1, "", "q"), " "); !strings.Contains(a, "-cpu host ") {
		t.Errorf("nested is on by default: %s", a)
	}
	d.accel = "whpx"
	if a := strings.Join(mustArgs(t, d, rec, "/d", 1, "", "q"), " "); !strings.Contains(a, "-accel whpx,kernel-irqchip=off -accel tcg") || !strings.Contains(a, "-cpu max") {
		t.Errorf("whpx args: %s", a)
	}
}

// fakeQMP serves one QMP connection: greeting, scripted replies, events.
func TestQMP(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "q.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		fmt.Fprintln(c, `{"QMP":{"version":{},"capabilities":[]}}`)
		dec := json.NewDecoder(c)
		for {
			var req struct {
				Execute string
				ID      int
			}
			if dec.Decode(&req) != nil {
				return
			}
			switch req.Execute {
			case "query-status":
				fmt.Fprintf(c, `{"event":"RESET","data":{}}`+"\n"+`{"return":{"status":"running"},"id":%d}`+"\n", req.ID)
			case "boom":
				fmt.Fprintf(c, `{"error":{"class":"GenericError","desc":"nope"},"id":%d}`+"\n", req.ID)
			case "quit":
				fmt.Fprintf(c, `{"return":{},"id":%d}`+"\n", req.ID)
				return
			default:
				fmt.Fprintf(c, `{"return":{},"id":%d}`+"\n", req.ID)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, err := dialQMP(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := q.cmdWaitEvent(ctx, "query-status", nil, func(e qmpEvent) bool { return e.Name == "RESET" })
	if err != nil || ev.Name != "RESET" {
		t.Fatalf("event: %v %v", ev, err)
	}
	var st struct{ Status string }
	if err := q.cmd(ctx, "query-status", nil, &st); err != nil || st.Status != "running" {
		t.Fatalf("query-status: %v %v", st, err)
	}
	if err := q.cmd(ctx, "boom", nil, nil); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error reply: %v", err)
	}
	q.cmd(ctx, "quit", nil, nil)
	select {
	case <-q.done:
	case <-time.After(3 * time.Second):
		t.Fatal("done not closed on disconnect")
	}
	if err := q.cmd(ctx, "x", nil, nil); !errors.Is(err, errQMPClosed) {
		t.Fatalf("after close: %v", err)
	}
}

// TestBootTimeout runs real qemu (TCG is fine) on a disk without an OS: the
// VM must go to error after BootTimeout and its qemu must be gone.
func TestBootTimeout(t *testing.T) {
	needQemuIO(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "guest")
	os.WriteFile(bin, []byte("x"), 0o755)
	img := filepath.Join(dir, "empty.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", img, "16M")
	d, err := New(Options{DataDir: filepath.Join(dir, "data"), GuestBinary: bin, BootTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var mu sync.Mutex
	var states []string
	d.SetNotify(func(s proto.VMStatus) { mu.Lock(); states = append(states, s.State); mu.Unlock() })
	d.Store().ImportImage("empty", img)
	ctx := context.Background()
	spec := proto.VMSpec{SandboxID: "sb", ImageID: "empty", Size: proto.Size{VCPU: 1, MemoryMiB: 128, DiskGiB: 1}}
	if err := d.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, spec); !errors.Is(err, vm.ErrExists) {
		t.Fatalf("second start: %v", err)
	}
	if _, err := d.GuestAddr("sb"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	r := d.vms["sb"].run
	d.mu.Unlock()
	err = d.Wait(ctx, "sb")
	if err == nil || !strings.Contains(err.Error(), "boot timeout") {
		t.Fatalf("wait: %v", err)
	}
	select {
	case <-r.exited:
	case <-time.After(15 * time.Second):
		t.Fatal("qemu still running after boot timeout")
	}
	if st, _ := d.Status("sb"); st.State != proto.VMError {
		t.Fatalf("state %+v", st)
	}
	if err := d.Destroy(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(states, []string{proto.VMStarting, proto.VMError}) {
		t.Fatalf("notified states %v", states)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "data", "vms")); len(ents) != 0 {
		t.Fatalf("vm dir left: %v", ents)
	}
}

// TestPlanMergeHiddenBase: the merge base may be a layer that is deleted
// (hidden) locally but still in use, e.g. a fork's parent superseded in the
// source's chain. An offline stop merges onto it and keeps the data.
func TestPlanMergeHiddenBase(t *testing.T) {
	needQemuIO(t)
	dir, src := t.TempDir(), t.TempDir()
	img := filepath.Join(src, "base.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", img, "64M")
	d, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Store().ImportImage("img", img); err != nil {
		t.Fatal(err)
	}
	if d.store.imageData("img") != 0 {
		t.Fatal("empty image has no data")
	}
	back, _ := d.Store().ImagePath("img")
	parent := ""
	for i, lvl := range []int{1, 0, 0, 0} { // s0 (level 1) <- s1 <- s2 <- s3
		id := fmt.Sprintf("s%d", i)
		rel := "disks/" + id + ".qcow2"
		qimg(t, "create", "-q", "-f", "qcow2", "-b", back, "-F", "qcow2", d.store.abs(rel), "64M")
		qio(t, d.store.abs(rel), fmt.Sprintf("write -P 0x%d1 %dM 64k", i+1, i))
		if _, err := d.addLayer("bx_t", id, rel, parent, "img", lvl, nil); err != nil {
			t.Fatal(err)
		}
		back, parent = d.store.abs(rel), id
	}
	d.Store().DeleteSnapshot("s0") // hidden, still the base of s1..s3
	step, err := d.planMerge("s3", "img")
	if err != nil || step.n != 3 || step.parent != "s0" || step.level != 1 || strings.Join(step.supersedes, ",") != "s3,s2,s1" {
		t.Fatalf("plan %+v %v", step, err)
	}
	// Freeze a VM overlay on s3 through the plan: data of s1..s3 moves in.
	ov := "disks/vm.qcow2"
	qimg(t, "create", "-q", "-f", "qcow2", "-b", back, "-F", "qcow2", d.store.abs(ov), "64M")
	m := &machine{id: "vm", rec: vmRecord{ImageID: "img", Disk: ov, Parent: "s3"}}
	os.MkdirAll(d.vmDir("vm"), 0o700)
	ref, err := d.freeze(m, "s4")
	if err != nil || ref.ParentID != "s0" || len(ref.Supersedes) != 3 {
		t.Fatalf("freeze %+v %v", ref, err)
	}
	p, _ := d.Store().SnapshotPath("s4")
	for i := range 4 {
		qio(t, p, fmt.Sprintf("read -P 0x%d1 %dM 64k", i+1, i))
	}
}

func mustArgs(t *testing.T, d *Driver, rec vmRecord, dir string, port int, netSock, qmpArg string) []string {
	t.Helper()
	a, err := d.args(rec, dir, port, netSock, []string{"-qmp", qmpArg})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGPUArgs(t *testing.T) {
	d := &Driver{store: &store{dir: "/data"}, accel: "kvm"}
	rec := vmRecord{Spec: proto.VMSpec{SandboxID: "sb", Size: proto.Size{VCPU: 4, MemoryMiB: 8192},
		GPUDevices: []string{"0000:0b:00.0", "0000:0b:00.1"}}, Disk: "disks/a.qcow2"}
	a := strings.Join(mustArgs(t, d, rec, "/data/vms/sb", 4242, "", "q"), " ")
	for _, want := range []string{"-device pcie-root-port,id=gpurp0,chassis=10,slot=10",
		"-device vfio-pci,host=0000:0b:00.0,bus=gpurp0,addr=00.0,multifunction=on",
		"-device vfio-pci,host=0000:0b:00.1,bus=gpurp0,addr=00.1", "-display none"} {
		if !strings.Contains(a, want) {
			t.Errorf("gpu args lack %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "virtio-balloon") {
		t.Errorf("balloon with VFIO: %s", a)
	}
	rec.Spec.GPUDevices = nil
	d.virgl = "/dev/dri/renderD128"
	a = strings.Join(mustArgs(t, d, rec, "/data/vms/sb", 4242, "", "q"), " ")
	for _, want := range []string{"-display egl-headless,rendernode=/dev/dri/renderD128", "-device virtio-gpu-gl-pci", "virtio-balloon-pci"} {
		if !strings.Contains(a, want) {
			t.Errorf("virgl args lack %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "vfio") {
		t.Errorf("vfio without GPU: %s", a)
	}
	if got := strings.Join(gpuArgs([]string{"0000:0b:00.0", "0000:0c:00.0"}), " "); strings.Contains(got, "multifunction") || !strings.Contains(got, "gpurp1") {
		t.Errorf("two slots: %s", got)
	}
}

// TestDeployAfterMerge (review sonnet-13 B1): a saved snapshot's layer was
// deleted on the node (merged away) but kept because the saver builds on
// it. A new deploy must name the layer's real file, not a snap- path that
// never gets written; a record whose file is gone is imported anew.
func TestDeployAfterMerge(t *testing.T) {
	needQemuIO(t)
	dir, src := t.TempDir(), t.TempDir()
	img := filepath.Join(src, "base.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", img, "64M")
	d, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := d.store
	if err := st.ImportImage("img", img); err != nil {
		t.Fatal(err)
	}
	imgPath, _ := st.ImagePath("img")
	layerOn := func(rel, back, pattern string) {
		qimg(t, "create", "-q", "-f", "qcow2", "-b", back, "-F", "qcow2", st.abs(rel), "64M")
		qio(t, st.abs(rel), "write -P "+pattern+" 1M 64k")
	}
	// The saver's frozen overlay became the saved layer; its next layer builds on it.
	saved := "disks/saver-1.qcow2"
	layerOn(saved, imgPath, "0x22")
	if _, err := d.addLayer("bx_t", "saved", saved, "", "img", 0, nil); err != nil {
		t.Fatal(err)
	}
	layerOn("disks/saver-2.qcow2", st.abs(saved), "0x33")
	if _, err := d.addLayer("bx_t", "saver2", "disks/saver-2.qcow2", "saved", "img", 0, nil); err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(src, "download.qcow2") // what the server would send
	qimg(t, "convert", "-O", "qcow2", "-B", imgPath, "-F", "qcow2", st.abs(saved), dl)

	// A deploy's compaction superseded it: deleted, file kept.
	st.DeleteSnapshot("saved")
	if st.HasSnapshot("saved") || fileSize(st.abs(saved)) == 0 {
		t.Fatal("saved layer must be hidden but kept")
	}
	// The next deploy uses the kept file and undeletes it.
	rel, local := st.useLocal("saved")
	if !local || rel != saved || !st.HasSnapshot("saved") {
		t.Fatalf("useLocal = %q %v, want %q local", rel, local, saved)
	}
	ov := st.abs("disks/deploy.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", "-b", st.abs(rel), "-F", "qcow2", ov, "64M")
	qio(t, ov, "read -P 0x22 1M 64k")
	// A download of it afterwards resurrects the same file.
	cp := filepath.Join(src, "dl1.qcow2")
	qimg(t, "convert", "-O", "qcow2", dl, cp)
	if err := st.ImportSnapshot(proto.SnapshotRef{ID: "saved", ImageID: "img"}, cp); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.SnapshotPath("saved"); p != st.abs(saved) {
		t.Fatalf("path %s, want %s", p, st.abs(saved))
	}

	// A record whose file is gone is not trusted: it is imported at snap-<id>.
	removeFile(st.abs(saved))
	if st.HasSnapshot("saved") {
		t.Fatal("HasSnapshot trusts a record without its file")
	}
	if rel, local = st.useLocal("saved"); local || rel != "disks/snap-saved.qcow2" {
		t.Fatalf("useLocal without file = %q %v", rel, local)
	}
	if err := st.ImportSnapshot(proto.SnapshotRef{ID: "saved", ImageID: "img"}, dl); err != nil {
		t.Fatal(err)
	}
	p, err := st.SnapshotPath("saved")
	if err != nil || p != st.abs(rel) {
		t.Fatalf("path %s %v, want %s", p, err, st.abs(rel))
	}
	qio(t, p, "read -P 0x22 1M 64k")
}

// Windows QEMU refuses dump-guest-core=off; elsewhere guest RAM stays out of core files.
func TestMachineOpts(t *testing.T) {
	if o := machineOpts("windows"); strings.Contains(o, "dump-guest-core") {
		t.Fatalf("windows: %s", o)
	}
	if o := machineOpts("linux"); !strings.Contains(o, "dump-guest-core=off") {
		t.Fatalf("linux: %s", o)
	}
}
