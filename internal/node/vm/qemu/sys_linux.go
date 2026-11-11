package qemu

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func defaultAccel() string {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return "tcg"
	}
	f.Close()
	return "kvm"
}

// qmpEndpoint returns the -qmp argument and the address to dial for a VM dir.
func qmpEndpoint(dir string) (arg, network, addr string, err error) {
	p := filepath.Join(dir, "qmp.sock")
	if len(p) > 100 { // sun_path limit
		sum := sha256.Sum256([]byte(dir))
		p = filepath.Join(os.TempDir(), fmt.Sprintf("crowdvm-qmp-%x.sock", sum[:8]))
	}
	os.Remove(p)
	return "unix:" + p + ",server=on,wait=off", "unix", p, nil
}

// sysProcAttr detaches qemu from the node's process group so VMs survive a
// node restart (they are adopted again) and terminal signals.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// pidIsOurs reports whether pid is a live process whose command line
// contains marker (guards against pid reuse before killing).
func pidIsOurs(pid int, marker string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Contains(b, []byte(marker))
}
