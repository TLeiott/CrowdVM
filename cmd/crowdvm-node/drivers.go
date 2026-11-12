package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/vm"
)

// drivers maps --driver names to constructors (registered by drivers_*.go).
var drivers = map[string]func(cfg *node.Config, log *slog.Logger) (vm.Driver, error){}

func newDriver(cfg *node.Config, log *slog.Logger) (vm.Driver, error) {
	name := cfg.Driver
	if name == "" {
		name = "qemu"
	}
	f, ok := drivers[name]
	if !ok {
		var names []string
		for k := range drivers {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown or unavailable driver %q (available: %s)", name, strings.Join(names, ", "))
	}
	// Building a driver adopts the data dir's VMs and cleans up the ones it
	// cannot reach: never while a daemon runs on it (bugs-r13-sonnet 1). The
	// lock stays until the process exits; Run takes its own reference.
	release, err := node.LockDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	drv, err := f(cfg, log)
	if err != nil {
		release()
	}
	return drv, err
}

// logf adapts slog for drivers that take a printf-style logger.
func logf(log *slog.Logger, driver string) func(string, ...any) {
	return func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...), "driver", driver) }
}
