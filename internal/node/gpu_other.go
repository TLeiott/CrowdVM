//go:build !linux

package node

import "errors"

func memlockLimit() (uint64, bool) { return 0, false }

func rwAccess(string) error { return errors.New("not supported on this OS") }
