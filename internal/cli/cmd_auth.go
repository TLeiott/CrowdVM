package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *App) loginCmd() *cobra.Command {
	var server, apiKey, username, email, code string
	var keyStdin, passStdin, newLink, wait, noBrowser bool
	cmd := &cobra.Command{
		Use:   "login [key]",
		Short: "Sign in (browser device login, API key, or username/password)",
		Long: `Sign in to a CrowdVM server and save the credentials in the config file.

Without a key, crowdvm prints a URL and a code and opens the browser; approve
the login there. Without a terminal (agents, scripts), 'crowdvm login --json'
prints the URL and code and exits; then 'crowdvm login --wait' (or
'crowdvm onboard --wait') finishes the login.`,
		Example: `  crowdvm login --server http://10.0.0.5:8420
  crowdvm login --email you@example.com          # a 6-digit code by email
  crowdvm login --email you@example.com --code 123456
  crowdvm login --key-stdin <<< "$CROWDVM_API_KEY"
  crowdvm login --api-key cvm_abc123...
  crowdvm login --username alice --password-stdin <<< "$PASSWORD"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("email") {
				// --email ADDR: the address may land in args (the flag's value is optional).
				if strings.TrimSpace(email) == "" && len(args) == 1 && strings.Contains(args[0], "@") {
					email, args = args[0], nil
				}
				if len(args) > 0 || apiKey != "" || keyStdin || username != "" {
					return usagef("--email cannot be combined with a key or --username")
				}
				srv := client.NormalizeBase(firstNonEmpty(server, a.server(), defaultServer))
				return a.emailLogin(cmd, srv, strings.TrimSpace(email), code)
			}
			if code != "" {
				return usagef("--code needs --email")
			}
			if len(args) == 1 {
				if apiKey != "" || keyStdin {
					return usagef("give the key once: as an argument, --api-key or --key-stdin")
				}
				apiKey = args[0]
			}
			if keyStdin {
				if apiKey != "" {
					return usagef("--key-stdin and --api-key cannot be combined")
				}
				b, err := io.ReadAll(a.In)
				if err != nil {
					return err
				}
				apiKey = strings.TrimSpace(string(b))
				if apiKey == "" {
					return usagef("--key-stdin read an empty key")
				}
			}
			srv := client.NormalizeBase(firstNonEmpty(server, a.server(), defaultServer))
			switch {
			case apiKey != "":
				return a.finishLogin(srv, apiKey, nil, "api_key")
			case username != "":
				pass, err := a.readPassword(passStdin)
				if err != nil {
					return err
				}
				res, err := a.newClient(srv, "").Login(cmd.Context(), username, pass)
				if err != nil {
					return err
				}
				return a.finishLogin(srv, res.APIKey, res.User, "password")
			}
			return a.deviceLogin(cmd, srv, newLink, wait || !a.jsonOut, noBrowser)
		},
	}
	f := cmd.Flags()
	f.StringVar(&server, "server", "", "CrowdVM server URL, e.g. http://10.0.0.5:8420")
	f.StringVar(&apiKey, "api-key", "", "sign in with this API key (cvm_...)")
	f.BoolVar(&keyStdin, "key-stdin", false, "read the API key from stdin (keeps it out of process arguments)")
	f.StringVar(&username, "username", "", "sign in with a username and password")
	f.BoolVar(&passStdin, "password-stdin", false, "read the password from stdin")
	f.BoolVar(&newLink, "new", false, "start a new browser login even if one is pending")
	f.BoolVar(&wait, "wait", false, "wait for the browser approval (default when a terminal is attached)")
	f.BoolVar(&noBrowser, "no-browser", false, "print the URL but do not open a browser")
	f.StringVar(&email, "email", "", "sign in with a 6-digit code sent to this address (asks when empty)")
	f.Lookup("email").NoOptDefVal = " "
	f.StringVar(&code, "code", "", "with --email: the code from the email (for scripts and agents)")
	f.StringVar(&a.invite, "invite", "", "with --email: invite code for a new account on a closed server")
	return cmd
}

// emailLogin signs in with a code sent by email. Without a terminal (and
// without --code) it sends the code and exits; `login --email ADDR --code N`
// finishes.
func (a *App) emailLogin(cmd *cobra.Command, srv, email, code string) error {
	ctx := cmd.Context()
	interactive := !a.jsonOut && isTerminal(a.In)
	if email == "" {
		if !interactive {
			return usagef("give the address: --email you@example.com")
		}
		var err error
		if email, err = a.ask("email: "); err != nil {
			return err
		}
	}
	c := a.newClient(srv, "")
	if code == "" {
		r, err := c.StartEmailLogin(ctx, email)
		if err != nil {
			return err
		}
		ev := map[string]any{"event": "email_code_sent", "email": email, "server": srv, "expiresInSeconds": r["expiresInSeconds"],
			"next": "crowdvm login --email " + email + " --code <code>"}
		if !interactive {
			return a.result(ev, func() {
				fmt.Fprintf(a.Err, "A code was sent to %s. Finish with: crowdvm login --email %s --code <code>\n", email, email)
			})
		}
		fmt.Fprintf(a.Err, "A 6-digit code was sent to %s (valid 10 minutes).\n", email)
		if code, err = a.ask("code: "); err != nil {
			return err
		}
	}
	res, err := c.VerifyEmailLogin(ctx, email, strings.ReplaceAll(code, " ", ""), a.invite)
	if err != nil {
		return err
	}
	return a.finishLogin(srv, res.APIKey, res.User, "email")
}

// ask reads one line from the terminal.
func (a *App) ask(prompt string) (string, error) {
	fmt.Fprint(a.Err, prompt)
	line, err := readLine(a.In)
	if err != nil && line == "" {
		return "", err
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", usagef("no answer given")
	}
	return line, nil
}

// readLine reads up to a newline without buffering past it.
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	b := make([]byte, 1)
	for sb.Len() < 4096 {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			sb.WriteByte(b[0])
		}
		if err != nil {
			return strings.TrimRight(sb.String(), "\r"), err
		}
	}
	return strings.TrimRight(sb.String(), "\r"), nil
}

func (a *App) onboardCmd() *cobra.Command {
	var server, email, purpose string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Set up this computer: sign in, say what the sandboxes are for, show credits",
		Long: `Set up this computer. It signs you in through the browser (or --email),
saves the token, asks once whether your sandboxes are for your own work or for
other people (a platform for third parties; that answer makes your default
environment safe for third parties), and shows your credit balance.

Running it again keeps a working sign-in; --email signs in again.`,
		Example: `  crowdvm onboard
  crowdvm onboard --email you@example.com
  crowdvm onboard --for others --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if purpose != "" && purpose != "own" && purpose != "others" {
				return usagef("--for wants own or others")
			}
			srv := client.NormalizeBase(firstNonEmpty(server, a.server(), defaultServer))
			signedIn := false
			if !cmd.Flags().Changed("email") && a.apiKey() != "" && srv == a.server() {
				if me, err := a.newClient(srv, a.apiKey()).Me(cmd.Context()); err == nil {
					signedIn = true
					if err := a.result(map[string]any{"event": "signed_in", "server": srv, "user": me}, func() {
						a.printf("already signed in to %s as %s\n", srv, me.Login)
					}); err != nil {
						return err
					}
				}
			}
			if !signedIn {
				var err error
				if cmd.Flags().Changed("email") {
					err = a.emailLogin(cmd, srv, strings.TrimSpace(email), "")
				} else {
					err = a.deviceLogin(cmd, srv, false, true, noBrowser)
				}
				if err != nil || a.apiKey() == "" {
					return err // the email flow without a terminal stops after sending the code
				}
			}
			return a.onboardSteps(cmd, purpose)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "CrowdVM server URL")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open a browser")
	cmd.Flags().Bool("wait", true, "wait for the browser approval")
	cmd.Flags().StringVar(&email, "email", "", "sign in with a code sent to this address")
	cmd.Flags().Lookup("email").NoOptDefVal = " "
	cmd.Flags().StringVar(&a.invite, "invite", "", "with --email: invite code for a new account on a closed server")
	cmd.Flags().StringVar(&purpose, "for", "", "what the sandboxes are for: own (your work) or others (a platform for third parties)")
	return cmd
}

// onboardSteps asks the third-party question once and shows the credits.
func (a *App) onboardSteps(cmd *cobra.Command, purpose string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	p := a.prof()
	// The server keeps the answer (GET /account-settings useCase); older
	// servers do not, so the profile remembers it too.
	settings, serr := c.AccountSettings(cmd.Context())
	answered := p.Purpose != ""
	if serr == nil {
		answered = settings["onboarded"] == true
	}
	if purpose == "" && !answered && !a.jsonOut && isTerminal(a.In) {
		fmt.Fprint(a.Err, "\nWhat are your sandboxes for?\n  1) my own work\n  2) a platform for other people (third parties run code in them)\n")
		ans, err := a.ask("answer [1/2]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(ans) {
		case "1", "own", "mine":
			purpose = "own"
		case "2", "others", "other", "platform":
			purpose = "others"
		default:
			return usagef("answer 1 or 2")
		}
	}
	out := map[string]any{"event": "onboarded", "purpose": firstNonEmpty(purpose, p.Purpose)}
	if serr == nil && purpose == "" {
		switch settings["useCase"] {
		case "own":
			out["purpose"] = "own"
		case "platform":
			out["purpose"] = "others"
		}
	}
	if purpose != "" && serr == nil {
		useCase := map[string]string{"own": "own", "others": "platform"}[purpose]
		if _, err := c.UpdateAccountSettings(cmd.Context(), client.Object{"useCase": useCase}); err != nil {
			return err
		}
	}
	if purpose != "" {
		if purpose == "others" {
			if env, err := a.defaultEnvironment(cmd, c); err == nil && env != nil && !env.SafeForThirdParties {
				if _, err := c.UpdateEnvironment(cmd.Context(), env.ID, &client.UpdateEnvironmentRequest{SafeForThirdParties: boolPtr(true)}); err != nil {
					return err
				}
				out["environment"] = env.Name
				a.info("environment %s is now safe for third parties: sandboxes get none of your secrets or logins\n", env.Name)
			}
		}
		p.Purpose = purpose
		if err := a.saveConfig(); err != nil {
			return err
		}
	}
	if cr, err := c.Credits(cmd.Context()); err == nil {
		if w, ok := cr["wallet"].(map[string]any); ok {
			out["credits"] = w["balanceCredits"]
		}
	}
	return a.result(out, func() {
		if v, ok := out["credits"]; ok {
			a.printf("credits: %v (see `crowdvm credits`)\n", v)
		}
		a.printf("Ready. Create a sandbox with: crowdvm new\n")
	})
}

func (a *App) defaultEnvironment(cmd *cobra.Command, c *client.Client) (*client.Environment, error) {
	envs, err := c.ListEnvironments(cmd.Context())
	if err != nil {
		return nil, err
	}
	for i := range envs {
		if envs[i].IsDefault {
			return &envs[i], nil
		}
	}
	for i := range envs {
		if envs[i].Name == "base" {
			return &envs[i], nil
		}
	}
	return nil, nil
}

func (a *App) readPassword(fromStdin bool) (string, error) {
	if f, ok := a.In.(*os.File); ok && !fromStdin && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.Err, "password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.Err)
		return string(b), err
	}
	b, err := io.ReadAll(io.LimitReader(a.In, 4096))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimRight(line, "\r"), nil
}

func (a *App) deviceLogin(cmd *cobra.Command, srv string, newLink, wait, noBrowser bool) error {
	ctx := cmd.Context()
	c := a.newClient(srv, "")
	p := a.prof()
	pend := p.PendingLogin
	if pend != nil {
		if exp, ok := parseTime(pend.ExpiresAt); pend.Server != srv || (ok && time.Now().After(exp)) {
			pend = nil
		}
	}
	replaced := false
	if pend == nil || newLink {
		replaced = pend != nil
		d, err := c.StartDeviceLogin(ctx, "crowdvm-cli")
		if err != nil {
			return err
		}
		url := firstNonEmpty(d.VerificationURLComplete, d.VerificationURL)
		pend = &PendingLogin{Server: srv, DeviceCode: d.DeviceCode, UserCode: d.UserCode,
			VerificationURL: url, ExpiresAt: d.ExpiresAt, Interval: d.Interval}
		p.PendingLogin = pend
		if err := a.saveConfig(); err != nil {
			return err
		}
	}
	ev := map[string]any{"event": "login", "url": pend.VerificationURL, "code": pend.UserCode, "expiresAt": pend.ExpiresAt, "server": srv}
	if replaced {
		ev["event"] = "login_replaced"
	}
	if a.jsonOut {
		a.emit(ev)
	} else {
		fmt.Fprintf(a.Err, "Open %s\nand confirm the code: %s\n", pend.VerificationURL, pend.UserCode)
	}
	if !noBrowser && !a.jsonOut && a.Getenv("CROWDVM_NO_BROWSER") == "" {
		_ = a.OpenURL(pend.VerificationURL)
	}
	if !wait {
		return nil
	}
	a.info("waiting for approval...\n")
	res, err := c.WaitDeviceLogin(ctx, &client.DeviceStart{DeviceCode: pend.DeviceCode, Interval: pend.Interval})
	if err != nil {
		if client.IsCode(err, client.CodeExpiredToken, client.CodeAccessDenied) {
			p.PendingLogin = nil
			_ = a.saveConfig()
		}
		return err
	}
	return a.finishLogin(srv, res.APIKey, res.User, "browser")
}

// finishLogin verifies key and saves it in the active profile.
func (a *App) finishLogin(srv, key string, user *client.User, method string) error {
	c := a.newClient(srv, key)
	me, err := c.Me(a.ctx)
	if err != nil {
		switch {
		case user != nil:
			me = user
		case method == "api_key" && client.IsStatus(err, 403):
			// A scoped key (e.g. sandbox-only) may not read the account;
			// it is still valid for the routes its scope allows.
			me = &client.User{Login: "(scoped key)"}
		default:
			return err
		}
	}
	p := a.prof()
	p.Server, p.APIKey, p.User, p.PendingLogin = srv, key, me.Login, nil
	if err := a.saveConfig(); err != nil {
		return err
	}
	a.cl = nil
	return a.result(map[string]any{"event": "signed_in", "server": srv, "user": me, "method": method, "profile": a.profile}, func() {
		a.printf("Signed in to %s as %s (profile %s)\n", srv, firstNonEmpty(me.Login, me.Email, me.ID), a.profile)
	})
}

func (a *App) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out and clear the local API key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := a.prof()
			p.APIKey, p.User, p.PendingLogin = "", "", nil
			if err := a.saveConfig(); err != nil {
				return err
			}
			return a.result(map[string]any{"event": "signed_out", "profile": a.profile}, func() {
				a.printf("Signed out (profile %s)\n", a.profile)
				if a.Getenv("CROWDVM_API_KEY") != "" {
					a.printf("note: CROWDVM_API_KEY is still set in the environment\n")
				}
			})
		},
	}
}

func (a *App) statusCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the server, the signed-in account and the config paths",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := a.server()
			out := map[string]any{
				"server":  srv,
				"profile": a.profile,
				"config":  filepath.Join(a.configDir(), "config.json"),
				"binary":  a.Exe,
				"version": client.Version,
			}
			account := map[string]any{"signedIn": false}
			api := map[string]any{"reachable": false}
			if srv != "" {
				c := a.newClient(srv, a.apiKey())
				c.MaxRetries = 0
				me, err := c.Me(cmd.Context())
				switch {
				case err == nil:
					api["reachable"] = true
					account["signedIn"] = true
					account["user"] = me
					if cr, err := c.Credits(cmd.Context()); err == nil {
						if w, ok := cr["wallet"].(map[string]any); ok {
							account["credits"] = w["balanceCredits"]
							account["creditsEnforced"] = w["enforced"]
						}
					}
				case client.IsStatus(err, 401) || client.IsStatus(err, 403):
					api["reachable"] = true
					account["error"] = err.Error()
				default:
					if e, ok := client.AsError(err); ok && e.Status != 0 {
						api["reachable"] = true
					}
					api["error"] = err.Error()
				}
			}
			out["api"], out["account"] = api, account
			_ = a.result(out, func() {
				a.printf("server:   %s\n", firstNonEmpty(srv, "(none; run crowdvm login --server URL)"))
				if api["reachable"] == true {
					a.printf("api:      reachable\n")
				} else {
					a.printf("api:      unreachable (%v)\n", api["error"])
				}
				if me, ok := account["user"].(*client.User); ok {
					a.printf("account:  %s", firstNonEmpty(me.Login, me.Email, me.ID))
					if me.Email != "" && me.Login != "" {
						a.printf(" <%s>", me.Email)
					}
					a.printf("\n")
				} else {
					a.printf("account:  not signed in\n")
				}
				if v, ok := account["credits"]; ok {
					a.printf("credits:  %v", v)
					if account["creditsEnforced"] == false {
						a.printf(" (not enforced on this server)")
					}
					a.printf("\n")
				}
				a.printf("profile:  %s\nconfig:   %s\nbinary:   %s\n", a.profile, out["config"], a.Exe)
			})
			if check && account["signedIn"] != true {
				return exitError{1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit 1 when this computer is not signed in")
	return cmd
}

func (a *App) dashboardCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the server's web dashboard, signed in (one-time 15-minute link)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := a.server()
			if srv == "" {
				return &client.Error{Code: "not_signed_in", Message: "no server configured"}
			}
			u, signedIn := srv+"/", false
			if key := a.apiKey(); key != "" {
				// A one-time link signs the browser in; older servers lack it.
				if r, err := a.newClient(srv, key).LoginLink(cmd.Context()); err == nil && s(r["url"]) != "" {
					u, signedIn = s(r["url"]), true
				}
			}
			if !noOpen && !a.jsonOut {
				_ = a.OpenURL(u)
			}
			return a.result(map[string]any{"url": u, "loginLink": signedIn}, func() {
				a.printf("%s\n", u)
				if signedIn {
					a.info("the link signs you in once and expires in 15 minutes\n")
				}
			})
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL only")
	return cmd
}

func (a *App) limitsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "Show start limits, active sandboxes and credits",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			l, err := c.Limits(cmd.Context())
			if err != nil {
				return err
			}
			return a.result(l, func() {
				if all {
					prettyObject(a.Out, l, "")
					return
				}
				a.printf("active sandboxes: %v/%v", l["activeSandboxes"], l["maxActiveSandboxes"])
				if st, ok := l["starts"].(map[string]any); ok {
					for _, w := range []string{"minute", "hour", "day"} {
						if m, ok := st[w].(map[string]any); ok {
							a.printf(" · starts left this %s: %v", w, m["remaining"])
						}
					}
				}
				if b, ok := l["creditBalance"]; ok {
					a.printf(" · credits: %v", b)
				}
				a.printf("\n")
				if mt, ok := l["machineTypes"].([]any); ok {
					var ok2, blocked []string
					for _, t := range mt {
						m, _ := t.(map[string]any)
						if m["available"] == true {
							ok2 = append(ok2, fmt.Sprint(m["type"]))
						} else {
							blocked = append(blocked, fmt.Sprintf("  %v: %v", m["type"], m["reason"]))
						}
					}
					a.printf("types: %s\n", strings.Join(ok2, ", "))
					for _, b := range blocked {
						a.printf("%s\n", b)
					}
				}
				if r, ok := l["startBlockedReason"].(string); ok && r != "" {
					a.printf("blocked: %s\n", r)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show every limit")
	cmd.Flags().BoolVar(&all, "details", false, "same as --all")
	return cmd
}

func (a *App) usageCmd() *cobra.Command {
	var since, until string
	cmd := &cobra.Command{
		Use:   "usage ID",
		Short: "Show the machine time and credits one sandbox used",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(args[0])
			if err != nil {
				return err
			}
			u, err := c.SandboxUsage(cmd.Context(), id, since, until)
			if err != nil {
				return err
			}
			return a.result(u, func() {
				a.printf("%s (%s, ×%g): %s billable", id, u.SandboxType, u.BillingMultiplier, shortDur(time.Duration(u.Seconds)*time.Second))
				if u.Credits != nil {
					a.printf(", %.2f credits", *u.Credits)
				} else if u.Dollars != 0 {
					a.printf(", %.4f credits", u.Dollars)
				}
				a.printf("\nwindow: %s → %s", u.Since, u.Until)
				if u.Running {
					a.printf(" (still running)")
				}
				a.printf("\n")
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "count from this time (ISO 8601 or Unix seconds)")
	cmd.Flags().StringVar(&until, "until", "", "count up to this time (ISO 8601 or Unix seconds)")
	return cmd
}

func (a *App) creditsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "credits",
		Aliases: []string{"wallet"},
		Short:   "Show your credit balance (CrowdVM's replacement for billing)",
		Long: `Show the credit balance in scope (personal, or --org). Running sandboxes
spend credits at their size multiplier; nodes earn credits for the VM time
they host. Admins grant credits on the server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			cr, err := c.Credits(cmd.Context())
			if client.IsStatus(err, 404) {
				cr, err = c.Limits(cmd.Context())
			}
			if err != nil {
				return err
			}
			return a.result(cr, func() { a.printCredits(cr) })
		},
	}
	var from, to string
	var amount float64
	transfer := &cobra.Command{
		Use: "transfer --to WALLET --amount N", Short: "Move credits to another wallet (user or org)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if to == "" || amount <= 0 {
				return usagef("give --to and a positive --amount")
			}
			r, err := c.CreditTransfer(cmd.Context(), from, to, amount)
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("moved %g credits to %s\n", amount, to) })
		},
	}
	transfer.Flags().StringVar(&from, "from", "", "source wallet (default: the one in scope: --org, CROWDVM_ORG or the active org; \"personal\" for your own)")
	transfer.Flags().StringVar(&to, "to", "", "target wallet: user or org id/name")
	transfer.Flags().Float64Var(&amount, "amount", 0, "credits to move")
	cmd.AddCommand(transfer)
	return cmd
}

func (a *App) printCredits(cr client.Object) {
	w, ok := cr["wallet"].(map[string]any)
	if !ok {
		prettyObject(a.Out, cr, "")
		return
	}
	a.printf("balance: %v credits", w["balanceCredits"])
	if w["enforced"] == false {
		a.printf(" (not enforced on this server)")
	}
	a.printf("\nearned:  %v (hosting VMs on your nodes)\nspent:   %v\n", cr["earnedCredits"], cr["spentCredits"])
	if rates, ok := cr["rates"].(map[string]any); ok {
		a.printf("rate:    %v credit per default-size hour\n", rates["creditsPerDefaultHour"])
		for _, k := range sortedKeys(mapOf(rates["multipliers"])) {
			a.printf("  %s: %v×\n", k, mapOf(rates["multipliers"])[k])
		}
	}
	if l, ok := cr["ledger"].([]any); ok && len(l) > 0 {
		var rows [][]string
		for i, e := range l {
			if i == 10 {
				break
			}
			m, _ := e.(map[string]any)
			rows = append(rows, []string{s(m["createdAt"]), s(m["delta"]), s(m["kind"]), firstNonEmpty(s(m["sandboxId"]), s(m["nodeId"])), s(m["reason"])})
		}
		a.printf("\nrecent:\n")
		table(a.Out, "WHEN\tDELTA\tKIND\tON\tREASON", rows)
	}
}

// clientAndID returns the client and a resolved sandbox id.
func (a *App) clientAndID(arg string) (*client.Client, string, error) {
	id, err := a.sandboxID(arg)
	if err != nil {
		return nil, "", err
	}
	c, err := a.client()
	return c, id, err
}
