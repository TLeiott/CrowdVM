//go:build windows

package node

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on path, held until the file is closed;
// errLocked when another open file holds it (also one of this process).
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, errLocked
	}
	return f, nil
}
