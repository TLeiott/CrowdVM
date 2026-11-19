package server_test

import (
	"fmt"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Issue #17: sandbox labels are validated, returned and filterable.
func TestSandboxLabels(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	many := map[string]string{}
	for i := range 21 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, labels := range map[string]map[string]string{
		"bad key":     {"-x": "1"},
		"key spaces":  {"a b": "1"},
		"long value":  {"a": string(make([]byte, 257))},
		"newline":     {"a": "x\ny"},
		"too many":    many,
		"empty key":   {"": "1"},
		"long key 64": {"a123456789012345678901234567890123456789012345678901234567890123": "1"},
	} {
		if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"labels": labels}); r.Code() != "invalid_labels" {
			t.Fatalf("%s: %s", name, r)
		}
	}
	id := createSandbox(t, env, key, map[string]any{"labels": map[string]string{"agent": "a", "crowdvm.io/job": "build 7"}})
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.labels.agent") != "a" {
		t.Fatalf("labels not returned: %s", r)
	}
	createSandbox(t, env, key, map[string]any{"labels": map[string]string{"agent": "b"}})
	for q, n := range map[string]int{"agent%3Da": 1, "agent%3Db": 1, "agent%3Dc": 0, "agent%3Da&label=crowdvm.io%2Fjob%3Dbuild+7": 1} {
		if r := env.Do(key, "GET", "/api/v1/sandboxes?label="+q, nil); len(r.Get("sandboxes").([]any)) != n {
			t.Fatalf("label=%s: want %d: %s", q, n, r)
		}
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes?label=agent", nil); r.Code() != "invalid_label" {
		t.Fatalf("filter without =: %s", r)
	}
}
