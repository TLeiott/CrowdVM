package main

import (
	"flag"
	"testing"
)

func TestR3GrantReasonAfterPositionals(t *testing.T) {
	for _, args := range [][]string{{"alice", "25", "--reason", "test grant"}, {"--reason=test grant", "alice", "25"}, {"alice", "25", "-reason", "test grant"}} {
		f := flag.NewFlagSet("grant", flag.ContinueOnError)
		reason := f.String("reason", "", "reason")
		if err := parseGrantFlags(f, args); err != nil {
			t.Fatal(err)
		}
		if *reason != "test grant" || f.NArg() != 2 || f.Arg(0) != "alice" || f.Arg(1) != "25" {
			t.Fatalf("%s %v", *reason, f.Args())
		}
	}
}
