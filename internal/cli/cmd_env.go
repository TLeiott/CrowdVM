package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// findEnv resolves an environment by name or id.
func findEnv(ctx context.Context, c *client.Client, nameOrID string) (*client.Environment, error) {
	envs, err := c.ListEnvironments(ctx)
	if err != nil {
		return nil, err
	}
	for i := range envs {
		if envs[i].Name == nameOrID || envs[i].ID == nameOrID {
			return &envs[i], nil
		}
	}
	return nil, &client.Error{Status: 404, Code: "environment_not_found", Message: "no environment named " + nameOrID}
}

func (a *App) envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage environments (repos, secrets, credentials new sandboxes start from)",
		Long: `Environments hold what a sandbox gets at start: repositories, variables,
secret files and credentials. Each change makes a new immutable version; running
sandboxes stay on theirs until 'crowdvm env upgrade'. Add --org to work on an
organization's environments. Not the same as 'crowdvm new --env KEY=VALUE'.`,
	}
	// envRun wraps commands that take an environment name as args[0].
	envRun := func(fn func(cmd *cobra.Command, c *client.Client, e *client.Environment, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			e, err := findEnv(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			return fn(cmd, c, e, args[1:])
		}
	}
	changed := func(what string) func(r *client.EnvironmentChange, err error) error {
		return func(r *client.EnvironmentChange, err error) error {
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("%s (version %d)\n", what, r.VersionNumber) })
		}
	}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List environments", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			envs, err := c.ListEnvironments(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(map[string]any{"environments": nonNil(envs)})
				return nil
			}
			var rows [][]string
			for _, e := range envs {
				def := ""
				if e.IsDefault {
					def = "*"
				}
				var live []string
				for _, v := range e.Versions {
					if v.SandboxCount > 0 {
						live = append(live, fmt.Sprintf("v%d:%d", v.VersionNumber, v.SandboxCount))
					}
				}
				rows = append(rows, []string{def + e.Name, e.ID, "v" + strconv.Itoa(e.LatestVersionNumber), strings.Join(live, " ")})
			}
			table(a.Out, "NAME\tID\tLATEST\tLIVE SANDBOXES", rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "info NAME", Short: "Show one environment in full", Args: cobra.ExactArgs(1),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, _ []string) error {
			return a.result(e, func() { a.printEnv(e) })
		}),
	})
	cmd.AddCommand(&cobra.Command{
		Use: "new NAME", Short: "Create an environment", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			e, err := c.CreateEnvironment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(e, func() { a.printf("created environment %s\n", args[0]) })
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "rename NAME NEW_NAME", Short: "Rename an environment", Args: cobra.ExactArgs(2),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			r, err := c.UpdateEnvironment(cmd.Context(), e.ID, &client.UpdateEnvironmentRequest{Name: rest[0]})
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("renamed %s → %s\n", e.Name, rest[0]) })
		}),
	})
	cmd.AddCommand(&cobra.Command{
		Use: "default NAME", Short: "Make it the environment new sandboxes use", Args: cobra.ExactArgs(1),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, _ []string) error {
			r, err := c.UpdateEnvironment(cmd.Context(), e.ID, &client.UpdateEnvironmentRequest{IsDefault: boolPtr(true)})
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("%s is now the default environment\n", e.Name) })
		}),
	})
	cmd.AddCommand(&cobra.Command{
		Use: "rm NAME", Short: "Delete an environment (pinned sandboxes keep running)", Args: cobra.ExactArgs(1),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, _ []string) error {
			if err := c.DeleteEnvironment(cmd.Context(), e.ID); err != nil {
				return err
			}
			return a.result(map[string]any{"id": e.ID, "name": e.Name, "deleted": true}, func() { a.printf("deleted %s\n", e.Name) })
		}),
	})
	cmd.AddCommand(a.envSetCmd(envRun))
	cmd.AddCommand(a.envAccessCmd(envRun))
	cmd.AddCommand(&cobra.Command{
		Use: "set-var NAME KEY[=VALUE]", Short: "Set a variable (value from stdin when omitted)", Args: cobra.ExactArgs(2),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			k, v, ok := strings.Cut(rest[0], "=")
			if !ok {
				var err error
				if v, err = a.readSecret(""); err != nil {
					return err
				}
			}
			a.warnWithheld(e)
			return changed("set " + k)(c.SetEnvironmentVar(cmd.Context(), e.ID, k, v))
		}),
	})
	cmd.AddCommand(&cobra.Command{
		Use: "rm-var NAME KEY", Short: "Remove a variable", Args: cobra.ExactArgs(2),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			return changed("removed " + rest[0])(c.DeleteEnvironmentVar(cmd.Context(), e.ID, rest[0]))
		}),
	})
	var from string
	setFile := &cobra.Command{
		Use: "set-file NAME PATH", Short: "Add a secret file (contents from --from or stdin)", Args: cobra.ExactArgs(2),
		Long: "Add a secret file to an environment. PATH is relative to /home/user (for example backend/.env, or a repository folder followed by a path in it). Contents come from --from or stdin.",
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			var b []byte
			var err error
			if from != "" {
				b, err = os.ReadFile(from)
			} else {
				b, err = io.ReadAll(a.In)
			}
			if err != nil {
				return err
			}
			a.warnWithheld(e)
			return changed("set file " + rest[0])(c.SetEnvironmentSecretFile(cmd.Context(), e.ID, rest[0], string(b)))
		}),
	}
	setFile.Flags().StringVar(&from, "from", "", "local file to read")
	cmd.AddCommand(setFile)
	cmd.AddCommand(&cobra.Command{
		Use: "rm-file NAME PATH", Short: "Remove a secret file", Args: cobra.ExactArgs(2),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			return changed("removed file " + rest[0])(c.DeleteEnvironmentSecretFile(cmd.Context(), e.ID, rest[0]))
		}),
	})
	var branch string
	addRepo := &cobra.Command{
		Use: "add-repo NAME REPO", Short: "Clone a repository into new sandboxes", Args: cobra.ExactArgs(2),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			return changed("added " + rest[0])(c.AddEnvironmentRepo(cmd.Context(), e.ID, rest[0], branch))
		}),
	}
	addRepo.Flags().StringVar(&branch, "branch", "", "base branch (default main)")
	cmd.AddCommand(addRepo)
	cmd.AddCommand(&cobra.Command{
		Use: "rm-repo NAME REPO", Short: "Stop cloning a repository", Args: cobra.ExactArgs(2),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, rest []string) error {
			repo := rest[0]
			for _, r := range e.SelectedRepositories {
				if r["fullName"] == repo || r["name"] == repo {
					repo = firstNonEmpty(fmt.Sprint(r["databaseId"]), repo)
				}
			}
			return changed("removed " + rest[0])(c.DeleteEnvironmentRepo(cmd.Context(), e.ID, repo))
		}),
	})
	var upSandboxes []string
	upgrade := &cobra.Command{
		Use: "upgrade NAME", Short: "Move this environment's sandboxes (or --sandbox ones) to its latest version", Args: cobra.ExactArgs(1),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, _ []string) error {
			var ids []string
			for _, v := range upSandboxes {
				id, err := a.sandboxID(v)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			r, err := c.UpgradeEnvironment(cmd.Context(), e.ID, ids...)
			if err != nil {
				return err
			}
			return a.result(r, func() {
				a.printf("upgraded %d sandboxes (%d live, %d stopped), %d failed\n", r.Upgraded, r.UpgradedLive, r.UpgradedStopped, r.Failed)
			})
		}),
	}
	upgrade.Flags().StringArrayVar(&upSandboxes, "sandbox", nil, "only this sandbox (repeatable)")
	cmd.AddCommand(upgrade)
	return cmd
}

func (a *App) warnWithheld(e *client.Environment) {
	if e.SafeForThirdParties {
		fmt.Fprintln(a.Err, "warning: this environment is safe for third parties; the value is saved but not injected into sandboxes")
	}
}

func (a *App) printEnv(e *client.Environment) {
	yn := func(b bool) string {
		if e.SafeForThirdParties && b {
			return "no (withheld: safe for third parties)"
		}
		if b {
			return "yes"
		}
		return "no"
	}
	a.printf("name:        %s (%s)%s\n", e.Name, e.ID, map[bool]string{true: " [default]"}[e.IsDefault])
	a.printf("version:     v%d\n", e.LatestVersionNumber)
	a.printf("safe for third parties: %v\n", e.SafeForThirdParties)
	a.printf("github:      %s\nsecrets:     %s\ncrowdvm credentials: %s\nagent credentials:   %s\n",
		yn(e.PassGithub), yn(e.PassSecrets), yn(e.PassSandboxCredentials), yn(e.PassAgentsCredentials))
	if e.CredentialProfileID != "" {
		a.printf("credential profile: %s\n", e.CredentialProfileID)
	}
	var keys []string
	for _, l := range strings.Split(e.EnvContents, "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && k != "" && !strings.HasPrefix(k, "#") {
			keys = append(keys, k)
		}
	}
	a.printf("variables:   %s\n", strings.Join(keys, ", "))
	for _, f := range e.SecretFiles {
		a.printf("secret file: %s\n", f.Path)
	}
	for _, r := range e.SelectedRepositories {
		a.printf("repo:        %v (%v)\n", firstNonEmpty(fmt.Sprint(r["fullName"]), fmt.Sprint(r["name"])), r["baseBranch"])
	}
	for _, v := range e.Versions {
		a.printf("v%d: %d sandbox(es), %s\n", v.VersionNumber, v.SandboxCount, v.CreatedAt)
	}
}

type envRunner func(func(*cobra.Command, *client.Client, *client.Environment, []string) error) func(*cobra.Command, []string) error

func (a *App) envSetCmd(envRun envRunner) *cobra.Command {
	var safe, github, secrets, cvmCreds, agentCreds, proxy string
	var profile string
	cmd := &cobra.Command{
		Use:     "set NAME",
		Short:   "Change what the environment injects",
		Example: "  crowdvm env set base --safe-for-third-parties true\n  crowdvm env set base --github true --secrets false",
		Args:    cobra.ExactArgs(1),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, _ []string) error {
			req := &client.UpdateEnvironmentRequest{}
			var err error
			for _, f := range []struct {
				val string
				dst **bool
			}{{safe, &req.SafeForThirdParties}, {github, &req.PassGithub}, {secrets, &req.PassSecrets},
				{cvmCreds, &req.PassSandboxCredentials}, {agentCreds, &req.PassAgentsCredentials}, {proxy, &req.ProxyAICredentials}} {
				if f.val == "" {
					continue
				}
				b, perr := strconv.ParseBool(f.val)
				if perr != nil {
					return usagef("want true or false, got %q", f.val)
				}
				*f.dst = boolPtr(b)
			}
			if profile != "" {
				p := profile
				if p == "default" {
					p = ""
				}
				req.CredentialProfileID = &p
			}
			r, err := c.UpdateEnvironment(cmd.Context(), e.ID, req)
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("updated %s (version %d)\n", e.Name, r.LatestVersionNumber) })
		}),
	}
	f := cmd.Flags()
	f.StringVar(&safe, "safe-for-third-parties", "", "true withholds every secret (master switch)")
	f.StringVar(&github, "github", "", "pass GitHub access (true/false)")
	f.StringVar(&secrets, "secrets", "", "pass variables and secret files (true/false)")
	f.StringVar(&cvmCreds, "crowdvm-credentials", "", "give sandboxes CLI credentials to manage themselves (true/false)")
	f.StringVar(&cvmCreds, "boat-credentials", "", "alias of --crowdvm-credentials")
	_ = f.MarkHidden("boat-credentials")
	f.StringVar(&agentCreds, "agents-credentials", "", "pass agent logins (Claude, Codex, ...) (true/false)")
	f.StringVar(&profile, "credential-profile", "", "credential profile name or id (default = owner's default)")
	f.StringVar(&proxy, "proxy-ai-credentials", "", "with safe-for-third-parties: pass AI credentials as proxy placeholders")
	return cmd
}

func (a *App) envAccessCmd(envRun envRunner) *cobra.Command {
	var everyone bool
	var only, add, remove string
	cmd := &cobra.Command{
		Use:   "access NAME",
		Short: "Show or limit which org members can use an environment",
		Args:  cobra.ExactArgs(1),
		RunE: envRun(func(cmd *cobra.Command, c *client.Client, e *client.Environment, _ []string) error {
			if remove != "" && !strings.HasPrefix(remove, "usr_") {
				org := c.Org
				if org == "" {
					org = s(e.Owner["id"])
				}
				members, err := c.OrgMembers(cmd.Context(), org)
				if err != nil {
					return err
				}
				found := false
				for _, m := range objs(members["members"]) {
					if s(m["login"]) == remove || strings.EqualFold(s(m["email"]), remove) {
						remove = s(m["id"])
						found = true
						break
					}
				}
				if !found {
					return usagef("no organization member named %s", remove)
				}
			}
			acc, changed := updateAccess(e.Access, everyone, only, add, remove)
			if cmd.Flags().Changed("only") && only == "" {
				acc = &client.ResourceAccess{Restricted: true, Members: []string{}}
				changed = true
			}
			if changed {
				r, err := c.UpdateEnvironment(cmd.Context(), e.ID, &client.UpdateEnvironmentRequest{Access: acc})
				if err != nil {
					return err
				}
				if r.Access != nil {
					acc = r.Access
				}
			}
			return a.result(map[string]any{"name": e.Name, "access": acc}, func() { a.printAccess(acc) })
		}),
	}
	accessFlags(cmd, &everyone, &only, &add, &remove)
	return cmd
}

func accessFlags(cmd *cobra.Command, everyone *bool, only, add, remove *string) {
	cmd.Flags().BoolVar(everyone, "everyone", false, "every member can use it")
	cmd.Flags().StringVar(only, "only", "", "only owners, admins and these comma-separated emails")
	cmd.Flags().StringVar(add, "add", "", "add one member")
	cmd.Flags().StringVar(remove, "remove", "", "remove one member")
}

func updateAccess(cur *client.ResourceAccess, everyone bool, only, add, remove string) (*client.ResourceAccess, bool) {
	acc := &client.ResourceAccess{}
	if cur != nil {
		*acc = *cur
	}
	switch {
	case everyone:
		return &client.ResourceAccess{Restricted: false}, true
	case only != "":
		acc.Restricted, acc.Members = true, nil
		for _, m := range strings.Split(only, ",") {
			if m = strings.TrimSpace(m); m != "" {
				acc.Members = append(acc.Members, m)
			}
		}
		return acc, true
	case add != "":
		acc.Restricted = true
		acc.Members = append(acc.Members, add)
		return acc, true
	case remove != "":
		var keep []string
		for _, m := range acc.Members {
			if m != remove {
				keep = append(keep, m)
			}
		}
		acc.Members = keep
		return acc, true
	}
	return acc, false
}

func (a *App) printAccess(acc *client.ResourceAccess) {
	if acc == nil || !acc.Restricted {
		a.printf("access: every member\n")
		return
	}
	a.printf("access: owners, admins and %s\n", strings.Join(acc.Members, ", "))
}

// reposCmd lists the repositories your git hosts expose and manages the git
// hosts ("sources") environments clone from.
func (a *App) reposCmd() *cobra.Command {
	var query string
	cmd := &cobra.Command{
		Use: "repos", Short: "Repositories you can add to environments, and the git hosts behind them", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			q := url.Values{}
			setQ(q, "q", query)
			r, err := c.Repos(cmd.Context(), q)
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			var rows [][]string
			for _, in := range objs(r["installations"]) {
				for _, rp := range objs(in["repositories"]) {
					vis := "public"
					if rp["private"] == true {
						vis = "private"
					}
					rows = append(rows, []string{firstNonEmpty(s(rp["fullName"]), s(rp["name"])), vis, s(rp["defaultBranch"]), s(rp["databaseId"])})
				}
			}
			table(a.Out, "REPOSITORY\tVISIBILITY\tBRANCH\tID", rows)
			return nil
		}),
	}
	cmd.Flags().StringVar(&query, "query", "", "filter by name")
	sources := &cobra.Command{
		Use: "sources", Short: "List your git hosts (GitHub, GitLab, Gitea/Forgejo, plain git)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.RepoSources(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			var rows [][]string
			for _, g := range objs(r["sources"]) {
				rows = append(rows, []string{s(g["id"]), s(g["kind"]), s(g["name"]), s(g["baseUrl"]), "…" + s(g["token"])})
			}
			table(a.Out, "ID\tKIND\tNAME\tURL\tTOKEN", rows)
			return nil
		}),
	}
	var kind, name, baseURL, apiURL, token string
	add := &cobra.Command{
		Use:   "add",
		Short: "Connect a git host with a personal access token (token from stdin when --token is omitted)",
		Example: `  crowdvm repos sources add --kind gitlab --token "$GITLAB_TOKEN"
  crowdvm repos sources add --kind gitea --base-url https://git.lan < token.txt`,
		Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if token == "" {
				t, err := a.readSecret("-")
				if err != nil {
					return err
				}
				token = strings.TrimSpace(t)
			}
			body := client.Object{"kind": kind, "token": token}
			setObj(body, "name", name)
			setObj(body, "baseUrl", baseURL)
			setObj(body, "apiUrl", apiURL)
			r, err := c.AddRepoSource(cmd.Context(), body)
			if err != nil {
				return err
			}
			return a.result(r, func() {
				g := mapOf(firstNonNil(r["source"], r))
				a.printf("added %s %s (%s)\n", s(g["kind"]), firstNonEmpty(s(g["name"]), s(g["baseUrl"])), s(g["id"]))
			})
		}),
	}
	add.Flags().StringVar(&kind, "kind", "github", "github, gitlab, gitea (also Forgejo) or git")
	add.Flags().StringVar(&name, "name", "", "display name")
	add.Flags().StringVar(&baseURL, "base-url", "", "web URL of the host (default for github/gitlab: the public site)")
	add.Flags().StringVar(&apiURL, "api-url", "", "API URL, when it is not the usual one")
	add.Flags().StringVar(&token, "token", "", "personal access token (default: read stdin)")
	sources.AddCommand(add, &cobra.Command{
		Use: "rm SOURCE_ID", Aliases: []string{"remove"}, Short: "Disconnect a git host", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.DeleteRepoSource(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("removed %s\n", args[0]) })
		}),
	})
	cmd.AddCommand(sources)
	return cmd
}

func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}
