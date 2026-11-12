package servertest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// NodeOptions configures a FakeNode.
type NodeOptions struct {
	Name     string
	Capacity proto.Capacity // default 16 vCPU, 64 GiB, 1000 GiB, 20 VMs
	Labels   map[string]string
	Paused   bool             // the owner paused the node (no new sandboxes)
	Bench    *proto.Benchmark // default: silver tier, no GPU
	// PassthroughGPUs: GPUs the node can hand to VMs (VFIO).
	PassthroughGPUs []proto.PassthroughGPU
	Images          []string
	// BootDelay is the time from POST /v1/vms to running+guestReady (default 30 ms).
	BootDelay time.Duration
	// BootError makes boots fail with this vm.state error.
	BootError string
	// Guest serves guest calls (/v1/<rest>; header X-Sandbox-Id); default FakeGuest.
	Guest http.Handler
	// Tunnel handles raw tunnels to guest ports; default echoes bytes back.
	Tunnel func(sandboxID string, port int, c net.Conn)
	// HeartbeatInterval (default 100 ms).
	HeartbeatInterval time.Duration
	// NoUpload: do not upload snapshots taken by stop/snapshot.
	NoUpload bool
	// NoSnapshotReport: PeriodicSnapshot does not report the layer before
	// its upload (POST /node/v1/snapshots), like an older node or one whose
	// report failed.
	NoSnapshotReport bool
	// JoinToken (default: a new token from the env).
	JoinToken string
	// Routes adds handlers to the fake node API (e.g. a feature's node endpoints).
	Routes func(m *http.ServeMux)
	// GuestVersion is reported as VMStatus.GuestVersion once the guest is
	// ready (what the real node reads from guest /v1/health; "" = "dev").
	GuestVersion string
	// LazyChain: boot from a snapshot chain that is not local (VMStatus
	// Hydrating) until the test calls FinishHydration (LIFE-013).
	LazyChain bool
	// GuestChannel: advertise guest-channel boots (the server then sends a
	// boot stub; a test runs the guest side itself).
	GuestChannel bool
	// DirectHost: publish direct ports (ssh, VMSpec.DirectPorts, PUT
	// /v1/vms/{id}/direct-ports) as DirectHost:<port>; "" = only the fake
	// ssh endpoint, and direct-ports answers 409 direct_disabled.
	DirectHost string
}

// FakeNode is an in-process node: it registers, connects over yamux like the
// real node and simulates VMs and snapshots.
type FakeNode struct {
	ID, Token string
	opts      NodeOptions
	server    string // http://host:port
	info      proto.NodeInfo

	mu        sync.Mutex
	sess      *yamux.Session
	client    *http.Client
	vms       map[string]*fakeVM
	snapshots map[string]proto.SnapshotRef
	uploading map[string]string // snapshot -> sandbox, until its upload ended
	hold      chan struct{}     // HoldUploads: uploads wait until it is closed
	held      []chan struct{}   // LetNewUploadsThrough: holds of waiting uploads
	images    map[string]bool
	// uploadRequests: snapshots the server asked to upload (POST
	// /v1/snapshots/{id}/upload).
	uploadRequests []string
	restarts       int // Restart: uploads begun before it never finish
	calls          []string
	lastHB         proto.HeartbeatResponse
	closed         bool
	stopHB         chan struct{}
	wg             sync.WaitGroup
	// FailNext makes the next call whose "METHOD path" has this prefix fail with 500.
	failNext []string
	refuse   []refusal
	dieNext  []string // DieOnNext
}

type refusal struct {
	prefix string
	status int
	body   any
}

type fakeVM struct {
	spec   proto.VMSpec
	status proto.VMStatus
	base   string // current overlay's parent snapshot
	boots  int
	merge  bool // MergeNextSnapshot
}

// StartNode registers and connects a fake node to the env's server.
func (e *Env) StartNode(o NodeOptions) *FakeNode {
	e.T.Helper()
	if o.JoinToken == "" {
		o.JoinToken = e.JoinToken()
	}
	n, err := StartFakeNode(e.URL, o)
	if err != nil {
		e.T.Fatal(err)
	}
	e.nodes = append(e.nodes, n)
	e.Eventually("node "+n.ID+" online", func() bool { return e.S.NodeConn(n.ID) != nil })
	return n
}

// StartFakeNode registers with serverURL using o.JoinToken and connects.
func StartFakeNode(serverURL string, o NodeOptions) (*FakeNode, error) {
	if o.Name == "" {
		o.Name = "fake-" + strconv.FormatInt(time.Now().UnixNano()%100000, 10)
	}
	if o.Capacity == (proto.Capacity{}) {
		o.Capacity = proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 20}
	}
	if o.BootDelay == 0 {
		o.BootDelay = 30 * time.Millisecond
	}
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = 100 * time.Millisecond
	}
	if o.Guest == nil {
		o.Guest = NewFakeGuest()
	}
	if o.GuestVersion == "" {
		o.GuestVersion = "dev"
	}
	bench := proto.Benchmark{Tier: "silver", Score: 500, Categories: map[string]string{"cpu": "medium", "memory": "medium", "disk": "medium", "network": "high", "gpu": "none"},
		Virtualization: proto.VirtInfo{Accel: "kvm"}}
	if o.Bench != nil {
		bench = *o.Bench
	}
	n := &FakeNode{opts: o, server: strings.TrimRight(serverURL, "/"), vms: map[string]*fakeVM{}, snapshots: map[string]proto.SnapshotRef{}, uploading: map[string]string{}, images: map[string]bool{}}
	for _, im := range o.Images {
		n.images[im] = true
	}
	n.info = proto.NodeInfo{Name: o.Name, Version: "fake", OS: "linux", Arch: "amd64", Driver: "fake", Labels: o.Labels, Bench: bench, Capacity: o.Capacity, Images: o.Images, DirectHost: o.DirectHost, LazyChain: o.LazyChain, GPUs: o.PassthroughGPUs, Paused: o.Paused, GuestChannel: o.GuestChannel}
	body, _ := json.Marshal(proto.RegisterRequest{JoinToken: o.JoinToken, Info: n.info})
	resp, err := http.Post(n.server+"/node/v1/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register: %d %s", resp.StatusCode, b)
	}
	var rr proto.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, err
	}
	n.ID, n.Token = rr.NodeID, rr.NodeToken
	n.info.NodeID = n.ID
	return n, n.Connect()
}

// Connect (re)dials the server and serves the node API.
func (n *FakeNode) Connect() error {
	ws := "ws" + strings.TrimPrefix(n.server, "http") + "/node/v1/connect"
	sess, err := mux.Dial(context.Background(), ws, http.Header{"Authorization": {"Bearer " + n.Token}})
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.sess, n.client, n.closed = sess, mux.Client(sess), false
	n.stopHB = make(chan struct{})
	stop := n.stopHB
	n.mu.Unlock()
	n.wg.Add(2)
	go func() {
		defer n.wg.Done()
		_ = mux.Serve(sess, n.handler())
	}()
	go func() {
		defer n.wg.Done()
		t := time.NewTicker(n.opts.HeartbeatInterval)
		defer t.Stop()
		n.heartbeat()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if sess.IsClosed() {
					return
				}
				n.heartbeat()
			}
		}
	}()
	return nil
}

// Disconnect drops the session (the node goes offline; VMs keep running).
func (n *FakeNode) Disconnect() {
	n.mu.Lock()
	sess, stop := n.sess, n.stopHB
	n.sess, n.stopHB = nil, nil
	n.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if sess != nil {
		_ = sess.Close()
	}
	n.wg.Wait()
}

// Close disconnects for good.
func (n *FakeNode) Close() {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	n.Disconnect()
}

// Calls returns the "METHOD path" of every server -> node request so far.
func (n *FakeNode) Calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

// Called reports whether a call with this prefix happened.
func (n *FakeNode) Called(prefix string) bool {
	for _, c := range n.Calls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// FailNext makes the next call matching prefix fail with HTTP 500.
func (n *FakeNode) FailNext(prefix string) {
	n.mu.Lock()
	n.failNext = append(n.failNext, prefix)
	n.mu.Unlock()
}

// DieOnNext makes the node drop its session on the next call matching
// prefix, without answering it (the node died under that call).
func (n *FakeNode) DieOnNext(prefix string) {
	n.mu.Lock()
	n.dieNext = append(n.dieNext, prefix)
	n.mu.Unlock()
}

// DropVM forgets VM id, as a node that restarted without it.
func (n *FakeNode) DropVM(id string) {
	n.mu.Lock()
	delete(n.vms, id)
	n.mu.Unlock()
}

// RefuseNext makes the next call whose "METHOD path" has this prefix answer
// status with body (JSON), e.g. 503 out_of_capacity for "POST /v1/vms".
func (n *FakeNode) RefuseNext(prefix string, status int, body any) {
	n.mu.Lock()
	n.refuse = append(n.refuse, refusal{prefix, status, body})
	n.mu.Unlock()
}

// VM returns the status of a VM on this node.
func (n *FakeNode) VM(id string) (proto.VMStatus, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.vms[id]
	if !ok {
		return proto.VMStatus{}, false
	}
	return v.status, true
}

// Spec returns the VMSpec a VM was last booted with.
func (n *FakeNode) Spec(id string) (proto.VMSpec, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.vms[id]
	if !ok {
		return proto.VMSpec{}, false
	}
	return v.spec, true
}

// HasSnapshot reports a snapshot in the node's local store.
func (n *FakeNode) HasSnapshot(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.snapshots[id]
	return ok
}

// AddLocalSnapshot puts a snapshot into the node's local store (e.g. one an
// older build leaked), as if the node had taken or pulled it.
func (n *FakeNode) AddLocalSnapshot(ref proto.SnapshotRef) {
	n.mu.Lock()
	n.snapshots[ref.ID] = ref
	n.mu.Unlock()
}

// LastHeartbeatResponse returns the server's last heartbeat reply.
func (n *FakeNode) LastHeartbeatResponse() proto.HeartbeatResponse {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastHB
}

// SetVMState changes a VM's state and reports it (vm.state event).
func (n *FakeNode) SetVMState(id, state string, guestReady bool, errMsg string) {
	n.mu.Lock()
	v, ok := n.vms[id]
	if ok {
		v.status.State, v.status.GuestReady, v.status.Error = state, guestReady, errMsg
		if guestReady {
			v.status.GuestVersion = n.opts.GuestVersion
		}
	}
	var st proto.VMStatus
	if ok {
		st = v.status
	}
	n.mu.Unlock()
	if ok {
		n.sendEvent(proto.Event{SandboxID: id, Type: proto.EvVMState, Data: mustRaw(st)})
	}
}

// FinishHydration downloads the lazily booted VM's chain and reports
// hydrating:false (LIFE-013).
func (n *FakeNode) FinishHydration(id string) error {
	spec, ok := n.Spec(id)
	if !ok {
		return errors.New("vm not found")
	}
	var chain []string
	for p := spec.ParentID; p != "" && !n.HasSnapshot(p); {
		chain = append([]string{p}, chain...)
		var ref proto.SnapshotRef
		if err := n.do("GET", "/node/v1/snapshots/"+p, nil, &ref); err != nil {
			return err
		}
		p = ref.ParentID
	}
	for _, p := range chain {
		if err := n.fetchSnapshot(p); err != nil {
			return err
		}
	}
	n.mu.Lock()
	v, ok := n.vms[id]
	var st proto.VMStatus
	if ok {
		v.status.Hydrating = false
		st = v.status
	}
	n.mu.Unlock()
	if ok {
		n.sendEvent(proto.Event{SandboxID: id, Type: proto.EvVMState, Data: mustRaw(st)})
	}
	return nil
}

// SendGuestEvent relays a guest event as the node would.
func (n *FakeNode) SendGuestEvent(sandboxID, typ string, data any) {
	ge := proto.GuestEvent{Time: time.Now().UTC(), Type: typ, Data: mustRaw(data)}
	n.sendEvent(proto.Event{SandboxID: sandboxID, Type: proto.EvGuest, Data: mustRaw(ge)})
}

// PeriodicSnapshot takes a node-initiated live snapshot (own id) and uploads
// it (in the background while uploads are held).
func (n *FakeNode) PeriodicSnapshot(sandboxID string) (string, error) {
	id := api.NewID(api.PrefixSnapshot, 20)
	ref, err := n.snapshot(sandboxID, id, false)
	if err != nil {
		return "", err
	}
	if !n.opts.NoSnapshotReport { // like the real node: before it returns
		if err := n.do("POST", "/node/v1/snapshots", ref, nil); err != nil {
			return "", err
		}
	}
	n.uploadAsync(sandboxID, ref, true)
	return ref.ID, nil
}

// HoldUploads makes snapshot uploads wait until ReleaseUploads; they count
// as in flight meanwhile (GET /v1/snapshots), like a slow real upload.
func (n *FakeNode) HoldUploads() {
	n.mu.Lock()
	if n.hold == nil {
		n.hold = make(chan struct{})
	}
	n.mu.Unlock()
}

// ReleaseUploads lets held uploads go on.
func (n *FakeNode) ReleaseUploads() {
	n.mu.Lock()
	hs := append(n.held, n.hold)
	n.hold, n.held = nil, nil
	n.mu.Unlock()
	for _, h := range hs {
		if h != nil {
			close(h)
		}
	}
}

// LetNewUploadsThrough: uploads held so far keep waiting (until
// ReleaseUploads); later ones go through.
func (n *FakeNode) LetNewUploadsThrough() {
	n.mu.Lock()
	if n.hold != nil {
		n.held = append(n.held, n.hold)
		n.hold = nil
	}
	n.mu.Unlock()
}

// MergeNextSnapshot: the VM's next snapshot absorbs its current parent
// (tiered compaction): its parent is the grandparent and it supersedes the
// parent.
func (n *FakeNode) MergeNextSnapshot(sandboxID string) {
	n.mu.Lock()
	if v, ok := n.vms[sandboxID]; ok {
		v.merge = true
	}
	n.mu.Unlock()
}

// Restart simulates a node daemon restart: uploads in flight are lost
// (they never finish and send no event), the session drops and comes
// back. Local snapshots and VMs stay, and so does HoldUploads for the
// uploads that start afterwards.
func (n *FakeNode) Restart() error {
	n.mu.Lock()
	n.restarts++
	n.uploading = map[string]string{}
	old := append(n.held, n.hold)
	n.held = nil
	if n.hold != nil {
		n.hold = make(chan struct{})
	}
	n.mu.Unlock()
	for _, h := range old {
		if h != nil {
			close(h)
		}
	}
	n.Disconnect()
	return n.Connect()
}

// UploadRequests returns the snapshots the server asked this node to
// upload so far.
func (n *FakeNode) UploadRequests() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.uploadRequests...)
}

// Uploading reports whether snapshot id's upload has not ended yet.
func (n *FakeNode) Uploading(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.uploading[id]
	return ok
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (n *FakeNode) do(method, path string, body any, out any, hdr ...string) error {
	n.mu.Lock()
	c := n.client
	n.mu.Unlock()
	if c == nil {
		return errors.New("fake node: not connected")
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		rd = bytes.NewReader(mustRaw(b))
	}
	req, _ := http.NewRequest(method, "http://server"+path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (n *FakeNode) sendEvent(ev proto.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	_ = n.do("POST", "/node/v1/events", []proto.Event{ev}, nil)
}

func (n *FakeNode) heartbeat() {
	n.mu.Lock()
	hb := proto.Heartbeat{}
	for _, v := range n.vms {
		hb.VMs = append(hb.VMs, v.status)
		if v.status.State == proto.VMRunning || v.status.State == proto.VMStarting {
			hb.Usage.VCPU += v.spec.Size.VCPU
			hb.Usage.MemoryMiB += v.spec.Size.MemoryMiB
			hb.Usage.DiskGiB += v.spec.Size.DiskGiB
			hb.Usage.VMs++
		}
	}
	n.mu.Unlock()
	var resp proto.HeartbeatResponse
	if n.do("POST", "/node/v1/heartbeat", hb, &resp) == nil {
		n.mu.Lock()
		n.lastHB = resp
		n.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// node API (server -> node)
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (n *FakeNode) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		info := n.info
		for im := range n.images {
			if !slices.Contains(info.Images, im) {
				info.Images = append(info.Images, im)
			}
		}
		n.mu.Unlock()
		writeJSON(w, 200, info)
	})
	m.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		var out []proto.VMStatus
		for _, v := range n.vms {
			out = append(out, v.status)
		}
		n.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /v1/vms", n.handleCreate)
	m.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, ok := n.VM(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, st)
	})
	m.HandleFunc("PUT /v1/vms/{id}/direct-ports", func(w http.ResponseWriter, r *http.Request) {
		var req proto.DirectPortsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if n.opts.DirectHost == "" {
			writeJSON(w, 409, map[string]any{"ok": false, "code": "direct_disabled", "message": "no direct host"})
			return
		}
		n.mu.Lock()
		v, ok := n.vms[r.PathValue("id")]
		var st proto.VMStatus
		if ok {
			n.publish(v, req.Ports)
			st = v.status
		}
		n.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, st)
	})
	m.HandleFunc("POST /v1/vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		var req proto.StopRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := r.PathValue("id")
		ref, err := n.snapshot(id, req.SnapshotID, true)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, ref)
		n.uploadAsync(id, ref, false)
	})
	m.HandleFunc("POST /v1/vms/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req proto.SnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := r.PathValue("id")
		ref, err := n.snapshot(id, req.SnapshotID, false)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, ref)
		n.uploadAsync(id, ref, false)
	})
	m.HandleFunc("POST /v1/vms/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		n.mu.Lock()
		v, ok := n.vms[id]
		if ok {
			v.status.State, v.status.GuestReady = proto.VMStarting, false
			v.boots++
		}
		n.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, v.status)
		go func() {
			time.Sleep(n.opts.BootDelay)
			n.SetVMState(id, proto.VMRunning, true, "")
		}()
	})
	m.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		_, ok := n.vms[r.PathValue("id")]
		delete(n.vms, r.PathValue("id"))
		n.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("/v1/vms/{id}/guest/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		st, ok := n.VM(id)
		if !ok || st.State != proto.VMRunning {
			writeJSON(w, 409, map[string]string{"error": "machine_not_running"})
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/v1/" + r.PathValue("rest")
		r2.Header.Set("X-Sandbox-Id", id)
		n.opts.Guest.ServeHTTP(w, r2)
	})
	m.HandleFunc("GET /v1/vms/{id}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		port, _ := strconv.Atoi(r.URL.Query().Get("port"))
		if _, ok := n.VM(id); !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		c, rw, err := mux.HijackUpgrade(w)
		if err != nil {
			return
		}
		conn := &rwConn{Conn: c, r: rw.Reader}
		if n.opts.Tunnel != nil {
			n.opts.Tunnel(id, port, conn)
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	})
	m.HandleFunc("POST /v1/images/{id}/fetch", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := n.download("image", id); err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		n.mu.Lock()
		n.images[id] = true
		n.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("POST /v1/snapshots/{id}/fetch", func(w http.ResponseWriter, r *http.Request) {
		if err := n.fetchSnapshot(r.PathValue("id")); err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("POST /v1/snapshots/{id}/upload", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		n.mu.Lock()
		ref, ok := n.snapshots[id]
		_, up := n.uploading[id]
		if ok && !up && !n.opts.NoUpload {
			n.uploading[id] = ref.SandboxID
		}
		n.uploadRequests = append(n.uploadRequests, id)
		n.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		if !up {
			n.uploadAsync(ref.SandboxID, ref, false)
		}
		writeJSON(w, 202, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		list := []proto.NodeSnapshot{}
		for id, sb := range n.uploading {
			list = append(list, proto.NodeSnapshot{ID: id, SandboxID: sb, ParentID: n.snapshots[id].ParentID, Uploading: true})
		}
		if r.URL.Query().Get("uploading") != "true" {
			for id, ref := range n.snapshots {
				if _, up := n.uploading[id]; !up {
					list = append(list, proto.NodeSnapshot{ID: id, SandboxID: ref.SandboxID, ParentID: ref.ParentID})
				}
			}
		}
		n.mu.Unlock()
		writeJSON(w, 200, map[string]any{"snapshots": list})
	})
	m.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		delete(n.snapshots, r.PathValue("id"))
		n.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	if n.opts.Routes != nil {
		n.opts.Routes(m)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		n.mu.Lock()
		n.calls = append(n.calls, call)
		fail := false
		for i, p := range n.failNext {
			if strings.HasPrefix(call, p) {
				n.failNext = append(n.failNext[:i], n.failNext[i+1:]...)
				fail = true
				break
			}
		}
		die := false
		for i, p := range n.dieNext {
			if strings.HasPrefix(call, p) {
				n.dieNext = append(n.dieNext[:i], n.dieNext[i+1:]...)
				die = true
				break
			}
		}
		var ref *refusal
		for i, x := range n.refuse {
			if strings.HasPrefix(call, x.prefix) {
				x := x
				ref = &x
				n.refuse = append(n.refuse[:i], n.refuse[i+1:]...)
				break
			}
		}
		n.mu.Unlock()
		if die { // the node dies under this call: no answer
			go n.Disconnect()
			<-r.Context().Done()
			return
		}
		if fail {
			writeJSON(w, 500, map[string]string{"error": "injected failure"})
			return
		}
		if ref != nil {
			writeJSON(w, ref.status, ref.body)
			return
		}
		m.ServeHTTP(w, r)
	})
}

type rwConn struct {
	net.Conn
	r io.Reader
}

func (c *rwConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (n *FakeNode) handleCreate(w http.ResponseWriter, r *http.Request) {
	var spec proto.VMSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	n.mu.Lock()
	hydrating := false
	if spec.ParentID != "" {
		if _, ok := n.snapshots[spec.ParentID]; !ok && !n.opts.LazyChain {
			n.mu.Unlock()
			writeJSON(w, 409, map[string]string{"error": "parent snapshot " + spec.ParentID + " is not on this node"})
			return
		} else if !ok {
			hydrating = true
		}
	}
	if old, ok := n.vms[spec.SandboxID]; ok && old.status.State == proto.VMRunning {
		n.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "vm already running"})
		return
	}
	v := &fakeVM{spec: spec, base: spec.ParentID, status: proto.VMStatus{SandboxID: spec.SandboxID, State: proto.VMStarting, StartedAt: time.Now().UTC(),
		Direct: map[string]string{"ssh": "127.0.0.1:2" + strconv.Itoa(len(n.vms)+1000)}, Hydrating: hydrating}, boots: 1}
	n.publish(v, spec.DirectPorts)
	n.vms[spec.SandboxID] = v
	st := v.status
	n.mu.Unlock()
	writeJSON(w, 202, st)
	go func() {
		time.Sleep(n.opts.BootDelay)
		if n.opts.BootError != "" {
			n.SetVMState(spec.SandboxID, proto.VMError, false, n.opts.BootError)
			return
		}
		n.SetVMState(spec.SandboxID, proto.VMRunning, true, "")
	}()
}

// publish replaces the VM's direct ports (n.mu held): DirectHost:<port>.
func (n *FakeNode) publish(v *fakeVM, ports []proto.DirectPort) {
	if n.opts.DirectHost == "" {
		return
	}
	d := map[string]string{"ssh": v.status.Direct["ssh"]}
	for _, p := range ports {
		pr := p.Proto
		if pr == "" {
			pr = "tcp"
		}
		d[pr+"/"+strconv.Itoa(p.Port)] = n.opts.DirectHost + ":" + strconv.Itoa(p.Port)
	}
	v.status.Direct = d
}

// snapshot freezes the VM's overlay as snapshot id; stop also removes the VM.
func (n *FakeNode) snapshot(sandboxID, id string, stop bool) (proto.SnapshotRef, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.vms[sandboxID]
	if !ok {
		return proto.SnapshotRef{}, errors.New("vm not found")
	}
	if id == "" {
		id = api.NewID(api.PrefixSnapshot, 20)
	}
	parent, merged := v.base, []string(nil)
	if v.merge && v.base != "" {
		parent, merged, v.merge = n.snapshots[v.base].ParentID, []string{v.base}, false
	}
	ref := proto.SnapshotRef{ID: id, ParentID: parent, ImageID: v.spec.ImageID, SizeBytes: int64(len(snapContent(id, parent))), CreatedAt: time.Now().UTC(), SandboxID: sandboxID, Supersedes: merged}
	n.snapshots[id] = ref
	if !n.opts.NoUpload {
		n.uploading[id] = sandboxID // in flight before the caller hears of it, like the real node
	}
	if stop {
		delete(n.vms, sandboxID)
	} else {
		v.base = id
	}
	return ref, nil
}

func snapContent(id, parent string) []byte { return []byte("fake-snapshot:" + id + ":" + parent) }

// uploadAsync uploads ref in the background, held by the hold in place
// now (HoldUploads); unheld and wait, it uploads before returning.
func (n *FakeNode) uploadAsync(sandboxID string, ref proto.SnapshotRef, wait bool) {
	n.mu.Lock()
	h, gen := n.hold, n.restarts
	n.mu.Unlock()
	if h == nil && wait {
		n.upload(sandboxID, ref, nil, gen)
		return
	}
	go n.upload(sandboxID, ref, h, gen)
}

func (n *FakeNode) upload(sandboxID string, ref proto.SnapshotRef, h chan struct{}, gen int) {
	if n.opts.NoUpload {
		return
	}
	if h != nil {
		<-h
	}
	n.mu.Lock()
	lost := gen != n.restarts
	n.mu.Unlock()
	if lost { // the process that ran this upload is gone (Restart)
		return
	}
	data := snapContent(ref.ID, ref.ParentID)
	sum := sha256.Sum256(data)
	ref.SHA256 = hex.EncodeToString(sum[:])
	err := n.do("PUT", "/node/v1/blobs/snapshot/"+ref.ID, data, nil, "X-Crowdvm-Sha256", ref.SHA256, "X-Crowdvm-Snapshot", string(mustRaw(ref)))
	n.mu.Lock()
	delete(n.uploading, ref.ID)
	if err != nil && strings.Contains(err.Error(), ": 410 ") { // deleted on the server: keep nothing
		delete(n.snapshots, ref.ID)
	}
	n.mu.Unlock()
	if err != nil {
		n.sendEvent(proto.Event{SandboxID: sandboxID, Type: proto.EvSnapshotFailed, Data: mustRaw(map[string]string{"snapshotId": ref.ID, "error": err.Error()})})
		return
	}
	n.sendEvent(proto.Event{SandboxID: sandboxID, Type: proto.EvSnapshotUploaded, Data: mustRaw(ref)})
}

func (n *FakeNode) download(kind, id string) ([]byte, error) {
	n.mu.Lock()
	c := n.client
	n.mu.Unlock()
	if c == nil {
		return nil, errors.New("not connected")
	}
	resp, err := c.Get("http://server/node/v1/blobs/" + kind + "/" + id)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s/%s: %d", kind, id, resp.StatusCode)
	}
	b, err := io.ReadAll(bufio.NewReader(resp.Body))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if want := resp.Header.Get("X-Crowdvm-Sha256"); want != "" && want != hex.EncodeToString(sum[:]) {
		return nil, errors.New("sha256 mismatch")
	}
	return b, nil
}

func (n *FakeNode) fetchSnapshot(id string) error {
	if n.HasSnapshot(id) {
		return nil
	}
	var ref proto.SnapshotRef
	if err := n.do("GET", "/node/v1/snapshots/"+id, nil, &ref); err != nil {
		return err
	}
	if ref.ParentID != "" && !n.HasSnapshot(ref.ParentID) {
		return fmt.Errorf("parent %s missing", ref.ParentID)
	}
	if _, err := n.download("snapshot", id); err != nil {
		return err
	}
	n.mu.Lock()
	n.snapshots[id] = ref
	n.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Fake guest
// ---------------------------------------------------------------------------

// FakeGuest is a tiny guest agent: commands (sync and NDJSON stream), files
// (JSON and raw), prompts and the history export the server polls
// (GET /v1/internal/history/export + POST /v1/internal/history/ack).
type FakeGuest struct {
	mu      sync.Mutex
	files   map[string][]byte // key: sandbox|path
	history map[string]*proto.HistoryExport
	acked   map[string]int
	mux     *http.ServeMux
}

// AddHistory queues command records, output chunks and samples for the
// server's next history export pull of sandboxID.
func (g *FakeGuest) AddHistory(sandboxID string, ex proto.HistoryExport) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur := g.history[sandboxID]
	if cur == nil {
		cur = &proto.HistoryExport{}
		g.history[sandboxID] = cur
	}
	cur.Records = append(cur.Records, ex.Records...)
	cur.Chunks = append(cur.Chunks, ex.Chunks...)
	cur.Samples = append(cur.Samples, ex.Samples...)
	cur.Batch = "batch-" + strconv.Itoa(len(cur.Records)+len(cur.Chunks)+len(cur.Samples))
}

// HistoryAcked reports how many export batches of sandboxID the server acked.
func (g *FakeGuest) HistoryAcked(sandboxID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.acked[sandboxID]
}

// NewFakeGuest returns the default fake guest handler.
func NewFakeGuest() *FakeGuest {
	g := &FakeGuest{files: map[string][]byte{}, history: map[string]*proto.HistoryExport{}, acked: map[string]int{}, mux: http.NewServeMux()}
	g.mux.HandleFunc("POST /v1/commands", g.command)
	g.mux.HandleFunc("GET /v1/internal/history/export", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		ex := proto.HistoryExport{Records: []proto.CommandRecord{}, Chunks: []proto.HistoryChunk{}, Samples: []proto.GuestSample{}}
		if cur := g.history[r.Header.Get("X-Sandbox-Id")]; cur != nil {
			ex = *cur
		}
		g.mu.Unlock()
		api.WriteOK(w, 200, "history.export", ex)
	})
	g.mux.HandleFunc("POST /v1/internal/history/ack", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Batch string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		sb := r.Header.Get("X-Sandbox-Id")
		g.mu.Lock()
		if cur := g.history[sb]; cur != nil && cur.Batch == b.Batch {
			delete(g.history, sb)
			g.acked[sb]++
		}
		g.mu.Unlock()
		api.WriteOK(w, 200, "history.acked", nil)
	})
	g.mux.HandleFunc("GET /v1/files", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		b, ok := g.files[r.Header.Get("X-Sandbox-Id")+"|"+r.URL.Query().Get("path")]
		g.mu.Unlock()
		if !ok {
			api.WriteError(w, api.NewError(404, "not_found", "no such file", nil))
			return
		}
		api.WriteOK(w, 200, "file.content", map[string]any{"path": r.URL.Query().Get("path"), "content": string(b)})
	})
	g.mux.HandleFunc("PUT /v1/files", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Path, Content string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		g.files[r.Header.Get("X-Sandbox-Id")+"|"+req.Path] = []byte(req.Content)
		g.mu.Unlock()
		api.WriteOK(w, 200, "file.written", map[string]any{"path": req.Path})
	})
	g.mux.HandleFunc("GET /v1/files/raw", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		b, ok := g.files[r.Header.Get("X-Sandbox-Id")+"|"+r.URL.Query().Get("path")]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	g.mux.HandleFunc("PUT /v1/files/raw", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.files[r.Header.Get("X-Sandbox-Id")+"|"+r.URL.Query().Get("path")] = b
		g.mu.Unlock()
		api.WriteOK(w, 200, "file.written", map[string]any{"path": r.URL.Query().Get("path"), "size": len(b)})
	})
	g.mux.HandleFunc("POST /v1/prompt", func(w http.ResponseWriter, r *http.Request) {
		api.WriteOK(w, 202, "prompt.queued", map[string]any{"id": r.Header.Get("X-Sandbox-Id"), "promptId": "p1", "conversationId": "c1"})
	})
	g.mux.HandleFunc("GET /v1/processes", func(w http.ResponseWriter, r *http.Request) {
		api.WriteOK(w, 200, "process.list", map[string]any{"processes": []any{}, "auth": r.Header.Get("Authorization")})
	})
	return g
}

func (g *FakeGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

// command echoes the command. With "stream": NDJSON frames, one per
// whitespace-separated word, 20 ms apart, then an exit frame.
func (g *FakeGuest) command(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
		Stream  bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !req.Stream {
		api.WriteOK(w, 200, "command.result", map[string]any{"exitCode": 0, "stdout": req.Command + "\n", "stderr": "", "sandbox": r.Header.Get("X-Sandbox-Id")})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	for _, word := range strings.Fields(req.Command) {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "stdout", "data": word + "\n"})
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "exit", "exitCode": 0})
}
