//go:build !linux

package node

import "os/exec"

func dieWithNode(*exec.Cmd) {}

// NoDumps is Linux only.
func NoDumps() error { return nil }
