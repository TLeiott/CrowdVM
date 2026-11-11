package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// conversation resolves --convo values ("current" = this shell's).
func (a *App) conversation(id, v string) string {
	if v == "current" {
		return a.myConversation(id)
	}
	return v
}

// myConversation is this shell's (or, without the shell function, this
// profile's) current conversation on sandbox id.
func (a *App) myConversation(id string) string {
	if a.shellID() != "" {
		if st := a.shell(false); st != nil {
			return st.Conversations[id]
		}
		return ""
	}
	return a.prof().Conversations[id]
}

func (a *App) setConversation(id, conv string) {
	if conv == "" {
		return
	}
	set := func(m *map[string]string) {
		if *m == nil {
			*m = map[string]string{}
		}
		(*m)[id] = conv
	}
	set(&a.prof().Conversations)
	if st := a.shell(true); st != nil {
		set(&st.Conversations)
	}
	_ = a.saveConfig()
}

func (a *App) promptCmd() *cobra.Command {
	var req client.PromptRequest
	var fast, noWait bool
	var resume, localMode string
	var attach []string
	cmd := &cobra.Command{
		Use:   "prompt [ID] MESSAGE",
		Short: "Send a prompt to the coding agent inside a sandbox and stream its work",
		Long: `Send a natural-language prompt to an integrated agent (Claude Code, Codex,
OpenCode, ...) running inside the sandbox. Without --new or --resume the prompt
continues this profile's current conversation on that sandbox. Prints the
agent's events until the turn ends (JSON Lines with --json).

--local-login sends this machine's own login with the prompt, for that turn
only: a Claude or ChatGPT subscription (access token only, never the refresh
token) or a vendor API key, read fresh at every call. It needs --provider.
CROWDVM_LOCAL_LOGIN=1 (or subscription, api-key) turns it on for a session.`,
		Example: `  crowdvm prompt bx_f7k2q9hd "Fix the failing tests"
  crowdvm prompt bx_f7k2q9hd --provider codex --model gpt-6.1-sol --new "Refactor auth"
  crowdvm prompt bx_f7k2q9hd --provider codex --model gpt-6-luna --local-login "Fix the build"
  crowdvm prompt --attach ./trace.log "Why does this crash?"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			idArg, msg := "", args[0]
			if len(args) == 2 {
				idArg, msg = args[0], args[1]
			}
			c, id, err := a.clientAndID(idArg)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if cmd.Flags().Changed("fast") {
				req.Fast = &fast
			}
			switch {
			case req.New && resume != "":
				return usagef("--new and --resume cannot be combined")
			case resume != "":
				req.ConversationID = a.conversation(id, resume)
			case !req.New:
				req.ConversationID = a.myConversation(id)
			}
			if err := checkAttachments(attach); err != nil {
				return err
			}
			if !cmd.Flags().Changed("local-login") {
				localMode = a.Getenv("CROWDVM_LOCAL_LOGIN")
			}
			mode, err := parseLocalLoginMode(localMode)
			if err != nil {
				return err
			}
			var login *localLogin
			if mode != "" {
				if req.Provider == "" {
					return usagef("--local-login (or CROWDVM_LOCAL_LOGIN) needs --provider: claude, codex, opencode or pi")
				}
				if login, err = readLocalLogin(localProvider(req.Provider), mode, a.Getenv, time.Now()); err != nil {
					return err
				}
				req.Credentials = login.Creds
			}
			if len(attach) > 0 {
				for _, f := range attach {
					fh, err := os.Open(f)
					if err != nil {
						return err
					}
					dst := "/home/user/attachments/" + filepath.Base(f)
					err = c.Upload(ctx, id, dst, fh)
					fh.Close()
					if err != nil {
						return err
					}
					req.Attachments = append(req.Attachments, dst) // the guest names them in the prompt
				}
			}
			req.Prompt = msg
			// Remember what the conversation already holds so we print only new events.
			seen := map[string]bool{}
			if req.ConversationID != "" && !noWait {
				_ = a.eachEvent(ctx, c, id, []string{req.ConversationID}, false, func(e client.Event) { seen[e.ID] = true })
			}
			res, err := c.Prompt(ctx, id, &req)
			if err != nil {
				return err
			}
			conv := firstNonEmpty(res.ConversationID, req.ConversationID)
			if res.PromptRun != nil {
				conv = firstNonEmpty(conv, res.PromptRun.ConversationID)
			}
			a.setConversation(id, conv)
			if a.jsonOut {
				ev := map[string]any{"event": "queued", "id": id, "promptId": res.PromptID, "conversationId": conv,
					"new": req.New || req.ConversationID == "", "provider": res.Provider, "model": res.Model}
				if login != nil {
					ev["localLogin"] = login.info(time.Now())
				}
				a.emit(ev)
			} else {
				a.printf("queued: %s\n", firstNonEmpty(res.PromptID, res.ID))
				if login != nil {
					a.printf("local login: %s\n", login.String(time.Now()))
				}
				suffix := ""
				if req.New || req.ConversationID == "" {
					suffix = " (new)"
				}
				a.printf("conversation: %s%s\n", conv, suffix)
			}
			if noWait {
				return nil
			}
			return a.followPrompt(ctx, c, id, firstNonEmpty(res.PromptID, res.ID), conv, seen)
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Provider, "provider", "", "harness: claude, codex, opencode, pi, ... (default: server default)")
	f.StringVar(&req.Model, "model", "", "provider-specific model")
	f.StringVar(&req.ReasoningEffort, "reasoning-effort", "", "thinking level: none, low, medium, high, xhigh, max")
	f.BoolVar(&fast, "fast", false, "fast mode (only fast-capable models)")
	f.BoolVar(&req.New, "new", false, "start a new conversation")
	f.StringVar(&resume, "resume", "", "continue this conversation id (or current)")
	f.StringArrayVar(&attach, "attach", nil, "attach a file (saved under ~/attachments; repeatable)")
	f.StringArrayVar(&attach, "image", nil, "alias of --attach")
	f.BoolVar(&noWait, "no-wait", false, "return once the prompt is queued")
	f.StringVar(&localMode, "local-login", "", "use this machine's own login for this turn: auto (subscription, then API key), subscription or api-key; needs --provider (env CROWDVM_LOCAL_LOGIN)")
	f.Lookup("local-login").NoOptDefVal = "auto"
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		c.Parent().HelpFunc()(c, args)
		a.printCatalog()
	})
	return cmd
}

// printCatalog appends the server's live provider/model catalog to `prompt --help`.
func (a *App) printCatalog() {
	srv, key := a.server(), a.apiKey()
	if srv == "" || key == "" {
		fmt.Fprintf(a.Out, "\nSign in (crowdvm login) to list the providers and models of your server here.\n")
		return
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c := a.newClient(srv, key)
	c.MaxRetries = 0
	ps, err := c.ProviderModels(ctx)
	if err != nil {
		fmt.Fprintf(a.Out, "\n(could not load the provider catalog from %s: %v)\n", srv, err)
		return
	}
	fmt.Fprintf(a.Out, "\nProviders and models on %s (--provider, --model, --reasoning-effort):\n", srv)
	var fast []string
	for _, p := range ps {
		state := ""
		if p.Configured != nil && !*p.Configured {
			state = "  [no credentials: run crowdvm ai profiles / ai profile set-key]"
		}
		fmt.Fprintf(a.Out, "  %s  (%s)%s\n", p.Provider, p.Name, state)
		for _, m := range p.Models {
			line := "    " + m.ID
			if m.ID == p.DefaultModel {
				line += " (default)"
			}
			if len(m.ReasoningEfforts) > 0 {
				eff := strings.Join(m.ReasoningEfforts, ", ")
				if m.DefaultReasoningEffort != "" {
					eff = strings.Replace(eff, m.DefaultReasoningEffort, m.DefaultReasoningEffort+"*", 1)
				}
				line += "  effort: " + eff
			}
			fmt.Fprintln(a.Out, line)
			if m.FastMode {
				fast = append(fast, p.Provider+"/"+m.ID)
			}
		}
	}
	if len(fast) > 0 {
		fmt.Fprintf(a.Out, "\nFast mode (--fast):\n  %s\n", strings.Join(fast, "\n  "))
	}
}

// followPrompt prints new events of the conversation until the prompt is done.
func (a *App) followPrompt(ctx context.Context, c *client.Client, id, promptID, conv string, seen map[string]bool) error {
	var convs []string
	if conv != "" {
		convs = []string{conv}
	}
	for {
		err := a.eachEvent(ctx, c, id, convs, false, func(e client.Event) {
			if seen[e.ID] && e.ID != "" {
				return
			}
			seen[e.ID] = true
			a.printEvent(e)
		})
		if err != nil {
			return err
		}
		if promptID != "" {
			run, err := c.PromptRunStatus(ctx, id, promptID)
			if err == nil && run.Done {
				// One more pass for events written just before done.
				_ = a.eachEvent(ctx, c, id, convs, false, func(e client.Event) {
					if !seen[e.ID] {
						seen[e.ID] = true
						a.printEvent(e)
					}
				})
				if a.jsonOut {
					a.emit(map[string]any{"event": "done", "id": id, "promptId": promptID, "status": run.Status, "conversationId": conv})
				}
				switch run.Status {
				case "failed":
					return exitError{1}
				case "interrupted":
					return exitError{130}
				}
				return nil
			}
		}
		if err := sleepCtx(ctx, a.Poll); err != nil {
			return err
		}
	}
}

// eachEvent pages through events (oldest first) and calls fn for each.
func (a *App) eachEvent(ctx context.Context, c *client.Client, id string, convs []string, _ bool, fn func(client.Event)) error {
	o := &client.EventsOptions{Sort: "asc", Limit: 100, Conversations: convs}
	for i := 0; i < 1000; i++ {
		page, err := c.Events(ctx, id, o)
		if err != nil {
			return err
		}
		for _, e := range page.Events {
			fn(e)
		}
		if !page.PageInfo.HasMore || page.PageInfo.NextCursor == "" || page.PageInfo.NextCursor == o.Cursor {
			return nil
		}
		o.Cursor = page.PageInfo.NextCursor
	}
	return nil
}

func (a *App) printEvent(e client.Event) {
	if a.jsonOut {
		a.emit(e)
		return
	}
	d := e.Data
	s := func(k string) string {
		if v, ok := d[k].(string); ok {
			return v
		}
		return ""
	}
	switch e.Type {
	case "prompt":
		a.printf("> %s\n", s("prompt"))
	case "response":
		if b, _ := d["is_streaming"].(bool); b {
			return
		}
		if txt := s("content"); txt != "" {
			a.printf("%s\n", txt)
		}
		if tools, ok := d["tools"].([]any); ok {
			for _, t := range tools {
				if m, ok := t.(map[string]any); ok {
					use := mapOf(m["use"]) // {use: {name, input}, result: {...}}
					str := func(v any) string { x, _ := v.(string); return x }
					name := firstNonEmpty(str(use["name"]), str(m["name"]), str(m["type"]), "tool")
					if res := mapOf(m["result"]); res["is_error"] == true {
						name += " (error)"
					}
					a.printf("  [tool] %s\n", name)
				}
			}
		}
	case "steer":
		a.printf("↪ %s (%s)\n", s("message"), s("mode"))
	case "error":
		a.printf("error: %s\n", firstNonEmpty(s("message"), s("error"), fmt.Sprint(d)))
	case "completion":
		a.printf("— turn finished\n")
	default:
		if txt := firstNonEmpty(s("content"), s("message")); txt != "" {
			a.printf("[%s] %s\n", e.Type, txt)
		}
	}
}

func (a *App) eventsCmd() *cobra.Command {
	var follow bool
	var convos []string
	cmd := &cobra.Command{
		Use:   "events [ID]",
		Short: "Read the agent's prompts and responses on a sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(arg0(args))
			if err != nil {
				return err
			}
			var cs []string
			for _, v := range convos {
				if r := a.conversation(id, v); r != "" {
					cs = append(cs, r)
				}
			}
			seen := map[string]bool{}
			print := func(e client.Event) {
				if e.ID != "" && seen[e.ID] {
					return
				}
				seen[e.ID] = true
				a.printEvent(e)
			}
			for {
				if err := a.eachEvent(cmd.Context(), c, id, cs, false, print); err != nil {
					return err
				}
				if !follow {
					return nil
				}
				if err := sleepCtx(cmd.Context(), 2*a.Poll); err != nil {
					return nil
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events until interrupted")
	cmd.Flags().StringArrayVar(&convos, "convo", nil, "only this conversation id (or current); repeatable")
	cmd.Flags().StringArrayVar(&convos, "conversation", nil, "alias of --convo")
	return cmd
}

func (a *App) conversationsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "conversations [ID]",
		Aliases: []string{"convos"},
		Short:   "List the agent conversations of a sandbox, newest first",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(arg0(args))
			if err != nil {
				return err
			}
			cs, err := c.Conversations(cmd.Context(), id)
			if err != nil {
				return err
			}
			mine := a.myConversation(id)
			if a.jsonOut {
				for _, cv := range cs {
					a.emit(map[string]any{"id": cv.ID, "createdAt": cv.CreatedAt, "lastPromptAt": cv.LastPromptAt,
						"prompts": cv.Prompts, "running": cv.Running, "lastHarness": cv.LastHarness, "lastModel": cv.LastModel,
						"lastPromptPreview": cv.LastPromptPreview, "current": cv.Current, "thisShell": cv.ID == mine})
				}
				return nil
			}
			var rows [][]string
			for _, cv := range cs {
				run := "no"
				if cv.Running {
					run = "yes"
				}
				hm := cv.LastHarness
				if cv.LastModel != "" {
					hm += "/" + cv.LastModel
				}
				last := truncate(cv.LastPromptPreview, 50)
				var marks []string
				if cv.Current {
					marks = append(marks, "sandbox current")
				}
				if cv.ID == mine {
					marks = append(marks, "this shell")
				}
				if len(marks) > 0 {
					last += "  [" + strings.Join(marks, ", ") + "]"
				}
				rows = append(rows, []string{cv.ID, ago(cv.LastPromptAt), strconv.Itoa(cv.Prompts), run, hm, last})
			}
			table(a.Out, "ID\tWHEN\tPROMPTS\tRUNNING\tHARNESS/MODEL\tLAST PROMPT", rows)
			return nil
		},
	}
}

func (a *App) steerCmd() *cobra.Command {
	var convo string
	cmd := &cobra.Command{
		Use:   "steer [ID] MESSAGE",
		Short: "Add a message to the agent's running turn without stopping it",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			idArg, msg := "", args[0]
			if len(args) == 2 {
				idArg, msg = args[0], args[1]
			}
			c, id, err := a.clientAndID(idArg)
			if err != nil {
				return err
			}
			conv := a.conversation(id, firstNonEmpty(convo, "current"))
			res, err := c.Steer(cmd.Context(), id, msg, conv)
			if err != nil {
				return err
			}
			return a.result(res, func() {
				a.printf("steered %s (mode %s)\n", firstNonEmpty(res.ConversationID, conv), firstNonEmpty(res.Mode, "native"))
			})
		},
	}
	cmd.Flags().StringVar(&convo, "convo", "", "conversation id (or current; default: this profile's)")
	cmd.Flags().StringVar(&convo, "conversation", "", "alias of --convo")
	return cmd
}

func (a *App) interruptCmd() *cobra.Command {
	var convo string
	cmd := &cobra.Command{
		Use:   "interrupt [ID]",
		Short: "Stop running agent work (all conversations, or --convo)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, id, err := a.clientAndID(arg0(args))
			if err != nil {
				return err
			}
			res, err := c.Interrupt(cmd.Context(), id, a.conversation(id, convo))
			if err != nil {
				return err
			}
			return a.result(res, func() { a.printf("%s: %s\n", id, firstNonEmpty(res.Status, "interrupted")) })
		},
	}
	cmd.Flags().StringVar(&convo, "convo", "", "only this conversation id (or current)")
	cmd.Flags().StringVar(&convo, "conversation", "", "alias of --convo")
	return cmd
}

// Attachment limits (Boat): 12 files, 25 MB each, 64 MB in total.
const (
	maxAttachments    = 12
	maxAttachmentSize = 25 << 20
	maxAttachTotal    = 64 << 20
)

func checkAttachments(files []string) error {
	if len(files) > maxAttachments {
		return usagef("at most %d attachments per prompt (got %d)", maxAttachments, len(files))
	}
	var total int64
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			return err
		}
		if st.IsDir() {
			return usagef("--attach %s is a directory; attach files (or push the directory with crowdvm push)", f)
		}
		if st.Size() > maxAttachmentSize {
			return usagef("--attach %s is %s; the limit is 25 MB per file", f, humanBytes(st.Size()))
		}
		total += st.Size()
	}
	if total > maxAttachTotal {
		return usagef("attachments total %s; the limit is 64 MB per prompt", humanBytes(total))
	}
	return nil
}
