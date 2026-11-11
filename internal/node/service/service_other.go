//go:build !linux && !windows

package service

import (
	"context"
	"errors"
	"io"
)

func Install(Options, io.Writer) error { return errors.New("install-service: unsupported OS") }
func Uninstall(io.Writer) error        { return errors.New("uninstall-service: unsupported OS") }

// RunService: no service manager integration; handled = false.
func RunService(func(context.Context) error) (bool, error) { return false, nil }

// RestartsOnExit: no service manager here restarts the node.
func RestartsOnExit() bool { return false }
