package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

func (a *App) withClient(fn func(cmd *cobra.Command, c *client.Client, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c, err := a.client()
		if err != nil {
			return err
		}
		return fn(cmd, c, args)
	}
}

func (a *App) apiKeyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "api-key", Aliases: []string{"api-keys"}, Short: "Create and manage API keys (cvm_...)"}
	var ttl, preset, actions string
	var sandboxes, envs []string
	create := &cobra.Command{
		Use: "create NAME", Short: "Create a scoped API key (the secret is shown once)", Args: cobra.ExactArgs(1),
		Example: "  crowdvm api-key create ci --ttl 90d --preset ci\n  crowdvm api-key create agent --ttl 7d --sandbox bx_123 --actions sandbox.read,exec",
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if preset != "" && actions != "" {
				return usagef("--preset and --actions cannot be combined")
			}
			req := &client.CreateAPIKeyRequest{Name: args[0], TTL: ttl, Preset: preset, SandboxIDs: sandboxes, EnvironmentIDs: envs}
			if actions != "" {
				req.Actions = strings.Split(actions, ",")
			}
			r, err := c.CreateAPIKey(cmd.Context(), req)
			if err != nil {
				return err
			}
			return a.result(r, func() {
				a.printf("%s\n", r.Secret)
				a.info("key %s (%s) created; the secret is shown only now\n", r.APIKey.ID, r.APIKey.Name)
			})
		}),
	}
	create.Flags().StringVar(&ttl, "ttl", "90d", "lifetime: 90d, 24h, seconds, up to 365d, or never")
	create.Flags().StringVar(&preset, "preset", "", "read-only, full-sandbox, ci or admin")
	create.Flags().StringVar(&actions, "actions", "", "comma-separated actions (instead of --preset)")
	create.Flags().StringArrayVar(&sandboxes, "sandbox", nil, "limit to this sandbox (repeatable)")
	create.Flags().StringArrayVar(&envs, "env", nil, "limit to this environment id (repeatable)")
	var all bool
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List keys: scope, expiry, last use", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			l, err := c.ListAPIKeys(cmd.Context(), all)
			if err != nil {
				return err
			}
			keys := l.APIKeys
			if !all {
				keys = keys[:0:0]
				for _, k := range l.APIKeys {
					if k.SandboxID == "" {
						keys = append(keys, k)
					}
				}
			}
			if a.jsonOut {
				a.emit(map[string]any{"apiKeys": nonNil(keys)})
				return nil
			}
			var rows [][]string
			for _, k := range keys {
				scope := "full"
				if k.Scope != nil && len(k.Scope.Actions) > 0 {
					scope = strings.Join(k.Scope.Actions, ",")
				}
				rows = append(rows, []string{k.ID, k.Name, k.KeyPrefix + "…" + k.KeyLastFour, truncate(scope, 30),
					firstNonEmpty(k.ExpiresAt, "never"), firstNonEmpty(ago(k.LastUsedAt), "-"), strconv.Itoa(k.Usage.Requests)})
			}
			table(a.Out, "ID\tNAME\tKEY\tSCOPE\tEXPIRES\tLAST USED\tREQUESTS", rows)
			return nil
		}),
	}
	list.Flags().BoolVar(&all, "all", false, "also per-sandbox machine keys")
	var verbose bool
	usage := &cobra.Command{
		Use: "usage ID", Short: "30-day requests and live resources of a key", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			k, err := c.APIKeyUsage(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(k, func() {
				a.printf("%s (%s): %d requests in %d days, %d live resources\n", k.ID, k.Name, k.Usage.Requests, k.Usage.WindowDays, k.Resources.Total)
				if verbose {
					for _, r := range k.CreatedResources {
						a.printf("  %v %v %v\n", r["kind"], r["id"], r["state"])
					}
				}
			})
		}),
	}
	usage.Flags().BoolVar(&verbose, "verbose", false, "list the resources it created")
	cmd.AddCommand(create, list, usage,
		&cobra.Command{
			Use: "rotate ID", Short: "Replace a key's secret (the old one stops working)", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				r, err := c.RotateAPIKey(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return a.result(r, func() { a.printf("%s\n", r.Secret) })
			}),
		},
		&cobra.Command{
			Use: "revoke ID", Short: "Revoke a key", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				if err := c.RevokeAPIKey(cmd.Context(), args[0]); err != nil {
					return err
				}
				return a.result(map[string]any{"id": args[0], "revoked": true}, func() { a.printf("revoked %s\n", args[0]) })
			}),
		})
	return cmd
}

func webhookEvents(in []string) []string {
	var out []string
	for _, e := range in {
		for _, p := range strings.Split(e, ",") {
			if p = strings.TrimSpace(p); p != "" {
				if !strings.HasPrefix(p, "sandbox.") {
					p = "sandbox." + p
				}
				out = append(out, p)
			}
		}
	}
	return out
}

func (a *App) webhookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "webhook", Aliases: []string{"webhooks"}, Short: "Register sandbox lifecycle webhooks"}
	var name string
	var events []string
	printHook := func(r *client.WebhookResult) func() {
		return func() {
			a.printf("%s %s [%s]\n", r.Webhook.ID, r.Webhook.URL, strings.Join(r.Webhook.Events, ","))
			if r.Secret != "" {
				a.printf("signing secret (shown once): %s\n", r.Secret)
			}
		}
	}
	create := &cobra.Command{
		Use: "create URL", Short: "Create a webhook (events: ready, error, archived, hydrated, degraded, recovered)", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.CreateWebhook(cmd.Context(), &client.WebhookRequest{Name: name, URL: args[0], Events: webhookEvents(events)})
			if err != nil {
				return err
			}
			return a.result(r, printHook(r))
		}),
	}
	create.Flags().StringVar(&name, "name", "", "display name")
	create.Flags().StringArrayVar(&events, "event", nil, "event to send (repeatable or comma-separated; default all)")
	var uname, uurl string
	var uevents []string
	update := &cobra.Command{
		Use: "update ID", Short: "Change a webhook's name, URL or events", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.UpdateWebhook(cmd.Context(), args[0], &client.WebhookRequest{Name: uname, URL: uurl, Events: webhookEvents(uevents)})
			if err != nil {
				return err
			}
			return a.result(r, printHook(r))
		}),
	}
	update.Flags().StringVar(&uname, "name", "", "display name")
	update.Flags().StringVar(&uurl, "url", "", "new URL")
	update.Flags().StringArrayVar(&uevents, "event", nil, "events (replaces the list)")
	cmd.AddCommand(create, update,
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"}, Short: "List webhooks", Args: cobra.NoArgs,
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				hs, err := c.ListWebhooks(cmd.Context())
				if err != nil {
					return err
				}
				if a.jsonOut {
					a.emit(map[string]any{"webhooks": nonNil(hs)})
					return nil
				}
				var rows [][]string
				for _, h := range hs {
					rows = append(rows, []string{h.ID, h.Name, h.URL, strings.Join(h.Events, ",")})
				}
				table(a.Out, "ID\tNAME\tURL\tEVENTS", rows)
				return nil
			}),
		},
		&cobra.Command{
			Use: "get ID", Short: "Show a webhook", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				r, err := c.GetWebhook(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return a.result(r, printHook(r))
			}),
		},
		&cobra.Command{
			Use: "rotate ID", Short: "Replace the signing secret", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				r, err := c.RotateWebhook(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return a.result(r, printHook(r))
			}),
		},
		&cobra.Command{
			Use: "remove ID", Aliases: []string{"rm", "delete"}, Short: "Delete a webhook", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				if err := c.DeleteWebhook(cmd.Context(), args[0]); err != nil {
					return err
				}
				return a.result(map[string]any{"id": args[0], "deleted": true}, func() { a.printf("removed %s\n", args[0]) })
			}),
		})
	return cmd
}

func (a *App) telemetryCmd() *cobra.Command {
	show := func(r *client.TelemetryResult) func() {
		return func() {
			if r.Telemetry == nil {
				a.printf("no OpenTelemetry export\n")
				return
			}
			t := r.Telemetry
			a.printf("endpoint: %s\nheaders:  %s\nlast export: %s\n", t.Endpoint, strings.Join(t.Headers, ", "), firstNonEmpty(t.LastExportAt, "never"))
			if t.LastError != nil {
				a.printf("last error: %v\n", t.LastError["message"])
			}
			if r.Test != nil {
				a.printf("test export: ok=%v %v\n", r.Test["ok"], r.Test["message"])
			}
		}
	}
	cmd := &cobra.Command{
		Use: "telemetry", Short: "Send metrics and lifecycle events to an OTLP/HTTP endpoint", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.GetTelemetry(cmd.Context())
			if err != nil {
				return err
			}
			return a.result(r, show(r))
		}),
	}
	var headers []string
	set := &cobra.Command{
		Use: "set ENDPOINT", Short: "Set the export (replaces it) and send a test export", Args: cobra.ExactArgs(1),
		Example: "  crowdvm telemetry set https://otlp.example.com -H api-key=$KEY",
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			h := map[string]string{}
			for _, kv := range headers {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return usagef("--header wants key=value, got %q", kv)
				}
				h[k] = v
			}
			r, err := c.SetTelemetry(cmd.Context(), args[0], h)
			if err != nil {
				return err
			}
			return a.result(r, show(r))
		}),
	}
	set.Flags().StringArrayVarP(&headers, "header", "H", nil, "header key=value (repeatable)")
	cmd.AddCommand(set, &cobra.Command{
		Use: "unset", Short: "Remove the export", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if err := c.DeleteTelemetry(cmd.Context()); err != nil {
				return err
			}
			return a.result(map[string]any{"telemetry": nil}, func() { a.printf("telemetry export removed\n") })
		}),
	})
	return cmd
}

func (a *App) orgCmd() *cobra.Command {
	status := func(cmd *cobra.Command, c *client.Client, _ []string) error {
		l, err := c.ListOrgs(cmd.Context())
		if err != nil {
			return err
		}
		var active *client.Org
		for i := range l.Orgs {
			o := &l.Orgs[i]
			if (c.Org != "" && (o.ID == c.Org || strings.EqualFold(o.Name, c.Org))) || (c.Org == "" && o.Active) {
				active = o
			}
		}
		return a.result(map[string]any{"active": active, "override": c.Org != ""}, func() {
			if active == nil {
				a.printf("active: personal\n")
				return
			}
			a.printf("active: %s (%s, %s)\n", active.Name, active.ID, active.Type)
		})
	}
	cmd := &cobra.Command{
		Use: "org", Short: "Show or switch the organization new sandboxes belong to", Args: cobra.NoArgs,
		RunE: a.withClient(status),
	}
	orgInScope := func(cmd *cobra.Command, c *client.Client) (string, error) {
		if c.Org != "" {
			return c.Org, nil
		}
		l, err := c.ListOrgs(cmd.Context())
		if err != nil {
			return "", err
		}
		for _, o := range l.Orgs {
			if o.Active && o.Type == "org" {
				return o.ID, nil
			}
		}
		return "", usagef("no organization in scope; pass --org or run `crowdvm org switch`")
	}
	objOut := func(r client.Object, err error, text func(client.Object)) error {
		if err != nil {
			return err
		}
		return a.result(r, func() { text(r) })
	}
	var yes bool
	var role string
	del := &cobra.Command{
		Use: "delete", Short: "Delete the organization in scope (owner only)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			org, err := orgInScope(cmd, c)
			if err != nil {
				return err
			}
			if err := a.confirm(yes, "Delete organization "+org+"?"); err != nil {
				return err
			}
			if err := c.DeleteOrg(cmd.Context(), org); err != nil {
				return err
			}
			return a.result(map[string]any{"id": org, "deleted": true}, func() { a.printf("deleted %s\n", org) })
		}),
	}
	del.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	invite := &cobra.Command{
		Use: "invite [EMAIL]", Short: "Invite a member (prints a join code and link)", Args: cobra.MaximumNArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			org, err := orgInScope(cmd, c)
			if err != nil {
				return err
			}
			r, err := c.CreateOrgInvite(cmd.Context(), org, argN(args, 0), role)
			return objOut(r, err, func(r client.Object) {
				a.printf("code: %v\nlink: %v\n", r["code"], r["url"])
				a.info("they join with: crowdvm org join %v\n", r["code"])
			})
		}),
	}
	invite.Flags().StringVar(&role, "role", "member", "member or admin")
	var myes bool
	var transfer string
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Move your personal sandboxes (and their credit spend) into the organization in scope",
		Long: `Move your personal sandboxes into the organization in scope (--org, or
'crowdvm org switch'): from now on they spend the organization's credits.
Your personal credit balance stays on your account unless you pass
--transfer-credits N (or all). Owners only.`,
		Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			org, err := orgInScope(cmd, c)
			if err != nil {
				return err
			}
			if err := a.confirm(myes, "Move your personal sandboxes into "+org+"?"); err != nil {
				return err
			}
			var body client.Object
			switch {
			case transfer == "all":
				body = client.Object{"transferCredits": "all"}
			case transfer != "":
				n, err := strconv.ParseFloat(transfer, 64)
				if err != nil || n <= 0 {
					return usagef("--transfer-credits wants a positive number or all")
				}
				body = client.Object{"transferCredits": n}
			}
			r, err := c.MovePersonalToOrg(cmd.Context(), org, body)
			if e, ok := client.AsError(err); ok && (e.Status == 404 && e.Code != "org_not_found" || e.Status == 405 || e.Status == 501) {
				return &client.Error{Status: e.Status, Code: "not_supported", Message: "this server cannot move personal sandboxes into an organization yet"}
			}
			return objOut(r, err, func(r client.Object) {
				m := mapOf(r["moved"])
				a.printf("moved %s sandboxes", firstNonEmpty(s(m["sandboxes"]), "0"))
				if n := s(m["snapshots"]); n != "" {
					a.printf(" and %s saved snapshots", n)
				}
				a.printf(" into %s", org)
				if v, ok := r["transferredCredits"].(float64); ok && v != 0 {
					a.printf("; %v credits transferred", v)
				}
				a.printf("\n")
			})
		}),
	}
	migrate.Flags().BoolVarP(&myes, "yes", "y", false, "do not ask for confirmation")
	migrate.Flags().StringVar(&transfer, "transfer-credits", "", "also move this many personal credits (or all) to the organization")
	var saPreset, saTTL string
	sa := &cobra.Command{
		Use: "service-accounts", Short: "List the organization's service accounts (users that act only through org API keys)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			org, err := orgInScope(cmd, c)
			if err != nil {
				return err
			}
			r, err := c.ListServiceAccounts(cmd.Context(), org)
			return objOut(r, err, func(r client.Object) {
				var rows [][]string
				for _, k := range []string{"serviceAccounts", "users", "accounts"} {
					for _, u := range objs(r[k]) {
						rows = append(rows, []string{s(u["id"]), firstNonEmpty(s(u["login"]), s(u["username"])), s(u["createdAt"])})
					}
				}
				table(a.Out, "ID\tLOGIN\tCREATED", rows)
			})
		}),
	}
	saCreate := &cobra.Command{
		Use: "create NAME", Short: "Create a service account and its API key (the secret is shown once)", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			org, err := orgInScope(cmd, c)
			if err != nil {
				return err
			}
			body := client.Object{"name": args[0]}
			setObj(body, "preset", saPreset)
			setObj(body, "ttl", saTTL)
			r, err := c.CreateServiceAccount(cmd.Context(), org, false, body)
			return objOut(r, err, func(r client.Object) {
				a.printf("%v\n", r["secret"])
				a.info("service account %v created; the key is shown only now\n", mapOf(r["user"])["login"])
			})
		}),
	}
	saCreate.Flags().StringVar(&saPreset, "preset", "", "key preset: read-only, full-sandbox, ci or admin")
	saCreate.Flags().StringVar(&saTTL, "ttl", "", "key lifetime, e.g. 90d or never")
	sa.AddCommand(saCreate, &cobra.Command{
		Use: "remove USER", Aliases: []string{"rm"}, Short: "Delete a service account", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			org, err := orgInScope(cmd, c)
			if err != nil {
				return err
			}
			r, err := c.DeleteServiceAccount(cmd.Context(), org, args[0])
			return objOut(r, err, func(client.Object) { a.printf("removed %s\n", args[0]) })
		}),
	})
	cmd.AddCommand(del, invite, migrate, sa,
		&cobra.Command{
			Use: "create NAME", Short: "Create an organization", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				r, err := c.CreateOrg(cmd.Context(), args[0], false)
				return objOut(r, err, func(r client.Object) {
					if o, ok := r["org"].(map[string]any); ok {
						a.printf("created %v (%v)\n", o["name"], o["id"])
					}
				})
			}),
		},
		&cobra.Command{
			Use: "members", Short: "List the members and pending invites of the organization in scope", Args: cobra.NoArgs,
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				org, err := orgInScope(cmd, c)
				if err != nil {
					return err
				}
				r, err := c.OrgMembers(cmd.Context(), org)
				if err == nil {
					// Pending invites (visible to those who may list them).
					pending := []any{}
					if inv, ierr := c.OrgInvites(cmd.Context(), org); ierr == nil {
						is, _ := inv["invites"].([]any)
						for _, i := range is {
							if im, _ := i.(map[string]any); s(im["status"]) == "pending" {
								pending = append(pending, im)
							}
						}
					}
					r["pendingInvites"] = pending
				}
				return objOut(r, err, func(r client.Object) {
					ms, _ := r["members"].([]any)
					var rows [][]string
					for _, m := range ms {
						mm, _ := m.(map[string]any)
						rows = append(rows, []string{s(mm["id"]), firstNonEmpty(s(mm["login"]), s(mm["username"])), s(mm["email"]), s(mm["role"])})
					}
					is, _ := r["pendingInvites"].([]any)
					for _, i := range is {
						im, _ := i.(map[string]any)
						rows = append(rows, []string{s(im["id"]), "(invited)", s(im["email"]), s(im["role"]) + " (pending)"})
					}
					table(a.Out, "ID\tLOGIN\tEMAIL\tROLE", rows)
				})
			}),
		},
		&cobra.Command{
			Use: "transfer USER", Short: "Make USER the owner of the organization in scope", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				org, err := orgInScope(cmd, c)
				if err != nil {
					return err
				}
				r, err := c.TransferOrg(cmd.Context(), org, args[0])
				return objOut(r, err, func(client.Object) { a.printf("%s now owns %s\n", args[0], org) })
			}),
		},
		&cobra.Command{
			Use: "join CODE", Short: "Join an organization with an invite code", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				r, err := c.AcceptOrgInvite(cmd.Context(), args[0])
				return objOut(r, err, func(r client.Object) {
					if o, ok := r["org"].(map[string]any); ok {
						a.printf("joined %v\n", o["name"])
					}
				})
			}),
		},
	)
	cmd.AddCommand(
		&cobra.Command{Use: "status", Short: "Show the active organization", Args: cobra.NoArgs, RunE: a.withClient(status)},
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"}, Short: "Organizations you belong to", Args: cobra.NoArgs,
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				l, err := c.ListOrgs(cmd.Context())
				if err != nil {
					return err
				}
				if a.jsonOut {
					a.emit(l)
					return nil
				}
				var rows [][]string
				for _, o := range l.Orgs {
					act := ""
					if o.Active {
						act = "*"
					}
					rows = append(rows, []string{act + o.Name, o.ID, o.Type, o.Role, strconv.Itoa(o.MemberCount)})
				}
				table(a.Out, "NAME\tID\tTYPE\tROLE\tMEMBERS", rows)
				return nil
			}),
		},
		&cobra.Command{
			Use: "switch ORG", Short: "Make ORG (id or name, or personal) the active organization", Args: cobra.ExactArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				r, err := c.SetActiveOrg(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return a.result(r, func() { a.printf("active organization: %s\n", args[0]) })
			}),
		},
		&cobra.Command{
			Use: "default-visibility [private|shared]", Short: "Show or set whether new org sandboxes start private or shared", Args: cobra.MaximumNArgs(1),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				l, err := c.ListOrgs(cmd.Context())
				if err != nil {
					return err
				}
				var cur *client.Org
				for i, o := range l.Orgs {
					if o.Type == "org" && (o.ID == c.Org || strings.EqualFold(o.Name, c.Org) || c.Org == "" && o.Active) {
						cur = &l.Orgs[i]
					}
				}
				if cur == nil {
					return usagef("no organization in scope; pass --org or run `crowdvm org switch`")
				}
				if len(args) == 0 {
					vis := map[bool]string{true: "private", false: "shared"}[cur.NewSandboxesPrivate]
					out := map[string]any{"org": cur.ID, "name": cur.Name, "defaultVisibility": vis, "newSandboxesPrivate": cur.NewSandboxesPrivate}
					return a.result(out, func() { a.printf("new sandboxes start %s\n", vis) })
				}
				org := cur.ID
				if args[0] != "private" && args[0] != "shared" {
					return usagef("want private or shared")
				}
				r, err := c.UpdateOrg(cmd.Context(), org, client.Object{"newSandboxesPrivate": args[0] == "private"})
				if err != nil {
					return err
				}
				return a.result(r, func() { a.printf("new sandboxes start %s\n", args[0]) })
			}),
		},
	)
	return cmd
}

func (a *App) dataRetentionCmd() *cobra.Command {
	show := func(r *client.DataRetention) func() {
		return func() {
			a.printf("zero data retention: %v\nsnapshots off: %v\nmonitoring off: %v\n", r.Enabled, r.SnapshotsOff, r.MonitoringOff)
		}
	}
	statusRun := a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
		r, err := c.DataRetention(cmd.Context())
		if err != nil {
			return err
		}
		return a.result(r, show(r))
	})
	cmd := &cobra.Command{Use: "data-retention", Short: "Show or enable zero data retention", Args: cobra.NoArgs, RunE: statusRun}
	var yes bool
	enable := &cobra.Command{
		Use: "enable", Short: "Delete every sandbox permanently when it stops (irreversible)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if err := a.confirm(yes, "Enable zero data retention? Stopped sandboxes will be deleted, not kept."); err != nil {
				return err
			}
			r, err := c.UpdateDataRetention(cmd.Context(), &client.DataRetentionUpdate{Enabled: boolPtr(true), Confirmation: client.DataRetentionConfirmation})
			if err != nil {
				return err
			}
			return a.result(r, show(r))
		}),
	}
	enable.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Show the policy", Args: cobra.NoArgs, RunE: statusRun}, enable)
	return cmd
}

func (a *App) nodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List the machines lending compute, with their benchmark categories",
		Args:  cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.Nodes(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			nodes, _ := r["nodes"].([]any)
			var rows [][]string
			for _, n := range nodes {
				m, _ := n.(map[string]any)
				status := "offline"
				if m["online"] == true {
					status = "online"
				}
				if m["disabled"] == true {
					status = "disabled"
				}
				rows = append(rows, []string{s(m["id"]), s(m["name"]), status, s(m["os"]) + "/" + s(m["arch"]),
					s(m["tier"]), capacityOf(m["free"]) + " of " + capacityOf(m["capacity"]), s(m["sandboxes"]), benchSummary(m)})
			}
			if len(rows) == 0 {
				a.printf("no nodes have joined yet; on a machine to lend, run: crowdvm-node join --server %s --token <crowdvm nodes join-token>\n", a.server())
				return nil
			}
			table(a.Out, "ID\tNAME\tSTATUS\tOS\tTIER\tFREE\tSANDBOXES\tCATEGORIES", rows)
			return nil
		}),
	}
	var name string
	var ttl, maxUses int
	tok := &cobra.Command{
		Use: "join-token", Short: "Create a token for `crowdvm-node join` (admins)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{"name": name}
			if ttl > 0 {
				body["ttlSeconds"] = ttl
			}
			if maxUses > 0 {
				body["maxUses"] = maxUses
			}
			r, err := c.CreateJoinToken(cmd.Context(), body)
			if err != nil {
				return err
			}
			return a.result(r, func() {
				a.printf("%v\n", r["token"])
				a.info("on the machine to add: crowdvm-node join --server %v --token %v\n", firstNonEmpty(s(r["serverUrl"]), a.server()), r["token"])
			})
		}),
	}
	tok.Flags().StringVar(&name, "name", "", "label for the token")
	tok.Flags().IntVar(&ttl, "ttl", 0, "seconds until the token expires")
	tok.Flags().IntVar(&maxUses, "max-uses", 0, "how many nodes may join with it")
	cmd.AddCommand(a.nodeAdminCmds()...)
	cmd.AddCommand(tok, &cobra.Command{
		Use: "info NODE", Short: "Show one node: capacity, benchmark, sandboxes, earned credits", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.GetNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.result(r, func() {
				if n, ok := r["node"].(map[string]any); ok {
					prettyObject(a.Out, n, "")
				}
			})
		}),
	})
	return cmd
}

func capacityOf(v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return "-"
	}
	mem, _ := m["memoryMiB"].(float64)
	return fmt.Sprintf("%s vCPU/%.0f GB", s(m["vcpu"]), mem/1024)
}

func s(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func benchSummary(m map[string]any) string {
	var b map[string]any
	for _, k := range []string{"benchmark", "bench", "categories"} {
		if v, ok := m[k].(map[string]any); ok {
			b = v
			break
		}
	}
	if b == nil {
		return ""
	}
	if c, ok := b["categories"].(map[string]any); ok {
		b = c
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := b[k].(type) {
		case map[string]any:
			parts = append(parts, k+"="+firstNonEmpty(s(v["category"]), s(v["tier"]), s(v["score"])))
		default:
			parts = append(parts, k+"="+s(v))
		}
	}
	return strings.Join(parts, " ")
}

func (a *App) completionsCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completions [bash|zsh|fish|powershell]",
		Short:     "Print a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(a.Out, true)
			case "zsh":
				return root.GenZshCompletion(a.Out)
			case "fish":
				return root.GenFishCompletion(a.Out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(a.Out)
			}
			return usagef("unknown shell %q", args[0])
		},
	}
}
