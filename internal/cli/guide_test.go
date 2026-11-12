package cli

import (
	"regexp"
	"strings"
	"testing"
)

// The agent-instructions rule stays short, points to the skill, and shows only
// commands and flags the CLI has.
func TestAgentGuide(t *testing.T) {
	if n := strings.Count(strings.TrimSpace(agentGuide), "\n") + 1; n > 25 {
		t.Errorf("agent-instructions is %d lines; keep it short (details belong in skills/crowdvm)", n)
	}
	for _, want := range []string{"crowdvm skill install", "crowdvm run", "crowdvm new", "crowdvm exec", "crowdvm stop", "crowdvm delete"} {
		if !strings.Contains(agentGuide, want) {
			t.Errorf("agent-instructions lacks %q", want)
		}
	}
	tree := r8Root(t)
	checked := 0
	for _, m := range regexp.MustCompile("`([^`]*crowdvm [^`]+)`").FindAllStringSubmatch(agentGuide, -1) {
		for _, seg := range r8Segments(m[1]) {
			checked++
			for _, p := range r8Check(tree, seg) {
				t.Errorf("%s\n\t%s", p, m[1])
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d command lines checked; extractor broken?", checked)
	}
}
