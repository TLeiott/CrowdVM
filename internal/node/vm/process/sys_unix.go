//go:build !windows

package process

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"time"
)

// The guest runs in its own process group so Stop can kill the commands it
// started too, and terminal signals to the node do not reach it.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func terminate(p *os.Process) { syscall.Kill(-p.Pid, syscall.SIGTERM) }
func killGroup(p *os.Process) { syscall.Kill(-p.Pid, syscall.SIGKILL) }

// waitExit returns when the (non-child) process is gone or stop closes.
func waitExit(p *os.Process, stop <-chan struct{}) {
	for syscall.Kill(p.Pid, 0) == nil {
		select {
		case <-stop:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// pidIsOurs reports whether pid runs with marker on its command line (Linux
// /proc; elsewhere false, so nothing is killed on a guess).
func pidIsOurs(pid int, marker string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.Contains(b, []byte(marker))
}
