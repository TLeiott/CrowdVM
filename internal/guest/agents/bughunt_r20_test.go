package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexComputerBlock removes the block it wrote at the last start with
// TrimRight(before) + TrimLeft(after): when text sits on both sides of the
// block (the user's own settings above it, a table `codex mcp add` appended
// below it) the two pieces are glued together without a newline, and the
// last line above the block and the first table below it end up on one line.
func TestBughuntR20CodexBlockRewriteGluesNeighbouringTables(t *testing.T) {
	home := t.TempDir()
	m := New(Options{Home: home, Unprivileged: true, DataDir: t.TempDir(), SandboxID: "bx_test"})
	c := &mcpCommand{Command: "/usr/local/bin/crowdvm-guest", Args: []string{"mcp-computer"}}
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's own settings; the first agent start appends the block.
	if err := os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.codexComputerBlock(c)
	// `codex mcp add other` appends a table after the block.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n[mcp_servers.other]\ncommand = \"other\"\n")
	f.Close()
	// The next sandbox start rewrites the block.
	m.codexComputerBlock(c)
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "[mcp_servers.other]") && l != "[mcp_servers.other]" {
			t.Fatalf("the table header is glued to the line above it (config.toml no longer parses):\n%s", got)
		}
	}
	if !strings.Contains(got, "[mcp_servers.other]") || !strings.Contains(got, "[mcp_servers.computer]") {
		t.Fatalf("a table was lost:\n%s", got)
	}
}
