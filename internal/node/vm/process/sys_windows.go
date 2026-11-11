package process

import (
	"os"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func terminate(p *os.Process) { p.Kill() } // no SIGTERM on Windows
func killGroup(p *os.Process) { p.Kill() }

// waitExit: on Windows Wait works for any process handle, not only children.
func waitExit(p *os.Process, stop <-chan struct{}) {
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-stop:
	}
}

func pidIsOurs(pid int, marker string) bool { return false }
