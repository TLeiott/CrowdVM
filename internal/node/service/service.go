// Package service installs crowdvm-node as a background service: a systemd
// user unit on Linux; on Windows an SCM service (automatic start, no
// logged-in user needed; elevation required) or, with LogonTask, a logon
// scheduled task of the current user.
package service

import "errors"

// ErrRestart ends a daemon whose owner asked for a restart on the local
// page; the service manager starts it again (RestartsOnExit, RunService).
var ErrRestart = errors.New("restarting (requested on the local page)")

// Options for Install.
type Options struct {
	Exe     string   // absolute path of crowdvm-node
	Args    []string // e.g. ["run", "--data-dir", "..."]
	NoStart bool     // only write/register, don't enable+start
	DryRun  bool     // print what would be done
	// LogonTask (Windows): a logon scheduled task instead of a service.
	LogonTask bool
}

// Name of the unit / scheduled task.
const Name = "crowdvm-node"
