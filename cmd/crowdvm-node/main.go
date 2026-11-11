// Command crowdvm-node is the CrowdVM node app: it benchmarks the machine,
// joins a CrowdVM server and hosts sandbox VMs for the pool within the
// capacity the owner shares.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/crowdvm/crowdvm/internal/node"
	"github.com/crowdvm/crowdvm/internal/node/bench"
	"github.com/crowdvm/crowdvm/internal/node/service"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type globals struct {
	dataDir string
	verbose bool
}

func rootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "crowdvm-node",
		Short:         "Lend this machine's compute to a CrowdVM pool",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.dataDir, "data-dir", node.DefaultDataDir(), "node data directory (config, images, snapshots, VMs)")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "debug logging")
	root.AddCommand(joinCmd(g), runCmd(g), benchCmd(g), statusCmd(g), openCmd(g), configCmd(g), imageCmd(g),
		installServiceCmd(g), uninstallServiceCmd(), versionCmd())
	return root
}

func (g *globals) logger() *slog.Logger { return g.loggerTo(os.Stderr) }

func (g *globals) loggerTo(w io.Writer) *slog.Logger {
	lvl := slog.LevelInfo
	if g.verbose {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
}

func (g *globals) config() (*node.Config, error) {
	return node.LoadConfig(g.dataDir)
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// nodeFlags are the settings shared by join and run; set flags are saved to
// the config file.
type nodeFlags struct {
	cpus, maxSandboxes          int
	memory, disk                string
	overcommit, reserve         string
	driver, directHost, guest   string
	cli                         string
	name, uiAddr, snapshotEvery string
	net                         string
}

func (f *nodeFlags) register(c *cobra.Command) {
	fl := c.Flags()
	fl.IntVar(&f.cpus, "cpus", 0, "vCPUs to share (default: half the logical CPUs)")
	fl.StringVar(&f.memory, "memory", "", "memory to share, e.g. 16G (default: half the RAM)")
	fl.StringVar(&f.disk, "disk", "", "real disk to share for images, snapshots and VM overlays, e.g. 200G (default: 90% of what the data dir can use)")
	fl.StringVar(&f.overcommit, "disk-overcommit", "", "sandbox disk sizes may add up to disk × this, as overlays are thin (default 4)")
	fl.StringVar(&f.reserve, "disk-reserve", "", "free space always kept on this machine's disk, e.g. 20G (default 10G)")
	fl.IntVar(&f.maxSandboxes, "max-sandboxes", 0, "max concurrent sandboxes (default: cpus/2)")
	fl.StringVar(&f.driver, "driver", "", "VM driver: qemu | process (process = no isolation, dev/tests only)")
	fl.StringVar(&f.directHost, "direct-host", "", "LAN or Tailscale IP on which to publish per-sandbox ssh ports")
	fl.StringVar(&f.guest, "guest-binary", "", "crowdvm-guest binary for the config drive (default: next to crowdvm-node)")
	fl.StringVar(&f.cli, "cli-binary", "", `linux/amd64 crowdvm CLI for the config drive, installed in sandboxes as crowdvm/host/boat (default: crowdvm-linux-amd64, or crowdvm on linux/amd64, next to crowdvm-node; "off" to disable)`)
	fl.StringVar(&f.name, "name", "", "node name (default: hostname)")
	fl.StringVar(&f.uiAddr, "ui-addr", "", `local status UI address (default 127.0.0.1:8421, "off" to disable)`)
	fl.StringVar(&f.snapshotEvery, "snapshot-interval", "", `periodic live snapshot interval override, e.g. 5m or "off" (default: server setting, 60s)`)
	fl.StringVar(&f.net, "net", "", "guest networking: netstack (default; blocks this machine, LAN and tailnet) | slirp (INSECURE fallback)")
}

func (f *nodeFlags) apply(c *cobra.Command, cfg *node.Config) error {
	set := func(flag, key, val string) error {
		if c.Flags().Changed(flag) {
			return cfg.Set(key, val)
		}
		return nil
	}
	for _, e := range []struct{ flag, key, val string }{
		{"cpus", "cpus", fmt.Sprint(f.cpus)}, {"memory", "memory", f.memory}, {"disk", "disk", f.disk},
		{"max-sandboxes", "max-sandboxes", fmt.Sprint(f.maxSandboxes)}, {"driver", "driver", f.driver},
		{"direct-host", "direct-host", f.directHost}, {"guest-binary", "guest-binary", f.guest}, {"cli-binary", "cli-binary", f.cli},
		{"disk-overcommit", "disk-overcommit", f.overcommit}, {"disk-reserve", "disk-reserve", f.reserve},
		{"name", "name", f.name}, {"ui-addr", "ui-addr", f.uiAddr}, {"snapshot-interval", "snapshot-interval", f.snapshotEvery},
		{"net", "net", f.net},
	} {
		if err := set(e.flag, e.key, e.val); err != nil {
			return err
		}
	}
	if cfg.Driver == "" {
		cfg.Driver = "qemu"
	}
	return nil
}

// ---------------------------------------------------------------------------

func joinCmd(g *globals) *cobra.Command {
	var f nodeFlags
	var server, token string
	var skipBench bool
	c := &cobra.Command{
		Use:   "join --server URL --token TOKEN",
		Short: "Benchmark this machine and register it with a CrowdVM server",
		RunE: func(c *cobra.Command, _ []string) error {
			if server == "" || token == "" {
				return errors.New("--server and --token are required")
			}
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if err := f.apply(c, cfg); err != nil {
				return err
			}
			drv, err := newDriver(cfg, g.logger())
			if err != nil {
				return err
			}
			defer drv.Close()
			n := node.New(cfg, drv, g.logger())
			if !skipBench {
				fmt.Fprintln(os.Stderr, "benchmarking this machine (network is measured once connected)…")
				if _, err := n.RunBenchmark(c.Context(), false); err != nil {
					return err
				}
			}
			if err := node.Join(c.Context(), cfg, server, token, n.Info()); err != nil {
				return err
			}
			b, _ := node.LoadBenchmark(cfg.DataDir)
			fmt.Printf("joined %s as %s (%s)\n", cfg.Server, cfg.NodeID, cfg.NodeName())
			if b.Tier != "" {
				fmt.Printf("benchmark: score %d, tier %s\n", b.Score, b.Tier)
			}
			cp := cfg.Capacity()
			plural := "es"
			if cp.MaxSandboxes == 1 {
				plural = ""
			}
			fmt.Printf("sharing: %d vCPU, %d MiB memory, %d GiB disk, up to %d sandbox%s\n", cp.VCPU, cp.MemoryMiB, cp.DiskGiB, cp.MaxSandboxes, plural)
			// A non-default data dir must be passed again, or run/install-service
			// would look for a config that is not there (field test).
			cmdName := "crowdvm-node"
			if g.dataDir != node.DefaultDataDir() {
				cmdName += " --data-dir " + shellArg(g.dataDir)
			}
			fmt.Printf("start it with: %s run   (or %s install-service)\n", cmdName, cmdName)
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "server URL, e.g. http://192.168.1.10:8420")
	c.Flags().StringVar(&token, "token", "", "join token from the server admin")
	c.Flags().BoolVar(&skipBench, "skip-bench", false, "register without benchmarking (runs on first start)")
	f.register(c)
	return c
}

func runCmd(g *globals) *cobra.Command {
	var f nodeFlags
	c := &cobra.Command{
		Use:   "run",
		Short: "Run the node daemon (connects to the server and hosts VMs)",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if err := f.apply(c, cfg); err != nil {
				return err
			}
			if c.Flags().NFlag() > 0 {
				if err := cfg.Save(); err != nil {
					return err
				}
			}
			// supervised: a service manager starts the daemon again after it
			// exits with an error, so the local page may offer "Restart node".
			daemon := func(ctx context.Context, log *slog.Logger, supervised bool) (err error) {
				// Secure layer: no core dumps of this process or the VMs it
				// starts (a QEMU core holds the guest's RAM) and no reading
				// this process's memory from the owner's other processes.
				if err := node.NoDumps(); err != nil {
					log.Warn("cannot disable core dumps", "err", err)
				}
				drv, err := newDriver(cfg, log)
				if err != nil {
					return err
				}
				defer drv.Close()
				n := node.New(cfg, drv, log)
				ctx, stop := context.WithCancel(ctx)
				defer stop()
				var restart atomic.Bool
				if supervised {
					n.SetRestarter(func() {
						log.Info("restart requested on the local page")
						restart.Store(true)
						stop()
					})
				}
				defer func() {
					if restart.Load() && err == nil {
						err = service.ErrRestart // non-zero exit: the service manager starts it again
					}
				}()
				if addr := cfg.UIListen(); addr != "off" {
					go func() {
						if err := n.ServeUI(ctx, addr); err != nil {
							log.Warn("status UI disabled", "addr", addr, "err", err)
						}
					}()
				}
				log.Info("crowdvm-node starting", "version", node.Version, "node", cfg.NodeID, "server", cfg.Server, "driver", drv.Name(), "data", cfg.DataDir)
				err = n.Run(ctx)
				return err
			}
			// Started by the Windows service manager: no console, log to a file.
			if handled, err := service.RunService(func(ctx context.Context) error {
				f, err := os.OpenFile(filepath.Join(cfg.DataDir, "node.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				return daemon(ctx, g.loggerTo(f), true)
			}); handled {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()
			return daemon(ctx, g.logger(), service.RestartsOnExit())
		},
	}
	f.register(c)
	return c
}

// daemonError is a refusal from the running daemon's local API.
type daemonError struct {
	status int
	msg    string
}

func (e *daemonError) Error() string { return e.msg }

func daemonStatus(err error) int {
	var de *daemonError
	if errors.As(err, &de) {
		return de.status
	}
	return 0
}

// uiURL is the local daemon's status UI base URL.
func uiURL(cfg *node.Config) string {
	addr := cfg.UIListen()
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)
	return "http://" + addr
}

// daemon calls the running daemon's local API; ok=false when it isn't running.
func daemon(ctx context.Context, cfg *node.Config, method, path string, out any) (bool, error) {
	return daemonBody(ctx, cfg, method, path, "", out)
}

func daemonBody(ctx context.Context, cfg *node.Config, method, path, body string, out any) (bool, error) {
	if cfg.UIListen() == "off" {
		return false, nil
	}
	req, _ := http.NewRequestWithContext(ctx, method, uiURL(cfg)+path, strings.NewReader(body))
	req.Header.Set(node.UIHeader, "cli")
	if t, err := node.ReadUIToken(cfg.DataDir); err == nil {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return true, &daemonError{status: resp.StatusCode, msg: e.Error.Message}
		}
		return true, &daemonError{status: resp.StatusCode, msg: fmt.Sprintf("daemon: %s: %s", resp.Status, bytes.TrimSpace(b))}
	}
	if out != nil {
		return true, json.Unmarshal(b, out)
	}
	return true, nil
}

func benchCmd(g *globals) *cobra.Command {
	var asJSON, quick, local bool
	c := &cobra.Command{
		Use:   "bench",
		Short: "Benchmark this machine (CPU, memory, disk, network to the server, GPUs, virtualization)",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()
			var b proto.Benchmark
			q := "?wait=1"
			if quick {
				q += "&quick=1"
			}
			ran := false
			if !local {
				if ran, err = daemon(ctx, cfg, "POST", "/api/bench"+q, &b); err != nil {
					return err
				}
			}
			if !ran {
				b, err = localBench(ctx, g, cfg, quick, !asJSON)
				if err != nil {
					return err
				}
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(b)
			}
			printBench(os.Stdout, b)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	c.Flags().BoolVar(&quick, "quick", false, "quick run (< 5 s, less accurate)")
	c.Flags().BoolVar(&local, "local", false, "don't ask the running daemon; benchmark in this process")
	return c
}

// localBench runs the benchmark in-process; with a joined node the network
// test runs over a short-lived server session.
func localBench(ctx context.Context, g *globals, cfg *node.Config, quick, progress bool) (proto.Benchmark, error) {
	if cfg.Server == "" || cfg.NodeToken == "" {
		o := bench.Options{Quick: quick, Dir: cfg.DataDir}
		if progress {
			o.Log = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "  "+f+"\n", a...) }
		}
		b := bench.Run(ctx, o)
		return b, node.SaveBenchmark(cfg.DataDir, b)
	}
	drv, err := newDriver(cfg, g.logger())
	if err != nil {
		return proto.Benchmark{}, err
	}
	defer drv.Close()
	log := g.logger()
	if !progress {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	n := node.New(cfg, drv, log)
	var b proto.Benchmark
	err = n.WithSession(ctx, func(ctx context.Context) error {
		b, err = n.RunBenchmark(ctx, quick)
		return err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "server unreachable (%v); benchmarking without network\n", err)
		b, err = n.RunBenchmark(ctx, quick)
	}
	return b, err
}

func printBench(w io.Writer, b proto.Benchmark) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	cat := b.Categories
	fmt.Fprintf(tw, "CPU\t%s, %d cores / %d threads\tsingle %.0f, multi %.0f units/s\t%s\n", b.CPU.Model, b.CPU.Cores, b.CPU.Threads, b.CPU.SingleScore, b.CPU.MultiScore, cat["cpu"])
	fmt.Fprintf(tw, "Memory\t%.1f GiB total, %.1f GiB available\t%.1f GB/s copy\t%s\n", float64(b.Memory.TotalMiB)/1024, float64(b.Memory.AvailableMiB)/1024, b.Memory.BandwidthMBps/1000, cat["memory"])
	fmt.Fprintf(tw, "Disk\t%.0f GiB free (%s)\twrite %.0f MB/s, read %.0f MB/s\t%s\n", b.Disk.FreeGiB, b.Disk.Path, b.Disk.WriteMBps, b.Disk.ReadMBps, cat["disk"])
	if b.Network.DownMbps > 0 {
		fmt.Fprintf(tw, "Network\tvia %s, %.2f ms\tdown %.0f Mbps, up %.0f Mbps\t%s\n", b.Network.Via, b.Network.LatencyMs, b.Network.DownMbps, b.Network.UpMbps, cat["network"])
	} else {
		fmt.Fprintf(tw, "Network\tnot measured (not connected)\t\t%s\n", cat["network"])
	}
	if len(b.GPUs) == 0 {
		fmt.Fprintf(tw, "GPU\tnone\t\t%s\n", cat["gpu"])
	}
	for _, gp := range b.GPUs {
		extra := ""
		if gp.Compute != "" {
			extra = ", compute " + gp.Compute
		}
		fmt.Fprintf(tw, "GPU\t%s (%s)\t%d MiB VRAM, driver %s%s\t%s\n", gp.Model, gp.Vendor, gp.VRAMMiB, gp.Driver, extra, cat["gpu"])
	}
	v := b.Virtualization
	q := v.QEMU
	if q == "" {
		q = "not found"
	}
	fmt.Fprintf(tw, "Virtualization\t%s (nested: %v)\tQEMU %s\t\n", v.Accel, v.Nested, q)
	tw.Flush()
	fmt.Fprintf(w, "\nScore %d / 1000 — tier %s  (took %.1fs)\n", b.Score, strings.ToUpper(b.Tier), float64(b.DurationMs)/1000)
}

func statusCmd(g *globals) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the node's connection, capacity, usage and VMs",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			var st node.Status
			running, err := daemon(c.Context(), cfg, "GET", "/api/status", &st)
			if err != nil {
				return err
			}
			if !running {
				b, _ := node.LoadBenchmark(cfg.DataDir)
				st = node.Status{NodeID: cfg.NodeID, Name: cfg.NodeName(), Version: node.Version, Server: cfg.Server,
					Driver: cfg.Driver, OS: runtime.GOOS, Arch: runtime.GOARCH, DataDir: cfg.DataDir, Paused: cfg.Paused,
					Pause: cfg.EffectivePause(time.Now()), Schedule: cfg.Schedule, Benchmark: b, Capacity: cfg.Capacity()}
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatus(os.Stdout, st, running, uiURL(cfg))
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

func printStatus(w io.Writer, s node.Status, running bool, ui string) {
	id := s.NodeID
	if id == "" {
		id = "(not joined)"
	}
	fmt.Fprintf(w, "Node      %s  %s  v%s  %s/%s  driver %s\n", s.Name, id, s.Version, s.OS, s.Arch, s.Driver)
	switch {
	case !running:
		fmt.Fprintf(w, "Daemon    not running\n")
	case s.Connection.Connected:
		fmt.Fprintf(w, "Server    %s  connected since %s, last heartbeat %s\n", s.Server, s.Connection.Since.Format(time.Kitchen), s.Connection.LastHeartbeat.Format(time.TimeOnly))
	default:
		fmt.Fprintf(w, "Server    %s  DISCONNECTED (%s)\n", s.Server, s.Connection.LastError)
	}
	if running {
		fmt.Fprintf(w, "UI        %s\n", ui)
	}
	fmt.Fprintf(w, "Accepting %s\n", acceptLine(s))
	if s.Schedule != "" {
		fmt.Fprintf(w, "Schedule  %s\n", s.Schedule)
	}
	if running && (s.EarnedCredits != 0 || s.Earned24h != 0) {
		fmt.Fprintf(w, "Earned    %.2f credits (%.2f in the last 24 h)\n", s.EarnedCredits, s.Earned24h)
	}
	if s.Benchmark.Tier != "" {
		fmt.Fprintf(w, "Benchmark score %d, tier %s  %v\n", s.Benchmark.Score, s.Benchmark.Tier, readableMap(s.Benchmark.Categories))
	}
	u, cp := s.Usage, s.Capacity
	fmt.Fprintf(w, "Usage     %d/%d vCPU  %d/%d MiB  %d/%d GiB  %d/%d sandboxes\n", u.VCPU, cp.VCPU, u.MemoryMiB, cp.MemoryMiB, u.DiskGiB, cp.DiskGiB, u.VMs, cp.MaxSandboxes)
	if d := s.Disk; d.BudgetGiB > 0 {
		fmt.Fprintf(w, "Disk      %.1f/%d GiB stored, %.1f GiB free on the host (%d GiB kept free), sandbox sizes may total %gx\n",
			float64(d.UsedBytes)/(1<<30), d.BudgetGiB, float64(d.FreeBytes)/(1<<30), d.ReserveGiB, d.Overcommit)
	}
	if len(s.VMs) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "\nSANDBOX\tSTATE\tSIZE\tDIRECT")
		for _, v := range s.VMs {
			st := v.State
			if v.Error != "" {
				st += " (" + v.Error + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s %dvCPU/%dMiB\t%v\n", v.SandboxID, st, v.Size.Name, v.Size.VCPU, v.Size.MemoryMiB, readableMap(v.Direct))
		}
		tw.Flush()
	}
	for _, t := range s.Transfers {
		fmt.Fprintf(w, "Transfer  %s %s %s  %d/%d bytes\n", t.Direction, t.Kind, t.ID, t.Bytes, t.Total)
	}
}

// acceptLine says whether the node takes new sandboxes, and why not.
func acceptLine(s node.Status) string {
	clock := func(t time.Time) string { return t.Local().Format("Mon 15:04") }
	switch {
	case s.Evacuation != nil:
		return fmt.Sprintf("EVACUATING: sandboxes still here at %s are stopped and resumed on other nodes", clock(s.Evacuation.Deadline))
	case s.Drained:
		return "DRAINED by you or an admin (no new sandboxes; crowdvm nodes undrain)"
	case s.Paused:
		return "PAUSED by you (no new sandboxes; crowdvm-node config set paused false)"
	case s.Pause.Paused && s.Pause.Reason == node.PauseHold:
		return "PAUSED until " + clock(s.Pause.Until) + " (hold)"
	case s.Pause.Paused:
		if s.Pause.Until.IsZero() {
			return "PAUSED (outside the schedule)"
		}
		return "PAUSED outside the schedule (offers again " + clock(s.Pause.Until) + ")"
	case !s.Pause.Until.IsZero():
		return "yes, until " + clock(s.Pause.Until)
	}
	return "yes"
}

func configCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Get or set node settings (" + strings.Join(node.ConfigKeys, ", ") + ")"}
	c.AddCommand(&cobra.Command{
		Use:   "get [key]",
		Short: "Print one setting, or all of them",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				v, err := cfg.Get(args[0])
				if err != nil {
					return err
				}
				fmt.Println(v)
				return nil
			}
			for _, k := range node.ConfigKeys {
				v, _ := cfg.Get(k)
				fmt.Printf("%-18s %s\n", k, v)
			}
			fmt.Printf("%-18s %s\n", "data-dir", cfg.DataDir)
			return nil
		},
	}, &cobra.Command{
		Use:   "set key value",
		Short: "Change a setting (capacity, pause and schedule keys apply at once; others after a restart)",
		Long: `Change a setting. While the daemon runs, the change goes through it: it
checks the value, saves only that key, and applies cpus, memory, disk,
max-sandboxes, paused, schedule and schedule-hold at once. Other keys apply
after a restart (sandboxes keep running). Capacity keys accept "" for the default.

schedule: weekly windows in which the node offers, in local time, e.g.
  "mon-fri 19:00-08:00; sat,sun" (days: mon..sun, daily, weekdays, weekends).
schedule-hold: "offer" or "pause" until the schedule's next boundary, "" to clear.`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]string{"key": args[0], "value": args[1]})
			var res struct {
				Applied bool `json:"applied"`
			}
			ran, err := daemonBody(c.Context(), cfg, "POST", "/api/config", string(body), &res)
			switch st := daemonStatus(err); {
			case ran && err == nil:
				if res.Applied {
					fmt.Println("Applied.")
				} else {
					fmt.Println("Saved. Restart the node to apply it (sandboxes keep running).")
				}
				return nil
			case err != nil && st != 401 && st != 404:
				return err // e.g. an invalid value
			}
			// The daemon isn't running, predates /api/config (404), or its
			// token isn't readable here (401): write the file ourselves.
			if err := cfg.Set(args[0], args[1]); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			switch {
			case !ran:
				fmt.Println("Saved. It applies when the node starts.")
			case daemonStatus(err) == 404 && args[0] == "paused":
				// A daemon from before the upgrade: it still knows /api/pause.
				p := strconv.FormatBool(cfg.Paused)
				if _, perr := daemonBody(c.Context(), cfg, "POST", "/api/pause", `{"paused":`+p+`}`, nil); perr == nil {
					fmt.Println("Applied.")
					return nil
				}
				fmt.Println("Saved. Restart the node to apply it (sandboxes keep running).")
			case daemonStatus(err) == 404:
				fmt.Println("Saved. The running node is older than this CLI: restart it to apply (sandboxes keep running).")
			default:
				fmt.Println("Saved, but the running node didn't apply it: its control token isn't readable here. Restart the node to apply.")
			}
			return nil
		},
	})
	return c
}

func openCmd(g *globals) *cobra.Command {
	var printOnly bool
	c := &cobra.Command{
		Use:   "open",
		Short: "Open the node's local page, logged in to control it",
		Long: `Open the node's local page (pause, schedule, shared resources, what runs
here and what it earned) in the browser, logged in with the control token
in the data dir. Without the login the page is read-only.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if cfg.UIListen() == "off" {
				return errors.New("the local page is off (ui-addr off); turn it on with: crowdvm-node config set ui-addr \"\"")
			}
			if ran, _ := daemon(c.Context(), cfg, "GET", "/api/status", nil); !ran {
				return errors.New("the node daemon is not running; start it (crowdvm-node run, or the installed service) first")
			}
			if _, err := node.ReadUIToken(cfg.DataDir); err != nil {
				return fmt.Errorf("cannot read the control token (%w); run this as the user who owns %s", err, cfg.DataDir)
			}
			var lc struct {
				Code string `json:"code"`
			}
			if _, err := daemonBody(c.Context(), cfg, "POST", "/api/login-code", "", &lc); err != nil {
				if daemonStatus(err) == 401 {
					return errors.New("the running node has another control token (was ui-token deleted?); restart the node, then run this again")
				}
				return err
			}
			u := uiURL(cfg) + "/login?c=" + lc.Code
			if printOnly || openBrowser(u) != nil {
				fmt.Println("Open this link on this machine within a minute (it logs you in once):")
			} else {
				fmt.Println("Opened the node's page. If no browser appeared, open this link on this machine:")
			}
			fmt.Println(u)
			return nil
		},
	}
	c.Flags().BoolVar(&printOnly, "print", false, "only print the login link")
	return c
}

// openBrowser opens url in the desktop's default browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no desktop session")
		}
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func installServiceCmd(g *globals) *cobra.Command {
	var noStart, dry, logonTask bool
	c := &cobra.Command{
		Use:   "install-service",
		Short: "Run the node in the background (systemd user unit; Windows service, or a logon task with --logon-task)",
		RunE: func(c *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			args := []string{"run"}
			// A Windows service runs as LocalSystem, whose default data dir
			// is not this user's: always name it.
			if g.dataDir != node.DefaultDataDir() || (runtime.GOOS == "windows" && !logonTask) {
				dir, err := filepath.Abs(g.dataDir)
				if err != nil {
					return err
				}
				args = append(args, "--data-dir", dir)
			}
			return service.Install(service.Options{Exe: exe, Args: args, NoStart: noStart, DryRun: dry, LogonTask: logonTask}, os.Stdout)
		},
	}
	c.Flags().BoolVar(&noStart, "no-start", false, "install only, don't enable/start now")
	c.Flags().BoolVar(&dry, "dry-run", false, "print what would be installed")
	c.Flags().BoolVar(&logonTask, "logon-task", false, "Windows: start at your logon with a scheduled task (no elevation) instead of a service")
	return c
}

func uninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-service",
		Short: "Stop and remove the background service",
		RunE:  func(*cobra.Command, []string) error { return service.Uninstall(os.Stdout) },
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(*cobra.Command, []string) {
			fmt.Printf("crowdvm-node %s %s/%s %s\n", node.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		},
	}
}

func readableMap(m map[string]string) string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	if len(pairs) == 0 {
		return "-"
	}
	return strings.Join(pairs, ", ")
}

// shellArg quotes s for a copy-and-paste command line when it needs it.
func shellArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"$`\\;&|<>()*?!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
