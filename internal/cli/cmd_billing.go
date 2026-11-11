package cli

import (
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// unsupported reports a route this server does not have yet.
func unsupported(err error, what string) error {
	if e, ok := client.AsError(err); ok && (e.Status == 404 && (e.Code == "not_found" || e.Code == "") || e.Status == 405 || e.Status == 501) {
		return &client.Error{Status: e.Status, Code: "not_supported", Message: "this server does not support " + what + " yet"}
	}
	return err
}

// billingCmd maps Boat's `billing` onto credits: CrowdVM has no paid plans.
// The "plan" is the wallet's policy (limits an administrator sets).
func (a *App) billingCmd() *cobra.Command {
	var all bool
	status := a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
		b, err := c.Billing(cmd.Context())
		if client.IsStatus(err, 404) { // older server: credits only
			var cr client.Object
			if cr, err = c.Credits(cmd.Context()); err != nil {
				return err
			}
			b = client.Object{"credits": cr}
		} else if err != nil {
			return err
		}
		var cr, lim client.Object
		if all {
			if cr == nil {
				cr, _ = c.Credits(cmd.Context())
			}
			lim, _ = c.Limits(cmd.Context())
			b["credits"], b["limits"] = cr, lim
		}
		return a.result(b, func() {
			if w := mapOf(b["wallet"]); len(w) > 0 {
				a.printf("balance:      %v credits", w["balanceCredits"])
				if w["enforced"] == false {
					a.printf(" (not enforced on this server)")
				}
				a.printf("\n")
			}
			if v, ok := b["spentThisMonth"]; ok {
				a.printf("spent (month): %v credits\n", v)
			}
			if g := mapOf(b["grace"]); g["active"] == true {
				a.printf("GRACE:        out of credits since %v; sandboxes stop at %v\n", g["since"], g["until"])
			}
			if x := mapOf(b["extraStarts"]); len(x) > 0 {
				a.printf("extra starts: %s (%v credits each)\n", onOff(x["enabled"] == true), x["creditsPerStart"])
			}
			if p := mapOf(b["policy"]); len(p) > 0 {
				a.printf("limits:\n")
				prettyObject(a.Out, p, "  ")
			}
			if cr, ok := b["credits"].(client.Object); ok && cr != nil {
				a.printf("\n")
				a.printCredits(cr)
			}
			if lim != nil {
				a.printf("\nall limits:\n")
				prettyObject(a.Out, lim, "  ")
			}
			a.info("\nCrowdVM has no paid plans: sandboxes spend credits, nodes earn them, administrators grant them.\n")
		})
	})
	cmd := &cobra.Command{
		Use:   "billing",
		Short: "Credits, limits and extra starts in scope (CrowdVM's billing: no plans, no cards)",
		Long: `Show the credit wallet in scope (personal, or --org). CrowdVM replaces
Boat's plans and payments with credits: sandboxes spend them at their size
multiplier, nodes earn them, administrators grant them and set the limits.

  crowdvm billing                   balance, month spend, grace, limits
  crowdvm billing --all             also the ledger and every limit
  crowdvm billing plan              the limits (policy) that replace plans
  crowdvm billing extra-starts on   pay credits for starts past a start limit`,
		Args: cobra.NoArgs,
		RunE: status,
	}
	cmd.Flags().BoolVar(&all, "all", false, "also show the ledger and every limit")
	cmd.Flags().BoolVar(&all, "details", false, "same as --all")
	st := &cobra.Command{Use: "status", Short: "Same as crowdvm billing", Args: cobra.NoArgs, RunE: status}
	st.Flags().BoolVar(&all, "all", false, "also show the ledger and every limit")
	var movePersonal bool
	plan := &cobra.Command{
		Use: "plan", Short: "Show the limits that replace plans (set by administrators)", Args: cobra.ArbitraryArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if movePersonal {
				// Boat: `--org acme billing plan 20 --move-personal`. There are no
				// plans here; the move itself is `org migrate`.
				org := c.Org
				if org == "" || strings.EqualFold(org, "personal") {
					l, err := c.ListOrgs(cmd.Context())
					if err != nil {
						return err
					}
					org = ""
					for _, o := range l.Orgs {
						if o.Active && o.Type == "org" {
							org = o.ID
						}
					}
				}
				if org == "" {
					return usagef("--move-personal needs an organization: --org ORG or `crowdvm org switch ORG`")
				}
				r, err := c.MovePersonalToOrg(cmd.Context(), org, nil)
				if err != nil {
					return err
				}
				return a.result(r, func() {
					a.printf("moved %s personal sandboxes into %s (no plan tiers on CrowdVM)\n", firstNonEmpty(s(mapOf(r["moved"])["sandboxes"]), "0"), org)
				})
			}
			if len(args) > 0 {
				return &client.Error{Code: "no_plans", Message: "CrowdVM has no plan tiers; ask an administrator for credits or higher limits (crowdvm admin policy)"}
			}
			b, err := c.Billing(cmd.Context())
			if client.IsStatus(err, 404) {
				b, err = c.Limits(cmd.Context())
				b = client.Object{"policy": b}
			}
			if err != nil {
				return err
			}
			out := client.Object{"plan": "credits", "policy": b["policy"]}
			return a.result(out, func() {
				a.printf("plan: credits (no tiers; an administrator sets these limits)\n")
				prettyObject(a.Out, mapOf(b["policy"]), "  ")
			})
		}),
	}
	var capCredits string
	extra := &cobra.Command{
		Use:   "extra-starts on|off",
		Short: "Pay credits for starts past a start limit (instead of being refused)",
		Args:  cobra.ExactArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if args[0] != "on" && args[0] != "off" {
				return usagef("want on or off")
			}
			body := client.Object{"enabled": args[0] == "on"}
			switch capCredits {
			case "":
			case "none":
				body["monthlyCapCredits"] = nil
			default:
				n, err := strconv.ParseFloat(capCredits, 64)
				if err != nil || n < 0 {
					return usagef("--monthly-cap wants a number of credits or none")
				}
				body["monthlyCapCredits"] = n
			}
			r, err := c.SetExtraStarts(cmd.Context(), body)
			if err != nil {
				return unsupported(err, "extra starts")
			}
			return a.result(r, func() {
				x := mapOf(r["extraStarts"])
				a.printf("extra starts: %s (%v credits each, monthly cap %v, used %v this month)\n",
					onOff(x["enabled"] == true), x["creditsPerStart"], firstNonEmpty(s(x["monthlyCapCredits"]), "none"), x["usedCreditsThisMonth"])
			})
		}),
	}
	extra.Flags().StringVar(&capCredits, "monthly-cap", "", "credits extra starts may use per month (or none)")
	extra.Flags().Bool("yes", false, "accepted for Boat compatibility (no confirmation is asked)")
	plan.Flags().BoolVar(&movePersonal, "move-personal", false, "move your personal sandboxes into the organization in scope (as org migrate)")
	cmd.AddCommand(st, plan, extra)
	return cmd
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// noticesCmd lists wallet and sandbox notices.
func (a *App) noticesCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use: "notices", Aliases: []string{"notifications"}, Short: "Notices: low credits, grace period, stopped or failing sandboxes, full disks", Args: cobra.NoArgs,
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			r, err := c.Notices(cmd.Context(), all)
			if err != nil {
				return unsupported(err, "notices")
			}
			if a.jsonOut {
				a.emit(map[string]any{"notices": nonNil(objs(r["notices"]))})
				return nil
			}
			ns := objs(r["notices"])
			if len(ns) == 0 {
				a.info("no notices\n")
				return nil
			}
			for _, n := range ns {
				on := ""
				if sb := s(n["sandboxId"]); sb != "" {
					on = " [" + sb + "]"
				}
				dis := ""
				if n["dismissedAt"] != nil {
					dis = " (dismissed)"
				}
				a.printf("%s %s %s%s: %s — %s (%s)%s\n", s(n["id"]), s(n["level"]), s(n["kind"]), on, s(n["title"]), s(n["message"]), ago(s(n["createdAt"])), dis)
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "include dismissed notices")
	var every bool
	dismiss := &cobra.Command{
		Use: "dismiss [ID]", Short: "Dismiss a notice (--all: every one)", Args: cobra.MaximumNArgs(1),
		RunE: a.withClient(func(cmd *cobra.Command, c *client.Client, args []string) error {
			if (len(args) == 0) != every {
				return usagef("give an ID or --all")
			}
			r, err := c.DismissNotice(cmd.Context(), arg0(args))
			if err != nil {
				return err
			}
			return a.result(r, func() { a.printf("dismissed\n") })
		}),
	}
	dismiss.Flags().BoolVar(&every, "all", false, "dismiss every notice")
	cmd.AddCommand(dismiss)
	return cmd
}
