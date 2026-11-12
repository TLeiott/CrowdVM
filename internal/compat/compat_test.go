package compat

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// A release may only cut off what an earlier one already declared outdated.
func TestGenerationsOrdered(t *testing.T) {
	for _, g := range []struct {
		name           string
		min, req, have int
	}{
		{"node", MinNodeGen, RequiredNodeGen, NodeGen},
		{"api", MinAPIGen, RequiredAPIGen, APIGen},
		{"server", MinServerGen, MinServerGen, ServerGen},
	} {
		if g.min < 0 || g.min > g.req || g.req > g.have {
			t.Errorf("%s: want 0 <= Min (%d) <= Required (%d) <= Gen (%d)", g.name, g.min, g.req, g.have)
		}
	}
}

func TestNodeState(t *testing.T) {
	if got := NodeState(NodeGen); got != StateCurrent {
		t.Fatalf("NodeState(NodeGen) = %s", got)
	}
	if RequiredNodeGen > 0 && NodeState(RequiredNodeGen-1) != StateOutdated && NodeState(RequiredNodeGen-1) != StateIncompatible {
		t.Fatal("a node below Required is not outdated")
	}
	if NodeState(MinNodeGen-1) != StateIncompatible {
		t.Fatal("a node below Min is not incompatible")
	}
}

// The SDKs declare the API generation they speak; keep them equal.
func TestSDKGenerations(t *testing.T) {
	for file, re := range map[string]string{
		"../../sdk/typescript/src/client.ts":     `export const API_GEN = (\d+);`,
		"../../sdk/python/src/crowdvm/client.py": `(?m)^API_GEN = (\d+)$`,
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(re).FindSubmatch(b)
		if m == nil || string(m[1]) != strconv.Itoa(APIGen) {
			t.Errorf("%s: API_GEN is %q, want %d (compat.APIGen)", file, m, APIGen)
		}
	}
}
