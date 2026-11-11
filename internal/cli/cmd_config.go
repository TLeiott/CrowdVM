package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// configKeys are the settings `crowdvm config` reads and writes.
var configKeys = []string{"server", "profile", "current", "user"}

func (a *App) configCmd() *cobra.Command {
	show := func(cmd *cobra.Command, args []string) error {
		p := a.prof()
		out := map[string]any{
			"profile": a.profile, "defaultProfile": firstNonEmpty(a.config().Profile, "default"),
			"server": p.Server, "user": p.User, "current": a.current(), "signedIn": p.APIKey != "",
			"path": filepath.Join(a.configDir(), "config.json"), "profiles": sortedKeys(a.config().Profiles),
		}
		return a.result(out, func() {
			for _, k := range []string{"profile", "defaultProfile", "server", "user", "current", "signedIn", "path"} {
				a.printf("%-15s %v\n", k+":", out[k])
			}
		})
	}
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change local settings (server URL, default profile)",
		Long: `Show or change the settings in the config file. Keys:
  server    the CrowdVM server URL of the active profile
  profile   the profile used when --profile and CROWDVM_PROFILE are not set
  current   the "current" sandbox (of this shell, with the shell function)
  user      the signed-in login (read-only)`,
		Example: `  crowdvm config set server http://10.0.0.5:8420
  crowdvm config get server
  crowdvm --profile work config set server http://work-box:8420`,
		Args: cobra.NoArgs,
		RunE: show,
	}
	get := &cobra.Command{
		Use: "get KEY", Short: "Print one setting", Args: cobra.ExactArgs(1), ValidArgs: configKeys,
		RunE: func(cmd *cobra.Command, args []string) error {
			var v string
			switch args[0] {
			case "server":
				v = a.prof().Server
			case "profile":
				v = firstNonEmpty(a.config().Profile, "default")
			case "current":
				v = a.current()
			case "user":
				v = a.prof().User
			default:
				return usagef("unknown key %q (want %s)", args[0], strings.Join(configKeys, ", "))
			}
			return a.result(map[string]any{"key": args[0], "value": v}, func() { a.printf("%s\n", v) })
		},
	}
	set := &cobra.Command{
		Use: "set KEY VALUE", Short: "Change one setting", Args: cobra.ExactArgs(2), ValidArgs: configKeys,
		RunE: func(cmd *cobra.Command, args []string) error {
			v := strings.TrimSpace(args[1])
			switch args[0] {
			case "server":
				v = client.NormalizeBase(v)
				p := a.prof()
				if p.Server != "" && p.Server != v && p.APIKey != "" {
					// A key belongs to one server: keep it only for the old one.
					p.APIKey, p.User = "", ""
					a.info("signed out: the saved key belongs to %s\n", p.Server)
				}
				p.Server = v
			case "profile":
				a.config().Profile = v
			case "current":
				id, err := a.sandboxID(v)
				if err != nil {
					return err
				}
				a.setCurrent(id)
				return a.result(map[string]any{"key": "current", "value": id}, func() { a.printf("current: %s\n", id) })
			default:
				return usagef("cannot set %q (want server, profile or current)", args[0])
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			return a.result(map[string]any{"key": args[0], "value": v}, func() { a.printf("%s: %s\n", args[0], v) })
		},
	}
	unset := &cobra.Command{
		Use: "unset KEY", Short: "Clear one setting", Args: cobra.ExactArgs(1), ValidArgs: configKeys,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "server":
				p := a.prof()
				p.Server, p.APIKey, p.User = "", "", ""
			case "profile":
				a.config().Profile = ""
			case "current":
				a.setCurrent("")
				return a.result(map[string]any{"key": "current", "value": ""}, func() { a.printf("current: -\n") })
			default:
				return usagef("cannot unset %q", args[0])
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			return a.result(map[string]any{"key": args[0], "value": ""}, func() { a.printf("%s: -\n", args[0]) })
		},
	}
	cmd.AddCommand(get, set, unset,
		&cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "Show all settings", Args: cobra.NoArgs, RunE: show},
		&cobra.Command{
			Use: "path", Short: "Print the config file path", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				p := filepath.Join(a.configDir(), "config.json")
				return a.result(map[string]any{"path": p}, func() { a.printf("%s\n", p) })
			},
		})
	return cmd
}

// shellInitCmd prints the shell function that gives each interactive shell
// its own "current" sandbox and conversation (the installer adds it).
func (a *App) shellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell-init bash|zsh|fish|powershell",
		Short: "Print the shell function that keeps `current` per shell",
		Long: `Print a shell function named crowdvm. It passes CROWDVM_SHELL_ID (the
shell's process id) to every call, so "current" and the current conversation
belong to this shell: two terminals can work on two sandboxes. The installer
adds it to your shell startup file; to add it yourself:

  bash/zsh:    eval "$(crowdvm shell-init bash)"      (zsh: shell-init zsh)
  fish:        crowdvm shell-init fish | source
  PowerShell:  crowdvm shell-init powershell | Out-String | Invoke-Expression

Scripts and CI jobs do not read startup files: pass sandbox ids there.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			a.rawOutput(cmd)
			s, err := shellFunction(args[0], a.Exe)
			if err != nil {
				return err
			}
			fmt.Fprint(a.Out, s)
			return nil
		},
	}
}

func shellFunction(shell, exe string) (string, error) {
	abs := exe != "" && filepath.IsAbs(exe)
	switch shell {
	case "bash", "zsh", "sh":
		bin := "command crowdvm"
		if abs {
			bin = shellQuote(exe)
		}
		return fmt.Sprintf("crowdvm() { CROWDVM_SHELL_ID=\"$$\" %s \"$@\"; }\n", bin), nil
	case "fish":
		bin := "crowdvm" // env runs the program, not this function
		if abs {
			bin = shellQuote(exe)
		}
		return fmt.Sprintf("function crowdvm\n    env CROWDVM_SHELL_ID=$fish_pid %s $argv\nend\n", bin), nil
	case "powershell", "pwsh":
		bin := "crowdvm.exe"
		if abs {
			bin = exe
		}
		bin = "'" + strings.ReplaceAll(bin, "'", "''") + "'"
		return "function crowdvm {\n  $prev = $env:CROWDVM_SHELL_ID\n  $env:CROWDVM_SHELL_ID = \"$PID\"\n" +
			"  try { & " + bin + " @args } finally { $env:CROWDVM_SHELL_ID = $prev }\n}\n", nil
	}
	return "", usagef("unknown shell %q (want bash, zsh, fish or powershell)", shell)
}

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the CLI version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.result(map[string]any{"version": client.Version, "os": runtime.GOOS, "arch": runtime.GOARCH},
				func() { a.printf("crowdvm %s (%s/%s)\n", client.Version, runtime.GOOS, runtime.GOARCH) })
		},
	}
}

// selfUpdateCmd replaces this binary with the server's build for this
// platform (GET /downloads/{os}/{arch}/crowdvm). It runs the download with
// --version first and refuses a build older than this one (CLI-025).
func (a *App) selfUpdateCmd() *cobra.Command {
	var check, force bool
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Install the newer CLI build the server offers",
		Long: `Download the crowdvm build for this platform from the server
(/downloads/<os>/<arch>/crowdvm), read its version, and replace this binary
when the download is newer. An older or equal build is not installed unless
you pass --force. When either version is not a release (a "dev" build), the
CLI compares file contents instead and installs any build that differs.

--check only reports whether an update is available (exit 1 if so).

After other commands the CLI checks the server's version at most every
6 hours and prints a one-line notice on the terminal when a newer release is
available. It never installs anything on its own. Turn the check off with
--no-update or CROWDVM_NO_UPDATE=1.`,
		Example: "  crowdvm self-update\n  crowdvm self-update --check && echo up to date",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := a.server()
			if srv == "" {
				return &client.Error{Code: "not_signed_in", Message: "no CrowdVM server configured; run `crowdvm config set server URL`"}
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			name, tmp := "crowdvm", exe+".new"
			if runtime.GOOS == "windows" {
				name += ".exe"
				tmp = strings.TrimSuffix(exe, ".exe") + ".new.exe" // Windows runs only .exe files
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			body, err := a.newClient(srv, "").DownloadBinary(ctx, runtime.GOOS, runtime.GOARCH, name)
			if err != nil {
				return err
			}
			defer body.Close()
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return fmt.Errorf("cannot write next to %s: %w", exe, err)
			}
			defer os.Remove(tmp)
			h := sha256.New()
			n, err := io.Copy(io.MultiWriter(f, h), body)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil && n < 1<<20 {
				err = fmt.Errorf("the download is only %d bytes; not a crowdvm binary", n)
			}
			if err != nil {
				return err
			}
			// Decide by version when both are releases, else by content.
			offered := binaryVersion(ctx, tmp)
			if offered == "" { // --check too: it is not an available update (bugs-r9-sonnet)
				// It does not even answer --version here (wrong platform,
				// damaged): never replace a working CLI with it (bugs-r8-sonnet).
				return fmt.Errorf("the binary from %s does not run on this machine (crowdvm --version failed); keeping %s", srv, exe)
			}
			cmpV, comparable := compareVersions(offered, client.Version)
			available := !sameHash(exe, h.Sum(nil))
			if comparable {
				available = cmpV > 0
			}
			out := map[string]any{"binary": exe, "server": srv, "version": client.Version, "offered": offered, "updated": false, "available": available}
			if check || (!available && !force) {
				if err := a.result(out, func() {
					switch {
					case available:
						a.printf("crowdvm %s is available from %s (you have %s); run `crowdvm self-update`\n", firstNonEmpty(offered, "(unknown version)"), srv, client.Version)
					case comparable && cmpV < 0:
						a.printf("the server offers crowdvm %s, older than this build (%s); not installing it (use --force to downgrade)\n", offered, client.Version)
					default:
						a.printf("crowdvm is up to date (%s, %s)\n", client.Version, exe)
					}
				}); err != nil {
					return err
				}
				if check && available {
					return exitError{1}
				}
				return nil
			}
			if err := replaceBinary(exe, tmp); err != nil {
				return err
			}
			out["updated"] = true
			return a.result(out, func() {
				a.printf("updated %s from %s to %s\n", exe, client.Version, firstNonEmpty(offered, "the server's build"))
			})
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only check; exit 1 when an update is available")
	cmd.Flags().BoolVar(&force, "force", false, "install the server's build even when it is not newer")
	return cmd
}

func sameHash(path string, sum []byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), sum)
}

// replaceBinary moves next over exe. Windows cannot overwrite a running
// executable, but it can rename it out of the way.
func replaceBinary(exe, next string) error {
	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
		if err := os.Rename(next, exe); err != nil {
			_ = os.Rename(old, exe)
			return err
		}
		return nil
	}
	return os.Rename(next, exe)
}

// feedbackCmd sends a note to the server operators.
func (a *App) feedbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "feedback [MESSAGE]",
		Short: "Send feedback to the operators of this CrowdVM server",
		Long: `Send feedback to the people who run this CrowdVM server (administrators
read it with 'crowdvm admin feedback'). Without MESSAGE the text is read from
stdin. You can send 5 per minute and 30 per day.`,
		Example: "  crowdvm feedback \"fork is slow on large disks\"\n  cat notes.txt | crowdvm feedback",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := strings.TrimSpace(strings.Join(args, " "))
			if msg == "" {
				if isTerminal(a.In) {
					fmt.Fprintln(a.Err, "Type your feedback, then Ctrl+D:")
				}
				b, err := io.ReadAll(io.LimitReader(a.In, 64<<10))
				if err != nil {
					return err
				}
				msg = strings.TrimSpace(string(b))
			}
			if msg == "" {
				return usagef("give a message (argument or stdin)")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			sbID := a.Getenv("CROWDVM_SANDBOX_ID") // sent from inside a sandbox
			r, err := c.Feedback(cmd.Context(), msg, sbID)
			if err != nil {
				if e, ok := client.AsError(err); ok && (e.Status == 404 || e.Status == 405 || e.Status == 501) {
					return &client.Error{Status: e.Status, Code: "feedback_unavailable",
						Message: "this server does not collect feedback; tell its administrator directly (server: " + a.server() + ")"}
				}
				return err
			}
			return a.result(r, func() { a.printf("thanks, your feedback was sent\n") })
		},
	}
}
