package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupHome(t *testing.T, e *env, agentDirs ...string) string {
	t.Helper()
	home := t.TempDir()
	e.vars["HOME"] = home
	for _, d := range agentDirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func withTTY(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func(*App) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })
}

func TestSkillSetupYesWritesSkillAndRuleOnce(t *testing.T) {
	e := newEnv(t)
	home := setupHome(t, e, ".claude", ".codex")
	claudeMD := filepath.Join(home, ".claude/CLAUDE.md")
	if err := os.WriteFile(claudeMD, []byte("# Mine\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.mustRun("skill", "setup", "--yes", "--json=false")
	for _, want := range []string{"added the CrowdVM rule: " + claudeMD, "added the CrowdVM rule: " + filepath.Join(home, ".codex/AGENTS.md"), "Restart running agent sessions"} {
		if !strings.Contains(r.out, want) {
			t.Fatalf("missing %q in:\n%s", want, r.out)
		}
	}
	for _, p := range []string{".claude/skills/crowdvm/SKILL.md", ".codex/skills/crowdvm/SKILL.md"} {
		if !exists(filepath.Join(home, p)) {
			t.Fatalf("%s not installed", p)
		}
	}
	if exists(filepath.Join(home, ".config/opencode")) {
		t.Fatal("touched an agent that is not installed")
	}
	got := readFile(t, claudeMD)
	if !strings.HasPrefix(got, "# Mine\nkeep me\n\n"+ruleBegin) || !strings.Contains(got, ruleHeading()) {
		t.Fatalf("CLAUDE.md:\n%s", got)
	}
	// Again: no second copy; an edited block is replaced in place.
	r = e.mustRun("skill", "setup", "--yes", "--json=false")
	if !strings.Contains(r.out, "CrowdVM rule up to date: "+claudeMD) || readFile(t, claudeMD) != got {
		t.Fatalf("second run changed the file:\n%s", r.out)
	}
	edited := strings.Replace(got, ruleHeading(), "## stale", 1) + "after\n"
	os.WriteFile(claudeMD, []byte(edited), 0o644)
	e.mustRun("skill", "setup", "--yes", "--json=false")
	if want := got + "after\n"; readFile(t, claudeMD) != want {
		t.Fatalf("not replaced in place:\n%s", readFile(t, claudeMD))
	}
	if strings.Count(readFile(t, claudeMD), ruleBegin) != 1 {
		t.Fatal("rule written twice")
	}
}

func TestSkillSetupLeavesHandPastedRule(t *testing.T) {
	e := newEnv(t)
	home := setupHome(t, e, ".codex")
	p := filepath.Join(home, ".codex/AGENTS.md")
	pasted := "# rules\n\n" + agentGuide
	os.WriteFile(p, []byte(pasted), 0o644)
	r := e.mustRun("skill", "setup", "--yes")
	if l := r.last(t); l["event"] != "done" {
		t.Fatalf("%s", r.out)
	}
	if readFile(t, p) != pasted {
		t.Fatal("hand-pasted rule changed")
	}
	if !strings.Contains(r.out, `"status":"present"`) {
		t.Fatalf("%s", r.out)
	}
}

func TestSkillSetupNonInteractivePrintsCommand(t *testing.T) {
	e := newEnv(t)
	home := setupHome(t, e, ".claude", ".cursor")
	r := e.mustRun("skill", "setup", "--json=false")
	for _, want := range []string{"Coding agents found: Claude Code, Cursor.", "~/.claude/CLAUDE.md", "Recommended: crowdvm skill setup --yes"} {
		if !strings.Contains(r.out, want) {
			t.Fatalf("missing %q in:\n%s", want, r.out)
		}
	}
	if exists(filepath.Join(home, ".claude/CLAUDE.md")) || exists(filepath.Join(home, ".claude/skills")) {
		t.Fatal("wrote without a yes")
	}
	withTTY(t)
	e.vars["CI"] = "true" // CI never prompts, even with a terminal
	if r := e.mustRun("skill", "setup", "--json=false"); !strings.Contains(r.out, "Recommended:") || strings.Contains(r.out, "[Y/n]") {
		t.Fatalf("CI prompted:\n%s", r.out)
	}
}

func TestSkillSetupInteractive(t *testing.T) {
	withTTY(t)
	e := newEnv(t)
	home := setupHome(t, e, ".claude", ".codex", ".config/opencode")
	r := e.runIn("n\n", "skill", "setup", "--json=false")
	if !strings.Contains(r.out, "Set up Claude Code, Codex and OpenCode to use CrowdVM? [Y/n] ") || !strings.Contains(r.out, "Skipped.") {
		t.Fatalf("%s", r.out)
	}
	if exists(filepath.Join(home, ".claude/CLAUDE.md")) {
		t.Fatal("wrote after no")
	}
	if r := e.runIn("", "skill", "setup", "--json=false"); !strings.Contains(r.out, "Skipped.") { // EOF is no
		t.Fatalf("%s", r.out)
	}
	r = e.runIn("\n", "skill", "setup", "--json=false") // Enter = yes
	if r.code != 0 || !strings.Contains(r.out, "Restart running") {
		t.Fatalf("%d %s %s", r.code, r.out, r.err)
	}
	for _, p := range []string{".claude/CLAUDE.md", ".codex/AGENTS.md", ".config/opencode/AGENTS.md", ".config/opencode/skills/crowdvm/SKILL.md"} {
		if !exists(filepath.Join(home, p)) {
			t.Fatalf("%s missing", p)
		}
	}
}

func TestSkillSetupEnvAndNoAgents(t *testing.T) {
	withTTY(t)
	e := newEnv(t)
	home := setupHome(t, e)
	if r := e.mustRun("skill", "setup", "--json=false"); !strings.Contains(r.out, "No coding agent found") || strings.Contains(r.out, "[Y/n]") {
		t.Fatalf("%s", r.out)
	}
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	e.vars["CROWDVM_AGENT_SETUP"] = "0"
	if r := e.mustRun("skill", "setup", "--json=false"); r.out != "" || exists(filepath.Join(home, ".codex/AGENTS.md")) {
		t.Fatalf("CROWDVM_AGENT_SETUP=0 did something: %q", r.out)
	}
	e.vars["CROWDVM_AGENT_SETUP"] = "1"
	if r := e.mustRun("skill", "setup", "--json=false"); strings.Contains(r.out, "[Y/n]") || !exists(filepath.Join(home, ".codex/AGENTS.md")) {
		t.Fatalf("CROWDVM_AGENT_SETUP=1 did not set up: %q", r.out)
	}
}
