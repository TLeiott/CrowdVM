package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func init() {
	HeartbeatInterval = 100 * time.Millisecond
	ReconnectMin = 20 * time.Millisecond
	ReconnectMax = 100 * time.Millisecond
	EventFlushDelay = 5 * time.Millisecond
	watchInterval = 20 * time.Millisecond
	snapshotTick = 50 * time.Millisecond
	healthInterval, healthTimeout = 50*time.Millisecond, time.Second // generous: probes must not time out under -race load
	eventDrainIdle = 20 * time.Millisecond
	warmTick = 30 * time.Millisecond
	warmToken = func() string { return "guest-tok" } // the fake guest's only token
}

type harness struct {
	srv   *fakeServer
	guest *fakeGuest
	drv   *fakeDriver
	node  *Node
	cfg   *Config
	sess  *yamux.Session
	log   *slog.Logger
	stop  func() // ends the running node
}

// start joins a node to a fake server and runs it until the test ends.
func start(t *testing.T, edit func(*Config)) *harness {
	t.Helper()
	h := &harness{srv: newFakeServer(t), guest: newFakeGuest(t)}
	h.drv = newFakeDriver(t, h.guest)
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.CPUs, cfg.MemoryMiB, cfg.DiskGiB, cfg.MaxSandboxes = 8, 16384, 200, 3
	cfg.SnapshotInterval = "off"
	if edit != nil {
		edit(cfg)
	}
	h.cfg = cfg
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	h.log = log
	h.node = New(cfg, h.drv, log)
	h.node.bench = proto.Benchmark{RanAt: time.Now(), Tier: "gold", Score: 600, Network: proto.NetworkBench{Via: "lan"}} // skip the real benchmark

	if err := Join(context.Background(), cfg, h.srv.srv.URL, "join-tok", h.node.Info()); err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != "node_test1" || cfg.NodeToken != "node-tok" {
		t.Fatalf("join did not save credentials: %+v", cfg)
	}
	h.run(t)
	t.Cleanup(func() { h.stop() })
	return h
}

// run starts h.node and waits until it is connected.
func (h *harness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.node.Run(ctx); close(done) }()
	h.stop = func() { cancel(); <-done }
	h.sess = h.srv.nextSession()
	waitFor(t, "node connected", func() bool { _, err := h.node.serverClient(); return err == nil })
}

// restart stops the node daemon and starts a new one on the same data
// (driver and store), like an owner restarting crowdvm-node.
func (h *harness) restart(t *testing.T) {
	t.Helper()
	h.stop()
	h.node = New(h.cfg, h.drv, h.log)
	h.node.bench = proto.Benchmark{RanAt: time.Now(), Tier: "gold", Score: 600, Network: proto.NetworkBench{Via: "lan"}}
	h.run(t)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func spec(id string) proto.VMSpec {
	return proto.VMSpec{SandboxID: id, Size: proto.Size{Name: "small", VCPU: 2, MemoryMiB: 4096, DiskGiB: 51},
		ImageID: "img1", Guest: proto.GuestConfig{SandboxID: id, Token: "guest-tok"}}
}

func (h *harness) waitState(t *testing.T, id, state string) {
	t.Helper()
	waitFor(t, id+" "+state, func() bool {
		for _, e := range h.srv.eventsOf(proto.EvVMState) {
			var st proto.VMStatus
			json.Unmarshal(e.Data, &st)
			if e.SandboxID == id && st.State == state {
				return true
			}
		}
		return false
	})
}

func TestJoinBadToken(t *testing.T) {
	srv := newFakeServer(t)
	cfg, _ := LoadConfig(t.TempDir())
	if err := Join(context.Background(), cfg, srv.srv.URL, "wrong", proto.NodeInfo{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestConnectHeartbeatReconnect(t *testing.T) {
	h := start(t, nil)
	waitFor(t, "heartbeat with info", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return len(h.srv.beats) > 0 && h.srv.beats[0].Info != nil && h.srv.beats[0].Info.NodeID == "node_test1"
	})
	h.srv.mu.Lock()
	info := h.srv.beats[0].Info
	h.srv.mu.Unlock()
	if info.Capacity.VCPU != 8 || info.Bench.Tier != "gold" || info.Driver != "fake" {
		t.Fatalf("bad info %+v", info)
	}
	// /v1/info over the session.
	resp, body := call(t, h.sess, "GET", "/v1/info", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"nodeId":"node_test1"`) {
		t.Fatalf("info: %d %s", resp.StatusCode, body)
	}
	// Server-pushed snapshot interval.
	h.srv.mu.Lock()
	h.srv.hbResp = proto.HeartbeatResponse{SnapshotIntervalSec: 42}
	h.srv.mu.Unlock()
	h.node.cfgMu.Lock()
	h.cfg.SnapshotInterval = ""
	h.node.cfgMu.Unlock()
	waitFor(t, "interval applied", func() bool { return h.node.snapshotInterval() == 42*time.Second })

	// Drop the session: the node must reconnect and resend its info.
	h.sess.Close()
	h.sess = h.srv.nextSession()
	resp, _ = call(t, h.sess, "GET", "/v1/vms", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("after reconnect: %d", resp.StatusCode)
	}
	h.srv.mu.Lock()
	n := h.srv.connects
	h.srv.mu.Unlock()
	if n < 2 {
		t.Fatalf("connects = %d", n)
	}
}

func TestVMLifecycle(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base image bytes"))

	resp, body := call(t, h.sess, "POST", "/v1/vms", spec("bx_1"))
	if resp.StatusCode != 202 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	h.waitState(t, "bx_1", proto.VMRunning)
	if !h.drv.store.HasImage("img1") {
		t.Fatal("image was not pulled")
	}
	_, body = call(t, h.sess, "GET", "/v1/vms/bx_1", nil)
	var st proto.VMStatus
	json.Unmarshal(body, &st)
	if st.State != proto.VMRunning || !st.GuestReady {
		t.Fatalf("status: %s", body)
	}

	// Guest events are forwarded.
	waitFor(t, "guest events", func() bool { return len(h.srv.eventsOf(proto.EvGuest)) >= 2 })
	ge := h.srv.eventsOf(proto.EvGuest)[0]
	if ge.SandboxID != "bx_1" || !strings.Contains(string(ge.Data), "setup.status") {
		t.Fatalf("guest event %+v", ge)
	}

	// Plain proxy: path, query, body, auth rewritten.
	_, body = call(t, h.sess, "POST", "/v1/vms/bx_1/guest/echo/a%2Fb?x=1", map[string]int{"n": 1})
	var echo map[string]string
	json.Unmarshal(body, &echo)
	if echo["uri"] != "/v1/echo/a%2Fb?x=1" || echo["method"] != "POST" || !strings.Contains(echo["body"], `"n":1`) {
		t.Fatalf("echo: %s", body)
	}

	// Streaming proxy: the first NDJSON line arrives before the second is produced.
	req, _ := http.NewRequest("GET", "http://node/v1/vms/bx_1/guest/slow", nil)
	sresp, err := mux.Client(h.sess).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(sresp.Body)
	line, err := readLineTimeout(br, 3*time.Second)
	if err != nil || !strings.Contains(line, `"line":1`) {
		t.Fatalf("first streamed line: %q %v", line, err)
	}
	h.guest.release <- struct{}{}
	line, _ = readLineTimeout(br, 3*time.Second)
	if !strings.Contains(line, `"line":2`) {
		t.Fatalf("second line: %q", line)
	}
	sresp.Body.Close()

	// Tunnel via Upgrade.
	conn, err := h.sess.Open()
	if err != nil {
		t.Fatal(err)
	}
	tc, err := mux.DialUpgrade(conn, "node", "/v1/vms/bx_1/tunnel?port=22", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(tc, "hello\n")
	line, _ = readLineTimeout(bufio.NewReader(tc), 3*time.Second)
	if line != "port22:hello\n" {
		t.Fatalf("tunnel echo %q", line)
	}
	tc.Close()

	// Live snapshot, then stop: the server gets both layers (parent first).
	resp, body = call(t, h.sess, "POST", "/v1/vms/bx_1/snapshot", proto.SnapshotRequest{SnapshotID: "snap_live"})
	if resp.StatusCode != 200 {
		t.Fatalf("snapshot: %d %s", resp.StatusCode, body)
	}
	h.drv.mu.Lock()
	h.drv.supersede = []string{"snap_live"} // the stop layer merged it (it may be a saved snapshot)
	h.drv.mu.Unlock()
	resp, body = call(t, h.sess, "POST", "/v1/vms/bx_1/stop", proto.StopRequest{SnapshotID: "snap_stop"})
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d %s", resp.StatusCode, body)
	}
	var ref proto.SnapshotRef
	json.Unmarshal(body, &ref)
	if ref.ID != "snap_stop" || ref.ParentID != "snap_live" {
		t.Fatalf("stop ref %+v", ref)
	}
	h.waitState(t, "bx_1", proto.VMStopped)
	// A repeated stop (a server that restarted mid-stop retries it) gets the
	// finished stop's snapshot, not 404 (opus-21 N1).
	resp2, body2 := call(t, h.sess, "POST", "/v1/vms/bx_1/stop", proto.StopRequest{SnapshotID: "snap_retry"})
	var again proto.SnapshotRef
	_ = json.Unmarshal(body2, &again)
	if resp2.StatusCode != 200 || again.ID != "snap_stop" {
		t.Fatalf("repeated stop: %d %s", resp2.StatusCode, body2)
	}
	waitFor(t, "uploads", func() bool {
		up := map[string]bool{}
		for _, e := range h.srv.eventsOf(proto.EvSnapshotUploaded) {
			var r proto.SnapshotRef
			json.Unmarshal(e.Data, &r)
			if r.SHA256 != "" {
				up[r.ID] = true
			}
		}
		return up["snap_live"] && up["snap_stop"]
	})
	// Only the server deletes superseded layers (sonnet-13 B1): its
	// snapshot locations must stay true for this node.
	for range 20 {
		if !h.drv.store.HasSnapshot("snap_live") {
			t.Fatal("the node dropped a superseded layer on its own")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.srv.mu.Lock()
	b := h.srv.blobs["snapshot/snap_stop"]
	h.srv.mu.Unlock()
	if string(b.data) != "disk-of-snap_stop" {
		t.Fatalf("uploaded blob %q", b.data)
	}
	if u := h.node.Usage(); u.VMs != 0 {
		t.Fatalf("usage after stop %+v", u)
	}
	// Transfer progress events were emitted.
	if len(h.srv.eventsOf(proto.EvTransfer)) == 0 {
		t.Fatal("no transfer events")
	}

	resp, _ = call(t, h.sess, "DELETE", "/v1/vms/bx_1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ = call(t, h.sess, "GET", "/v1/vms/bx_1", nil); resp.StatusCode != 404 {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
}

func readLineTimeout(br *bufio.Reader, d time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() { s, err := br.ReadString('\n'); ch <- res{s, err} }()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(d):
		return "", context.DeadlineExceeded
	}
}

func TestChainPull(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_a", ImageID: "img1"}, []byte("layer a"))
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_b", ParentID: "snap_a", ImageID: "img1"}, []byte("layer b"))

	s := spec("bx_2")
	s.ImageID, s.ParentID = "", "snap_b" // image id learned from the chain
	if resp, body := call(t, h.sess, "POST", "/v1/vms", s); resp.StatusCode != 202 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	h.waitState(t, "bx_2", proto.VMRunning)
	for _, id := range []string{"snap_a", "snap_b"} {
		if !h.drv.store.HasSnapshot(id) {
			t.Fatalf("%s not pulled", id)
		}
	}
	if !h.drv.store.HasImage("img1") {
		t.Fatal("image not pulled")
	}

	// Corrupted blob: sha256 verification fails and the VM goes to error.
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_bad", ImageID: "img1"}, []byte("good"))
	h.srv.mu.Lock()
	h.srv.blobs["snapshot/snap_bad"] = blob{[]byte("evil"), h.srv.blobs["snapshot/snap_bad"].sha}
	h.srv.mu.Unlock()
	s = spec("bx_3")
	s.ParentID = "snap_bad"
	call(t, h.sess, "POST", "/v1/vms", s)
	h.waitState(t, "bx_3", proto.VMError)
	_, body := call(t, h.sess, "GET", "/v1/vms/bx_3", nil)
	if !strings.Contains(string(body), "sha256 mismatch") {
		t.Fatalf("expected sha error: %s", body)
	}
	if h.drv.store.HasSnapshot("snap_bad") {
		t.Fatal("corrupt snapshot imported")
	}

	// Explicit fetch endpoint.
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_c", ParentID: "snap_b", ImageID: "img1"}, []byte("layer c"))
	if resp, body := call(t, h.sess, "POST", "/v1/snapshots/snap_c/fetch", nil); resp.StatusCode != 200 {
		t.Fatalf("fetch: %d %s", resp.StatusCode, body)
	}
	if !h.drv.store.HasSnapshot("snap_c") {
		t.Fatal("fetch did not import")
	}
	if resp, _ := call(t, h.sess, "DELETE", "/v1/snapshots/snap_c", nil); resp.StatusCode != 200 || h.drv.store.HasSnapshot("snap_c") {
		t.Fatal("snapshot delete failed")
	}
}

func TestCapacity(t *testing.T) {
	h := start(t, func(c *Config) { c.CPUs, c.MaxSandboxes = 4, 3 })
	h.srv.putImage("img1", []byte("base"))
	for _, id := range []string{"bx_a", "bx_b"} {
		if resp, body := call(t, h.sess, "POST", "/v1/vms", spec(id)); resp.StatusCode != 202 {
			t.Fatalf("create %s: %d %s", id, resp.StatusCode, body)
		}
	}
	resp, body := call(t, h.sess, "POST", "/v1/vms", spec("bx_c"))
	if resp.StatusCode != 503 || !strings.Contains(string(body), `"code":"out_of_capacity"`) {
		t.Fatalf("expected out_of_capacity: %d %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, h.sess, "POST", "/v1/vms", spec("bx_a")); resp.StatusCode != 409 {
		t.Fatalf("duplicate: %d", resp.StatusCode)
	}
	// Freeing capacity allows a new VM; pausing blocks it again.
	h.waitState(t, "bx_b", proto.VMRunning)
	call(t, h.sess, "DELETE", "/v1/vms/bx_b", nil)
	if err := h.node.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	resp, body = call(t, h.sess, "POST", "/v1/vms", spec("bx_c"))
	if resp.StatusCode != 503 || !strings.Contains(string(body), "paused") {
		t.Fatalf("expected paused: %d %s", resp.StatusCode, body)
	}
	h.node.SetPaused(false)
	if resp, body := call(t, h.sess, "POST", "/v1/vms", spec("bx_c")); resp.StatusCode != 202 {
		t.Fatalf("after free: %d %s", resp.StatusCode, body)
	}
}

func TestPeriodicSnapshotAndDirect(t *testing.T) {
	h := start(t, func(c *Config) { c.SnapshotInterval = "100ms"; c.DirectHost = "127.0.0.1" })
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_d"))
	h.waitState(t, "bx_d", proto.VMRunning)
	waitFor(t, "periodic snapshot upload", func() bool { return len(h.srv.eventsOf(proto.EvSnapshotUploaded)) > 0 })

	_, body := call(t, h.sess, "GET", "/v1/vms/bx_d", nil)
	var st proto.VMStatus
	json.Unmarshal(body, &st)
	addr := st.Direct["ssh"]
	if addr == "" {
		t.Fatalf("no direct port: %s", body)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "ssh?\n")
	line, _ := readLineTimeout(bufio.NewReader(c), 3*time.Second)
	if line != "port22:ssh?\n" {
		t.Fatalf("direct echo %q", line)
	}
}

func TestUI(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("index: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, _ = http.Get(srv.URL + "/api/status")
	var st Status
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.NodeID != "node_test1" || st.Capacity.VCPU != 8 {
		t.Fatalf("status %+v", st)
	}
	waitFor(t, "connected in status", func() bool { return h.node.Status().Connection.Connected })

	// Writes need the custom header.
	resp, _ = http.Post(srv.URL+"/api/pause", "application/json", strings.NewReader(`{"paused":true}`))
	if resp.StatusCode != 403 {
		t.Fatalf("pause without header: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set(UIHeader, "1")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 || !h.node.config().Paused {
		t.Fatalf("pause: %d", resp.StatusCode)
	}
	cfg, _ := LoadConfig(h.cfg.DataDir)
	if !cfg.Paused {
		t.Fatal("pause not persisted")
	}
	// DNS-rebinding style host is rejected.
	req, _ = http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Host = "evil.example:8421"
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("evil host: %d", resp.StatusCode)
	}
}

func TestBenchmarkOverSession(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the quick benchmark")
	}
	h := start(t, nil)
	b, err := h.node.RunBenchmark(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if b.Network.DownMbps <= 0 || b.Network.Via != "loopback" {
		t.Fatalf("network over session: %+v", b.Network)
	}
	waitFor(t, "info resent", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for _, hb := range h.srv.beats {
			if hb.Info != nil && hb.Info.Bench.Network.DownMbps > 0 {
				return true
			}
		}
		return false
	})
}

func TestConfig(t *testing.T) {
	c, _ := LoadConfig(t.TempDir())
	for k, v := range map[string]string{"cpus": "6", "memory": "12G", "disk": "1T", "max-sandboxes": "2", "snapshot-interval": "5m", "server": "http://x:8420/"} {
		if err := c.Set(k, v); err != nil {
			t.Fatal(k, err)
		}
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, _ := LoadConfig(c.DataDir)
	cp := c2.Capacity()
	if cp.VCPU != 6 || cp.MemoryMiB != 12288 || cp.DiskGiB != 1024 || cp.MaxSandboxes != 2 || c2.Server != "http://x:8420" {
		t.Fatalf("capacity %+v server %q", cp, c2.Server)
	}
	if d, ok := c2.SnapshotEvery(); !ok || d != 5*time.Minute {
		t.Fatal("snapshot interval")
	}
	if c2.Set("snapshot-interval", "bogus") == nil || c2.Set("nope", "1") == nil || c2.Set("node-id", "x") == nil {
		t.Fatal("expected errors")
	}
	if connectURL("https://h:1/") != "wss://h:1/node/v1/connect" || connectURL("h:2") != "ws://h:2/node/v1/connect" {
		t.Fatal("connectURL")
	}
}

func TestNetConfig(t *testing.T) {
	c, _ := LoadConfig(t.TempDir())
	if v, _ := c.Get("net"); v != "netstack" {
		t.Fatalf("default net %q", v)
	}
	for k, v := range map[string]string{"net.allow-lan": "true", "net.allow": "192.168.1.0/24, 10.0.0.5", "net.deny": "8.8.8.8", "server": "http://srv:8420"} {
		if err := c.Set(k, v); err != nil {
			t.Fatal(k, err)
		}
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, _ := LoadConfig(c.DataDir)
	if !c2.Net.AllowLAN || len(c2.Net.Allow) != 2 || c2.Net.Deny[0] != "8.8.8.8" || c2.ServerAddr() != "srv:8420" {
		t.Fatalf("%+v %s", c2.Net, c2.ServerAddr())
	}
	if v, _ := c2.Get("net.allow"); v != "192.168.1.0/24,10.0.0.5" {
		t.Fatalf("net.allow %q", v)
	}
	if c2.Set("net", "tap") == nil || c2.Set("net.allow", "10.0.0.0/33") == nil || c2.Set("net.allow-lan", "maybe") == nil {
		t.Fatal("expected errors")
	}
	if c2.Set("net", "slirp") != nil || c2.Set("net.allow", "") != nil || c2.Net.Mode != "slirp" || c2.Net.Allow != nil {
		t.Fatal("set slirp / clear allow")
	}
	if (Config{Server: "https://h"}).ServerAddr() != "h:443" || (Config{}).ServerAddr() != "" {
		t.Fatal("ServerAddr defaults")
	}
	b, _ := json.Marshal(Config{})
	if strings.Contains(string(b), `"net"`) {
		t.Fatalf("empty net config serialized: %s", b)
	}
}

// vmState is the last vm.state the server got for id.
func (h *harness) vmState(id string) (st proto.VMStatus) {
	for _, e := range h.srv.eventsOf(proto.EvVMState) {
		if e.SandboxID == id {
			st = proto.VMStatus{}
			json.Unmarshal(e.Data, &st)
		}
	}
	return st
}

// TestGuestHealth: a guest that stops answering turns guestReady off while
// the VM runs (server: degraded, recover allowed); answering again or a
// recover turns it back on.
func TestGuestHealth(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_h"))
	h.waitState(t, "bx_h", proto.VMRunning)
	waitFor(t, "ready", func() bool { return h.vmState("bx_h").GuestReady }) // past the clone gate
	setHung := func(v bool) { h.guest.mu.Lock(); h.guest.hung = v; h.guest.mu.Unlock() }
	setHung(true)
	waitFor(t, "degraded vm.state", func() bool {
		st := h.vmState("bx_h")
		return st.State == proto.VMRunning && !st.GuestReady
	})
	_, body := call(t, h.sess, "GET", "/v1/vms/bx_h", nil)
	if !strings.Contains(string(body), `"guestReady":false`) {
		t.Fatalf("GET must report the hung guest: %s", body)
	}
	setHung(false)
	waitFor(t, "recovered", func() bool { st := h.vmState("bx_h"); return st.State == proto.VMRunning && st.GuestReady })

	// Hung again, then a periodic snapshot must not be attempted and a
	// stop skips prepare-stop (the guest would not answer).
	setHung(true)
	waitFor(t, "degraded again", func() bool { return !h.vmState("bx_h").GuestReady })
	resp, body := call(t, h.sess, "POST", "/v1/vms/bx_h/restart", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("restart: %d %s", resp.StatusCode, body)
	}
	setHung(false)
	waitFor(t, "ready after recover", func() bool { st := h.vmState("bx_h"); return st.State == proto.VMRunning && st.GuestReady })
}

// TestStopSequence: stop runs the guest's prepare-stop with the stop's
// force flag, and a snapshots-off VM is neither snapshotted nor uploaded.
func TestStopSequence(t *testing.T) {
	h := start(t, func(c *Config) { c.SnapshotInterval = "100ms" })
	h.srv.putImage("img1", []byte("base"))
	sp := spec("bx_off")
	sp.NoSnapshots = true
	call(t, h.sess, "POST", "/v1/vms", sp)
	call(t, h.sess, "POST", "/v1/vms", spec("bx_on"))
	h.waitState(t, "bx_off", proto.VMRunning)
	h.waitState(t, "bx_on", proto.VMRunning)
	waitFor(t, "periodic snapshot of bx_on", func() bool { return len(h.srv.eventsOf(proto.EvSnapshotUploaded)) > 0 })
	time.Sleep(300 * time.Millisecond)
	for _, e := range h.srv.eventsOf(proto.EvSnapshotUploaded) {
		if e.SandboxID == "bx_off" {
			t.Fatalf("snapshots-off VM was snapshotted: %s", e.Data)
		}
	}
	resp, body := call(t, h.sess, "POST", "/v1/vms/bx_on/stop", proto.StopRequest{SnapshotID: "snap_f", Force: true})
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d %s", resp.StatusCode, body)
	}
	h.guest.mu.Lock()
	got := h.guest.bodies["POST /v1/internal/prepare-stop"]
	h.guest.mu.Unlock()
	var ps proto.PrepareStopRequest
	if json.Unmarshal([]byte(got), &ps) != nil || !ps.Force || ps.Freeze {
		t.Fatalf("prepare-stop body %q (fake driver: force passed, no freeze)", got)
	}
	// Snapshots off and no snapshot id: the disk is discarded, nothing uploaded.
	resp, body = call(t, h.sess, "POST", "/v1/vms/bx_off/stop", proto.StopRequest{})
	var ref proto.SnapshotRef
	json.Unmarshal(body, &ref)
	if resp.StatusCode != 200 || ref.ID != "" {
		t.Fatalf("stop snapshots-off: %d %s", resp.StatusCode, body)
	}
}

// TestDiskOvercommit: sandbox sizes are limits on thin overlays, so a
// node sharing 40 GiB of real disk hosts three 51 GiB sandboxes.
func TestDiskOvercommit(t *testing.T) {
	reserve := 0
	h := start(t, func(c *Config) { c.DiskGiB, c.DiskReserveGiB, c.CPUs, c.MaxSandboxes = 40, &reserve, 16, 8 })
	h.srv.putImage("img1", []byte("base"))
	for _, id := range []string{"bx_1", "bx_2", "bx_3"} {
		if resp, body := call(t, h.sess, "POST", "/v1/vms", spec(id)); resp.StatusCode != 202 {
			t.Fatalf("create %s: %d %s", id, resp.StatusCode, body)
		}
	}
	// 4 x 40 = 160 GiB virtual: a 4th 51 GiB sandbox does not fit.
	resp, body := call(t, h.sess, "POST", "/v1/vms", spec("bx_4"))
	if resp.StatusCode != 503 || !strings.Contains(string(body), "overcommit") {
		t.Fatalf("expected out_of_capacity: %d %s", resp.StatusCode, body)
	}
	cp, ds := h.node.capacity()
	if cp.DiskGiB != 160 || ds.BudgetGiB != 40 || ds.HeadroomBytes <= 0 {
		t.Fatalf("capacity %+v disk %+v", cp, ds)
	}
	// The host's free space below the reserve: no new VM, no free capacity advertised.
	huge := 1 << 20
	h.node.cfgMu.Lock()
	h.node.cfg.DiskReserveGiB = &huge
	h.node.cfgMu.Unlock()
	h.node.diskMu.Lock()
	h.node.diskAt = time.Time{}
	h.node.diskMu.Unlock()
	call(t, h.sess, "DELETE", "/v1/vms/bx_3", nil)
	resp, body = call(t, h.sess, "POST", "/v1/vms", spec("bx_5"))
	if resp.StatusCode != 503 || !strings.Contains(string(body), "free disk") {
		t.Fatalf("expected no free disk: %d %s", resp.StatusCode, body)
	}
	if cp, _ := h.node.capacity(); cp.DiskGiB != h.node.Usage().DiskGiB {
		t.Fatalf("full node must advertise no free disk: %+v", cp)
	}
}

// LIFE-009 + CMD-012: ready waits for the guest's repository clones; the
// guest version is reported.
func TestCloneGateAndGuestVersion(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	setCloning := func(v bool) { h.guest.mu.Lock(); h.guest.cloning = v; h.guest.mu.Unlock() }
	setCloning(true)
	call(t, h.sess, "POST", "/v1/vms", spec("bx_c"))
	waitFor(t, "running, not ready", func() bool { st := h.vmState("bx_c"); return st.State == proto.VMRunning && st.GuestVersion == "g1.2" })
	time.Sleep(200 * time.Millisecond)
	if h.vmState("bx_c").GuestReady {
		t.Fatal("ready while the guest still clones")
	}
	setCloning(false)
	waitFor(t, "ready after clones", func() bool { return h.vmState("bx_c").GuestReady })
	// A clone started later (apply-config) does not take ready away.
	setCloning(true)
	time.Sleep(200 * time.Millisecond)
	if !h.vmState("bx_c").GuestReady {
		t.Fatal("gate closed again")
	}
}

// URL-012: user TCP ports on the direct host (tunnel fallback without a NIC).
func TestDirectPorts(t *testing.T) {
	h := start(t, func(c *Config) { c.DirectHost = "127.0.0.1" })
	h.srv.putImage("img1", []byte("base"))
	sp := spec("bx_p")
	sp.DirectPorts = []proto.DirectPort{{Port: 3000}}
	call(t, h.sess, "POST", "/v1/vms", sp)
	h.waitState(t, "bx_p", proto.VMRunning)
	waitFor(t, "tcp/3000 published", func() bool { return h.vmState("bx_p").Direct["tcp/3000"] != "" })
	echo := func(addr, want string) {
		t.Helper()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, "hi\n")
		if line, _ := readLineTimeout(bufio.NewReader(c), 3*time.Second); line != want {
			t.Fatalf("%s: %q, want %q", addr, line, want)
		}
	}
	echo(h.vmState("bx_p").Direct["tcp/3000"], "port3000:hi\n")
	resp, body := call(t, h.sess, "PUT", "/v1/vms/bx_p/direct-ports", proto.DirectPortsRequest{Ports: []proto.DirectPort{{Port: 8080, Proto: "tcp"}}})
	var st proto.VMStatus
	json.Unmarshal(body, &st)
	if resp.StatusCode != 200 || st.Direct["tcp/8080"] == "" || st.Direct["tcp/3000"] != "" || st.Direct["ssh"] == "" {
		t.Fatalf("set direct ports: %d %s", resp.StatusCode, body)
	}
	echo(st.Direct["tcp/8080"], "port8080:hi\n")
	// the guest firewall follows: host --public opens the TCP port (Boat)
	waitFor(t, "guest firewall 8080", func() bool {
		h.guest.mu.Lock()
		defer h.guest.mu.Unlock()
		return h.guest.bodies["PUT /v1/internal/firewall"] == `{"open":["8080/tcp"]}`
	})
	if resp, body := call(t, h.sess, "PUT", "/v1/vms/bx_p/direct-ports", proto.DirectPortsRequest{Ports: []proto.DirectPort{{Port: 0}}}); resp.StatusCode != 400 {
		t.Fatalf("bad port: %d %s", resp.StatusCode, body)
	}
}

// A guest that is not ready (degraded, rebooting) but still answers gets a
// forced prepare-stop; a hung one is not waited for. The driver's
// readiness is set directly, so no health-monitor timing is involved.
func TestStopUnreadyGuest(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	notReady := func(id string) {
		h.drv.mu.Lock()
		h.drv.vms[id].status.GuestReady = false
		h.drv.mu.Unlock()
		waitFor(t, id+" not ready", func() bool { st := h.vmState(id); return st.State == proto.VMRunning && !st.GuestReady })
	}
	prepared := func() string {
		h.guest.mu.Lock()
		defer h.guest.mu.Unlock()
		b := h.guest.bodies["POST /v1/internal/prepare-stop"]
		delete(h.guest.bodies, "POST /v1/internal/prepare-stop")
		return b
	}
	for _, id := range []string{"bx_u", "bx_hung"} {
		call(t, h.sess, "POST", "/v1/vms", spec(id))
		h.waitState(t, id, proto.VMRunning) // readiness is set below, not awaited
	}
	prepared()

	notReady("bx_u")
	if resp, body := call(t, h.sess, "POST", "/v1/vms/bx_u/stop", proto.StopRequest{SnapshotID: "snap_u"}); resp.StatusCode != 200 {
		t.Fatalf("stop: %d %s", resp.StatusCode, body)
	}
	var ps proto.PrepareStopRequest
	if got := prepared(); json.Unmarshal([]byte(got), &ps) != nil || !ps.Force {
		t.Fatalf("prepare-stop body %q, want force", got)
	}

	notReady("bx_hung")
	h.guest.mu.Lock()
	h.guest.hung = true
	h.guest.mu.Unlock()
	t0 := time.Now()
	if resp, body := call(t, h.sess, "POST", "/v1/vms/bx_hung/stop", proto.StopRequest{SnapshotID: "snap_h"}); resp.StatusCode != 200 {
		t.Fatalf("stop hung: %d %s", resp.StatusCode, body)
	}
	if got := prepared(); got != "" || time.Since(t0) > 8*time.Second {
		t.Fatalf("hung guest: prepare-stop %q after %s", got, time.Since(t0))
	}
}

// LIFE-013: with a lazy-capable driver the VM boots before the chain is
// downloaded; missing layers are readable through the blob proxy; once
// the download ends the driver switches and Hydrating turns false.
func TestLazyChain(t *testing.T) {
	h := start(t, nil)
	h.drv.mu.Lock()
	h.drv.lazy = true
	h.drv.mu.Unlock()
	if !h.node.Info().LazyChain {
		t.Fatal("NodeInfo.LazyChain")
	}
	h.srv.putImage("img1", []byte("base"))
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_a", ImageID: "img1"}, []byte("layer a data"))
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_b", ParentID: "snap_a", ImageID: "img1"}, []byte("layer b data"))
	if err := h.node.PullImage(context.Background(), "", "img1"); err != nil { // the image must be local
		t.Fatal(err)
	}
	hold := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.holdFull = hold // layer downloads wait
	h.srv.mu.Unlock()

	s := spec("bx_l")
	s.ParentID = "snap_b"
	call(t, h.sess, "POST", "/v1/vms", s)
	waitFor(t, "running while hydrating", func() bool { st := h.vmState("bx_l"); return st.State == proto.VMRunning && st.Hydrating })
	if h.drv.store.HasSnapshot("snap_a") || h.drv.store.HasSnapshot("snap_b") {
		t.Fatal("chain pulled before the boot")
	}
	h.drv.mu.Lock()
	reads := fmt.Sprint(h.drv.lazyReads)
	h.drv.mu.Unlock()
	if reads != "map[snap_a:206 layer snap_b:206 layer]" {
		t.Fatalf("lazy reads through the proxy: %s", reads)
	}
	p, _ := h.node.lazyURLs()
	if resp, err := http.Get(p.base + "/b/snapshot/snap_a/bad"); err != nil || resp.StatusCode != 403 {
		t.Fatalf("bad token: %v %v", resp, err)
	}
	close(hold)
	waitFor(t, "hydrated", func() bool { return !h.vmState("bx_l").Hydrating })
	if !h.drv.store.HasSnapshot("snap_a") || !h.drv.store.HasSnapshot("snap_b") {
		t.Fatal("chain not downloaded")
	}
	h.drv.mu.Lock()
	hyd := fmt.Sprint(h.drv.hydrated)
	h.drv.mu.Unlock()
	if hyd != "[bx_l]" {
		t.Fatalf("Hydrated calls: %s", hyd)
	}
}

// SIZE-012: a create of the pool's shape adopts a pre-booted VM.
func TestWarmPool(t *testing.T) {
	h := start(t, func(c *Config) { c.WarmPool = 1 })
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_w1")) // teaches the pool its shape
	h.waitState(t, "bx_w1", proto.VMRunning)
	waitFor(t, "pool vm ready", func() bool {
		h.node.mu.Lock()
		defer h.node.mu.Unlock()
		for _, w := range h.node.warm {
			return w.ready
		}
		return false
	})
	if u := h.node.Usage(); u.VMs != 1 || u.VCPU != 2 {
		t.Fatalf("reported usage must not count the pool vm: %+v", u)
	}
	for _, st := range h.node.Statuses() {
		if strings.HasPrefix(st.SandboxID, warmPrefix) {
			t.Fatalf("pool vm reported to the server: %+v", st)
		}
	}
	call(t, h.sess, "POST", "/v1/vms", spec("bx_w2"))
	waitFor(t, "adopted and ready", func() bool { st := h.vmState("bx_w2"); return st.State == proto.VMRunning && st.GuestReady })
	h.drv.mu.Lock()
	renames := strings.Join(h.drv.renames, ",")
	starts := 0
	for _, s := range h.drv.starts {
		if s.SandboxID == "bx_w2" {
			starts++
		}
	}
	h.drv.mu.Unlock()
	if !strings.HasPrefix(renames, warmPrefix) || !strings.HasSuffix(renames, "->bx_w2") || starts != 0 {
		t.Fatalf("not adopted: renames %q, direct starts %d", renames, starts)
	}
	h.guest.mu.Lock()
	adopted := h.guest.adopted
	h.guest.mu.Unlock()
	if adopted != "bx_w2" {
		t.Fatalf("guest adopt body: %q", adopted)
	}
	// A different shape boots normally; the pool refills.
	other := spec("bx_w3")
	other.Size.VCPU = 1
	call(t, h.sess, "POST", "/v1/vms", other)
	h.waitState(t, "bx_w3", proto.VMRunning)
}

// SIZE-012: pool VMs never cost a create its room: another shape evicts them.
func TestWarmPoolEviction(t *testing.T) {
	h := start(t, func(c *Config) { c.WarmPool = 1; c.CPUs = 4 })
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_e1"))
	waitFor(t, "pool vm ready", func() bool {
		h.node.mu.Lock()
		defer h.node.mu.Unlock()
		for _, w := range h.node.warm {
			return w.ready
		}
		return false
	})
	other := spec("bx_e2")
	other.Size.MemoryMiB = 2048 // another shape, needs the pool vm's vCPUs
	if resp, body := call(t, h.sess, "POST", "/v1/vms", other); resp.StatusCode != 202 {
		t.Fatalf("create next to the pool: %d %s", resp.StatusCode, body)
	}
	h.waitState(t, "bx_e2", proto.VMRunning)
	h.node.mu.Lock()
	n := len(h.node.warm)
	h.node.mu.Unlock()
	if n != 0 {
		t.Fatalf("pool vm kept: %d", n)
	}
}

// B1: an upload counts as in flight from the snapshot on; a 410 (deleted on
// the server) ends it at once and the node erases its copy.
func TestUploadInFlightAndGone(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_g"))
	h.waitState(t, "bx_g", proto.VMRunning)
	hold := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.holdPut, h.srv.gone = hold, map[string]bool{"snap_g1": true}
	h.srv.mu.Unlock()
	if resp, body := call(t, h.sess, "POST", "/v1/vms/bx_g/snapshot", proto.SnapshotRequest{SnapshotID: "snap_g1"}); resp.StatusCode != 200 {
		t.Fatalf("snapshot: %d %s", resp.StatusCode, body)
	}
	list := func(q string) []proto.NodeSnapshot {
		var out struct{ Snapshots []proto.NodeSnapshot }
		_, body := call(t, h.sess, "GET", "/v1/snapshots"+q, nil)
		json.Unmarshal(body, &out)
		return out.Snapshots
	}
	if l := list("?uploading=true"); len(l) != 1 || l[0].ID != "snap_g1" || l[0].SandboxID != "bx_g" || !l[0].Uploading {
		t.Fatalf("in flight right after the snapshot: %+v", l)
	}
	close(hold)
	waitFor(t, "snapshot.failed", func() bool { return len(h.srv.eventsOf(proto.EvSnapshotFailed)) > 0 })
	for _, ns := range list("") {
		if ns.ID == "snap_g1" {
			t.Fatalf("node kept a snapshot the server deleted: %+v", ns)
		}
	}
	if h.drv.Store().HasSnapshot("snap_g1") {
		t.Fatal("snapshot still in the store")
	}
	h.srv.mu.Lock()
	puts := h.srv.puts
	h.srv.mu.Unlock()
	if puts != 1 {
		t.Fatalf("upload retried after 410: %d PUTs", puts)
	}
}

// The driver's "vm not found" (qemu, after a stop) is a 404, so a server that
// restarted mid-stop reads the retried pre-save as "no machine" (sonnet-16 B1).
func TestWriteErrVMNotFound(t *testing.T) {
	for _, err := range []error{vm.ErrNotFound, fmt.Errorf("snapshot bx_1: %w", vm.ErrNotFound), errNotFound} {
		rec := httptest.NewRecorder()
		writeErr(rec, err)
		if rec.Code != 404 {
			t.Errorf("%v: %d", err, rec.Code)
		}
	}
}

// opus-18: a periodic layer is reported (with its parent) before its upload,
// and a layer whose parent the server has not stored yet uploads the parent
// first (joining its upload in flight).
func TestPeriodicReportedBeforeUpload(t *testing.T) {
	h := start(t, func(c *Config) { c.SnapshotInterval = "100ms" })
	h.srv.putImage("img1", []byte("base"))
	hold := make(chan struct{})
	h.srv.mu.Lock()
	h.srv.holdPut = hold
	h.srv.mu.Unlock()
	call(t, h.sess, "POST", "/v1/vms", spec("bx_p"))
	h.waitState(t, "bx_p", proto.VMRunning)
	var first, second proto.SnapshotRef
	waitFor(t, "two periodic layers reported", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for _, ref := range h.srv.reported {
			if ref.ParentID != "" {
				if p, ok := h.srv.reported[ref.ParentID]; ok {
					first, second = p, ref
					return true
				}
			}
		}
		return false
	})
	if second.SandboxID != "bx_p" {
		t.Fatalf("report without its sandbox: %+v", second)
	}
	var out struct{ Snapshots []proto.NodeSnapshot }
	_, body := call(t, h.sess, "GET", "/v1/snapshots?uploading=true", nil)
	json.Unmarshal(body, &out)
	found := false
	for _, ns := range out.Snapshots {
		if ns.ID == second.ID {
			found = ns.ParentID == first.ID && ns.Uploading
		}
	}
	if !found {
		t.Fatalf("in-flight list lacks %s with parent %s: %s", second.ID, first.ID, body)
	}
	h.srv.mu.Lock()
	puts := len(h.srv.putOrder)
	h.srv.mu.Unlock()
	if puts != 0 {
		t.Fatalf("uploads went through the hold: %d", puts)
	}
	close(hold)
	waitFor(t, "both stored", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		_, a := h.srv.snaps[first.ID]
		_, b := h.srv.snaps[second.ID]
		return a && b
	})
	h.srv.mu.Lock()
	order := append([]string(nil), h.srv.putOrder...)
	h.srv.mu.Unlock()
	if i, j := slices.Index(order, first.ID), slices.Index(order, second.ID); i < 0 || j < 0 || i > j {
		t.Fatalf("parent stored after its child: %v", order)
	}
}

// opus-19 N1: a restart cuts off an upload. The shutdown does not report
// it as failed, the restarted node does not upload on its own, and it
// uploads the layer (parent first) when the server asks, once.
func TestUploadRequestedAfterRestart(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_r"))
	h.waitState(t, "bx_r", proto.VMRunning)
	h.srv.mu.Lock()
	h.srv.failPut = true // the uploads retry (after a backoff) until the restart
	h.srv.mu.Unlock()
	for _, id := range []string{"snap_r1", "snap_r2"} {
		if resp, body := call(t, h.sess, "POST", "/v1/vms/bx_r/snapshot", proto.SnapshotRequest{SnapshotID: id}); resp.StatusCode != 200 {
			t.Fatalf("snapshot %s: %d %s", id, resp.StatusCode, body)
		}
	}
	waitFor(t, "uploads under way", func() bool { h.srv.mu.Lock(); defer h.srv.mu.Unlock(); return h.srv.puts > 0 })
	h.restart(t)
	h.srv.mu.Lock()
	h.srv.failPut = false
	h.srv.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	h.srv.mu.Lock()
	stored, puts := len(h.srv.snaps), h.srv.puts
	h.srv.mu.Unlock()
	if stored != 0 {
		t.Fatalf("stored %d layers before the server asked", stored)
	}
	if evs := h.srv.eventsOf(proto.EvSnapshotFailed); len(evs) != 0 {
		t.Fatalf("a shutdown reported its cut-off uploads as failed: %+v", evs)
	}
	if resp, _ := call(t, h.sess, "POST", "/v1/snapshots/snap_nope/upload", nil); resp.StatusCode != 404 {
		t.Fatalf("upload of an unknown snapshot: %d", resp.StatusCode)
	}
	for range 2 {
		if resp, body := call(t, h.sess, "POST", "/v1/snapshots/snap_r2/upload", nil); resp.StatusCode != 202 {
			t.Fatalf("upload request: %d %s", resp.StatusCode, body)
		}
	}
	waitFor(t, "both stored", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		_, a := h.srv.snaps["snap_r1"]
		_, b := h.srv.snaps["snap_r2"]
		return a && b
	})
	time.Sleep(100 * time.Millisecond)
	h.srv.mu.Lock()
	order, n := append([]string(nil), h.srv.putOrder...), h.srv.puts-puts
	h.srv.mu.Unlock()
	if !slices.Equal(order, []string{"snap_r1", "snap_r2"}) || n != 2 {
		t.Fatalf("uploads after the request: order %v, %d PUTs (want parent first, once each)", order, n)
	}
	if len(h.srv.eventsOf(proto.EvSnapshotUploaded)) != 2 {
		t.Fatalf("snapshot.uploaded events: %d", len(h.srv.eventsOf(proto.EvSnapshotUploaded)))
	}
}
