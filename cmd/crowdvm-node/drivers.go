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
	return f(cfg, log)
}

// logf adapts slog for drivers that take a printf-style logger.
func logf(log *slog.Logger, driver string) func(string, ...any) {
	return func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...), "driver", driver) }
}
