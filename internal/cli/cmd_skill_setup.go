package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// setupAgents are the coding agents `skill setup` names, in this order. rule is
// the global instruction file (relative to $HOME) that gets the CrowdVM rule;
// Cursor keeps its user rules in its settings, so it only gets the skill.
var setupAgents = []struct{ id, name, rule string }{
	{"claude", "Claude Code", ".claude/CLAUDE.md"},
	{"codex", "Codex", ".codex/AGENTS.md"},
	{"opencode", "OpenCode", ".config/opencode/AGENTS.md"},
	{"cursor", "Cursor", ""},
}

// The rule sits between these markers so a later setup replaces it in place.
const (
	ruleBegin = "<!-- crowdvm:begin (managed by crowdvm skill setup) -->"
	ruleEnd   = "<!-- crowdvm:end -->"
)

// stdinIsTerminal is replaced in tests.
var stdinIsTerminal = func(a *App) bool { return isTerminal(a.In) }

func (a *App) skillSetupCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Teach the coding agents on this machine to use CrowdVM (skill + global rule)",
		Long: `Install the crowdvm skill into every coding agent found in your home folder
and add the CrowdVM rule (crowdvm agent-instructions) to its global instructions:
~/.claude/CLAUDE.md (Claude Code), ~/.codex/AGENTS.md (Codex) and
~/.config/opencode/AGENTS.md (OpenCode). Cursor gets the skill only.
Running it again updates both in place.

Asks first on a terminal. Without one (or with CI set) it only prints the
command; --yes or CROWDVM_AGENT_SETUP=1 sets up without asking,
CROWDVM_AGENT_SETUP=0 skips it.

  crowdvm skill setup
  crowdvm skill setup --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return a.skillSetup(yes) },
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "set up without asking")
	return cmd
}

func (a *App) skillSetup(yes bool) error {
	home := a.homeDir()
	var names, rules []string
	for _, ag := range setupAgents {
		if exists(filepath.Join(home, skillAgentHomes[ag.id])) {
			names = append(names, ag.name)
			if ag.rule != "" {
				rules = append(rules, filepath.Join(home, filepath.FromSlash(ag.rule)))
			}
		}
	}
	const later = "crowdvm skill setup --yes"
	if len(names) == 0 {
		return a.result(map[string]any{"event": "no_agents", "next": "crowdvm skill setup"}, func() {
			a.printf("No coding agent found (Claude Code, Codex, OpenCode, Cursor). After installing one, run: crowdvm skill setup\n")
		})
	}
	switch a.Getenv("CROWDVM_AGENT_SETUP") {
	case "1":
		yes = true
	case "0":
		if !yes {
			return nil
		}
	}
	interactive := !yes && !a.jsonOut && a.Getenv("CI") == "" && stdinIsTerminal(a)
	if !yes && !interactive {
		return a.result(map[string]any{"event": "agents_found", "agents": names, "files": rules, "next": later}, func() {
			a.printSetupIntro(names, rules)
			a.printf("Recommended: %s\n", later)
		})
	}
	if interactive {
		a.printSetupIntro(names, rules)
		a.printf("Set up %s to use CrowdVM? [Y/n] ", joinAnd(names))
		ans, err := readLine(a.In)
		if ans = strings.ToLower(strings.TrimSpace(ans)); (err != nil && ans == "") || (ans != "" && ans != "y" && ans != "yes") {
			a.printf("Skipped. Later: %s\n", later)
			return nil
		}
	}
	dirs, err := a.skillTargets(nil, "", false)
	if err != nil {
		return err
	}
	if err := a.installSkills(dirs); err != nil {
		return err
	}
	for _, p := range rules {
		st, err := writeRule(p)
		if err != nil {
			return err
		}
		a.result(map[string]any{"event": "rule", "path": p, "status": st}, func() {
			a.printf("%s: %s\n", map[string]string{
				"added": "added the CrowdVM rule", "updated": "updated the CrowdVM rule",
				"unchanged": "CrowdVM rule up to date", "present": "already has a CrowdVM rule, left as is",
			}[st], p)
		})
	}
	return a.result(map[string]any{"event": "done", "agents": names}, func() {
		a.printf("Restart running agent sessions to pick up the change.\n")
	})
}

func (a *App) printSetupIntro(names, rules []string) {
	a.printf("Coding agents found: %s.\n", strings.Join(names, ", "))
	if len(rules) == 0 {
		a.printf("They can use CrowdVM sandboxes once they have the crowdvm skill.\n")
		return
	}
	home := a.homeDir()
	short := make([]string, len(rules))
	for i, p := range rules {
		if r, err := filepath.Rel(home, p); err == nil {
			p = filepath.Join("~", r)
		}
		short[i] = p
	}
	a.printf("They can use CrowdVM sandboxes once they have the crowdvm skill\nand the CrowdVM rule (%s).\n", strings.Join(short, ", "))
}

// writeRule puts the agent-instructions rule between the markers in path:
// "added" (appended), "updated" (replaced in place), "unchanged", or "present"
// when an unmarked copy (pasted by hand) is already there and is left alone.
func writeRule(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	old := string(b)
	block := ruleBegin + "\n" + strings.TrimSpace(agentGuide) + "\n" + ruleEnd
	var next, status string
	i := strings.Index(old, ruleBegin)
	j := strings.Index(old, ruleEnd)
	switch {
	case i >= 0 && j > i:
		next, status = old[:i]+block+old[j+len(ruleEnd):], "updated"
		if next == old {
			return "unchanged", nil
		}
	case strings.Contains(old, ruleHeading()):
		return "present", nil
	default:
		next, status = old, "added"
		if next != "" {
			next = strings.TrimRight(next, "\n") + "\n\n"
		}
		next += block + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return status, os.WriteFile(path, []byte(next), 0o644)
}

// ruleHeading is the first line of the rule, used to spot a hand-pasted copy.
func ruleHeading() string {
	h, _, _ := strings.Cut(strings.TrimSpace(agentGuide), "\n")
	return h
}

// joinAnd joins names as "A", "A and B", "A, B and C".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
