package process

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile reflinks src into dst (btrfs, xfs, ...).
func cloneFile(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
