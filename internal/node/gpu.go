package node

// GPU passthrough (VFIO). A node owner lists GPUs to lend with
//
//	crowdvm-node config set gpu.passthrough 0000:0b:00.0[,0000:0c:00.0]
//
// Each must be a display controller in its own IOMMU group whose devices
// are all bound to vfio-pci (bridges excepted), with /dev/vfio/<group>
// usable by the node's user and a memlock limit that covers a VM's RAM. The
// node checks this when it starts and refuses to start otherwise, naming
// the device and the fix. Only these GPUs are advertised as schedulable
// (proto.NodeInfo.GPUs); a VM with VMSpec.GPU gets one for itself, with the
// rest of its IOMMU group (e.g. the HDMI audio function).
//
// gpu.virgl (optional, separate capability): every VM gets a virtio-gpu
// with OpenGL rendered on this machine's GPU (QEMU egl-headless +
// virglrenderer), for 3D desktops and EGL apps. No CUDA.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/proto"
)

var pciAddrRe = regexp.MustCompile(`^(?:([0-9a-f]{4}):)?([0-9a-f]{2}):([0-9a-f]{2})\.([0-7])$`)

// normPCI turns "0b:00.0" or "0000:0B:00.0" into "0000:0b:00.0".
func normPCI(s string) (string, error) {
	m := pciAddrRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return "", fmt.Errorf("%q is not a PCI address (want e.g. 0000:0b:00.0; `lspci -D` lists them)", s)
	}
	dom := m[1]
	if dom == "" {
		dom = "0000"
	}
	return dom + ":" + m[2] + ":" + m[3] + "." + m[4], nil
}

// parsePCIList parses a comma/space separated list of PCI addresses.
func parsePCIList(val string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ' ' }) {
		a, err := normPCI(f)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// gpuHost is the machine the checks look at (a fake tree in tests).
type gpuHost struct {
	sys, dev string
	goos     string
	// memlock: the soft RLIMIT_MEMLOCK in bytes (unlimited = false limited).
	memlock func() (bytes uint64, limited bool)
	// access: nil when the node's user can read and write path.
	access func(path string) error
	// name: the device's model name ("" if unknown).
	name func(addr string) string
}

func realGPUHost() gpuHost {
	return gpuHost{sys: "/sys", dev: "/dev", goos: runtime.GOOS, memlock: memlockLimit, access: rwAccess, name: lspciName}
}

func lspciName(addr string) string {
	out, err := exec.Command("lspci", "-mm", "-s", addr).Output()
	if err != nil {
		return ""
	}
	// 0b:00.0 "VGA compatible controller" "NVIDIA Corporation" "GA102 [GeForce RTX 3080]" ...
	parts := strings.Split(string(out), `"`)
	if len(parts) >= 6 {
		return strings.TrimSpace(parts[5])
	}
	return ""
}

var gpuVendors = map[string]string{"0x10de": "nvidia", "0x1002": "amd", "0x1022": "amd", "0x8086": "intel"}

func readTrim(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }

// passthrough validates the owner's gpu.passthrough list for a node that
// shares memMiB of RAM. Any problem is an error that names the device and
// the fix; nothing is advertised then.
func (h gpuHost) passthrough(addrs []string, memMiB int) ([]proto.PassthroughGPU, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	if h.goos != "linux" {
		return nil, errors.New("gpu.passthrough needs a Linux node (VFIO); unset it with: crowdvm-node config set gpu.passthrough \"\"")
	}
	var out []proto.PassthroughGPU
	groups := map[int]string{}
	for _, addr := range addrs {
		g, err := h.checkGPU(addr)
		if err != nil {
			return nil, fmt.Errorf("gpu.passthrough %s: %w", addr, err)
		}
		if other, dup := groups[g.IOMMUGroup]; dup {
			return nil, fmt.Errorf("gpu.passthrough %s: in IOMMU group %d with %s; a group goes to one VM, so list only one of them", addr, g.IOMMUGroup, other)
		}
		groups[g.IOMMUGroup] = addr
		out = append(out, g)
	}
	if err := h.access(filepath.Join(h.dev, "vfio/vfio")); err != nil {
		return nil, fmt.Errorf("gpu.passthrough: /dev/vfio/vfio: %w (load the vfio-pci module: modprobe vfio-pci)", err)
	}
	if lim, limited := h.memlock(); limited && lim < uint64(memMiB)<<20 {
		return nil, fmt.Errorf("gpu.passthrough: the memlock limit (%d MiB) is below the memory this node shares (%d MiB); "+
			"VFIO pins all RAM of a GPU VM. Raise it (systemd: LimitMEMLOCK=infinity; shell: ulimit -l unlimited, "+
			"or /etc/security/limits.conf) or lower `memory`", lim>>20, memMiB)
	}
	return out, nil
}

func (h gpuHost) checkGPU(addr string) (proto.PassthroughGPU, error) {
	g := proto.PassthroughGPU{Address: addr}
	dir := filepath.Join(h.sys, "bus/pci/devices", addr)
	if _, err := os.Stat(dir); err != nil {
		return g, errors.New("no such PCI device (`lspci -D` lists them)")
	}
	if class := readTrim(filepath.Join(dir, "class")); !strings.HasPrefix(class, "0x03") {
		return g, fmt.Errorf("not a display controller (PCI class %s)", class)
	}
	vid := readTrim(filepath.Join(dir, "vendor"))
	g.Vendor = gpuVendors[vid]
	if g.Vendor == "" {
		g.Vendor = vid
	}
	g.Model = strings.TrimPrefix(vid, "0x") + ":" + strings.TrimPrefix(readTrim(filepath.Join(dir, "device")), "0x")
	if h.name != nil {
		if n := h.name(addr); n != "" {
			g.Model = n
		}
	}
	link, err := os.Readlink(filepath.Join(dir, "iommu_group"))
	if err != nil {
		return g, errors.New("the IOMMU is off: enable VT-d / AMD-Vi in the firmware and boot with intel_iommu=on (Intel) or amd_iommu=on (AMD) and iommu=pt")
	}
	if g.IOMMUGroup, err = strconv.Atoi(filepath.Base(link)); err != nil {
		return g, fmt.Errorf("unreadable IOMMU group %q", link)
	}
	members, _ := os.ReadDir(filepath.Join(h.sys, "kernel/iommu_groups", strconv.Itoa(g.IOMMUGroup), "devices"))
	g.Functions = []string{addr}
	for _, m := range members {
		ma := m.Name()
		mdir := filepath.Join(h.sys, "bus/pci/devices", ma)
		class := readTrim(filepath.Join(mdir, "class"))
		if strings.HasPrefix(class, "0x0604") || strings.HasPrefix(class, "0x0600") { // PCI bridges / host bridges stay
			continue
		}
		drv := "none"
		if l, err := os.Readlink(filepath.Join(mdir, "driver")); err == nil {
			drv = filepath.Base(l)
		}
		if drv != "vfio-pci" {
			what := "it"
			if ma != addr {
				what = "its IOMMU group " + strconv.Itoa(g.IOMMUGroup) + " also holds " + ma + ", which"
			}
			return g, fmt.Errorf("%s is bound to %s, not vfio-pci: every device of the group must be (e.g. `sudo driverctl set-override %s vfio-pci`; the host can then no longer use it)", what, drv, ma)
		}
		if ma != addr {
			g.Functions = append(g.Functions, ma)
		}
	}
	slices.Sort(g.Functions[1:])
	vf := filepath.Join(h.dev, "vfio", strconv.Itoa(g.IOMMUGroup))
	if err := h.access(vf); err != nil {
		return g, fmt.Errorf("%s: %w (give the node's user the device, e.g. a udev rule SUBSYSTEM==\"vfio\", OWNER=\"<user>\", or run the node as root)", vf, err)
	}
	return g, nil
}

// ---------------------------------------------------------------------------
// Node: validation at start, allocation per VM
// ---------------------------------------------------------------------------

// initGPUs validates gpu.passthrough when the node starts (an error stops
// the start) and remembers the GPUs it may hand out.
func (n *Node) initGPUs(h gpuHost) error {
	c := n.config()
	if len(c.GPU.Passthrough) == 0 {
		return nil
	}
	if n.drv.Name() != "qemu" {
		return fmt.Errorf("gpu.passthrough needs the qemu driver (this node runs %q)", n.drv.Name())
	}
	gpus, err := h.passthrough(c.GPU.Passthrough, c.Capacity().MemoryMiB)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.gpus = gpus
	n.mu.Unlock()
	n.markInfoDirty()
	for _, g := range gpus {
		n.log.Info("GPU passthrough ready", "gpu", g.Address, "model", g.Model, "iommuGroup", g.IOMMUGroup, "functions", g.Functions)
	}
	return nil
}

// gpusLocked (n.mu held) lists the passthrough GPUs with their current user.
func (n *Node) gpusLocked() []proto.PassthroughGPU {
	if len(n.gpus) == 0 {
		return nil
	}
	out := slices.Clone(n.gpus)
	for i := range out {
		for _, r := range n.vms {
			if active(r.status.State) && gpuOf(r.spec) == out[i].Address {
				out[i].InUse = r.id
			}
		}
	}
	return out
}

// takeGPULocked (n.mu held) gives spec a free GPU: one VM per device.
func (n *Node) takeGPULocked(spec *proto.VMSpec) error {
	if !spec.GPU {
		spec.GPUDevices = nil
		return nil
	}
	gpus := n.gpusLocked()
	if len(gpus) == 0 {
		return &capacityError{"this node passes no GPU through (crowdvm-node config set gpu.passthrough <PCI address>)"}
	}
	for _, g := range gpus {
		if g.InUse == "" {
			spec.GPUDevices = slices.Clone(g.Functions)
			return nil
		}
	}
	return &capacityError{fmt.Sprintf("no free GPU (%d passed through, all in use)", len(gpus))}
}

func gpuOf(spec proto.VMSpec) string {
	if len(spec.GPUDevices) == 0 {
		return ""
	}
	return spec.GPUDevices[0]
}
