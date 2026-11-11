package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// sandboxFlags are shared by new, resume, fork and run.
type sandboxFlags struct {
	typ, environment       string
	ttl                    int
	noAutoStop, noEnv      bool
	noSnapshots, noMonitor bool
	failFast               bool
	env                    []string
	// placement (CrowdVM extension)
	gpu, nested            bool
	node, minTier          string
	nodeLabels, categories []string
}

// placement returns the placement flags, or nil when none was given.
func (s *sandboxFlags) placement() (*client.Placement, error) {
	labels, err := parseKV("--node-label", s.nodeLabels)
	if err != nil {
		return nil, err
	}
	cats, err := parseKV("--category", s.categories)
	if err != nil {
		return nil, err
	}
	p := &client.Placement{GPU: s.gpu, Nested: s.nested, Node: s.node, MinTier: s.minTier, Labels: labels, Categories: cats}
	if !p.GPU && !p.Nested && p.Node == "" && p.MinTier == "" && len(labels) == 0 && len(cats) == 0 {
		return nil, nil
	}
	return p, nil
}

func parseKV(flag string, kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range kvs {
		for _, part := range strings.Split(kv, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || k == "" {
				return nil, usagef("%s wants KEY=VALUE, got %q", flag, part)
			}
			m[k] = v
		}
	}
	return m, nil
}

func (s *sandboxFlags) bind(cmd *cobra.Command, lifecycle bool) {
	f := cmd.Flags()
	f.StringVar(&s.typ, "type", "", "machine size: small, default, large or xlarge")
	f.StringVar(&s.environment, "environment", "", "environment to start from (see: crowdvm env list)")
	f.StringArrayVarP(&s.env, "env", "e", nil, "per-sandbox variable KEY=VALUE (repeatable)")
	f.BoolVar(&s.noEnv, "no-env", false, "give the sandbox none of your secrets")
	f.BoolVar(&s.failFast, "fail-fast", false, "fail with no_ready_machine instead of waiting for capacity")
	f.BoolVar(&s.gpu, "gpu", false, "placement: run on a node that passes a GPU through (VFIO); the sandbox gets that GPU to itself")
	f.BoolVar(&s.nested, "nested", false, "placement: run on a node with nested virtualization (KVM inside the sandbox)")
	f.StringVar(&s.node, "node", "", "placement: run on this node (id or name, see: crowdvm nodes)")
	f.StringArrayVar(&s.nodeLabels, "node-label", nil, "placement: node label KEY=VALUE that must match (repeatable)")
	f.StringVar(&s.minTier, "min-tier", "", "placement: minimum node tier: bronze, silver, gold or platinum")
	f.StringArrayVar(&s.categories, "category", nil, "placement: minimum benchmark level DIM=LEVEL, e.g. cpu=high (low|medium|high|extreme; repeatable)")
	if lifecycle {
		f.IntVar(&s.ttl, "ttl", 3600, "seconds until auto-stop, counted from start")
		f.BoolVar(&s.noAutoStop, "no-auto-stop", false, "never stop automatically")
	}
}

func (s *sandboxFlags) bindData(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&s.noSnapshots, "no-snapshots", false, "never snapshot; stopping erases the sandbox")
	cmd.Flags().BoolVar(&s.noMonitor, "no-monitoring", false, "keep no command output, process list or metrics")
}

func (a *App) newCmd() *cobra.Command {
	var sf sandboxFlags
	var setupFile, from, name, visibility string
	var count int
	var personal, noWait bool
	var hosts, files []string
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a sandbox and wait until it is ready",
		Long: `Create a sandbox. With --json, prints one JSON object per line:
{"event":"created",...}, {"event":"state",...} and finally {"event":"ready","id":...}.`,
		Example: `  crowdvm new --type large --ttl 7200
  crowdvm new --from web-stack --host 3000 --file workspace/.env=./session.env
  id=$(crowdvm new --json | jq -r 'select(.event=="ready") | .id')`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			env, err := parseEnv(sf.env)
			if err != nil {
				return err
			}
			req := &client.CreateSandboxRequest{
				Type: sf.typ, TTLSeconds: ttlFor(cmd, sf.ttl, sf.noAutoStop), Env: env, Environment: sf.environment,
				NoEnv: sf.noEnv, FailFast: sf.failFast, Name: name, Visibility: visibility, From: from,
			}
			if sf.noSnapshots {
				req.Snapshots = boolPtr(false)
			}
			if sf.noMonitor {
				req.Monitoring = boolPtr(false)
			}
			if req.Placement, err = sf.placement(); err != nil {
				return err
			}
			if count > 1 {
				if from == "" {
					return usagef("--count needs --from")
				}
				if sf.failFast || len(hosts) > 0 {
					return usagef("--count cannot be combined with --fail-fast or --host")
				}
				req.Count = count
			}
			if personal {
				c.Org = ""
				req.Org = "personal"
			}
			if setupFile != "" {
				b, err := os.ReadFile(setupFile)
				if err != nil {
					return err
				}
				if len(b) > 64<<10 {
					return usagef("--setup-file is larger than 64KB")
				}
				req.SetupScript = string(b)
			}
			for _, h := range hosts {
				hp, err := parseHostFlag(h)
				if err != nil {
					return err
				}
				req.HostedPorts = append(req.HostedPorts, hp)
			}
			for _, f := range files {
				dst, src, ok := strings.Cut(f, "=")
				if !ok || dst == "" || src == "" {
					return usagef("--file wants SANDBOX_PATH=LOCAL_FILE, got %q", f)
				}
				b, err := os.ReadFile(src)
				if err != nil {
					return err
				}
				req.Files = append(req.Files, client.FileSpec{Path: dst, Content: string(b), Encoding: "utf8"})
			}
			res, err := c.CreateSandbox(cmd.Context(), req, "")
			if err != nil {
				return err
			}
			if req.Count > 1 || len(res.Sandboxes) > 1 {
				for _, s := range res.Sandboxes {
					if a.jsonOut {
						a.emit(map[string]any{"event": "created", "id": s.ID, "state": displayState(s.State), "sandbox": s})
					} else {
						a.printf("%s\t%s\n", s.ID, displayState(s.State))
					}
				}
				return nil
			}
			sb := res.Sandbox
			if sb == nil && len(res.Sandboxes) == 1 {
				sb = &res.Sandboxes[0]
			}
			if sb == nil {
				return fmt.Errorf("the server returned no sandbox")
			}
			a.setCurrent(sb.ID)
			return a.announce(cmd, c, sb, res.HostedPorts, noWait)
		},
	}
	sf.bind(cmd, true)
	sf.bindData(cmd)
	f := cmd.Flags()
	f.StringVar(&setupFile, "setup-file", "", "local shell script to run once after the sandbox is ready (64KB)")
	f.StringVar(&from, "from", "", "start from a saved snapshot name or share code")
	f.IntVar(&count, "count", 1, "with --from: create this many sandboxes at once")
	f.BoolVar(&personal, "personal", false, "bill your personal account even when an org is active")
	f.StringVar(&name, "name", "", "display name")
	f.StringVar(&visibility, "visibility", "", "org sandboxes: private or shared")
	f.StringArrayVar(&hosts, "host", nil, "host PORT (private URL) or PORT:public (repeatable)")
	f.StringArrayVar(&files, "file", nil, "write SANDBOX_PATH=LOCAL_FILE before ready (repeatable)")
	f.BoolVar(&noWait, "no-wait", false, "return after the create is accepted")
	return cmd
}

func parseHostFlag(h string) (client.HostPortRequest, error) {
	port, mode, _ := strings.Cut(h, ":")
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return client.HostPortRequest{}, usagef("--host wants PORT or PORT:public, got %q", h)
	}
	hp := client.HostPortRequest{Port: n}
	switch mode {
	case "", "private":
	case "public":
		hp.Public = true
	case "localhost":
		hp.Localhost = true
	default:
		return hp, usagef("--host: unknown mode %q (use public)", mode)
	}
	return hp, nil
}

// announce prints the created event, waits for ready (unless noWait) and
// prints the ready event.
func (a *App) announce(cmd *cobra.Command, c *client.Client, sb *client.Sandbox, ports []client.HostedPort, noWait bool) error {
	created := map[string]any{"event": "created", "id": sb.ID, "state": displayState(sb.State), "sandbox": sb}
	if len(ports) > 0 {
		created["hostedPorts"] = ports
	}
	if a.jsonOut {
		a.emit(created)
	} else {
		a.printf("%s\n", sb.ID)
		a.info("created %s (%s)\n", sb.ID, displayState(sb.State))
		for _, p := range ports {
			if p.URL != "" {
				a.info("  port %d → %s\n", p.Port, p.URL)
			}
		}
	}
	if noWait {
		return nil
	}
	ready, err := a.waitReady(cmd.Context(), c, sb.ID, sb.State)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emit(map[string]any{"event": "ready", "id": ready.ID, "state": displayState(ready.State), "ip": ready.IP, "sandbox": ready})
	} else {
		a.info("ready %s\n", ready.ID)
	}
	return nil
}

func (a *App) listCmd() *cobra.Command {
	var filter string
	var all bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your sandboxes (default: up or running)",
		Long: `List sandboxes. --filter takes state group letters:
  r up/running (ready, cloning, idle, running)   s stopped   p pending
  t stopping   e error.  Combine them (--filter sr), or use --all.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if filter == "" {
				filter = "r"
			}
			list, err := c.ListAllSandboxes(cmd.Context(), client.ListSandboxesOptions{})
			if err != nil {
				return err
			}
			rows := []client.Sandbox{}
			for _, s := range list {
				if all || strings.ContainsRune(filter, stateGroup(s.State)) {
					rows = append(rows, s)
				}
			}
			if a.jsonOut {
				a.emit(map[string]any{"sandboxes": rows})
				return nil
			}
			var t [][]string
			for _, s := range rows {
				vis := ""
				if s.Visibility != "" && s.Team != nil {
					vis = s.Visibility
				}
				t = append(t, []string{s.ID, displayState(s.State), s.Type, s.Name, ago(s.CreatedAt), vis})
			}
			if len(t) == 0 {
				if all {
					a.info("no sandboxes\n")
				} else {
					a.info("no sandboxes (try --all)\n")
				}
				return nil
			}
			table(a.Out, "ID\tSTATE\tTYPE\tNAME\tCREATED\t", t)
			return nil
		},
	}
	cmd.Flags().StringVar(&filter, "filter", "", "state groups: r, s, p, t, e (combine letters)")
	cmd.Flags().BoolVar(&all, "all", false, "include every state")
	return cmd
}

func stateGroup(s string) rune {
	switch s {
	case "ready", "cloning", "idle", "running":
		return 'r'
	case "archived":
		return 's'
	case "init", "provisioning", "provisioned":
		return 'p'
	case "archiving":
		return 't'
	case "error", "cancelled":
		return 'e'
	}
	return '?'
}

func (a *App) infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info [ID]",
		Short: "Show one sandbox: state, size, IP, desktop, TTL",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(arg0(args))
			if err != nil {
				return err
			}
			s, err := c.GetSandbox(cmd.Context(), id)
			if err != nil {
				return err
			}
			// Boat's shape: {"sandbox": {...}} with the CLI's state names, so
			// `jq -r .sandbox.state` loops wait for "stopped" as documented.
			var m map[string]any
			b, _ := json.Marshal(s)
			_ = json.Unmarshal(b, &m)
			if st, ok := m["state"].(string); ok {
				m["state"] = displayState(st)
			}
			return a.result(map[string]any{"sandbox": m}, func() { a.printSandbox(s) })
		},
	}
}

func arg0(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (a *App) printSandbox(s *client.Sandbox) {
	line := func(k string, v any) {
		if str := fmt.Sprint(v); str != "" && str != "<nil>" {
			a.printf("%-11s%s\n", k+":", str)
		}
	}
	line("id", s.ID)
	line("name", s.Name)
	st := displayState(s.State)
	if s.Health == "degraded" {
		st += " (degraded: " + s.HealthReason + ")"
	}
	line("state", st)
	if s.WaitingForCapacity {
		line("waiting", "for capacity (waitingForCapacity: true)")
	}
	line("message", s.StatusMessage)
	if s.Error != "" {
		line("error", s.Error)
	}
	size := s.Type
	if s.VCPU > 0 {
		size += fmt.Sprintf(" (%d vCPU, %d GB)", s.VCPU, s.MemoryGB)
	}
	line("type", size)
	line("node", s.Node)
	line("ip", s.IP)
	line("ssh", s.SSHEndpoint)
	line("url", s.URL)
	if s.DesktopAvailable {
		line("desktop", "available (crowdvm desktop "+s.ID+")")
	}
	switch {
	case s.TTLSeconds == nil && isUp(s.State):
		line("auto-stop", "off")
	case s.ArchiveAfter != "":
		line("auto-stop", s.ArchiveAfter+" ("+ago(s.ArchiveAfter)+")")
	}
	line("snapshot", s.SnapshotCompletedAt)
	if s.Team != nil {
		line("org", fmt.Sprintf("%v (%s)", s.Team["name"], s.Visibility))
	}
	if s.Environment != "" {
		v := s.Environment
		if s.EnvironmentVersion != nil {
			v += fmt.Sprintf(" v%d", *s.EnvironmentVersion)
		}
		line("env", v)
	}
	if s.SetupStatus != "" {
		v := s.SetupStatus
		if s.SetupError != "" {
			v += ": " + s.SetupError
		}
		line("setup", v)
	}
	if s.LastReboot != nil {
		line("rebooted", fmt.Sprintf("%v (%v)", s.LastReboot["at"], s.LastReboot["reason"]))
	}
	if s.Stop != nil && s.Stop.Status != "completed" {
		line("stop", s.Stop.ID+" "+s.Stop.Status+" "+s.Stop.Error)
	}
	line("created", s.CreatedAt)
}

func (a *App) renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename ID NAME",
		Short: "Rename a sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			s, err := c.UpdateSandbox(cmd.Context(), id, &client.UpdateSandboxRequest{Name: args[1]})
			if err != nil {
				return err
			}
			return a.result(s, func() { a.printf("%s renamed to %q\n", id, args[1]) })
		},
	}
}

func (a *App) subdomainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "subdomain ID LABEL",
		Short: "Set the DNS label of a sandbox's hosted URLs (<label>-<port>.<domain>)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			s, err := c.UpdateSandbox(cmd.Context(), id, &client.UpdateSandboxRequest{Subdomain: args[1]})
			if err != nil {
				return err
			}
			return a.result(s, func() { a.printf("%s: subdomain %s\n", id, args[1]) })
		},
	}
}

func (a *App) extendCmd() *cobra.Command {
	var hours float64
	var ttl int
	var off bool
	cmd := &cobra.Command{
		Use:   "extend ID",
		Short: "Set the auto-stop timer of a running sandbox (from now)",
		Example: `  crowdvm extend bx_f7k2q9hd --hours 12
  crowdvm extend bx_f7k2q9hd --no-auto-stop`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			var t *client.TTL
			switch {
			case off:
				t = client.NoAutoStop()
			case cmd.Flags().Changed("ttl"):
				t = client.TTLSeconds(ttl)
			case cmd.Flags().Changed("hours"):
				t = client.TTLSeconds(int(hours * 3600))
			default:
				return usagef("give --hours, --ttl or --no-auto-stop")
			}
			s, err := c.UpdateSandbox(cmd.Context(), id, &client.UpdateSandboxRequest{TTLSeconds: t})
			if err != nil {
				return err
			}
			return a.result(s, func() {
				if off {
					a.printf("%s: auto-stop off\n", id)
				} else {
					a.printf("%s: stops %s\n", id, firstNonEmpty(s.ArchiveAfter, "later"))
				}
			})
		},
	}
	cmd.Flags().Float64Var(&hours, "hours", 0, "hours from now")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "seconds from now")
	cmd.Flags().BoolVar(&off, "no-auto-stop", false, "keep running until stopped")
	return cmd
}

func (a *App) stopCmd() *cobra.Command {
	var force, wait bool
	var status string
	cmd := &cobra.Command{
		Use:   "stop ID",
		Short: "Stop a sandbox: snapshot its disk, then free the machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			if status != "" {
				op, err := c.GetStop(cmd.Context(), id, status)
				if err != nil {
					return err
				}
				return a.result(map[string]any{"id": id, "operation": op}, func() {
					a.printf("%s %s", op.ID, op.Status)
					if op.Error != "" {
						a.printf(": %s", op.Error)
					}
					a.printf("\n")
				})
			}
			res, err := c.StopSandbox(cmd.Context(), id, force)
			if err != nil {
				return err
			}
			op := firstOp(res)
			out := map[string]any{"event": "stopping", "id": id, "state": "stopping"}
			if op != nil {
				out["operation"] = op
			}
			if a.jsonOut {
				a.emit(out)
			} else {
				a.printf("%s: stopping", id)
				if op != nil {
					a.printf(" (%s)", op.ID)
				}
				a.printf("\n")
			}
			if !wait {
				return nil
			}
			for {
				s, err := c.GetSandbox(cmd.Context(), id)
				if err != nil {
					return err
				}
				if s.State == "archived" || s.State == "error" {
					return a.result(map[string]any{"event": "stopped", "id": id, "state": displayState(s.State)}, func() {
						a.printf("%s: %s\n", id, displayState(s.State))
					})
				}
				if err := sleepCtx(cmd.Context(), a.Poll); err != nil {
					return err
				}
			}
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop now and keep only the last successful snapshot (loses newer data)")
	cmd.Flags().StringVar(&status, "status", "", "show the status of a stop operation id")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the sandbox is stopped")
	cmd.Flags().BoolP("yes", "y", false, "accepted for symmetry with delete; stop never asks")
	return cmd
}

func firstOp(r *client.SandboxAction) *client.StopOperation {
	if r.Operation != nil {
		return r.Operation
	}
	if r.Stop != nil {
		return r.Stop
	}
	if r.Sandbox != nil {
		return r.Sandbox.Stop
	}
	return nil
}

func (a *App) resumeCmd() *cobra.Command {
	var sf sandboxFlags
	var noWait bool
	cmd := &cobra.Command{
		Use:   "resume ID",
		Short: "Resume a stopped sandbox from its last snapshot",
		Long: `Resume a stopped sandbox (on any node with room) and wait until it runs
commands. --type, --environment and --no-env change a stopped sandbox.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			env, err := parseEnv(sf.env)
			if err != nil {
				return err
			}
			pl, err := sf.placement()
			if err != nil {
				return err
			}
			res, err := c.ResumeSandbox(cmd.Context(), id, &client.ResumeRequest{
				FailFast: sf.failFast, Type: sf.typ, Env: env, Environment: sf.environment, NoEnv: sf.noEnv,
				TTLSeconds: ttlFor(cmd, sf.ttl, sf.noAutoStop), Placement: pl,
			})
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(map[string]any{"event": "resuming", "id": id, "status": res.Status})
			} else {
				a.info("%s: resuming\n", id)
			}
			if noWait {
				return nil
			}
			s, err := a.waitReady(cmd.Context(), c, id, "")
			if err != nil {
				return err
			}
			return a.result(map[string]any{"event": "ready", "id": id, "state": displayState(s.State), "ip": s.IP, "sandbox": s}, func() {
				a.printf("%s: ready\n", id)
			})
		},
	}
	sf.bind(cmd, true)
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return when the resume is accepted")
	return cmd
}

func (a *App) forkCmd() *cobra.Command {
	var sf sandboxFlags
	var wait bool
	var name string
	cmd := &cobra.Command{
		Use:   "fork ID",
		Short: "Copy a sandbox from its latest snapshot into a new one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			env, err := parseEnv(sf.env)
			if err != nil {
				return err
			}
			req := &client.ForkRequest{FailFast: sf.failFast, Env: env, Type: sf.typ, Environment: sf.environment,
				NoEnv: sf.noEnv, TTLSeconds: ttlFor(cmd, sf.ttl, sf.noAutoStop), Name: name}
			if sf.noSnapshots {
				req.Snapshots = boolPtr(false)
			}
			if sf.noMonitor {
				req.Monitoring = boolPtr(false)
			}
			if req.Placement, err = sf.placement(); err != nil {
				return err
			}
			res, err := c.ForkSandbox(cmd.Context(), id, req, "")
			if err != nil {
				return err
			}
			sb := res.Sandbox
			if sb == nil {
				sb = &client.Sandbox{ID: res.ID, State: "provisioning"}
			}
			if sb.ID == "" {
				sb.ID = res.ID
			}
			a.setCurrent(sb.ID)
			return a.announce(cmd, c, sb, nil, !wait)
		},
	}
	sf.bind(cmd, true)
	sf.bindData(cmd)
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the fork is ready")
	cmd.Flags().StringVar(&name, "name", "", "display name of the fork")
	return cmd
}

func (a *App) recoverCmd() *cobra.Command {
	return a.simpleAction("recover ID", "Power-cycle the machine of a sandbox that does not respond", "recovering",
		func(c *client.Client, cmd *cobra.Command, id string) (*client.SandboxAction, error) {
			return c.RecoverSandbox(cmd.Context(), id)
		})
}

func (a *App) shareCmd(share bool) *cobra.Command {
	if share {
		return a.simpleAction("share ID", "Let every org member use this sandbox", "shared",
			func(c *client.Client, cmd *cobra.Command, id string) (*client.SandboxAction, error) {
				return c.ShareSandbox(cmd.Context(), id)
			})
	}
	return a.simpleAction("unshare ID", "Make an org sandbox private to you", "private",
		func(c *client.Client, cmd *cobra.Command, id string) (*client.SandboxAction, error) {
			return c.UnshareSandbox(cmd.Context(), id)
		})
}

func (a *App) simpleAction(use, short, word string, fn func(*client.Client, *cobra.Command, string) (*client.SandboxAction, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			res, err := fn(c, cmd, id)
			if err != nil {
				return err
			}
			if res.ID == "" {
				res.ID = id
			}
			return a.result(res, func() {
				a.printf("%s: %s\n", id, firstNonEmpty(res.Status, word))
				if res.RestartRequired {
					a.printf("note: %s\n", firstNonEmpty(res.Message, "the change takes effect after `crowdvm stop` and `crowdvm resume`"))
				}
			})
		},
	}
}

func (a *App) deleteCmd() *cobra.Command {
	var yes, noWait bool
	cmd := &cobra.Command{
		Use:     "delete ID",
		Aliases: []string{"rm"},
		Short:   "Permanently delete a sandbox and its snapshots",
		Long: `Permanently delete a sandbox and the snapshots only it uses. This cannot be
undone; to keep the data, use 'crowdvm stop'. Scripts must pass --yes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			if err := a.confirm(yes, fmt.Sprintf("Permanently delete %s and its snapshots?", id)); err != nil {
				return err
			}
			op, err := c.DeleteSandbox(cmd.Context(), id)
			if err != nil {
				return err
			}
			a.forgetSandbox(id)
			if a.jsonOut {
				a.emit(map[string]any{"event": "deleting", "id": id, "operation": op})
			} else {
				a.printf("%s: deleting (%s)\n", id, op.ID)
			}
			if noWait || op.ID == "" {
				return nil
			}
			return a.followDeletion(cmd, c, op)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not follow the deletion operation")
	return cmd
}

func (a *App) followDeletion(cmd *cobra.Command, c *client.Client, op *client.DeletionOperation) error {
	deadline := time.Now().Add(10 * time.Minute)
	last := op.Status
	for op.Status != "completed" && op.Status != "blocked" && time.Now().Before(deadline) {
		if err := sleepCtx(cmd.Context(), a.Poll); err != nil {
			return err
		}
		next, err := c.DeletionOperation(cmd.Context(), op.ID)
		if err != nil {
			if client.IsStatus(err, 404) { // gone: finished and cleaned up
				op.Status = "completed"
				break
			}
			return err
		}
		op = next
		if op.Status != last {
			last = op.Status
			a.info("%s: %s\n", op.ID, op.Status)
		}
	}
	// Only a completed operation is "deleted" (bugs-r2-opus 4): a blocked
	// one needs its node's owner, and one still running is reported as such.
	switch op.Status {
	case "blocked":
		_ = a.result(map[string]any{"event": "deletion_blocked", "id": op.TargetID, "operation": op}, func() {
			a.printf("%s: blocked: %s\n", firstNonEmpty(op.TargetID, op.ID), op.LastError)
		})
		return &client.Error{Status: 409, Code: "deletion_blocked", Message: fmt.Sprintf("deletion %s is blocked: %s", op.ID, firstNonEmpty(op.LastError, "see crowdvm deletion status "+op.ID))}
	case "completed", "":
		return a.result(map[string]any{"event": "deleted", "id": op.TargetID, "operation": op}, func() {
			a.printf("%s: %s\n", firstNonEmpty(op.TargetID, op.ID), firstNonEmpty(op.Status, "deleted"))
		})
	}
	return a.result(map[string]any{"event": "deletion_pending", "id": op.TargetID, "operation": op}, func() {
		a.printf("%s: still %s; follow it with: crowdvm deletion status %s\n", firstNonEmpty(op.TargetID, op.ID), op.Status, op.ID)
	})
}

func (a *App) deletionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "deletion", Short: "Read deletion operations"}
	cmd.AddCommand(&cobra.Command{
		Use:   "status OPERATION_ID",
		Short: "Show a deletion operation: pending, processing, blocked or completed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			op, err := c.DeletionOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(op, func() {
				a.printf("%s %s (%s %s", op.ID, op.Status, op.Kind, op.TargetID)
				if op.Stage != "" {
					a.printf(", %s", op.Stage)
				}
				a.printf(")\n")
			})
		},
	})
	return cmd
}
