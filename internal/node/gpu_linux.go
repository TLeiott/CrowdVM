//go:build linux

package node

import "golang.org/x/sys/unix"

// memlockLimit is the soft RLIMIT_MEMLOCK (limited=false: unlimited).
func memlockLimit() (uint64, bool) {
	var r unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_MEMLOCK, &r) != nil || r.Cur == unix.RLIM_INFINITY {
		return 0, false
	}
	return r.Cur, true
}

func rwAccess(p string) error { return unix.Access(p, unix.R_OK|unix.W_OK) }
