package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

func (a *App) aiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ai",
		Short: "Manage AI credentials: profiles, keys, accounts, proxy, usage, blocking",
	}
	var profile string
	prof := func() string { return firstNonEmpty(profile, "default") }
	with := func(fn func(cmd *cobra.Command, c *client.Client, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			return fn(cmd, c, args)
		}
	}
	printAccounts := func(r *client.CredentialAccounts, err error) error {
		if err != nil {
			return err
		}
		return a.result(r, func() {
			for i, acc := range r.Accounts {
				mark := " "
				if acc.Active {
					mark = "*"
				}
				a.printf("%s %d  %s  (%s)\n", mark, i+1, acc.Label, acc.ID)
			}
		})
	}
	profileOut := func(what string) func(*client.CredentialProfile, error) error {
		return func(p *client.CredentialProfile, err error) error {
			if err != nil {
				return err
			}
			return a.result(p, func() { a.printf("%s\n", what) })
		}
	}

	accounts := &cobra.Command{
		Use: "accounts CREDENTIAL", Short: "List the accounts of a credential (the one in use is marked)", Args: cobra.ExactArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return printAccounts(c.ListCredentialAccounts(cmd.Context(), prof(), args[0]))
		}),
	}
	switchCmd := &cobra.Command{
		Use: "switch CREDENTIAL ACCOUNT", Short: "Switch a credential to another account (number, id or label)", Args: cobra.ExactArgs(2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			acc, err := a.resolveAccount(cmd.Context(), c, prof(), args[0], args[1])
			if err != nil {
				return err
			}
			return printAccounts(c.SwitchCredentialAccount(cmd.Context(), prof(), args[0], acc))
		}),
	}
	addAccount := &cobra.Command{
		Use: "add-account CREDENTIAL [VALUE]", Short: "Add another API key to switch to (stdin when VALUE is omitted or -)", Args: cobra.RangeArgs(1, 2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			v, err := a.readSecret(argN(args, 1))
			if err != nil {
				return err
			}
			return printAccounts(c.AddCredentialAccount(cmd.Context(), prof(), args[0], v))
		}),
	}
	forget := &cobra.Command{
		Use: "forget-account CREDENTIAL ACCOUNT", Short: "Delete an account of a credential", Args: cobra.ExactArgs(2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			acc, err := a.resolveAccount(cmd.Context(), c, prof(), args[0], args[1])
			if err != nil {
				return err
			}
			return printAccounts(c.DeleteCredentialAccount(cmd.Context(), prof(), args[0], acc))
		}),
	}
	for _, c := range []*cobra.Command{accounts, switchCmd, addAccount, forget} {
		c.Flags().StringVar(&profile, "profile", "", "credential profile (default: default)")
		cmd.AddCommand(c)
	}

	cmd.AddCommand(&cobra.Command{
		Use: "proxy [on|off]", Short: "Show or switch the AI credentials proxy", Args: cobra.MaximumNArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			var p *client.AIProxy
			var err error
			switch argN(args, 0) {
			case "":
				p, err = c.GetAIProxy(cmd.Context())
			case "on":
				p, err = c.SetAIProxy(cmd.Context(), true)
			case "off":
				p, err = c.SetAIProxy(cmd.Context(), false)
			default:
				return usagef("want on or off")
			}
			if err != nil {
				return err
			}
			return a.result(p, func() {
				state := "off"
				if p.Enabled {
					state = "on"
				}
				a.printf("AI proxy: %s", state)
				if p.ProxyURL != "" {
					a.printf(" (%s)", p.ProxyURL)
				}
				a.printf("\n")
			})
		}),
	})
	var orgOnly bool
	profiles := &cobra.Command{
		Use: "profiles", Short: "List the credential profiles you can use", Args: cobra.NoArgs,
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			ps, err := c.ListCredentialProfiles(cmd.Context())
			if err != nil {
				return err
			}
			if orgOnly {
				var keep []client.CredentialProfile
				for _, p := range ps {
					if p.Owner.Kind == "org" {
						keep = append(keep, p)
					}
				}
				ps = keep
			}
			if a.jsonOut {
				a.emit(map[string]any{"profiles": nonNil(ps)})
				return nil
			}
			var rows [][]string
			for _, p := range ps {
				def := ""
				if p.IsDefault {
					def = "*"
				}
				rows = append(rows, []string{def + p.Name, p.ID, p.Owner.Kind + ":" + p.Owner.Name, strings.Join(p.Connected, ","), strings.Join(p.CustomAPIs, ",")})
			}
			table(a.Out, "NAME\tID\tOWNER\tCONNECTED\tCUSTOM APIS", rows)
			return nil
		}),
	}
	profiles.Flags().BoolVar(&orgOnly, "org-only", false, "only organization profiles")
	cmd.AddCommand(profiles)

	pc := &cobra.Command{Use: "profile", Short: "Create and change credential profiles"}
	var copyFrom string
	newP := &cobra.Command{
		Use: "new NAME", Short: "Create a profile (owned by --org when given)", Args: cobra.ExactArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return profileOut("created " + args[0])(c.CreateCredentialProfile(cmd.Context(),
				&client.CreateCredentialProfileRequest{Name: args[0], Org: c.Org, CopyFrom: copyFrom}))
		}),
	}
	newP.Flags().StringVar(&copyFrom, "copy-from", "", "copy credentials from this profile")
	pc.AddCommand(newP)
	pc.AddCommand(&cobra.Command{
		Use: "default PROFILE", Short: "Make a profile its owner's default", Args: cobra.ExactArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return profileOut(args[0] + " is now the default")(c.UpdateCredentialProfile(cmd.Context(), args[0], &client.UpdateCredentialProfileRequest{IsDefault: boolPtr(true)}))
		}),
	})
	pc.AddCommand(&cobra.Command{
		Use: "rename PROFILE NEW_NAME", Short: "Rename a profile", Args: cobra.ExactArgs(2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return profileOut("renamed")(c.UpdateCredentialProfile(cmd.Context(), args[0], &client.UpdateCredentialProfileRequest{Name: args[1]}))
		}),
	})
	var everyone bool
	var only, add, remove string
	access := &cobra.Command{
		Use: "access PROFILE", Short: "Show or set which org members can use a profile", Args: cobra.ExactArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			ps, err := c.ListCredentialProfiles(cmd.Context())
			if err != nil {
				return err
			}
			var cur *client.ResourceAccess
			for _, p := range ps {
				if p.Name == args[0] || p.ID == args[0] {
					cur = p.Access
				}
			}
			acc, changed := updateAccess(cur, everyone, only, add, remove)
			if changed {
				p, err := c.UpdateCredentialProfile(cmd.Context(), args[0], &client.UpdateCredentialProfileRequest{Access: acc})
				if err != nil {
					return err
				}
				if p.Access != nil {
					acc = p.Access
				}
			}
			return a.result(map[string]any{"profile": args[0], "access": acc}, func() { a.printAccess(acc) })
		}),
	}
	accessFlags(access, &everyone, &only, &add, &remove)
	pc.AddCommand(access)
	pc.AddCommand(&cobra.Command{
		Use: "rm PROFILE", Short: "Delete a profile", Args: cobra.ExactArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if err := c.DeleteCredentialProfile(cmd.Context(), args[0]); err != nil {
				return err
			}
			return a.result(map[string]any{"profile": args[0], "deleted": true}, func() { a.printf("deleted %s\n", args[0]) })
		}),
	})
	var label, accountID, region, accessKeyID, sessionToken string
	signIn := &cobra.Command{
		Use:   "sign-in PROFILE CREDENTIAL [TOKEN]",
		Short: "Paste a subscription sign-in token, e.g. claude-subscription (stdin when TOKEN is omitted)",
		Long: `Store a subscription sign-in (Claude, ChatGPT, ...) in a credential profile.
Get the token on a computer where you are signed in (for Claude Code:
'claude setup-token'), then paste it here.`,
		Args: cobra.RangeArgs(2, 3),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if argN(args, 2) == "" {
				a.showGetURL(cmd, c, args[1])
			}
			v, err := a.readSecret(argN(args, 2))
			if err != nil {
				return err
			}
			body := client.Object{"value": v}
			setObj(body, "label", label)
			setObj(body, "accountId", accountID)
			setObj(body, "region", region)
			setObj(body, "accessKeyId", accessKeyID)
			setObj(body, "sessionToken", sessionToken)
			return profileOut("signed in " + args[1])(c.SetCredentialSignIn(cmd.Context(), args[0], args[1], body))
		}),
	}
	signIn.Flags().StringVar(&label, "label", "", "label of this account")
	signIn.Flags().StringVar(&accountID, "account-id", "", "provider account id, when the provider needs one")
	signIn.Flags().StringVar(&region, "region", "", "region, when the provider needs one")
	signIn.Flags().StringVar(&accessKeyID, "access-key-id", "", "Bedrock IAM access key id; TOKEN is then the secret access key")
	signIn.Flags().StringVar(&sessionToken, "session-token", "", "Bedrock IAM session token (optional, with --access-key-id)")
	pc.AddCommand(signIn)
	pc.AddCommand(&cobra.Command{
		Use: "set-key PROFILE CREDENTIAL [VALUE]", Short: "Set an API key, e.g. anthropic-api-key (stdin when VALUE is omitted)", Args: cobra.RangeArgs(2, 3),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if argN(args, 2) == "" {
				a.showGetURL(cmd, c, args[1])
			}
			v, err := a.readSecret(argN(args, 2))
			if err != nil {
				return err
			}
			return profileOut("set " + args[1])(c.SetCredentialKey(cmd.Context(), args[0], args[1], v))
		}),
	})
	pc.AddCommand(&cobra.Command{
		Use: "rm-key PROFILE CREDENTIAL", Short: "Remove an API key or sign-in", Args: cobra.ExactArgs(2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return profileOut("removed " + args[1])(c.DeleteCredentialKey(cmd.Context(), args[0], args[1]))
		}),
	})
	var baseURL, value string
	setAPI := &cobra.Command{
		Use: "set-api PROFILE NAME --base-url URL [--value V]", Short: "Set a custom API (sandboxes get NAME and NAME_BASE_URL)", Args: cobra.ExactArgs(2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if baseURL == "" {
				return usagef("--base-url is required")
			}
			v, err := a.readSecret(value)
			if err != nil {
				return err
			}
			return profileOut("set " + args[1])(c.SetCustomAPI(cmd.Context(), args[0], args[1], v, baseURL))
		}),
	}
	setAPI.Flags().StringVar(&baseURL, "base-url", "", "API base URL")
	setAPI.Flags().StringVar(&value, "value", "", "secret value (default: stdin)")
	pc.AddCommand(setAPI)
	pc.AddCommand(&cobra.Command{
		Use: "rm-api PROFILE NAME", Short: "Remove a custom API", Args: cobra.ExactArgs(2),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			return profileOut("removed " + args[1])(c.DeleteCustomAPI(cmd.Context(), args[0], args[1]))
		}),
	})
	cmd.AddCommand(pc)

	var days int
	usage := &cobra.Command{
		Use: "usage [ID]", Short: "Proxied AI usage: per sandbox, or per day and model for one sandbox", Args: cobra.MaximumNArgs(1),
		RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
			var u client.Object
			var err error
			if len(args) == 1 {
				id, ierr := a.sandboxID(args[0])
				if ierr != nil {
					return ierr
				}
				u, err = c.SandboxAIUsage(cmd.Context(), id, days)
			} else {
				u, err = c.AIUsage(cmd.Context(), days, "")
			}
			if err != nil {
				return err
			}
			return a.result(u, func() { prettyObject(a.Out, u, "") })
		}),
	}
	usage.Flags().IntVar(&days, "days", 30, "days to cover")
	cmd.AddCommand(usage)
	for _, block := range []bool{true, false} {
		block := block
		use, short := "block ID", "Refuse the sandbox's AI placeholders"
		if !block {
			use, short = "unblock ID", "Accept the sandbox's AI placeholders again"
		}
		cmd.AddCommand(&cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: with(func(cmd *cobra.Command, c *client.Client, args []string) error {
				id, err := a.sandboxID(args[0])
				if err != nil {
					return err
				}
				r, err := c.SetSandboxAIAccess(cmd.Context(), id, block)
				if err != nil {
					return err
				}
				return a.result(r, func() { a.printf("%s: AI access blocked=%v\n", id, block) })
			}),
		})
	}
	return cmd
}

func argN(args []string, n int) string {
	if len(args) > n {
		return args[n]
	}
	return ""
}

// resolveAccount maps a 1-based number, an id, or a label suffix to an account id.
func (a *App) resolveAccount(ctx context.Context, c *client.Client, profile, credential, ref string) (string, error) {
	l, err := c.ListCredentialAccounts(ctx, profile, credential)
	if err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(l.Accounts) {
		return l.Accounts[n-1].ID, nil
	}
	for _, acc := range l.Accounts {
		if acc.ID == ref || acc.Label == ref || strings.HasSuffix(acc.Label, ref) {
			return acc.ID, nil
		}
	}
	return "", &client.Error{Status: 404, Code: "account_not_found", Message: "no account " + ref + " on " + credential}
}

// showGetURL tells a person where to get a credential before asking for it.
func (a *App) showGetURL(cmd *cobra.Command, c *client.Client, credential string) {
	if a.jsonOut || !isTerminal(a.In) {
		return
	}
	cat, err := c.CredentialCatalog(cmd.Context())
	if err != nil {
		return
	}
	for _, cr := range cat {
		if cr.ID == credential && cr.GetURL != "" {
			a.info("get it at: %s\n", cr.GetURL)
		}
	}
}
