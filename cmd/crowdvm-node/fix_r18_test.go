package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
)

// bugs-r18-opus 5: an updated build that failed before it counted its start
// (here: it cannot read its config) was never rolled back and crash-looped.
// The start is now counted first.
func TestR18UpdatedBuildCountsStartBeforeConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(dir, "update-pending.json")
	b, _ := json.Marshal(map[string]any{"from": "0.0.1", "to": node.Version, "starts": 0})
	if err := os.WriteFile(pending, b, 0o600); err != nil {
		t.Fatal(err)
	}
	old := supervisedByService
	supervisedByService = func() bool { return true }
	defer func() { supervisedByService = old }()
	if err := run(t, "--data-dir", dir, "run"); err == nil {
		t.Fatal("a broken config started")
	}
	var p struct{ Starts int }
	b, _ = os.ReadFile(pending)
	if json.Unmarshal(b, &p) != nil || p.Starts != 1 {
		t.Fatalf("the start of the updated build was not counted: %s", b)
	}
}
