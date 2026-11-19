package server_test

import (
	"slices"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Issue 8: --egress-allow-udp reaches the node's kill switch, which then
// relays UDP to those destinations (and only those).
func TestEgressAllowUDPReachesNode(t *testing.T) {
	list := []string{"192.0.2.7:27015", "198.51.100.0/24"}
	env := servertest.New(t, func(c *server.Config) { c.EgressAllowUDP = list })
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	sp, _ := node.Spec(id)
	if sp.Net == nil || !slices.Contains(sp.Net.Deny, "0.0.0.0/0") || !slices.Equal(sp.Net.UDP, list) {
		t.Fatalf("net policy: %+v", sp.Net)
	}
}
