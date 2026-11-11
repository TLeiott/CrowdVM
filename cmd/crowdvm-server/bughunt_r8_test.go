package main

import "testing"

// join-token create must print the --public-url the server runs with, not a
// guessed LAN address (bugs-r8-opus 7).
func TestJoinServerURLUsesServePublicURL(t *testing.T) {
	dir := t.TempDir()
	savePublicURL(dir, "http://100.64.0.1:9960")
	if got := joinServerURL("", dir); got != "http://100.64.0.1:9960" {
		t.Fatalf("got %q, want the saved public URL", got)
	}
	if got := joinServerURL("http://flag:1", dir); got != "http://flag:1" {
		t.Fatalf("flag must win, got %q", got)
	}
	if got := joinServerURL("", t.TempDir()); got == "" {
		t.Fatal("no fallback")
	}
}
