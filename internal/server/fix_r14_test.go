package server_test

import (
	"net"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r14-sonnet 3: a retry is only accepted with the nonce of the first
// redemption; the node, which knows the ticket but not the nonce (inside
// TLS), still gets ticket_used.
func TestR14TicketRetryNeedsTheFirstNonce(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	spec, _ := node.Spec(id)
	ticket := spec.Guest.Channel.Ticket
	if _, w := rawChannelTryNonce(t, env, id, ticket, "guest-nonce"); w.Error != "" {
		t.Fatalf("first redemption: %s", w.Error)
	}
	for _, n := range []string{"", "node-guess"} {
		if _, w := rawChannelTryNonce(t, env, id, ticket, n); w.Error != "ticket_used" {
			t.Fatalf("redemption with nonce %q: %q, want ticket_used", n, w.Error)
		}
	}
	if _, w := rawChannelTryNonce(t, env, id, ticket, "guest-nonce"); w.Error != "" {
		t.Fatalf("the guest's own retry: %q", w.Error)
	}
}

// bugs-r14-opus 4: after a server restart, rebuildLost counted the server's
// own downtime as the node's: a node that dropped just before the restart
// had its sandboxes rebuilt elsewhere (and its newer disk dropped) before it
// could reconnect.
func TestR14RebuildLostWaitsAfterServerStart(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 5 * time.Second })
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if _, err := a.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	env.Eventually("head stored", func() bool {
		sb, _ := env.S.SandboxByID(id)
		return sb.HeadSnapshotID != ""
	})
	env.StartNode(servertest.NodeOptions{Name: "b"})
	a.Disconnect()
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", servertest.WaitTimeout, "error")
	// It went offline long ago by the clock, but this server only just
	// started: the node never had the grace to come back.
	if _, err := env.S.DB.Exec(`UPDATE sandboxes SET degraded_since=?, updated_at=? WHERE id=?`, time.Now().Add(-time.Hour).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if sb, _ := env.S.SandboxByID(id); sb.NodeID != a.ID || sb.State != "error" {
		t.Fatalf("rebuilt within the first second of the server (state %s on %s)", sb.State, sb.NodeID)
	}
}

// bugs-r14-opus 5: a resume after the machine failed booted the older head
// while its node still uploaded a newer snapshot.
func TestR14ResumeAfterFailureUsesNewerSnapshot(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	old, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually("first snapshot is the head", func() bool {
		sb, _ := env.S.SandboxByID(id)
		return sb.HeadSnapshotID == old
	})
	a.HoldUploads()
	newer, err := a.PeriodicSnapshot(id) // still uploading
	if err != nil {
		t.Fatal(err)
	}
	a.SetVMState(id, "error", false, "the machine crashed")
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", servertest.WaitTimeout, "error")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status/100 != 2 {
		t.Fatalf("resume: %s", r)
	}
	if sb, _ := env.S.SandboxByID(id); sb.HeadSnapshotID != newer {
		t.Fatalf("resume booted from %s, want the newer %s (older %s)", sb.HeadSnapshotID, newer, old)
	}
	a.ReleaseUploads()
	env.WaitState(key, id, "ready")
}

// bugs-r16-opus 2: an egress flow to a destination that never reads stayed
// open after its channel session ended (stop, delete, reboot); each new
// session could park 256 more. Flows now end with their session.
func TestR16EgressFlowEndsWithItsSession(t *testing.T) {
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := sink.Accept(); err == nil {
			accepted <- c // never read
		}
	}()
	env := servertest.New(t, func(c *server.Config) { c.EgressAllow = []string{sink.Addr().String()} })
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	spec, _ := node.Spec(id)
	sess, _ := rawChannel(t, env, id, spec.Guest.Channel.Ticket)
	st, err := sess.Open()
	if err != nil {
		t.Fatal(err)
	}
	up, err := mux.DialUpgrade(st, "server", "/v1/egress?addr="+sink.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var c net.Conn
	select {
	case c = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the flow never reached the destination")
	}
	defer c.Close()
	_ = c.(*net.TCPConn).SetReadBuffer(4096)
	go func() { // fill the destination's buffers so the server's copy blocks
		buf := make([]byte, 64<<10)
		for {
			if _, err := up.Write(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(2 * time.Second)
	_ = sess.Close() // the guest goes away
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1<<20)
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the server kept the egress flow open after its channel session ended")
			}
			return // EOF or reset: closed
		}
	}
}

// bugs-r17-opus 1: within a live session, a flow to a destination that
// never reads never ended, and 256 of them cut the sandbox off the network.
// A write the destination does not take within the stall time ends it.
func TestR17StalledEgressFlowEnds(t *testing.T) {
	defer server.SetEgressStall(300 * time.Millisecond)()
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := sink.Accept(); err == nil {
			accepted <- c
		}
	}()
	env := servertest.New(t, func(c *server.Config) { c.EgressAllow = []string{sink.Addr().String()} })
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	spec, _ := node.Spec(id)
	sess, _ := rawChannel(t, env, id, spec.Guest.Channel.Ticket)
	st, err := sess.Open()
	if err != nil {
		t.Fatal(err)
	}
	up, err := mux.DialUpgrade(st, "server", "/v1/egress?addr="+sink.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := <-accepted
	defer c.Close()
	_ = c.(*net.TCPConn).SetReadBuffer(4096)
	go func() { // the guest keeps writing
		buf := make([]byte, 64<<10)
		for {
			if _, err := up.Write(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(time.Second) // the destination's buffers fill, then the write stalls
	// The destination sees the flow end (a reset) while the session is up.
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	buf := make([]byte, 1<<20)
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("a flow whose destination takes nothing stayed open (the session is still up)")
			}
			return
		}
	}
}

// bugs-r17-opus 9: a disk never shrinks, but after a resume at a smaller
// type the API (and the node's reservation) showed the smaller disk.
func TestR17SmallerTypeKeepsTheBiggerDisk(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	node := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 64, MemoryMiB: 1 << 18, DiskGiB: 10000, MaxSandboxes: 10}})
	id := createSandbox(t, env, key, map[string]any{"type": "small"})
	env.WaitState(key, id, "ready")
	small := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Get("sandbox.storage.diskGB").(float64)
	resize := func(typ string) {
		env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
		env.WaitState(key, id, "archived")
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": typ}); r.Status/100 != 2 {
			t.Fatalf("resume %s: %s", typ, r)
		}
		env.WaitState(key, id, "ready")
	}
	resize("large")
	large := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Get("sandbox.storage.diskGB").(float64)
	resize("small")
	r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil)
	if got := r.Get("sandbox.storage.diskGB").(float64); got != large || got <= small {
		t.Fatalf("after large then small: diskGB %v, want the large disk %v (small is %v)", got, large, small)
	}
	if sp, ok := node.Spec(id); !ok || float64(sp.Size.DiskGiB) != large {
		t.Fatalf("the node was asked for a %d GiB disk, want %v", sp.Size.DiskGiB, large)
	}
}
