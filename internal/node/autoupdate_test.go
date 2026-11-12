package node

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// countRuns counts autoUpdateRun calls until 50 ms pass without one.
func countRuns(ran chan struct{}) int {
	got := 0
	for {
		select {
		case <-ran:
			got++
		case <-time.After(50 * time.Millisecond):
			return got
		}
	}
}

func stubAutoUpdate(t *testing.T) chan struct{} {
	ran := make(chan struct{}, 10)
	run, jitter := autoUpdateRun, autoUpdateJitter
	t.Cleanup(func() { autoUpdateRun, autoUpdateJitter = run, jitter })
	autoUpdateRun = func(*Node) { ran <- struct{}{} }
	autoUpdateJitter = 0
	return ran
}

func TestAutoUpdateDecision(t *testing.T) {
	ran := stubAutoUpdate(t)
	for _, c := range []struct {
		mode, state, server string
		supervised          bool
		want                bool
		unavailable         string
	}{
		{"", compat.StateOutdated, "https://crowd.example", true, true, ""},
		{"", compat.StateIncompatible, "https://crowd.example", true, true, ""},
		{"", compat.StateAvailable, "https://crowd.example", true, false, ""},
		{AutoUpdateAlways, compat.StateAvailable, "https://crowd.example", true, true, ""},
		{AutoUpdateAlways, compat.StateCurrent, "https://crowd.example", true, false, ""},
		{AutoUpdateOff, compat.StateOutdated, "https://crowd.example", true, false, "turned off"},
		{"", compat.StateOutdated, "https://crowd.example", false, false, "not running as a service"}, // nothing could restart it
		// Plain http on a network: nothing authenticates the download.
		{"", compat.StateOutdated, "http://192.168.1.10:8420", true, false, "server is not https"},
		{"", compat.StateOutdated, "http://100.101.102.103:8420", true, true, ""}, // tailnet
		{"", compat.StateOutdated, "http://127.0.0.1:8420", true, true, ""},
	} {
		n := New(&Config{DataDir: t.TempDir(), AutoUpdate: c.mode, Server: c.server}, nil, nil)
		if c.supervised {
			n.SetRestarter(func() {})
		}
		n.maybeAutoUpdate(&proto.NodeUpdate{State: c.state})
		n.maybeAutoUpdate(&proto.NodeUpdate{State: c.state}) // throttled
		if want := map[bool]int{true: 1, false: 0}[c.want]; countRuns(ran) != want {
			t.Errorf("mode %q state %s server %s supervised %v: want %d runs", c.mode, c.state, c.server, c.supervised, want)
		}
		if got := n.autoUpdateStatus(""); got.Unavailable != c.unavailable {
			t.Errorf("mode %q server %s supervised %v: status %+v, want %q", c.mode, c.server, c.supervised, got, c.unavailable)
		}
	}
}

// A release newer than the last attempt's skips the hourly throttle; the
// jitter delays the run.
func TestAutoUpdateThrottleAndJitter(t *testing.T) {
	ran := stubAutoUpdate(t)
	n := New(&Config{DataDir: t.TempDir(), Server: "https://crowd.example"}, nil, nil)
	n.SetRestarter(func() {})
	up := func(latest string) { n.maybeAutoUpdate(&proto.NodeUpdate{State: compat.StateOutdated, Latest: latest}) }
	up("v1.3.0")
	up("v1.3.0")
	up("v1.4.0")
	if got := countRuns(ran); got != 2 {
		t.Fatalf("%d runs, want 2", got)
	}
	autoUpdateJitter = time.Hour
	up("v1.5.0")
	if got := countRuns(ran); got != 0 {
		t.Fatalf("ran without waiting: %d", got)
	}
}

func TestTrustedUpdateServer(t *testing.T) {
	for s, want := range map[string]bool{
		"https://crowd.example":          true,
		"http://localhost:8420":          true,
		"http://127.0.0.1:8420":          true,
		"http://[::1]:8420":              true,
		"http://100.64.0.1:8420":         true,
		"http://100.127.255.254":         true,
		"http://[fd7a:115c:a1e0::1]:80":  true,
		"http://192.168.1.10:8420":       false,
		"http://100.128.0.1:8420":        false,
		"http://crowd.example":           false,
		"192.168.1.10:8420":              false,
		"http://[fd7a:115c:a1e1::1]:80":  false,
		"http://localhost.evil.example/": false,
	} {
		if TrustedUpdateServer(s) != want {
			t.Errorf("%s: want %v", s, want)
		}
	}
}
