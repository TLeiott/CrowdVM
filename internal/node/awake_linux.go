package node

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// dieWithNode: the inhibitor must not outlive a killed node, or the machine
// stays awake with nothing to host (systemd-inhibit takes its command along).
func dieWithNode(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }

// noDumps: neither this process nor the VMs it starts may write core
// dumps, and this process cannot be read through ptrace or /proc/<pid>/mem
// by the owner's other processes. A crashed QEMU's core holds the guest's
// RAM; systemd-coredump would keep it readable for the owner (secure
// layer). The hard limit 0 is inherited by QEMU and cannot be raised again
// without root.
func noDumps() error {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
