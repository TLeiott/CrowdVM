package node

import (
	"fmt"
	"path/filepath"
)

func lockPath(dir string) string { return filepath.Join(dir, "node.lock") }

func errDataDirLocked(dir string) error {
	return fmt.Errorf("another crowdvm-node daemon is running on %s (stop it first, e.g. systemctl --user stop crowdvm-node)", dir)
}
