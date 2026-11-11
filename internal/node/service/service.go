// Package service installs crowdvm-node as a background service: a systemd
// user unit on Linux; on Windows an SCM service (automatic start, no
// logged-in user needed; elevation required) or, with LogonTask, a logon
// scheduled task of the current user.
package service

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
