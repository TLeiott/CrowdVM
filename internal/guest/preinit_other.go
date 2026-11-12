//go:build !linux

package guest

import "errors"

// Preinit is Linux only (it runs as PID 1 in the VM).
func Preinit() error { return errors.New("preinit: linux only") }
