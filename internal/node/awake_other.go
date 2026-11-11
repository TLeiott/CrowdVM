//go:build !linux

package node

import "os/exec"

func dieWithNode(*exec.Cmd) {}

func noDumps() error { return nil }
