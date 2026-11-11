//go:build !windows

package bench

import "os/exec"

func hideWindow(*exec.Cmd) {}
