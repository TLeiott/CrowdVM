//go:build !linux && !windows

package qemu

import (
	"os"
	"path/filepath"
	"syscall"
)

func defaultAccel() string { return "tcg" }

func qmpEndpoint(dir string) (arg, network, addr string, err error) {
	p := filepath.Join(dir, "qmp.sock")
	os.Remove(p)
	return "unix:" + p + ",server=on,wait=off", "unix", p, nil
}

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func pidIsOurs(pid int, marker string) bool { return false }
