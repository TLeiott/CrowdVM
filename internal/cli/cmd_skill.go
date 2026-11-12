package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crowdvm/crowdvm/skills"
	"github.com/spf13/cobra"
)

// skillAgents maps an agent to its user-level skills folder, relative to $HOME.
// Every one of them loads <dir>/<skill>/SKILL.md.
var skillAgents = map[string]string{
	"claude":   ".claude/skills",          // Claude Code
	"codex":    ".codex/skills",           // Codex CLI
	"opencode": ".config/opencode/skills", // OpenCode
	"cursor":   ".cursor/skills",          // Cursor
	"agents":   ".agents/skills",          // the shared folder of `npx skills` (Codex, Kimi and others read it too)
}

// skillProjectDirs are the same folders inside a project (--project).
var skillProjectDirs = map[string]string{
	"claude": ".claude/skills", "codex": ".codex/skills", "opencode": ".opencode/skills", "cursor": ".cursor/skills",
	"agents": ".agents/skills",
}

// skillAgentHomes tells which agents are installed: their config folder exists.
var skillAgentHomes = map[string]string{"claude": ".claude", "codex": ".codex", "opencode": ".config/opencode", "cursor": ".cursor",
	"agents": ".agents"}

func (a *App) homeDir() string {
	for _, k := range []string{"HOME", "USERPROFILE"} {
		if v := a.Getenv(k); v != "" {
			return v
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

func (a *App) skillCmd() *cobra.Command {
	var agents []string
	var project string
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install the crowdvm skill for Claude Code, Codex and OpenCode on this machine",
		Long: `The crowdvm skill teaches a coding agent on your computer when and how to use
sandboxes (create, exec, push, host, snapshots, stop). It is the same skill the
integrated agents inside a sandbox already have.

  crowdvm skill install                  # every agent found in your home folder
  crowdvm skill install --agent claude   # one agent (claude, codex, opencode, cursor, agents)
  crowdvm skill install --project .      # into this project (.claude/skills, ...)
  crowdvm skill path                     # where it is (or would be) installed
  crowdvm skill setup                    # the skill plus the rule in each agent's global instructions

Also give the agent the docs (<server>/llms.txt, <server>/mcp) and the AGENTS.md
rule from crowdvm agent-instructions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	targets := func(all bool) ([]string, error) { return a.skillTargets(agents, project, all) }
	install := &cobra.Command{
		Use:   "install",
		Short: "Write the crowdvm skill into the agents' skills folders (replaces an older copy)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := targets(false)
			if err != nil {
				return err
			}
			return a.installSkills(dirs)
		},
	}
	path := &cobra.Command{
		Use:   "path",
		Short: "Print the skill folder of each agent and whether the crowdvm skill is installed there",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := targets(true)
			if err != nil {
				return err
			}
			for _, d := range dirs {
				p := filepath.Join(d, "crowdvm")
				ok := exists(filepath.Join(p, "SKILL.md"))
				a.result(map[string]any{"path": p, "installed": ok}, func() {
					a.printf("%s\t%s\n", p, map[bool]string{true: "installed", false: "not installed"}[ok])
				})
			}
			return nil
		},
	}
	for _, c := range []*cobra.Command{install, path} {
		c.Flags().StringArrayVar(&agents, "agent", nil, "claude, codex, opencode, cursor or agents (repeatable; default: the agents found)")
		c.Flags().StringVar(&project, "project", "", "install into this project folder instead of your home folder")
	}
	cmd.AddCommand(install, path, a.skillSetupCmd())
	return cmd
}

// skillTargets returns the skills folders to use: the named agents, else the
// agents found (all of them with all), else claude and agents.
func (a *App) skillTargets(agents []string, project string, all bool) ([]string, error) {
	base := a.homeDir()
	dirs := skillAgents
	if project != "" {
		abs, err := filepath.Abs(project)
		if err != nil {
			return nil, err
		}
		base, dirs = abs, skillProjectDirs
	}
	names := agents
	if len(names) == 0 {
		for n := range dirs {
			if all || project != "" || exists(filepath.Join(base, skillAgentHomes[n])) {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			names = []string{"claude", "agents"}
		}
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		d, ok := dirs[n]
		if !ok {
			return nil, usagef("unknown agent %q (want claude, codex, opencode, cursor or agents)", n)
		}
		out = append(out, filepath.Join(base, filepath.FromSlash(d)))
	}
	return out, nil
}

// installSkills writes every embedded skill into each skills folder.
func (a *App) installSkills(dirs []string) error {
	for _, d := range dirs {
		for _, sk := range skills.Names() {
			dest := filepath.Join(d, sk)
			n, err := writeSkill(sk, dest)
			if err != nil {
				return err
			}
			a.result(map[string]any{"event": "installed", "skill": sk, "path": dest, "files": n},
				func() { a.printf("installed %s skill: %s\n", sk, dest) })
		}
	}
	return nil
}

// writeSkill copies skill name from the embedded skills to dest, replacing an older copy.
func writeSkill(name, dest string) (int, error) {
	if err := os.RemoveAll(dest); err != nil {
		return 0, err
	}
	n := 0
	for p, b := range skills.Files() {
		rel, ok := strings.CutPrefix(p, name+"/")
		if !ok {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return n, err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
