package qemu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// gpuArgs gives a VM the PCI functions of a passthrough GPU (the node
// validated them: proto.VMSpec.GPUDevices, the GPU first). Functions of one
// physical slot sit behind one PCIe root port with their own function
// numbers (multifunction), so the guest sees a PCIe GPU with its audio
// function, as on bare metal.
func gpuArgs(devs []string) []string {
	type slotFns struct {
		slot string
		fns  []string
	}
	var slots []*slotFns
	for _, d := range devs {
		slot, _, _ := strings.Cut(d, ".")
		i := slices.IndexFunc(slots, func(s *slotFns) bool { return s.slot == slot })
		if i < 0 {
			slots = append(slots, &slotFns{slot: slot})
			i = len(slots) - 1
		}
		slots[i].fns = append(slots[i].fns, d)
	}
	var a []string
	for i, s := range slots {
		port := fmt.Sprintf("gpurp%d", i)
		a = append(a, "-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d,slot=%d", port, 10+i, 10+i))
		for _, d := range s.fns {
			_, fn, _ := strings.Cut(d, ".")
			dev := fmt.Sprintf("vfio-pci,host=%s,bus=%s,addr=00.%s", d, port, fn)
			if len(s.fns) > 1 && fn == "0" {
				dev += ",multifunction=on"
			}
			a = append(a, "-device", dev)
		}
	}
	return a
}

// checkVirGL validates Options.VirGL ("on" or a render node path) and
// returns the render node: it must be usable by this user, and QEMU must
// have virtio-gpu-gl-pci and the egl-headless display.
func checkVirGL(qemuBin, want string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("gpu.virgl needs a Linux node")
	}
	node := want
	if node == "on" || node == "true" {
		nodes, _ := filepath.Glob("/dev/dri/renderD*")
		slices.Sort(nodes)
		if len(nodes) == 0 {
			return "", errors.New("gpu.virgl: no DRM render node (/dev/dri/renderD*): install the GPU's driver")
		}
		node = nodes[0]
	}
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("gpu.virgl: %w (add the node's user to the render group)", err)
	}
	f.Close()
	dev, _ := exec.Command(qemuBin, "-device", "help").Output()
	if !strings.Contains(string(dev), `"virtio-gpu-gl-pci"`) {
		return "", errors.New("gpu.virgl: this QEMU has no virtio-gpu-gl-pci (built without virglrenderer/OpenGL)")
	}
	disp, _ := exec.Command(qemuBin, "-display", "help").Output()
	if !strings.Contains(string(disp), "egl-headless") {
		return "", errors.New("gpu.virgl: this QEMU has no egl-headless display")
	}
	return node, nil
}

// VirGL reports whether VMs get an OpenGL virtio-gpu (proto.NodeInfo.VirGL).
func (d *Driver) VirGL() bool { return d.virgl != "" }
