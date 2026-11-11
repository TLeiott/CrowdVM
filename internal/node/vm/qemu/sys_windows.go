package qemu

import (
	"fmt"
	"syscall"
)

func defaultAccel() string { return "whpx" }

// qmpEndpoint: QEMU on Windows has no unix sockets; use loopback TCP.
func qmpEndpoint(dir string) (arg, network, addr string, err error) {
	port, err := freePort()
	if err != nil {
		return "", "", "", err
	}
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	return "tcp:" + addr + ",server=on,wait=off", "tcp", addr, nil
}

func sysProcAttr() *syscall.SysProcAttr {
	const createNoWindow = 0x08000000
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
}

func pidIsOurs(pid int, marker string) bool { return false }
