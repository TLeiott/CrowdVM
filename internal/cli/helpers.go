package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// displayState maps API states to the CLI's words.
func displayState(s string) string {
	switch s {
	case "archived":
		return "stopped"
	case "archiving":
		return "stopping"
	case "init", "provisioning", "provisioned":
		return "starting"
	}
	return s
}

func isUp(state string) bool { return state == "ready" || state == "idle" || state == "running" }

func isFinalBad(state string) bool {
	return state == "error" || state == "archived" || state == "cancelled"
}

// isNodeOffline reports the passing error a node restart leaves on a sandbox:
// the node comes back and the sandbox returns to ready.
func isNodeOffline(s *client.Sandbox) bool {
	return s.State == "error" && strings.HasPrefix(s.Error, "node_offline")
}

func offlineErr(s *client.Sandbox) error {
	return &client.Error{Code: "sandbox_error", Message: "sandbox entered state error: " + s.Error}
}

// readyTimeout is how long waitReady waits (a var so tests can shorten it).
var readyTimeout = 30 * time.Minute

// waitReady polls until the sandbox is up and answers a no-op command.
// In --json mode it prints {"event":"state"} lines on every change.
func (a *App) waitReady(ctx context.Context, c *client.Client, id, last string) (*client.Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	var sb *client.Sandbox
	start, told := time.Now(), ""
	for {
		s, err := c.GetSandbox(ctx, id)
		if err != nil {
			if e, ok := client.AsError(err); !ok || !e.Retryable() {
				if sb != nil && isNodeOffline(sb) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return sb, offlineErr(sb)
				}
				return nil, err
			}
		} else {
			sb = s
			if s.State != last {
				last = s.State
				if a.jsonOut {
					a.emit(map[string]any{"event": "state", "id": id, "state": displayState(s.State)})
				} else {
					a.info("%s: %s\n", id, displayState(s.State))
				}
			}
			if isUp(s.State) {
				break
			}
			if isFinalBad(s.State) && !isNodeOffline(s) {
				msg := "sandbox entered state " + displayState(s.State)
				if s.Error != "" {
					msg += ": " + s.Error
				}
				return s, &client.Error{Code: "sandbox_" + s.State, Message: msg}
			}
			if s.State == "provisioning" && time.Since(start) > 20*time.Second {
				// Like Boat, a create waits for capacity (or here for an
				// upload); say so instead of looking hung.
				msg := "waiting for a node with free capacity (see crowdvm nodes; --fail-fast fails instead of waiting)"
				if s.WaitingForSnapshot != "" {
					msg = "waiting for snapshot " + s.WaitingForSnapshot + " to finish uploading to the server from the node that holds it"
				} else if told != "" {
					msg = told // said already; the rest of the boot is no capacity wait
				}
				if msg != told {
					told = msg
					if a.jsonOut {
						a.emit(map[string]any{"event": "waiting", "id": id, "state": "provisioning", "message": msg})
					} else {
						a.info("%s: %s\n", id, msg)
					}
				}
			}
		}
		if err := sleepCtx(ctx, a.Poll); err != nil {
			if sb != nil && isNodeOffline(sb) && errors.Is(err, context.DeadlineExceeded) {
				return sb, offlineErr(sb)
			}
			if errors.Is(err, context.Canceled) && sb != nil && !isUp(sb.State) {
				a.info("stopped waiting; %s is still %s. Remove it with: crowdvm delete %s --yes\n", id, displayState(sb.State), id)
			}
			return nil, err
		}
	}
	// The sandbox can run commands once a no-op succeeds.
	for {
		res, err := c.Exec(ctx, id, client.CommandRequest{Command: "true", TimeoutSeconds: 30})
		if err == nil && client.ExitCode(res.ExitCode) == 0 {
			return sb, nil
		}
		if err != nil {
			if e, ok := client.AsError(err); ok && !e.Retryable() && e.Status != 409 && e.Status != 502 {
				return sb, err
			}
		}
		if err := sleepCtx(ctx, a.Poll); err != nil {
			return sb, err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseEnv turns KEY=VALUE flags into a map.
func parseEnv(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, usagef("--env wants KEY=VALUE, got %q", kv)
		}
		m[k] = v
	}
	return m, nil
}

func ttlFor(cmd *cobra.Command, ttl int, noAutoStop bool) *client.TTL {
	if noAutoStop {
		return client.NoAutoStop()
	}
	if cmd.Flags().Changed("ttl") {
		return client.TTLSeconds(ttl)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

func table(w io.Writer, header string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if header != "" {
		fmt.Fprintln(tw, header)
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// ago renders an RFC 3339 time (or Unix ms/seconds) as "4 mins ago".
func ago(ts string) string {
	t, ok := parseTime(ts)
	if !ok {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "in " + humanDur(-d)
	case d < 10*time.Second:
		return "just now"
	}
	return humanDur(d) + " ago"
}

func parseTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d mins", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// shellQuote quotes one argument for a POSIX shell.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@,+%", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// commandLine joins argv: one argument is a shell line, more are quoted.
func commandLine(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = shellQuote(s)
	}
	return strings.Join(q, " ")
}

// reparse handles commands with SetInterspersed(false): flags may follow the
// first positional (the sandbox id); parsing stops at the next positional or "--".
func reparse(cmd *cobra.Command, args []string) (first string, rest []string, err error) {
	if len(args) == 0 {
		return "", nil, nil
	}
	first = args[0]
	if first == "--" {
		return "", args[1:], nil
	}
	if !looksLikeID(first) || cmd.ArgsLenAtDash() == 0 {
		return "", args, nil // no id: the command starts here; use the current sandbox
	}
	if err := cmd.Flags().Parse(args[1:]); err != nil {
		return "", nil, usageError{err.Error()}
	}
	return first, cmd.Flags().Args(), nil
}

// confirm asks a yes/no question on a terminal; scripts must pass --yes.
func (a *App) confirm(yes bool, question string) error {
	if yes {
		return nil
	}
	if a.jsonOut || !isTerminal(a.In) {
		return &client.Error{Code: "confirmation_required", Message: "refusing without --yes (no terminal to ask)"}
	}
	fmt.Fprintf(a.Err, "%s [y/N] ", question)
	var ans string
	_, _ = fmt.Fscanln(a.In, &ans)
	if ans = strings.ToLower(strings.TrimSpace(ans)); ans == "y" || ans == "yes" {
		return nil
	}
	return exitError{1}
}

// readSecret reads a value argument, or stdin when it is "" or "-".
func (a *App) readSecret(arg string) (string, error) {
	if arg != "" && arg != "-" {
		return arg, nil
	}
	if f, ok := a.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.Err, "value (hidden, end with Enter): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.Err)
		return strings.TrimSpace(string(b)), err
	}
	b, err := io.ReadAll(a.In)
	return strings.TrimRight(string(b), "\r\n"), err
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// pretty prints an open JSON object as indented key: value lines.
func prettyObject(w io.Writer, o map[string]any, indent string) {
	for _, k := range sortedKeys(o) {
		if k == "ok" || k == "type" {
			continue
		}
		switch v := o[k].(type) {
		case map[string]any:
			fmt.Fprintf(w, "%s%s:\n", indent, k)
			prettyObject(w, v, indent+"  ")
		case []any:
			fmt.Fprintf(w, "%s%s: %d item(s)\n", indent, k, len(v))
			for _, it := range v {
				if m, ok := it.(map[string]any); ok {
					fmt.Fprintf(w, "%s  -\n", indent)
					prettyObject(w, m, indent+"    ")
				} else {
					fmt.Fprintf(w, "%s  - %v\n", indent, it)
				}
			}
		case nil:
			fmt.Fprintf(w, "%s%s: -\n", indent, k)
		default:
			fmt.Fprintf(w, "%s%s: %v\n", indent, k, v)
		}
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func isDir(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// remotePath makes a sandbox path absolute (relative = under /home/user).
func remotePath(p string) string {
	if p == "" || p == "~" {
		return "/home/user"
	}
	if strings.HasPrefix(p, "~/") {
		return "/home/user/" + p[2:]
	}
	if !strings.HasPrefix(p, "/") {
		return "/home/user/" + p
	}
	return p
}

// looksLikeID reports a sandbox id or alias.
func looksLikeID(s string) bool {
	return strings.HasPrefix(s, "bx_") || strings.HasPrefix(s, "bx_") || s == "current" || s == "self"
}

// nonNil makes JSON print [] instead of null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
