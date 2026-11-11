package node

import (
	"os/exec"
	"syscall"
)

// dieWithNode: the inhibitor must not outlive a killed node, or the machine
// stays awake with nothing to host (systemd-inhibit takes its command along).
func dieWithNode(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
