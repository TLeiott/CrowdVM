package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// fakeGPUHost builds a sysfs/dev tree: GPU 0000:0b:00.0 + audio 0000:0b:00.1
// in IOMMU group 16, a bridge in it too, both functions bound to drv.
func fakeGPUHost(t *testing.T, gpuDrv, audioDrv string) (gpuHost, string) {
	t.Helper()
	root := t.TempDir()
	sys, dev := filepath.Join(root, "sys"), filepath.Join(root, "dev")
	grp := filepath.Join(sys, "kernel/iommu_groups/16/devices")
	os.MkdirAll(grp, 0o755)
	os.MkdirAll(filepath.Join(sys, "bus/pci/drivers"), 0o755)
	for _, d := range []struct{ addr, class, drv string }{
		{"0000:0b:00.0", "0x030000", gpuDrv}, {"0000:0b:00.1", "0x040300", audioDrv}, {"0000:00:03.1", "0x060400", "pcieport"},
	} {
		dir := filepath.Join(sys, "bus/pci/devices", d.addr)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "class"), []byte(d.class+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "vendor"), []byte("0x10de\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "device"), []byte("0x2206\n"), 0o644)
		os.Symlink("../../../kernel/iommu_groups/16", filepath.Join(dir, "iommu_group"))
		if d.drv != "" {
			os.Symlink("../../../bus/pci/drivers/"+d.drv, filepath.Join(dir, "driver"))
		}
		os.Symlink("../../../../bus/pci/devices/"+d.addr, filepath.Join(grp, d.addr))
	}
	os.MkdirAll(filepath.Join(dev, "vfio"), 0o755)
	os.WriteFile(filepath.Join(dev, "vfio/16"), nil, 0o600)
	os.WriteFile(filepath.Join(dev, "vfio/vfio"), nil, 0o600)
	h := gpuHost{sys: sys, dev: dev, goos: "linux",
		memlock: func() (uint64, bool) { return 0, false },
		access: func(p string) error {
			if _, err := os.Stat(p); err != nil {
				return errors.New("permission denied")
			}
			return nil
		}}
	return h, root
}

func TestGPUPassthroughChecks(t *testing.T) {
	h, root := fakeGPUHost(t, "vfio-pci", "vfio-pci")
	gpus, err := h.passthrough([]string{"0000:0b:00.0"}, 16384)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("valid GPU refused: %v", err)
	}
	g := gpus[0]
	if g.Vendor != "nvidia" || g.IOMMUGroup != 16 || strings.Join(g.Functions, ",") != "0000:0b:00.0,0000:0b:00.1" || g.Model != "10de:2206" {
		t.Fatalf("gpu %+v", g)
	}
	fail := func(name string, h gpuHost, addrs []string, want string) {
		t.Helper()
		if _, err := h.passthrough(addrs, 16384); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", name, err, want)
		}
	}
	hostDrv, _ := fakeGPUHost(t, "nvidia", "vfio-pci")
	fail("host driver", hostDrv, []string{"0000:0b:00.0"}, "it is bound to nvidia, not vfio-pci")
	audio, _ := fakeGPUHost(t, "vfio-pci", "snd_hda_intel")
	fail("group member", audio, []string{"0000:0b:00.0"}, "IOMMU group 16 also holds 0000:0b:00.1, which is bound to snd_hda_intel")
	fail("not a GPU", h, []string{"0000:0b:00.1"}, "not a display controller")
	fail("missing", h, []string{"0000:0c:00.0"}, "no such PCI device")
	fail("same group twice", h, []string{"0000:0b:00.0", "0000:0b:00.0"}, "")
	low := h
	low.memlock = func() (uint64, bool) { return 64 << 20, true }
	fail("memlock", low, []string{"0000:0b:00.0"}, "memlock limit (64 MiB) is below")
	win := h
	win.goos = "windows"
	fail("windows", win, []string{"0000:0b:00.0"}, "needs a Linux node")
	os.Remove(filepath.Join(root, "dev/vfio/16"))
	fail("no access", h, []string{"0000:0b:00.0"}, "/dev/vfio/16")
	noIOMMU, root2 := fakeGPUHost(t, "vfio-pci", "vfio-pci")
	os.Remove(filepath.Join(root2, "sys/bus/pci/devices/0000:0b:00.0/iommu_group"))
	fail("iommu off", noIOMMU, []string{"0000:0b:00.0"}, "the IOMMU is off")
}

func TestPCIList(t *testing.T) {
	got, err := parsePCIList("0B:00.0, 0000:0c:00.0 0b:00.0")
	if err != nil || strings.Join(got, ",") != "0000:0b:00.0,0000:0c:00.0" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parsePCIList("0b:00"); err == nil {
		t.Fatal("bad address accepted")
	}
	var c Config
	if err := c.Set("gpu.passthrough", "0b:00.0"); err != nil || c.GPU.Passthrough[0] != "0000:0b:00.0" {
		t.Fatalf("set: %v %+v", err, c.GPU)
	}
	if v, _ := c.Get("gpu.virgl"); v != "off" {
		t.Fatalf("virgl default %q", v)
	}
	if err := c.Set("gpu.virgl", "maybe"); err == nil {
		t.Fatal("bad virgl accepted")
	}
	if err := c.Set("gpu.virgl", "/dev/dri/renderD128"); err != nil || c.GPU.VirGL != "/dev/dri/renderD128" {
		t.Fatal(err)
	}
}

// One VM per passthrough GPU; usage, status and NodeInfo show it.
func TestGPUAllocation(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	if err := h.node.initGPUs(gpuHost{}); err != nil { // nothing configured: nothing to check
		t.Fatal(err)
	}
	h.node.cfgMu.Lock()
	h.node.cfg.GPU.Passthrough = []string{"0000:0b:00.0"}
	h.node.cfgMu.Unlock()
	if err := h.node.initGPUs(gpuHost{}); err == nil || !strings.Contains(err.Error(), "qemu driver") {
		t.Fatalf("fake driver accepted: %v", err)
	}
	sp := spec("bx_g1")
	sp.GPU = true
	resp, body := call(t, h.sess, "POST", "/v1/vms", sp)
	if resp.StatusCode != 503 || !strings.Contains(string(body), "passes no GPU through") {
		t.Fatalf("no GPUs: %d %s", resp.StatusCode, body)
	}
	h.node.mu.Lock()
	h.node.gpus = []proto.PassthroughGPU{{Address: "0000:0b:00.0", Vendor: "nvidia", IOMMUGroup: 16, Functions: []string{"0000:0b:00.0", "0000:0b:00.1"}}}
	h.node.mu.Unlock()
	if resp, body := call(t, h.sess, "POST", "/v1/vms", sp); resp.StatusCode != 202 {
		t.Fatalf("gpu create: %d %s", resp.StatusCode, body)
	}
	h.waitState(t, "bx_g1", proto.VMRunning)
	if got := h.drv.specOf("bx_g1").GPUDevices; strings.Join(got, ",") != "0000:0b:00.0,0000:0b:00.1" {
		t.Fatalf("driver got devices %v", got)
	}
	sp2 := spec("bx_g2")
	sp2.GPU = true
	if resp, body := call(t, h.sess, "POST", "/v1/vms", sp2); resp.StatusCode != 503 || !strings.Contains(string(body), "no free GPU") {
		t.Fatalf("second GPU VM: %d %s", resp.StatusCode, body)
	}
	if resp, body := call(t, h.sess, "POST", "/v1/vms", spec("bx_plain")); resp.StatusCode != 202 {
		t.Fatalf("plain VM: %d %s", resp.StatusCode, body)
	}
	if h.drv.specOf("bx_plain").GPUDevices != nil {
		t.Fatal("plain VM got a GPU")
	}
	if u := h.node.Usage(); u.GPUs != 1 {
		t.Fatalf("usage %+v", u)
	}
	if info := h.node.Info(); len(info.GPUs) != 1 || info.GPUs[0].InUse != "bx_g1" {
		t.Fatalf("info gpus %+v", info.GPUs)
	}
	if st := h.vmState("bx_g1"); st.GPU != "0000:0b:00.0" {
		t.Fatalf("status gpu %q", st.GPU)
	}
	call(t, h.sess, "DELETE", "/v1/vms/bx_g1", nil)
	if resp, body := call(t, h.sess, "POST", "/v1/vms", sp2); resp.StatusCode != 202 {
		t.Fatalf("GPU free again: %d %s", resp.StatusCode, body)
	}
}

func (d *fakeDriver) specOf(id string) proto.VMSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.starts) - 1; i >= 0; i-- {
		if d.starts[i].SandboxID == id {
			return d.starts[i]
		}
	}
	return proto.VMSpec{}
}

// CROWDVM_E2E_GPU=<PCI address>: run the start-time checks on this machine
// (a GPU still bound to its host driver must be refused with the fix).
func TestGPUPassthroughRealHost(t *testing.T) {
	addr := os.Getenv("CROWDVM_E2E_GPU")
	if addr == "" {
		t.Skip("set CROWDVM_E2E_GPU=<PCI address of a GPU>")
	}
	gpus, err := realGPUHost().passthrough([]string{addr}, 8192)
	t.Logf("gpus=%+v err=%v", gpus, err)
	if err == nil && len(gpus) != 1 {
		t.Fatal("no error and no GPU")
	}
}
