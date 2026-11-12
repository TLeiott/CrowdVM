package node

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r14-opus 7: a channel sandbox never takes a pool VM, yet the pool
// kept one booted. A channel create now clears the pool's shape.
func TestR14ChannelCreateEmptiesWarmPool(t *testing.T) {
	h := start(t, func(c *Config) { c.WarmPool = 1 })
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_p1")) // teaches the pool its shape
	h.waitState(t, "bx_p1", proto.VMRunning)
	waitFor(t, "pool vm ready", func() bool {
		h.node.mu.Lock()
		defer h.node.mu.Unlock()
		return len(h.node.warm) == 1
	})
	ch := spec("bx_p2")
	ch.Guest.Channel = &proto.GuestChannel{URL: "https://server", Ticket: "t"}
	call(t, h.sess, "POST", "/v1/vms", ch)
	waitFor(t, "pool emptied", func() bool {
		h.node.mu.Lock()
		defer h.node.mu.Unlock()
		return len(h.node.warm) == 0 && h.node.warmShape == nil
	})
}
