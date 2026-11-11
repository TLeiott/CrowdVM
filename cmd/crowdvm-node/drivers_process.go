package main

import (
	"log/slog"
	"path/filepath"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/node/vm/process"
)

// The process driver runs crowdvm-guest as a plain host process: no isolation.
// Development and tests only.
func init() {
	drivers["process"] = func(cfg *node.Config, log *slog.Logger) (vm.Driver, error) {
		log.Warn("process driver: sandboxes run as host processes WITHOUT isolation; use only for development")
		return process.New(process.Options{
			DataDir:     filepath.Join(cfg.DataDir, "vm-process"),
			GuestBinary: cfg.GuestBinaryPath(),
			Logf:        logf(log, "process"),
		})
	}
}
