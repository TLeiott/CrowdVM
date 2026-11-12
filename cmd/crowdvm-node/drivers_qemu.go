package main

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/node/vm/netstack"
	"github.com/crowdvm/crowdvm/internal/node/vm/qemu"
)

func init() {
	drivers["qemu"] = func(cfg *node.Config, log *slog.Logger) (vm.Driver, error) {
		allow, err := netstack.ParsePrefixes(cfg.Net.Allow)
		if err != nil {
			return nil, fmt.Errorf("net.allow: %w", err)
		}
		deny, err := netstack.ParsePrefixes(cfg.Net.Deny)
		if err != nil {
			return nil, fmt.Errorf("net.deny: %w", err)
		}
		if cfg.Net.Mode == qemu.NetSlirp {
			log.Warn("net=slirp: sandboxes can reach this machine's localhost services, its LAN and its tailnet; " +
				"untrusted sandboxes are NOT contained. Switch back with: crowdvm-node config set net netstack")
		}
		return qemu.New(qemu.Options{
			DataDir:     filepath.Join(cfg.DataDir, "vm"),
			GuestBinary: cfg.GuestBinaryPath(),
			CLIBinary:   cfg.CLIBinaryPath(),
			NoNested:    cfg.NoNested,
			Net:         cfg.Net.Mode,
			NetPolicy:   netstack.Policy{AllowLAN: cfg.Net.AllowLAN, Allow: allow, Deny: deny},
			ServerAddr:  cfg.ServerAddr(),
			ServerURL:   cfg.Server,
			VirGL:       cfg.GPU.VirGL,
			Logf:        logf(log, "qemu"),
		})
	}
}
