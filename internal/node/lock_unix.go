//go:build !windows

package node

import (
	"errors"
	"os"
	"syscall"
)

// lockDataDir takes an exclusive lock on dir/node.lock for the life of the
// process: two daemons on one data dir took each other's VMs and server
// session (bugs-r12-opus 3).
func lockDataDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(lockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errDataDirLocked(dir)
		}
		return nil, err
	}
	return f, nil
}
