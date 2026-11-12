//go:build windows

package node

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockDataDir takes an exclusive lock on dir/node.lock for the life of the
// process (bugs-r12-opus 3).
func lockDataDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(lockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, errDataDirLocked(dir)
	}
	return f, nil
}
