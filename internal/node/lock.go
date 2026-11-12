package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

func lockPath(dir string) string { return filepath.Join(dir, "node.lock") }

var errLocked = errors.New("locked by another process")

// lockDataDir takes an exclusive lock on dir/node.lock for the life of the
// process: two daemons on one data dir took each other's VMs and server
// session (bugs-r12-opus 3).
func lockDataDir(dir string) (*os.File, error) {
	f, err := lockFile(lockPath(dir))
	if errors.Is(err, errLocked) {
		return nil, errDataDirLocked(dir)
	}
	return f, err
}

func errDataDirLocked(dir string) error {
	return fmt.Errorf("another crowdvm-node daemon is running on %s (stop it first, e.g. systemctl --user stop crowdvm-node)", dir)
}

// Data-dir locks this process holds, counted: the CLI takes one before it
// builds a VM driver (which adopts and cleans up VMs) and Run takes another
// (bugs-r13-sonnet 1).
var (
	heldMu sync.Mutex
	held   = map[string]*heldLock{}
)

type heldLock struct {
	f *os.File
	n int
}

// LockDataDir takes the exclusive lock on dir/node.lock, or another
// reference to it when this process already holds it. release drops the
// reference; the lock goes with the last one.
func LockDataDir(dir string) (release func(), err error) {
	key, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	heldMu.Lock()
	defer heldMu.Unlock()
	h := held[key]
	if h == nil {
		if err := os.MkdirAll(dir, 0o700); err != nil { // join creates it
			return nil, err
		}
		f, err := lockDataDir(dir)
		if err != nil {
			return nil, err
		}
		h = &heldLock{f: f}
		held[key] = h
	}
	h.n++
	var once sync.Once
	return func() {
		once.Do(func() {
			heldMu.Lock()
			defer heldMu.Unlock()
			if h.n--; h.n == 0 {
				h.f.Close()
				delete(held, key)
			}
		})
	}, nil
}
