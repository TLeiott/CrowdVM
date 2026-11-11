package node

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r11-opus 5: one guest's event flood neither pushes another sandbox's
// events out of the node's queue nor the node's own lifecycle events.
func TestR11OpusGuestEventFloodKeepsOthers(t *testing.T) {
	h := start(t, nil)
	h.stop() // no flusher: the queue only fills
	n := h.node
	n.mu.Lock()
	n.events = nil // no VM ran: nothing else was queued
	n.mu.Unlock()

	guestEv := func(sb, typ string) proto.Event {
		return proto.Event{Time: time.Now().UTC(), SandboxID: sb, Type: proto.EvGuest, Data: json.RawMessage(`{"type":"` + typ + `"}`)}
	}
	n.emit("bx_bob", proto.EvVMState, proto.VMStatus{SandboxID: "bx_bob", State: proto.VMRunning})
	n.emitEvent(guestEv("bx_bob", "setup.status"))
	for i := 0; i < 30000; i++ {
		n.emitEvent(guestEv("bx_flood", "noise.flood"))
	}
	// Many guests together must leave room for the node's own events too.
	for s := 0; s < 40; s++ {
		for i := 0; i < 600; i++ {
			n.emitEvent(guestEv("bx_many"+string(rune('a'+s%26))+string(rune('a'+s/26)), "noise"))
		}
	}
	n.emit("bx_bob", proto.EvVMState, proto.VMStatus{SandboxID: "bx_bob", State: proto.VMStopping})
	n.emit("bx_bob", proto.EvSnapshotUploaded, proto.SnapshotRef{ID: "snap_x"})

	n.mu.Lock()
	defer n.mu.Unlock()
	var bobGuest, lifecycle, flood int
	for _, e := range n.events {
		switch {
		case e.SandboxID == "bx_bob" && e.Type == proto.EvGuest:
			bobGuest++
		case e.SandboxID == "bx_bob":
			lifecycle++
		case e.SandboxID == "bx_flood":
			flood++
		}
	}
	if bobGuest != 1 || lifecycle != 3 {
		t.Errorf("bob lost events: %d guest (want 1), %d lifecycle (want 3)", bobGuest, lifecycle)
	}
	if flood > 500 {
		t.Errorf("the flooding sandbox holds %d queued events", flood)
	}
	if len(n.events) > maxQueuedEvents {
		t.Errorf("queue holds %d events", len(n.events))
	}
}

// r11Driver is the fake driver with a per-VM stall in Snapshot, a disk that
// always changed (so the node syncs the guest first) and a different guest
// address for one VM.
type r11Driver struct {
	*fakeDriver
	stall    string        // sandbox whose Snapshot blocks until release
	release  chan struct{} // ends the stall
	syncAddr map[string]string
	mu       sync.Mutex
	snaps    map[string]int
}

func (d *r11Driver) DiskChanged(string) (bool, error) { return true, nil }

func (d *r11Driver) count(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snaps[id]
}

func (d *r11Driver) Snapshot(ctx context.Context, id, snapID string) (proto.SnapshotRef, error) {
	d.mu.Lock()
	d.snaps[id]++
	d.mu.Unlock()
	if id == d.stall {
		select {
		case <-d.release:
		case <-ctx.Done():
			return proto.SnapshotRef{}, ctx.Err()
		}
	}
	return d.fakeDriver.Snapshot(ctx, id, snapID)
}

func (d *r11Driver) GuestAddr(id string) (string, error) {
	if a := d.syncAddr[id]; a != "" {
		if _, err := d.fakeDriver.GuestAddr(id); err != nil {
			return "", err
		}
		return a, nil
	}
	return d.fakeDriver.GuestAddr(id)
}

// startR11 is start() with r11Driver around the fake driver.
func startR11(t *testing.T) (*harness, *r11Driver) {
	t.Helper()
	h := &harness{srv: newFakeServer(t), guest: newFakeGuest(t)}
	h.drv = newFakeDriver(t, h.guest)
	d := &r11Driver{fakeDriver: h.drv, release: make(chan struct{}), syncAddr: map[string]string{}, snaps: map[string]int{}}
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.CPUs, cfg.MemoryMiB, cfg.DiskGiB, cfg.MaxSandboxes = 8, 16384, 200, 3
	cfg.SnapshotInterval = "100ms"
	h.cfg = cfg
	h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.node = New(cfg, d, h.log)
	h.node.bench = proto.Benchmark{RanAt: time.Now(), Tier: "gold", Score: 600, Network: proto.NetworkBench{Via: "lan"}}
	if err := Join(context.Background(), cfg, h.srv.srv.URL, "join-tok", h.node.Info()); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	t.Cleanup(func() { h.stop() })
	t.Cleanup(func() {
		select {
		case <-d.release:
		default:
			close(d.release)
		}
	}) // runs first
	h.srv.putImage("img1", []byte("base"))
	return h, d
}

// bugs-r11-opus 7: a guest whose snapshot stalls does not hold up the other
// VMs' periodic snapshots.
func TestR11OpusStalledSnapshotDoesNotBlockOthers(t *testing.T) {
	h, d := startR11(t)
	d.stall = "bx_stall"
	call(t, h.sess, "POST", "/v1/vms", spec("bx_stall"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_ok"))
	h.waitState(t, "bx_stall", proto.VMRunning)
	h.waitState(t, "bx_ok", proto.VMRunning)
	waitFor(t, "the stalling VM to be in its snapshot", func() bool { return d.count("bx_stall") >= 1 })
	waitFor(t, "other VM snapshots while the first stalls", func() bool { return d.count("bx_ok") >= 3 })
}

// bugs-r11-opus 7: a guest that did not answer sync is not asked again every
// round; its periodic snapshots go on (crash-consistent) meanwhile.
func TestR11OpusSyncTimeoutBacksOff(t *testing.T) {
	h, d := startR11(t)
	var syncs atomic.Int32
	target, _ := url.Parse(h.guest.srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/internal/sync" {
			syncs.Add(1)
			http.Error(w, "stuck", 500)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	d.syncAddr["bx_sync"] = front.Listener.Addr().String()
	var _ vm.ChangeTracker = d
	call(t, h.sess, "POST", "/v1/vms", spec("bx_sync"))
	h.waitState(t, "bx_sync", proto.VMRunning)
	waitFor(t, "periodic snapshots of the VM with a stuck sync", func() bool { return d.count("bx_sync") >= 3 })
	if n := syncs.Load(); n != 1 {
		t.Fatalf("the node asked a guest whose sync failed %d times in 3 rounds", n)
	}
}
