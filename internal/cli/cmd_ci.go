package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// ciCmd mirrors `boat ci`: GitHub Actions jobs on CrowdVM sandboxes.
func (a *App) ciCmd() *cobra.Command {
	var account, repo, state string
	var limit int
	var all bool
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "GitHub Actions on CrowdVM: setup, problems, stats and last jobs",
		Long: `Show the CI setup steps, problems, 7-day stats and the last jobs. Jobs run
in sandboxes when a workflow uses runs-on: crowdvm (or crowdvm-small, -large,
-xlarge). Connect GitHub first with 'crowdvm ci connect --token <token>'.`,
		Example: `  crowdvm ci
  crowdvm ci --repo acme/api --state failed
  crowdvm ci repos
  crowdvm ci job <job id>
  crowdvm ci logs <job id> --follow`,
		Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			q := url.Values{}
			setQ(q, "account", account)
			setQ(q, "repo", repo)
			setQ(q, "state", state)
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			r, err := c.CIOverview(cmd.Context(), q)
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printCI(r, all) })
		}),
	}
	f := cmd.Flags()
	f.StringVar(&account, "account", "", "only this linked GitHub account")
	f.StringVar(&repo, "repo", "", "only this repository (owner/name)")
	f.StringVar(&state, "state", "", "all, active or failed")
	f.IntVar(&limit, "limit", 0, "show at most this many jobs (1-500)")
	f.BoolVar(&all, "all", false, "show every job, not just the last 10")

	repos := &cobra.Command{
		Use: "repos", Short: "Repositories the linked GitHub accounts can see, and their status", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.CIOverview(cmd.Context(), url.Values{"limit": {"1"}})
			if err != nil {
				return err
			}
			var out []map[string]any
			for _, in := range objs(r["installations"]) {
				for _, rp := range objs(in["repositories"]) {
					rp["account"] = in["accountLogin"]
					out = append(out, rp)
				}
			}
			if a.jsonOut {
				a.emit(map[string]any{"repositories": nonNil(out)})
				return nil
			}
			if len(out) == 0 {
				a.info("no repositories; connect GitHub with `crowdvm ci connect --token <token>`\n")
				return nil
			}
			var rows [][]string
			for _, rp := range out {
				vis := "public"
				if rp["private"] == true {
					vis = "private"
				}
				rows = append(rows, []string{s(rp["fullName"]), vis, s(rp["status"]), s(rp["jobs7d"]), s(rp["lastState"]), ago(s(rp["lastAt"]))})
			}
			table(a.Out, "REPOSITORY\tVISIBILITY\tSTATUS\tJOBS 7D\tLAST\tWHEN", rows)
			return nil
		}),
	}
	job := &cobra.Command{
		Use: "job JOB_ID", Short: "One job: wait, run time, cost, sandbox, error", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.CIJob(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(r, func() {
				if j, ok := r["job"].(map[string]any); ok {
					prettyObject(a.Out, j, "")
				}
			})
		}),
	}
	var follow bool
	logs := &cobra.Command{
		Use: "logs JOB_ID", Short: "Print the GitHub log of a job (--follow while it runs)", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			a.rawOutput(cmd)
			printed := 0
			for {
				r, err := c.CIJobLog(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				log := s(r["log"])
				if a.jsonOut {
					a.emit(r)
				} else if len(log) > printed {
					fmt.Fprint(a.Out, log[printed:])
					printed = len(log)
				} else if len(log) < printed { // the server keeps the tail only
					fmt.Fprint(a.Out, log)
					printed = len(log)
				}
				if !follow {
					return nil
				}
				j, err := c.CIJob(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if st := s(mapOf(j["job"])["state"]); st != "queued" && st != "running" {
					if r, err := c.CIJobLog(cmd.Context(), args[0]); err == nil && !a.jsonOut {
						if l := s(r["log"]); len(l) > printed {
							fmt.Fprint(a.Out, l[printed:])
						}
					}
					return nil
				}
				if err := sleepCtx(cmd.Context(), 3*time.Second); err != nil {
					return nil
				}
			}
		}),
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing while the job runs")

	var token, cAccount, apiURL, linkID string
	var yes bool
	connect := &cobra.Command{
		Use:   "connect",
		Short: "Link a GitHub account or organization with a token (jobs bill the wallet in scope)",
		Long: `Link a GitHub user or organization so its workflows can run on CrowdVM.
CrowdVM has no GitHub App: give a token (fine-grained: Actions read,
Administration read/write, Metadata read; classic: repo, plus admin:org for
organizations). Jobs bill the wallet in scope (--org or 'crowdvm org switch').
Without --yes the link stays pending until you confirm it.`,
		Example: `  crowdvm ci connect --token "$GH_TOKEN" --yes
  crowdvm --org acme ci connect --account acme-github --token "$GH_TOKEN" --yes
  crowdvm ci connect --link cil_123 --yes      # confirm a pending link`,
		Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{}
			if linkID != "" {
				body["linkId"], body["confirm"] = linkID, yes
			} else {
				if token == "" || token == "-" {
					t, err := a.readSecret("-")
					if err != nil {
						return err
					}
					token = strings.TrimSpace(t)
				}
				if token == "" {
					return usagef("give a GitHub token: --token TOKEN (or on stdin)")
				}
				body["token"] = token
				if yes {
					body["confirm"] = true
				}
				setObj(body, "account", cAccount)
				setObj(body, "apiUrl", apiURL)
				setObj(body, "org", c.Org)
			}
			r, err := c.CIConnect(cmd.Context(), body)
			if err != nil {
				return err
			}
			return a.result(r, func() {
				l := mapOf(r["link"])
				a.printf("%s %s (%s) → %s\n", s(l["id"]), s(l["accountLogin"]), s(l["accountType"]), s(l["status"]))
				if l["confirmRequired"] == true {
					a.printf("confirm with: crowdvm ci connect --link %s --yes\n", s(l["id"]))
				}
			})
		}),
	}
	connect.Flags().StringVar(&token, "token", "", "GitHub token (- or empty: read stdin)")
	connect.Flags().StringVar(&cAccount, "account", "", "GitHub account or organization to link (default: the token's user)")
	connect.Flags().StringVar(&apiURL, "api-url-github", "", "GitHub-compatible API URL (GitHub Enterprise)")
	connect.Flags().StringVar(&linkID, "link", "", "confirm this pending link id")
	connect.Flags().BoolVarP(&yes, "yes", "y", false, "link now, without a pending confirmation step")

	var users string
	var anyone, dryRun bool
	public := &cobra.Command{
		Use:   "public ACCOUNT [on|off]",
		Short: "Show or set whether public repositories of a linked account run on CrowdVM",
		Example: `  crowdvm ci public my-account on --users "me, @a-teammate"
  crowdvm ci public my-account
  crowdvm ci public my-account off`,
		Args: cobra.RangeArgs(1, 2),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{"account": args[0]}
			switch argN(args, 1) {
			case "":
			case "on":
				body["allow"] = true
			case "off":
				body["allow"] = false
			default:
				return usagef("want on or off")
			}
			switch {
			case anyone && users != "":
				return usagef("--anyone and --users cannot be combined")
			case anyone:
				body["users"] = []string{}
			case users != "":
				var list []string
				for _, u := range strings.FieldsFunc(users, func(r rune) bool { return r == ',' || r == ' ' }) {
					list = append(list, strings.TrimPrefix(u, "@"))
				}
				body["users"] = list
			}
			if len(args) == 1 && users == "" && !anyone {
				body["dryRun"] = true // show the setting
			} else if dryRun {
				body["dryRun"] = true
			}
			setObj(body, "org", c.Org)
			r, err := c.CIPublicRepos(cmd.Context(), body)
			if err != nil {
				return err
			}
			return a.result(r, func() {
				state := "off"
				if r["allowPublicRepos"] == true {
					state = "on"
				}
				var names []string
				for _, u := range objs(r["publicRepoUsers"]) {
					names = append(names, s(u["login"]))
				}
				who := "anyone"
				if len(names) > 0 {
					who = strings.Join(names, ", ")
				}
				a.printf("public repositories of %s: %s (users: %s)\n", args[0], state, who)
			})
		}),
	}
	public.Flags().StringVar(&users, "users", "", "GitHub users whose runs may use CrowdVM (comma-separated)")
	public.Flags().BoolVar(&anyone, "anyone", false, "clear the user list: every run uses CrowdVM")
	public.Flags().BoolVar(&dryRun, "dry-run", false, "validate without saving")

	unlink := &cobra.Command{
		Use: "unlink LINK_ID", Short: "Remove a linked GitHub account", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.CIUnlink(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("unlinked %s\n", args[0]) })
		}),
	}
	cmd.AddCommand(repos, job, logs, connect, public, unlink)
	return cmd
}

func (a *App) printCI(r client.Object, all bool) {
	setup := mapOf(r["setup"])
	if setup["complete"] != true {
		a.printf("Setup:\n")
		for _, st := range objs(setup["steps"]) {
			mark := "[ ]"
			if st["done"] == true {
				mark = "[x]"
			}
			a.printf("  %s %s: %s\n", mark, s(st["title"]), s(st["detail"]))
		}
	}
	if probs := objs(setup["problems"]); len(probs) > 0 {
		a.printf("Problems:\n")
		for _, p := range probs {
			a.printf("  %s: %s (%v×, last %s; crowdvm ci job %s)\n", s(p["repo"]), s(p["message"]), p["count"], ago(s(p["at"])), s(p["jobId"]))
		}
	}
	st := mapOf(r["stats"])
	a.printf("Last 7 days: %v jobs, %v running", st["jobs"], st["running"])
	if v, ok := st["successRate"].(float64); ok {
		a.printf(", %.0f%% passed", v*100)
	}
	if v, ok := st["medianWaitSeconds"].(float64); ok {
		a.printf(", median wait %s", shortDur(time.Duration(v*float64(time.Second))))
	}
	if v, ok := st["medianRunSeconds"].(float64); ok {
		a.printf(", median run %s", shortDur(time.Duration(v*float64(time.Second))))
	}
	a.printf("\n")
	jobs := objs(r["jobs"])
	if len(jobs) == 0 {
		a.printf("no jobs yet\n")
		return
	}
	if !all && len(jobs) > 10 {
		jobs = jobs[:10]
	}
	var rows [][]string
	for _, j := range jobs {
		run := "-"
		if v, ok := j["runSeconds"].(float64); ok {
			run = shortDur(time.Duration(v * float64(time.Second)))
		}
		rows = append(rows, []string{s(j["id"]), s(j["repo"]), truncate(s(j["jobName"]), 30), s(j["state"]), run, ago(s(j["queuedAt"])), truncate(s(j["error"]), 50)})
	}
	table(a.Out, "JOB\tREPO\tNAME\tSTATE\tRUN\tQUEUED\tERROR", rows)
}

func setQ(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func setObj(o client.Object, k, v string) {
	if v != "" {
		o[k] = v
	}
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// objs returns the objects of a JSON array.
func objs(v any) []map[string]any {
	l, _ := v.([]any)
	var out []map[string]any
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
