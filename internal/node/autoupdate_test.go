package node

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestAutoUpdateDecision(t *testing.T) {
	ran := make(chan struct{}, 10)
	defer func(f func(*Node)) { autoUpdateRun = f }(autoUpdateRun)
	autoUpdateRun = func(*Node) { ran <- struct{}{} }
	for _, c := range []struct {
		mode, state string
		supervised  bool
		want        bool
	}{
		{"", compat.StateOutdated, true, true},
		{"", compat.StateIncompatible, true, true},
		{"", compat.StateAvailable, true, false},
		{AutoUpdateAlways, compat.StateAvailable, true, true},
		{AutoUpdateAlways, compat.StateCurrent, true, false},
		{AutoUpdateOff, compat.StateOutdated, true, false},
		{"", compat.StateOutdated, false, false}, // nothing could restart it
	} {
		n := New(&Config{DataDir: t.TempDir(), AutoUpdate: c.mode}, nil, nil)
		if c.supervised {
			n.SetRestarter(func() {})
		}
		n.maybeAutoUpdate(&proto.NodeUpdate{State: c.state})
		n.maybeAutoUpdate(&proto.NodeUpdate{State: c.state}) // throttled
		got := 0
		for done := false; !done; {
			select {
			case <-ran:
				got++
			case <-time.After(50 * time.Millisecond):
				done = true
			}
		}
		if want := map[bool]int{true: 1, false: 0}[c.want]; got != want {
			t.Errorf("mode %q state %s supervised %v: %d runs, want %d", c.mode, c.state, c.supervised, got, want)
		}
	}
}
