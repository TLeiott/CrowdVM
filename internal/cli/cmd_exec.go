package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

type execFlags struct {
	cwd                  string
	timeout              int
	stream, detach       bool
	status, kill, attach int
	push                 string
}

func (a *App) execCmd() *cobra.Command {
	var ef execFlags
	cmd := &cobra.Command{
		Use:   "exec [ID] [flags] [--] COMMAND...",
		Short: "Run a command in a sandbox and exit with its exit code",
		Long: `Run a command through the API (no SSH). One argument is a shell command
line; more arguments run as argv. The default timeout is 30 seconds; above 600
or with 0 (no limit) the command runs in the background and its output streams
live, surviving network cuts. Exit code 124 means the timeout hit. ID may also
be the sandbox's name; without one the current sandbox runs the command.`,
		Example: `  crowdvm exec bx_f7k2q9hd "npm test"
  crowdvm exec bx_f7k2q9hd --cwd my-repo --timeout 0 -- make -j16 test
  pid=$(crowdvm exec bx_f7k2q9hd --detach "npm run dev" | jq -r .processId)
  crowdvm exec bx_f7k2q9hd --status "$pid"
  crowdvm exec bx_f7k2q9hd --attach "$pid"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			first, rest, err := reparse(cmd, args, a.isSandboxName)
			if err != nil {
				return err
			}
			c, id, err := a.clientAndID(first)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			switch {
			case ef.status > 0:
				st, err := c.CommandStatus(ctx, id, ef.status, &client.CommandStatusOptions{TailBytes: 8192})
				if err != nil {
					return err
				}
				return a.result(st, func() { a.printStatus(st) })
			case ef.kill > 0:
				r, err := c.StopCommand(ctx, id, ef.kill)
				if err != nil {
					return err
				}
				return a.result(r, func() { a.printf("process %d stopped\n", ef.kill) })
			case ef.attach > 0:
				a.rawOutput(cmd)
				code, err := a.attach(ctx, c, id, ef.attach)
				if err != nil {
					return err
				}
				return exitCode(code)
			}
			if len(rest) == 0 {
				return usagef("no command given: crowdvm exec ID [--] COMMAND...")
			}
			if !ef.detach {
				a.rawOutput(cmd)
			}
			if ef.push != "" {
				to, err := a.push(ctx, c, id, ef.push, "")
				if err != nil {
					return err
				}
				if !cmd.Flags().Changed("cwd") {
					ef.cwd = to
				}
			}
			req := client.CommandRequest{Command: commandLine(rest), Cwd: ef.cwd, TimeoutSeconds: ef.timeout}
			code, err := a.runCommand(ctx, c, id, req, ef.detach, ef.stream)
			if err != nil {
				return err
			}
			return exitCode(code)
		},
	}
	cmd.Flags().SetInterspersed(false)
	f := cmd.Flags()
	f.StringVar(&ef.cwd, "cwd", "", "working directory, relative to /home/user")
	f.IntVar(&ef.timeout, "timeout", 30, "stop after this many seconds and exit 124; 0 = no limit")
	f.BoolVar(&ef.stream, "stream", false, "print output as it is written")
	f.BoolVar(&ef.detach, "detach", false, "run in the background and print the process id")
	f.IntVar(&ef.status, "status", 0, "show status and log tails of a detached process")
	f.IntVar(&ef.kill, "kill", 0, "stop a detached process and its children")
	f.IntVar(&ef.attach, "attach", 0, "follow a detached process's output until it ends")
	f.StringVar(&ef.push, "push", "", "push this local directory first and run in it")
	return cmd
}

func exitCode(code int) error {
	if code == 0 {
		return nil
	}
	if code < 0 {
		code = 1
	}
	return exitError{code}
}

// runCommand runs req in one of the exec modes and returns the exit code.
func (a *App) runCommand(ctx context.Context, c *client.Client, id string, req client.CommandRequest, detach, stream bool) (int, error) {
	if detach {
		req.TimeoutSeconds = 0
		st, err := c.ExecDetached(ctx, id, req)
		if err != nil {
			return 0, err
		}
		return 0, a.result(st, func() {
			a.printf("%d\n", st.ProcessID)
			a.info("started process %d; follow it with: crowdvm exec %s --attach %d\n", st.ProcessID, id, st.ProcessID)
		})
	}
	if req.TimeoutSeconds <= 0 || req.TimeoutSeconds > 600 {
		return a.runLong(ctx, c, id, req)
	}
	// The sync endpoint buffers and cuts each stream at 8 MiB (documented API
	// limit, `stdoutTruncated`), so text mode does not use it: no cap, no
	// silent loss. It runs detached and follows, as a long command does: only a
	// detached command has a process id, so Ctrl-C can kill it (the sandbox
	// keeps running a sync or stream command until its timeout).
	// --json keeps the sync result with its truncation flags, and --stream
	// --json the stream frames.
	if !a.jsonOut {
		return a.runLong(ctx, c, id, req)
	}
	if stream {
		return a.runStream(ctx, c, id, req)
	}
	res, err := c.Exec(ctx, id, req)
	if err != nil {
		return 0, err
	}
	code := commandExitCode(res.ExitCode, res.Signal)
	if res.TimedOut {
		code = 124
	}
	if a.jsonOut {
		a.emit(res)
	} else {
		io.WriteString(a.Out, res.Stdout)
		io.WriteString(a.Err, res.Stderr)
		if res.TimedOut {
			fmt.Fprintf(a.Err, "crowdvm: command timed out after %ds\n", req.TimeoutSeconds)
		}
	}
	return code, nil
}

func (a *App) runStream(ctx context.Context, c *client.Client, id string, req client.CommandRequest) (int, error) {
	st, err := c.ExecStream(ctx, id, req)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	for {
		f, err := st.Recv()
		if err != nil {
			if err == io.EOF {
				return 0, fmt.Errorf("stream ended before the command exited")
			}
			return 0, err
		}
		if a.jsonOut {
			a.emit(f)
		}
		switch f.Type {
		case "stdout":
			if !a.jsonOut {
				io.WriteString(a.Out, f.Data)
			}
		case "stderr":
			if !a.jsonOut {
				io.WriteString(a.Err, f.Data)
			}
		case "exit":
			if f.TimedOut {
				if !a.jsonOut {
					fmt.Fprintf(a.Err, "crowdvm: command timed out after %ds\n", req.TimeoutSeconds)
				}
				return 124, nil
			}
			return commandExitCode(f.ExitCode, f.Signal), nil
		case "error":
			return 0, client.FrameError(f.Error, f.Message, f.Retryable)
		}
	}
}

// runLong starts the command detached, follows it, enforces the timeout on
// the client side and kills it on Ctrl-C.
func (a *App) runLong(ctx context.Context, c *client.Client, id string, req client.CommandRequest) (int, error) {
	timeout := req.TimeoutSeconds
	req.TimeoutSeconds = 0
	if timeout > 0 {
		// The CLI enforces --timeout; the sandbox kills it a little later
		// in case the CLI dies first (kill -9, bugs-r9-opus 5).
		req.KillAfterSeconds = timeout + 15
	}
	st, err := c.ExecDetached(ctx, id, req)
	if err != nil {
		return 0, err
	}
	actx := ctx
	var cancel context.CancelFunc = func() {}
	if timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	defer cancel()
	code, err := a.attach(actx, c, id, st.ProcessID)
	if err != nil {
		// Whatever ended the follow loop (Ctrl-C, timeout, a lost connection,
		// an error frame), the command has no deadline of its own: stop it, or
		// it runs on unseen (bugs-r8-opus 5). Best effort. Not when the process
		// is already known to be gone.
		if e, ok := client.AsError(err); !ok || e.Code != "process_lost" {
			kctx, kcancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer kcancel()
			_, _ = c.StopCommand(kctx, id, st.ProcessID)
		}
		if actx.Err() == nil {
			return 0, err
		}
		if ctx.Err() != nil {
			return 130, nil
		}
		if a.jsonOut {
			a.emit(map[string]any{"type": "exit", "processId": st.ProcessID, "timedOut": true, "exitCode": 124})
		} else {
			fmt.Fprintf(a.Err, "crowdvm: command timed out after %ds\n", timeout)
		}
		return 124, nil
	}
	return code, err
}

// attach follows a detached process from its start until it exits,
// reconnecting with offsets when the stream ends.
func (a *App) attach(ctx context.Context, c *client.Client, id string, pid int) (int, error) {
	var outOff, errOff int64
	failures := 0
	for {
		st, err := c.AttachCommand(ctx, id, pid, outOff, errOff)
		if err != nil {
			if e, ok := client.AsError(err); (ok && !e.Retryable()) || ctx.Err() != nil || failures > 20 {
				return 0, err
			}
			failures++
			if err := sleepCtx(ctx, a.Poll); err != nil {
				return 0, err
			}
			continue
		}
		prevOut, prevErr := outOff, errOff
		code, done, reconnect, err := a.readAttach(st, &outOff, &errOff)
		st.Close()
		progressed := outOff > prevOut || errOff > prevErr
		if done {
			return code, err
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if reconnect || progressed {
			failures = 0
			continue
		}
		// The stream ended without an exit frame: ask whether it still runs.
		s, serr := c.CommandStatus(ctx, id, pid, &client.CommandStatusOptions{StdoutOffset: outOff, StderrOffset: errOff})
		if serr == nil && !s.Running && s.Status != "running" {
			if !a.jsonOut {
				io.WriteString(a.Out, s.Stdout)
				io.WriteString(a.Err, s.Stderr)
			}
			if a.jsonOut {
				a.emit(map[string]any{"type": "exit", "processId": pid, "status": s.Status, "exitCode": s.ExitCode})
			}
			if s.Status == "lost" {
				return 0, &client.Error{Code: "process_lost", Message: fmt.Sprintf("process %d was lost (the sandbox restarted)", pid)}
			}
			return commandExitCode(s.ExitCode, s.Signal), nil
		}
		failures++
		if failures > 20 {
			return 0, fmt.Errorf("attach: stream keeps ending: %v", err)
		}
	}
}

// readAttach prints frames until the stream ends. done means the process
// exited; reconnect means the server asked to reopen the stream.
func (a *App) readAttach(st *client.Stream[client.AttachFrame], outOff, errOff *int64) (code int, done, reconnect bool, err error) {
	for {
		f, err := st.Recv()
		if err != nil {
			return 0, false, false, err
		}
		if a.jsonOut && f.Type != "heartbeat" {
			a.emit(f)
		}
		switch f.Type {
		case "attached":
			if f.StdoutOffset > *outOff {
				*outOff = f.StdoutOffset
			}
			if f.StderrOffset > *errOff {
				*errOff = f.StderrOffset
			}
		case "stdout", "stderr":
			w, off := a.Out, outOff
			if f.Type == "stderr" {
				w, off = a.Err, errOff
			}
			if !a.jsonOut {
				io.WriteString(w, f.Data)
			}
			if f.NextOffset > 0 {
				*off = f.NextOffset
			} else {
				*off += int64(len(f.Data))
			}
		case "reconnect":
			if f.StdoutOffset > 0 {
				*outOff = f.StdoutOffset
			}
			if f.StderrOffset > 0 {
				*errOff = f.StderrOffset
			}
			return 0, false, true, nil
		case "exit":
			if f.Status == "lost" {
				return 0, true, false, &client.Error{Code: "process_lost", Message: "the process was lost (the sandbox restarted)"}
			}
			if f.OutputTruncated && !a.jsonOut {
				fmt.Fprintf(a.Err, "crowdvm: the command wrote more output than the sandbox keeps per stream; the rest was dropped\n")
			}
			return commandExitCode(f.ExitCode, f.Signal), true, false, nil
		case "error":
			return 0, true, false, client.FrameError(f.Error, f.Message, false)
		}
	}
}

func (a *App) printStatus(st *client.CommandStatus) {
	a.printf("process %d: %s", st.ProcessID, st.Status)
	if st.ExitCode != nil {
		a.printf(" (exit %d)", *st.ExitCode)
	}
	if st.Command != "" {
		a.printf("  %s", st.Command)
	}
	a.printf("\n")
	if st.Stdout != "" {
		a.printf("--- stdout (tail) ---\n%s", st.Stdout)
		if st.Stdout[len(st.Stdout)-1] != '\n' {
			a.printf("\n")
		}
	}
	if st.Stderr != "" {
		a.printf("--- stderr (tail) ---\n%s", st.Stderr)
		if st.Stderr[len(st.Stderr)-1] != '\n' {
			a.printf("\n")
		}
	}
}

func (a *App) runCmd() *cobra.Command {
	var sf sandboxFlags
	var from, pushDir, cwd string
	var timeout int
	var keep, personal bool
	cmd := &cobra.Command{
		Use:   "run [flags] [--] COMMAND...",
		Short: "Run one command in a new sandbox, then delete it",
		Long: `Create a sandbox, optionally push a directory, run the command with live
output, print time and credits, and delete the sandbox. Exits with the exit
code of the command. Ctrl-C deletes the sandbox.`,
		Example: `  crowdvm run -- npm test
  crowdvm run --from ci-base --type xlarge --push . -- make -j16 test`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.rawOutput(cmd)
			c, err := a.client()
			if err != nil {
				return err
			}
			env, err := parseEnv(sf.env)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			start := time.Now()
			req := &client.CreateSandboxRequest{Type: sf.typ, Env: env, Environment: sf.environment, NoEnv: sf.noEnv,
				FailFast: sf.failFast, From: from, TTLSeconds: client.TTLSeconds(900), Name: "crowdvm run"}
			if !keep {
				req.Snapshots = boolPtr(false)
			}
			if personal {
				c.Org = ""
				req.Org = "personal"
			}
			res, err := c.CreateSandbox(ctx, req, "")
			if err != nil {
				return err
			}
			if res.Sandbox == nil {
				return fmt.Errorf("the server returned no sandbox")
			}
			id := res.Sandbox.ID
			a.info("%s: created\n", id)
			cleanup := func() bool {
				if keep {
					return false
				}
				dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_, err := c.DeleteSandbox(dctx, id)
				return err == nil
			}
			code, runErr := func() (int, error) {
				sb, err := a.waitReady(ctx, c, id, res.Sandbox.State)
				if err != nil {
					return 0, err
				}
				*res.Sandbox = *sb
				if pushDir != "" {
					to, err := a.push(ctx, c, id, pushDir, "")
					if err != nil {
						return 0, err
					}
					if cwd == "" {
						cwd = to
					}
				}
				// Keep the sandbox alive while the command runs.
				go a.keepAlive(ctx, c, id)
				return a.runCommand(ctx, c, id, client.CommandRequest{Command: commandLine(args), Cwd: cwd, TimeoutSeconds: timeout}, false, true)
			}()
			// read the cost before the delete: a deleted sandbox has no usage any more
			usage, usageErr := c.SandboxUsage(context.Background(), id, "", "")
			deleted := cleanup()
			elapsed := time.Since(start)
			summary := map[string]any{"event": "summary", "id": id, "exitCode": code, "elapsedSeconds": int(elapsed.Seconds()),
				"sandboxType": firstNonEmpty(res.Sandbox.Type, sf.typ, "default"), "deleted": deleted}
			if usageErr == nil && id != "" {
				summary["credits"] = firstFloat(usage.Credits, usage.Dollars)
				summary["dollars"] = usage.Dollars
			}
			if a.jsonOut {
				a.emit(summary)
			} else {
				tail := "kept (stops in 15 minutes)"
				if deleted {
					tail = "deleted"
				} else if !keep {
					tail = "NOT deleted; run: crowdvm delete " + id + " --yes"
				}
				cr := ""
				if v, ok := summary["credits"].(float64); ok {
					cr = fmt.Sprintf(" · %.4f credits", v)
				}
				fmt.Fprintf(a.Err, "exit %d · %s · %s%s · %s %s\n", code, shortDur(elapsed), summary["sandboxType"], cr, id, tail)
			}
			if runErr != nil {
				return runErr
			}
			return exitCode(code)
		},
	}
	cmd.Flags().SetInterspersed(false)
	sf.bind(cmd, false)
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "start from a saved snapshot or share code")
	f.StringVar(&pushDir, "push", "", "copy this local directory in first and run there")
	f.StringVar(&cwd, "cwd", "", "working directory, relative to /home/user")
	f.IntVar(&timeout, "timeout", 0, "stop after this many seconds and exit 124 (0 = no limit)")
	f.BoolVar(&keep, "keep", false, "keep the sandbox afterwards (it stops 15 minutes later)")
	f.BoolVar(&personal, "personal", false, "bill your personal account")
	return cmd
}

func firstFloat(p *float64, d float64) float64 {
	if p != nil {
		return *p
	}
	return d
}

// keepAlive pushes the auto-stop 15 minutes ahead every 5 minutes.
func (a *App) keepAlive(ctx context.Context, c *client.Client, id string) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = c.UpdateSandbox(ctx, id, &client.UpdateSandboxRequest{TTLSeconds: client.TTLSeconds(900)})
		}
	}
}

func (a *App) pushCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "push ID DIR",
		Short: "Copy a local directory into a sandbox (git-aware)",
		Long: `Copy a local directory into the sandbox as one archive. In a git repository
only tracked files and new files that .gitignore does not exclude are sent
(node_modules, build output and .git stay local). Default target:
/home/user/<dir name>. Existing files are replaced; nothing is deleted.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			dest, err := a.push(cmd.Context(), c, id, args[1], to)
			if err != nil {
				return err
			}
			if !a.jsonOut {
				a.printf("pushed %s → %s:%s\n", args[1], id, dest)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "directory on the sandbox (default /home/user/<dir name>)")
	return cmd
}

// push uploads dir and returns the remote directory.
func (a *App) push(ctx context.Context, c *client.Client, id, dir, to string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !isDir(abs) {
		return "", usagef("%s is not a directory", dir)
	}
	if to == "" {
		to = "/home/user/" + filepath.Base(abs)
	}
	to = remotePath(to)
	files, err := a.pushFiles(abs)
	if err != nil {
		return "", err
	}
	n, size, err := a.uploadTree(ctx, c, id, abs, files, to, false)
	if err != nil {
		return "", err
	}
	if a.jsonOut {
		a.emit(map[string]any{"event": "pushed", "id": id, "to": to, "files": n, "bytes": size})
	} else {
		a.info("pushed %d files (%s) to %s\n", n, humanBytes(size), to)
	}
	return to, nil
}

func (a *App) historyCmd() *cobra.Command {
	var limit int
	var mode string
	var stderr, follow bool
	cmd := &cobra.Command{
		Use:   "history ID [COMMAND_ID]",
		Short: "List commands that ran in a sandbox, or print one's stored output",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if len(args) == 1 {
				h, err := c.CommandHistory(ctx, id, mode, "", limit, "")
				if err != nil {
					return err
				}
				if a.jsonOut {
					a.emit(h)
					return nil
				}
				var rows [][]string
				for _, r := range h.Commands {
					ec := "-"
					if r.ExitCode != nil {
						ec = strconv.Itoa(*r.ExitCode)
					}
					rows = append(rows, []string{r.CommandID, r.Mode, r.Status, ec, ago(r.StartedAt), truncate(r.Command, 60)})
				}
				table(a.Out, "HISTORY\tMODE\tSTATUS\tEXIT\tSTARTED\tCOMMAND", rows)
				return nil
			}
			a.rawOutput(cmd)
			stream := "stdout"
			w := a.Out
			if stderr {
				stream, w = "stderr", a.Err
			}
			var off int64
			for {
				l, err := c.CommandLogs(ctx, id, args[1], stream, off, 0)
				if err != nil {
					return err
				}
				if a.jsonOut {
					a.emit(l)
				} else {
					w.Write(l.Bytes())
				}
				if l.NextOffset > off {
					off = l.NextOffset
					if !l.Done {
						continue
					}
				}
				if !follow || l.Done {
					return nil
				}
				if err := sleepCtx(ctx, a.Poll); err != nil {
					return err
				}
			}
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many commands to list (max 100)")
	cmd.Flags().StringVar(&mode, "mode", "", "only these modes: commands, process, sync, stream, detached")
	cmd.Flags().BoolVar(&stderr, "stderr", false, "print stderr instead of stdout")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output until the command ends")
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (a *App) psCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ps ID",
		Short: "List the processes in a sandbox, busiest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			pl, err := c.Processes(cmd.Context(), id)
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(pl)
				return nil
			}
			if !pl.Live && pl.SampledAt != "" {
				a.printf("last sample: %s (%s)\n", pl.SampledAt, ago(pl.SampledAt))
			}
			var rows [][]string
			for _, p := range pl.Processes {
				rows = append(rows, []string{strconv.Itoa(p.PID), fmt.Sprintf("%.1f", p.CPUPercent),
					fmt.Sprintf("%.0f", p.MemoryMB), p.User, firstNonEmpty(p.CommandID, "-"), truncate(p.Command, 70)})
			}
			table(a.Out, "PID\tCPU%\tMEM MB\tUSER\tHISTORY\tCOMMAND", rows)
			return nil
		},
	}
}

func commandExitCode(code *int, signal string) int {
	if code != nil {
		return *code
	}
	names := []string{"", "SIGHUP", "SIGINT", "SIGQUIT", "SIGILL", "SIGTRAP", "SIGABRT", "SIGBUS", "SIGFPE", "SIGKILL", "SIGUSR1", "SIGSEGV", "SIGUSR2", "SIGPIPE", "SIGALRM", "SIGTERM", "SIGSTKFLT", "SIGCHLD", "SIGCONT", "SIGSTOP", "SIGTSTP", "SIGTTIN", "SIGTTOU", "SIGURG", "SIGXCPU", "SIGXFSZ", "SIGVTALRM", "SIGPROF", "SIGWINCH", "SIGIO", "SIGPWR", "SIGSYS"}
	for n, name := range names {
		if n > 0 && name == signal {
			return 128 + n
		}
	}
	return 1
}
