//go:build !linux

package node

import "os/exec"

func dieWithNode(*exec.Cmd) {}
