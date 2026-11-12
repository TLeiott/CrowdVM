package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/version"
)

// exitCode ends the program with that status and no "error:" line.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func updateCmd(g *globals) *cobra.Command {
	var check, rollback, force bool
	c := &cobra.Command{
		Use:   "update",
		Short: "Install the newer crowdvm-node the server offers (sandboxes keep running)",
		Long: `Download crowdvm-node from the node's server, with crowdvm-guest and
crowdvm-linux-amd64 when they sit next to it, check each against the
server's sha256, and swap them in. The previous files stay as <name>.old.
A running node restarts into the new build, which brings the service unit
up to date; its sandboxes keep running. The crowdvm CLI is not touched: it
updates through its own installer.

An older or equal release is not installed unless you pass --force, nor a
release that was rolled back on this node. When either version is not a
release (a "dev" build), any file that differs from the server's is
installed.

--check only reports whether an update is available (exit 1 if so).
--rollback puts the .old files back (run it again to go forward).`,
		Example: "  crowdvm-node update\n  crowdvm-node update --check || echo update available",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if !check && !rollback && cfg.Server != "" && !node.TrustedUpdateServer(cfg.Server) {
				fmt.Fprintf(os.Stderr, "warning: %s is plain http, so nothing authenticates the download (use https or a Tailscale address)\n", cfg.Server)
			}
			ctx, cancel := signalCtx()
			defer cancel()
			ctx, cancel = context.WithTimeout(ctx, 15*time.Minute)
			defer cancel()
			res, err := node.SelfUpdate(ctx, cfg, node.UpdateOptions{Check: check, Rollback: rollback, Force: force})
			if err != nil {
				return err
			}
			switch {
			case res.Available && !res.Updated && res.Latest == res.Current: // only --check gets here
				fmt.Printf("crowdvm-guest or crowdvm-linux-amd64 differ from %s's; run `crowdvm-node update`\n", res.Server)
				return exitCode(1)
			case res.Available && !res.Updated:
				fmt.Printf("crowdvm-node %s is available from %s (you have %s); run `crowdvm-node update`\n", or(res.Latest, "(a different build)"), res.Server, res.Current)
				return exitCode(1)
			case rollback:
				fmt.Printf("rolled back: %s\n", strings.Join(res.Files, ", "))
			case res.Updated:
				fmt.Printf("updated %s to %s\n", strings.Join(res.Files, ", "), or(res.Latest, "the server's build"))
			default:
				fmt.Printf("crowdvm-node is up to date (%s)\n", res.Current)
				if c, ok := version.Compare(res.Latest, res.Current); ok && c < 0 {
					fmt.Printf("The server offers an older release, %s; --force installs it.\n", res.Latest)
				}
			}
			if !res.Updated {
				return nil
			}
			ran, err := daemon(ctx, cfg, "POST", "/api/restart", nil)
			switch {
			case ran && err == nil:
				fmt.Println("Restarting the node; its sandboxes keep running.")
			case ran && daemonStatus(err) == 409:
				fmt.Println("Restart the node to use it (its sandboxes keep running).")
			case ran:
				return err
			default:
				fmt.Println("No running node answered on its local page; the node uses the new version when it next starts.")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only check; exit 1 when an update is available")
	c.Flags().BoolVar(&rollback, "rollback", false, "put back the files the last update replaced")
	c.Flags().BoolVar(&force, "force", false, "install the server's build even when it is not newer, or was rolled back here")
	return c
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
