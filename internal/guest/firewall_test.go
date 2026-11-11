package guest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestFirewallDirectPorts(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	m := e.mustOK("PUT", "/v1/internal/firewall", map[string]any{"open": []string{"8000/tcp", "8000/TCP", "53/udp"}})
	d := m
	if got := toStrings(d["added"]); !slices.Equal(got, []string{"53/udp", "8000/tcp"}) || d["active"] != false {
		t.Fatalf("first %v", m)
	}
	m = e.mustOK("PUT", "/v1/internal/firewall", map[string]any{"open": []string{"8000/tcp"}})
	d = m
	if len(toStrings(d["added"])) != 0 || !slices.Equal(toStrings(d["removed"]), []string{"53/udp"}) {
		t.Fatalf("second %v", m)
	}
	for _, bad := range []string{"0/tcp", "70000/tcp", "22/icmp", "x"} {
		if st, _ := e.do("PUT", "/v1/internal/firewall", map[string]any{"open": []string{bad}}); st != 400 {
			t.Errorf("%q: %d", bad, st)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, x.(string))
	}
	return out
}

func TestSkillsInstalled(t *testing.T) {
	root := t.TempDir()
	home, data := root+"/home", root+"/data"
	newEnvAt(t, proto.GuestConfig{}, home, data)
	for _, d := range []string{".claude/skills", ".codex/skills", ".config/opencode/skills", ".agents/skills"} {
		b, err := os.ReadFile(filepath.Join(home, d, "crowdvm/SKILL.md"))
		if err != nil || !strings.Contains(string(b), "name: crowdvm") {
			t.Fatalf("%s: %v", d, err)
		}
		if _, err := os.Stat(filepath.Join(home, d, "crowdvm/references/cli.md")); err != nil {
			t.Fatal(err)
		}
	}
	// the user's edit and deletion survive the next boot
	edited := filepath.Join(home, ".claude/skills/crowdvm/SKILL.md")
	os.WriteFile(edited, []byte("mine"), 0o644)
	os.RemoveAll(filepath.Join(home, ".codex/skills/crowdvm"))
	newEnvAt(t, proto.GuestConfig{SandboxID: "bx_next"}, home, data)
	if b, _ := os.ReadFile(edited); string(b) != "mine" {
		t.Fatalf("user edit overwritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex/skills/crowdvm/SKILL.md")); err == nil {
		t.Fatal("deleted skill came back")
	}
	if _, err := os.Stat(filepath.Join(home, ".pi/agent/skills/crowdvm/SKILL.md")); err != nil {
		t.Fatal("untouched skill missing")
	}
}
