package guest

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r14-opus 6: the node's ops token got the names of the files a
// prepare-stop deleted. A channel guest answers its node with counts only.
func TestR14PrepareStopGivesNodeNoPaths(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.a.cfgMu.Lock()
	e.a.cfg.Channel = &proto.GuestChannel{} // a channel guest: its local API is the node's
	e.a.cfgMu.Unlock()
	m := e.mustOK("POST", "/v1/internal/prepare-stop", map[string]any{"force": true})
	if _, ok := m["removedPaths"]; ok {
		t.Fatalf("the node got the removed file names: %v", m["removedPaths"])
	}
	if _, ok := m["removed"]; !ok {
		t.Fatalf("no count: %v", m)
	}
}
