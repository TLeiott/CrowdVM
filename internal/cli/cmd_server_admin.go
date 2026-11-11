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

// adminCmd groups the server-administrator commands (Boat's ops tooling,
// adapted: accounts, credits, invite codes, sandboxes, nodes).
func (a *App) adminCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Server administration: users, credits, invite codes, sandboxes, nodes",
		Long:  "Commands for administrators of the CrowdVM server (other accounts get 403 admin_required).",
	}
	objOut := func(text func(client.Object)) func(client.Object, error) error {
		return func(r client.Object, err error) error {
			if err != nil {
				return err
			}
			return a.result(r, func() { text(r) })
		}
	}
	userLine := func(r client.Object) {
		u := mapOf(r["user"])
		a.printf("%s %s admin=%v suspended=%v credits=%v\n", s(u["id"]), firstNonEmpty(s(u["login"]), s(u["username"])), u["isAdmin"], u["suspended"], u["credits"])
	}

	users := &cobra.Command{
		Use: "users", Short: "List accounts", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.AdminUsers(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			var rows [][]string
			for _, u := range objs(r["users"]) {
				flags := []string{}
				if u["isAdmin"] == true || u["admin"] == true {
					flags = append(flags, "admin")
				}
				if u["suspended"] == true {
					flags = append(flags, "suspended")
				}
				rows = append(rows, []string{s(u["id"]), firstNonEmpty(s(u["login"]), s(u["username"])), s(u["email"]),
					strings.Join(flags, ","), s(u["credits"]), s(u["activeSandboxes"])})
			}
			table(a.Out, "ID\tLOGIN\tEMAIL\tFLAGS\tCREDITS\tACTIVE", rows)
			return nil
		}),
	}
	var email, display string
	var isAdmin, passStdin bool
	var credits float64
	create := &cobra.Command{
		Use: "create USERNAME", Short: "Create an account (password from stdin with --password-stdin)", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{"username": args[0], "admin": isAdmin}
			setObj(body, "email", email)
			setObj(body, "displayName", display)
			if passStdin || isTerminal(a.In) {
				p, err := a.readPassword(passStdin)
				if err != nil {
					return err
				}
				setObj(body, "password", p)
			}
			if cmd.Flags().Changed("credits") {
				body["credits"] = credits
			}
			return objOut(userLine)(c.AdminCreateUser(cmd.Context(), body))
		}),
	}
	create.Flags().StringVar(&email, "email", "", "email address")
	create.Flags().StringVar(&display, "display-name", "", "display name")
	create.Flags().BoolVar(&isAdmin, "admin", false, "make the account an administrator")
	create.Flags().BoolVar(&passStdin, "password-stdin", false, "read the password from stdin")
	create.Flags().Float64Var(&credits, "credits", 0, "starting credits")
	users.AddCommand(create)

	update := func(use, short string, body func(args []string) (client.Object, error)) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(strings.Count(use, " ")),
			RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
				b, err := body(args)
				if err != nil {
					return err
				}
				return objOut(userLine)(c.AdminUpdateUser(cmd.Context(), args[0], b))
			}),
		}
	}
	suspend := update("suspend USER", "Suspend an account (signs it out everywhere)", func([]string) (client.Object, error) {
		return client.Object{"disabled": true}, nil
	})
	unsuspend := update("unsuspend USER", "Reactivate a suspended account", func([]string) (client.Object, error) {
		return client.Object{"disabled": false}, nil
	})
	setAdmin := update("set-admin USER on|off", "Grant or remove administrator rights", func(args []string) (client.Object, error) {
		switch args[1] {
		case "on", "off":
			return client.Object{"admin": args[1] == "on"}, nil
		}
		return nil, usagef("want on or off")
	})
	var pwStdin bool
	resetPw := &cobra.Command{
		Use: "reset-password USER", Short: "Set a new password for an account", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			p, err := a.readPassword(pwStdin)
			if err != nil {
				return err
			}
			if p == "" {
				return usagef("empty password")
			}
			return objOut(userLine)(c.AdminUpdateUser(cmd.Context(), args[0], client.Object{"password": p}))
		}),
	}
	resetPw.Flags().BoolVar(&pwStdin, "password-stdin", false, "read the password from stdin")

	var gUser, gOrg, reason string
	grant := &cobra.Command{
		Use: "grant AMOUNT (--user USER | --org ORG)", Short: "Add credits to a wallet (a negative amount removes)", Args: cobra.ExactArgs(1),
		Example: "  crowdvm admin credits grant 100 --user alice --reason \"welcome\"\n  crowdvm admin credits grant 500 --org acme",
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			amt, err := strconv.ParseFloat(args[0], 64)
			if err != nil || amt == 0 {
				return usagef("AMOUNT must be a non-zero number")
			}
			if (gUser == "") == (gOrg == "") {
				return usagef("give exactly one of --user and --org")
			}
			body := client.Object{"amount": amt}
			setObj(body, "user", gUser)
			setObj(body, "org", gOrg)
			setObj(body, "reason", reason)
			return objOut(func(r client.Object) {
				w := mapOf(r["wallet"])
				a.printf("%s: balance %v credits\n", firstNonEmpty(gUser, gOrg), w["balanceCredits"])
			})(c.AdminGrant(cmd.Context(), body))
		}),
	}
	grant.Flags().StringVar(&gUser, "user", "", "user id or login")
	grant.Flags().StringVar(&gOrg, "org", "", "organization id or name")
	grant.Flags().StringVar(&reason, "reason", "", "ledger note")
	creditsCmd := &cobra.Command{Use: "credits", Short: "Grant or remove credits"}
	creditsCmd.AddCommand(grant)

	invites := &cobra.Command{
		Use: "invite-codes", Aliases: []string{"invites"}, Short: "List signup invite codes", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.AdminInviteCodes(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			var rows [][]string
			for _, ic := range objs(r["inviteCodes"]) {
				state := "active"
				if ic["revokedAt"] != nil {
					state = "revoked"
				}
				rows = append(rows, []string{s(ic["id"]), s(ic["prefix"]) + "…", fmt.Sprintf("%v/%v", ic["uses"], ic["maxUses"]),
					firstNonEmpty(s(ic["expiresAt"]), "never"), state, s(ic["note"])})
			}
			table(a.Out, "ID\tCODE\tUSES\tEXPIRES\tSTATE\tNOTE", rows)
			return nil
		}),
	}
	var note string
	var maxUses int
	var ttl time.Duration
	inviteCreate := &cobra.Command{
		Use: "create", Short: "Create a signup invite code (shown once)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{"note": note, "maxUses": maxUses}
			if ttl > 0 {
				body["ttlSeconds"] = int(ttl.Seconds())
			}
			return objOut(func(r client.Object) {
				a.printf("%v\n", r["code"])
				a.info("sign-up link: %v\n", r["signupUrl"])
			})(c.AdminCreateInviteCode(cmd.Context(), body))
		}),
	}
	inviteCreate.Flags().StringVar(&note, "note", "", "who it is for")
	inviteCreate.Flags().IntVar(&maxUses, "max-uses", 1, "how many accounts may sign up with it")
	inviteCreate.Flags().DurationVar(&ttl, "ttl", 0, "lifetime, e.g. 168h (default: no expiry)")
	invites.AddCommand(inviteCreate, &cobra.Command{
		Use: "revoke ID", Short: "Revoke an invite code", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return objOut(func(client.Object) { a.printf("revoked %s\n", args[0]) })(c.AdminRevokeInviteCode(cmd.Context(), args[0]))
		}),
	})

	var aState, aNode string
	sandboxes := &cobra.Command{
		Use: "sandboxes", Short: "List every account's sandboxes (newest 500)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			q := url.Values{}
			setQ(q, "state", aState)
			setQ(q, "node", aNode)
			r, err := c.AdminSandboxes(cmd.Context(), q)
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			var rows [][]string
			for _, sb := range objs(r["sandboxes"]) {
				owner := mapOf(sb["owner"])
				rows = append(rows, []string{s(sb["id"]), displayState(s(sb["state"])), s(sb["type"]),
					firstNonEmpty(s(owner["login"]), s(sb["createdBy"]), s(sb["ownerId"])), s(sb["node"]), ago(s(sb["createdAt"]))})
			}
			table(a.Out, "ID\tSTATE\tTYPE\tOWNER\tNODE\tCREATED", rows)
			return nil
		}),
	}
	sandboxes.Flags().StringVar(&aState, "state", "", "only this API state (e.g. running, archived, error)")
	sandboxes.Flags().StringVar(&aNode, "node", "", "only sandboxes on this node")

	var into string
	merge := &cobra.Command{
		Use: "merge USER --into TARGET", Short: "Move everything of USER (sandboxes, keys, credits...) into TARGET", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if into == "" {
				return usagef("--into is required")
			}
			if err := a.confirm(yes, "Merge "+args[0]+" into "+into+"? This cannot be undone."); err != nil {
				return err
			}
			return objOut(func(r client.Object) { a.printf("merged %s into %s\n", args[0], into) })(c.AdminMergeUser(cmd.Context(), args[0], into))
		}),
	}
	merge.Flags().StringVar(&into, "into", "", "the account that keeps everything")
	merge.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	var saOrg, saPreset, saTTL string
	svc := &cobra.Command{
		Use: "service-account NAME", Short: "Create a service account (optionally in --org) with an API key", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{"name": args[0]}
			setObj(body, "preset", saPreset)
			setObj(body, "ttl", saTTL)
			return objOut(func(r client.Object) {
				a.printf("%v\n", r["secret"])
				a.info("service account %v created; the key is shown only now\n", mapOf(r["user"])["login"])
			})(c.CreateServiceAccount(cmd.Context(), saOrg, true, body))
		}),
	}
	svc.Flags().StringVar(&saOrg, "in-org", "", "organization the account belongs to")
	svc.Flags().StringVar(&saPreset, "preset", "", "key preset: read-only, full-sandbox, ci or admin")
	svc.Flags().StringVar(&saTTL, "ttl", "", "key lifetime, e.g. 90d or never")
	var policySet []string
	policy := &cobra.Command{
		Use:   "policy WALLET",
		Short: "Show or change the limits of a wallet (user or org): credits, starts, sizes, TTL",
		Long: `Show the restriction policy of a wallet (a user id/login or an org id/name), or
change fields with --set KEY=VALUE (VALUE null = server default). Keys:
monthlyCredits, maxActive, startsPerMinute, startsPerHour, startsPerDay,
maxTtlSeconds, allowedSizes (comma-separated), savedSnapshotAllowance,
creditCapMonthly, lowCreditAlert.`,
		Example: "  crowdvm admin policy alice --set maxActive=3 --set allowedSizes=small,default",
		Args:    cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			var body client.Object
			if len(policySet) > 0 {
				cur, err := c.AdminPolicy(cmd.Context(), args[0], nil)
				if err != nil {
					return unsupported(err, "wallet policies")
				}
				body = client.Object{}
				for k, v := range mapOf(cur["policy"]) {
					if k != "effective" && k != "wallet" { // read-only
						body[k] = v
					}
				}
				for _, kv := range policySet {
					k, v, ok := strings.Cut(kv, "=")
					if !ok || k == "" {
						return usagef("--set wants KEY=VALUE, got %q", kv)
					}
					body[k] = policyValue(k, v)
				}
			}
			r, err := c.AdminPolicy(cmd.Context(), args[0], body)
			if err != nil {
				return unsupported(err, "wallet policies")
			}
			return a.result(r, func() { prettyObject(a.Out, mapOf(r["policy"]), "") })
		}),
	}
	policy.Flags().StringArrayVar(&policySet, "set", nil, "KEY=VALUE to change (repeatable; null = default)")
	feedback := &cobra.Command{
		Use: "feedback", Short: "Read the feedback users sent with `crowdvm feedback`", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.AdminFeedback(cmd.Context())
			if err != nil {
				return unsupported(err, "feedback")
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			for _, f := range objs(r["feedback"]) {
				a.printf("%s  %s  %s%s\n  %s\n", s(f["createdAt"]), s(f["login"]), s(f["id"]), map[bool]string{true: "  (" + s(f["sandboxId"]) + ")"}[s(f["sandboxId"]) != ""], s(f["text"]))
			}
			return nil
		}),
	}
	cmd.AddCommand(users, suspend, unsuspend, setAdmin, resetPw, merge, svc, creditsCmd, policy, feedback, invites, sandboxes, a.nodesCmd())
	return cmd
}

// nodeAdminCmds are the node-management subcommands of `nodes`.
func (a *App) nodeAdminCmds() []*cobra.Command {
	nodeOut := func(cmd *cobra.Command, r client.Object, err error, word string) error {
		if err != nil {
			return err
		}
		return a.result(r, func() {
			n := mapOf(r["node"])
			a.printf("%s: %s\n", firstNonEmpty(s(n["name"]), s(n["id"]), s(r["id"])), word)
		})
	}
	drain := &cobra.Command{
		Use: "drain NODE", Short: "Stop placing new sandboxes on a node (running ones keep running)", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.UpdateNode(cmd.Context(), args[0], client.Object{"disabled": true})
			return nodeOut(cmd, r, err, "drained (no new sandboxes)")
		}),
	}
	undrain := &cobra.Command{
		Use: "undrain NODE", Aliases: []string{"enable"}, Short: "Place sandboxes on a drained node again", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.UpdateNode(cmd.Context(), args[0], client.Object{"disabled": false})
			word := "accepting sandboxes"
			if mapOf(mapOf(r["node"])["info"])["paused"] == true {
				word = "paused locally; run crowdvm-node config set paused false on the node"
				r["message"] = word
			}
			return nodeOut(cmd, r, err, word)
		}),
	}
	var name string
	var labels []string
	update := &cobra.Command{
		Use: "update NODE", Short: "Rename a node or replace its labels", Args: cobra.ExactArgs(1),
		Example: "  crowdvm nodes update n_123 --name lab-gpu --label gpu=a100 --label room=2",
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			body := client.Object{}
			setObj(body, "name", name)
			if cmd.Flags().Changed("label") {
				// --label "" clears all labels: an empty value is no label.
				var kvs []string
				for _, l := range labels {
					if l != "" {
						kvs = append(kvs, l)
					}
				}
				m, err := parseKV("--label", kvs)
				if err != nil {
					return err
				}
				if m == nil {
					m = map[string]string{}
				}
				body["labels"] = m
			}
			if len(body) == 0 {
				return usagef("give --name or --label")
			}
			r, err := c.UpdateNode(cmd.Context(), args[0], body)
			return nodeOut(cmd, r, err, "updated")
		}),
	}
	update.Flags().StringVar(&name, "name", "", "new name")
	update.Flags().StringArrayVar(&labels, "label", nil, "label KEY=VALUE (repeatable; replaces all labels; --label \"\" removes them all)")
	var force, yes bool
	remove := &cobra.Command{
		Use: "remove NODE", Aliases: []string{"evict", "rm"}, Short: "Remove a node (--force evicts it while it runs sandboxes)", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if err := a.confirm(yes, "Remove node "+args[0]+"? It must join again to host sandboxes."); err != nil {
				return err
			}
			r, err := c.DeleteNode(cmd.Context(), args[0], force)
			return nodeOut(cmd, r, err, "removed")
		}),
	}
	remove.Flags().BoolVar(&force, "force", false, "remove even while it runs sandboxes")
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	tokens := &cobra.Command{
		Use: "join-tokens", Short: "List node join tokens (admins)", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.ListJoinTokens(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(r)
				return nil
			}
			var rows [][]string
			for _, t := range objs(r["joinTokens"]) {
				rows = append(rows, []string{s(t["id"]), s(t["prefix"]) + "…", s(t["name"]), fmt.Sprintf("%v/%v", t["uses"], firstNonEmpty(s(t["maxUses"]), "∞")),
					firstNonEmpty(s(t["expiresAt"]), "never"), s(t["status"])})
			}
			table(a.Out, "ID\tTOKEN\tNAME\tUSES\tEXPIRES\tSTATUS", rows)
			return nil
		}),
	}
	tokens.AddCommand(&cobra.Command{
		Use: "revoke TOKEN_ID", Short: "Revoke a join token", Args: cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if err := c.RevokeJoinToken(cmd.Context(), args[0]); err != nil {
				return err
			}
			return a.result(map[string]any{"id": args[0], "revoked": true}, func() { a.printf("revoked %s\n", args[0]) })
		}),
	})
	return []*cobra.Command{drain, undrain, update, remove, tokens}
}

// policyValue turns a --set value into JSON: null, numbers, bools, lists.
func policyValue(k, v string) any {
	switch {
	case v == "null" || v == "":
		return nil
	case v == "true" || v == "false":
		return v == "true"
	case k == "allowedSizes":
		return strings.Split(v, ",")
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return n
	}
	return v
}
