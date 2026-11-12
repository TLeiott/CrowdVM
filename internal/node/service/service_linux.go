//go:build linux

package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// UnitPath is ~/.config/systemd/user/crowdvm-node.service ($XDG_CONFIG_HOME honored).
func UnitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", Name+".service")
}

// Unit renders the systemd user unit.
func Unit(o Options) string {
	q := make([]string, 0, len(o.Args)+1)
	for _, a := range append([]string{o.Exe}, o.Args...) {
		q = append(q, systemdQuote(a))
	}
	return fmt.Sprintf(`[Unit]
Description=CrowdVM node (lends this machine's compute to a CrowdVM pool)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=5
# Only the node process: its VMs keep running through a restart (a config
# change, an upgrade) and the node adopts them when it starts again.
# KillMode=mixed killed every VM on "systemctl --user restart" (bugs-r8-opus 1).
KillMode=process
TimeoutStopSec=60
# The VMs' QMP and NIC sockets live in systemd's fd store, not at paths
# another process of this user could connect to; a restart gets them back.
NotifyAccess=main
FileDescriptorStoreMax=1024
FileDescriptorStorePreserve=yes
Environment=CROWDVM_FDSTORE=1

[Install]
WantedBy=default.target
`, strings.Join(q, " "))
}

func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// Install writes the user unit and (unless NoStart) enables and starts it.
func Install(o Options, out io.Writer) error {
	p := UnitPath()
	unit := Unit(o)
	if o.DryRun {
		fmt.Fprintf(out, "# would write %s:\n%s", p, unit)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", p)
	if err := systemctl(out, "daemon-reload"); err != nil {
		return err
	}
	if !o.NoStart {
		if err := systemctl(out, "enable", "--now", Name+".service"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "\nTo keep the node running while you are logged out, enable lingering once:\n  loginctl enable-linger %s\n", os.Getenv("USER"))
	fmt.Fprintf(out, "Logs: journalctl --user -u %s -f\n", Name)
	return nil
}

// Uninstall stops/disables the unit and removes it.
func Uninstall(out io.Writer) error {
	// KillMode=process leaves the VMs running on a stop (a restart adopts
	// them). Uninstalling ends the node for good: its VMs go too
	// (bugs-r9-sonnet).
	_ = systemctl(io.Discard, "kill", "--kill-whom=all", "--signal=SIGTERM", Name+".service")
	time.Sleep(3 * time.Second)
	_ = systemctl(io.Discard, "kill", "--kill-whom=all", "--signal=SIGKILL", Name+".service")
	_ = systemctl(out, "disable", "--now", Name+".service")
	if err := os.Remove(UnitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", UnitPath())
	return systemctl(out, "daemon-reload")
}

func systemctl(out io.Writer, args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// RunService: Linux services are plain processes (systemd); handled = false.
func RunService(func(context.Context) error) (bool, error) { return false, nil }

// RestartsOnExit reports whether this process runs as the crowdvm-node
// systemd unit, whose Restart=always starts it again after any exit.
func RestartsOnExit() bool {
	if os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(b), "/"+Name+".service")
}
