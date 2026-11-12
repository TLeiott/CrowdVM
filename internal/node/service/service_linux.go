//go:build linux

package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	return renderUnit(strings.Join(q, " "), o.FDStore)
}

// managedLine marks a unit this code wrote: Refresh replaces it.
const managedLine = "# Managed by crowdvm-node: an update replaces edits. Put local changes in a drop-in (systemctl --user edit crowdvm-node)."

func renderUnit(execStart string, fdStore bool) string {
	return fmt.Sprintf(`%s
[Unit]
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
%s
[Install]
WantedBy=default.target
`, managedLine, execStart, fdStoreUnit(fdStore))
}

// Refresh rewrites an installed unit from this build's template, keeping
// its ExecStart (binary, data dir), and reloads systemd when that changed
// anything, so an update brings unit changes without a new install-service.
// The daemon calls it as it starts, so the unit comes from the new build.
// changed is false when the unit is current or none is installed.
//
// Only a unit this code wrote is replaced: one with managedLine, or one
// that is exactly what older releases wrote (they lacked the line; each
// wrote this template without it, with or without the fd store lines).
// Any other unit was edited by its owner: it stays as is, and the error
// says so.
func Refresh(out io.Writer) (changed bool, err error) {
	p := UnitPath()
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	old, execStart := string(b), ""
	for _, l := range strings.Split(old, "\n") {
		if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
			execStart = v
			break
		}
	}
	if execStart == "" {
		return false, fmt.Errorf("%s has no ExecStart; run crowdvm-node install-service", p)
	}
	legacy := func(fd bool) string { return strings.Replace(renderUnit(execStart, fd), managedLine+"\n", "", 1) }
	if !strings.Contains(old, managedLine) && old != legacy(false) && old != legacy(true) {
		return false, fmt.Errorf("%s was edited (it lacks the line %q), so it stays as is; `crowdvm-node install-service` writes the current unit", p, managedLine)
	}
	unit := renderUnit(execStart, systemdVersion() >= 254 || strings.Contains(old, "FileDescriptorStorePreserve=yes"))
	if unit == old {
		return false, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(unit), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return false, err
	}
	fmt.Fprintf(out, "updated %s\n", p)
	return true, systemctl(out, "daemon-reload")
}

// fdStoreUnit: the VMs' QMP and NIC sockets live in systemd's fd store, not
// at paths another process of this user could connect to; a restart (and a
// stop/start, with Preserve) gets them back.
func fdStoreUnit(on bool) string {
	if !on {
		return ""
	}
	return `# The VMs' QMP and NIC sockets live in systemd's fd store, not at paths
# another process of this user could connect to; a restart gets them back.
NotifyAccess=main
FileDescriptorStoreMax=1024
FileDescriptorStorePreserve=yes
Environment=CROWDVM_FDSTORE=1
`
}

// systemdVersion is the running systemd's version (0 if unknown).
func systemdVersion() int {
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "systemd" {
		return 0
	}
	v, _ := strconv.Atoi(f[1])
	return v
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
	o.FDStore = o.FDStore || systemdVersion() >= 254
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

// systemctl runs systemctl --user (a variable for tests).
var systemctl = func(out io.Writer, args ...string) error {
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
// systemd unit and the unit starts it again after a failed exit
// (Restart=always or on-failure; ErrRestart exits with status 1).
func RestartsOnExit() bool {
	if os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && restartsOnExit(string(b))
}

func restartsOnExit(cgroup string) bool {
	if !strings.Contains(cgroup, "/"+Name+".service") {
		return false
	}
	p := restartPolicy(strings.Contains(cgroup, "/user@")) // a user unit, or a system one
	return p == "always" || p == "on-failure"
}

// restartPolicy is the unit's Restart= setting (a variable for tests).
var restartPolicy = func(user bool) string {
	args := []string{"show", "-p", "Restart", "--value", Name + ".service"}
	if user {
		args = append([]string{"--user"}, args...)
	}
	out, _ := exec.Command("systemctl", args...).Output()
	return strings.TrimSpace(string(out))
}
