package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// ---------------------------------------------------------------------------
// Fake guest agent (one HTTP server shared by all fake VMs)
// ---------------------------------------------------------------------------

type fakeGuest struct {
	srv     *httptest.Server
	release chan struct{} // unblocks the second line of /v1/slow
	mu      sync.Mutex
	paths   []string
	hung    bool   // /v1/health hangs
	cloning bool   // /v1/health says reposCloning
	adopted string // sandbox id after POST /v1/internal/adopt (warm pool)
	bodies  map[string]string
}

func newFakeGuest(t *testing.T) *fakeGuest {
	g := &fakeGuest{release: make(chan struct{}, 10), bodies: map[string]string{}}
	m := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer guest-tok" {
				http.Error(w, "unauthorized", 401)
				return
			}
			if r.URL.Path == "/v1/health" { // probed every few seconds: not logged
				h(w, r)
				return
			}
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			g.mu.Lock()
			g.paths = append(g.paths, r.Method+" "+r.URL.RequestURI())
			g.bodies[r.Method+" "+r.URL.Path] = string(b)
			g.mu.Unlock()
			h(w, r)
		}
	}
	m.HandleFunc("GET /v1/internal/events/stream", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i := range 2 {
			fmt.Fprintf(w, `{"time":"2026-01-01T00:00:0%dZ","type":"setup.status","data":{"i":%d}}`+"\n", i, i)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	m.HandleFunc("POST /v1/internal/sync", auth(func(w http.ResponseWriter, r *http.Request) {}))
	m.HandleFunc("POST /v1/internal/prepare-stop", auth(func(w http.ResponseWriter, r *http.Request) {}))
	m.HandleFunc("PUT /v1/internal/firewall", auth(func(w http.ResponseWriter, r *http.Request) {}))
	m.HandleFunc("POST /v1/internal/adopt", auth(func(w http.ResponseWriter, r *http.Request) {
		var cfg proto.GuestConfig
		json.NewDecoder(r.Body).Decode(&cfg)
		g.mu.Lock()
		g.adopted = cfg.SandboxID
		g.mu.Unlock()
	}))
	m.HandleFunc("GET /v1/health", auth(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		hung, cloning := g.hung, g.cloning
		g.mu.Unlock()
		if hung {
			<-r.Context().Done()
			return
		}
		g.mu.Lock()
		adopted := g.adopted
		g.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"version":"g1.2","reposCloning":%v,"sandboxId":%q}`+"\n", cloning, adopted)
	}))
	m.HandleFunc("GET /v1/slow", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"line":1}`)
		w.(http.Flusher).Flush()
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintln(w, `{"line":2}`)
	}))
	m.HandleFunc("/v1/echo/{rest...}", auth(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "uri": r.URL.RequestURI(), "body": string(b)})
	}))
	m.HandleFunc("GET /v1/tunnel", auth(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := mux.HijackUpgrade(w)
		if err != nil {
			return
		}
		defer c.Close()
		prefix := "port" + r.URL.Query().Get("port") + ":"
		sc := bufio.NewScanner(rw)
		for sc.Scan() {
			if _, err := c.Write([]byte(prefix + sc.Text() + "\n")); err != nil {
				return
			}
		}
	}))
	g.srv = httptest.NewServer(m)
	t.Cleanup(g.srv.Close)
	return g
}

// ---------------------------------------------------------------------------
// Fake driver + store
// ---------------------------------------------------------------------------

type fakeVM struct {
	spec   proto.VMSpec
	status proto.VMStatus
	top    string // snapshot the overlay sits on
}

type fakeDriver struct {
	lazy      bool
	supersede []string // set on the refs of new snapshots
	lazyReads map[string]string
	hydrated  []string
	renames   []string
	t         *testing.T
	guest     *fakeGuest
	store     *fakeStore
	mu        sync.Mutex
	vms       map[string]*fakeVM
	starts    []proto.VMSpec
}

func newFakeDriver(t *testing.T, g *fakeGuest) *fakeDriver {
	return &fakeDriver{t: t, guest: g, store: newFakeStore(t), vms: map[string]*fakeVM{}}
}

func (d *fakeDriver) Name() string { return "fake" }

// Lazy chains (LIFE-013): enabled with lazy; StartLazy reads the first
// bytes of every remote layer through its URL.
func (d *fakeDriver) LazyChain() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.lazy }

func (d *fakeDriver) StartLazy(ctx context.Context, spec proto.VMSpec, chain []proto.SnapshotRef, remote map[string]string) error {
	d.mu.Lock()
	d.lazyReads = map[string]string{}
	d.mu.Unlock()
	for id, u := range remote {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Range", "bytes=0-4")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		d.mu.Lock()
		d.lazyReads[id] = fmt.Sprintf("%d %s", resp.StatusCode, b)
		d.mu.Unlock()
	}
	spec.ParentID = "" // the fake store has no lazy layers
	return d.Start(ctx, spec)
}

func (d *fakeDriver) Lazy(id string) bool { return false }

// Rename (warm pool): the fake guest is shared, so only the record moves.
func (d *fakeDriver) Rename(ctx context.Context, from string, spec proto.VMSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.vms[from]
	if v == nil {
		return vm.ErrNotFound
	}
	delete(d.vms, from)
	v.spec, v.status.SandboxID = spec, spec.SandboxID
	d.vms[spec.SandboxID] = v
	d.renames = append(d.renames, from+"->"+spec.SandboxID)
	return nil
}

func (d *fakeDriver) Hydrated(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.vms[id] == nil {
		return vm.ErrNotFound
	}
	d.hydrated = append(d.hydrated, id)
	return nil
}

func (d *fakeDriver) Start(ctx context.Context, spec proto.VMSpec) error {
	if spec.ImageID != "" && !d.store.HasImage(spec.ImageID) {
		return fmt.Errorf("image %s missing", spec.ImageID)
	}
	if spec.ParentID != "" && !d.store.HasSnapshot(spec.ParentID) {
		return fmt.Errorf("snapshot %s missing", spec.ParentID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts = append(d.starts, spec)
	v := &fakeVM{spec: spec, top: spec.ParentID, status: proto.VMStatus{SandboxID: spec.SandboxID, State: proto.VMStarting, StartedAt: time.Now()}}
	d.vms[spec.SandboxID] = v
	time.AfterFunc(50*time.Millisecond, func() {
		d.mu.Lock()
		if v.status.State == proto.VMStarting {
			v.status.State, v.status.GuestReady = proto.VMRunning, true
		}
		d.mu.Unlock()
	})
	return nil
}

func (d *fakeDriver) freeze(id, snapID string) (proto.SnapshotRef, error) {
	v, ok := d.vms[id]
	if !ok {
		return proto.SnapshotRef{}, fmt.Errorf("no vm %s", id)
	}
	ref := proto.SnapshotRef{ID: snapID, ParentID: v.top, ImageID: v.spec.ImageID, CreatedAt: time.Now()}
	p := filepath.Join(d.t.TempDir(), snapID)
	os.WriteFile(p, []byte("disk-of-"+snapID), 0o644)
	if err := d.store.ImportSnapshot(ref, p); err != nil {
		return ref, err
	}
	v.top = snapID
	ref, _ = d.store.SnapshotRef(snapID)
	ref.Supersedes = d.supersede
	return ref, nil
}

func (d *fakeDriver) Stop(ctx context.Context, id, snapID string, force bool) (proto.SnapshotRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, err := d.freeze(id, snapID)
	if err == nil {
		d.vms[id].status = proto.VMStatus{SandboxID: id, State: proto.VMStopped}
	}
	return ref, err
}

func (d *fakeDriver) Snapshot(ctx context.Context, id, snapID string) (proto.SnapshotRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.freeze(id, snapID)
}

func (d *fakeDriver) Restart(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.vms[id]; ok {
		v.status.State, v.status.GuestReady = proto.VMRunning, true
		return nil
	}
	return fmt.Errorf("no vm")
}

func (d *fakeDriver) Destroy(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.vms, id)
	return nil
}

func (d *fakeDriver) Status(id string) (proto.VMStatus, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.vms[id]
	if !ok {
		return proto.VMStatus{}, false
	}
	return v.status, true
}

func (d *fakeDriver) List() []proto.VMStatus { return nil }

func (d *fakeDriver) GuestAddr(id string) (string, error) {
	st, ok := d.Status(id)
	if !ok || st.State != proto.VMRunning {
		return "", fmt.Errorf("vm %s not running", id)
	}
	return d.guest.srv.Listener.Addr().String(), nil
}

func (d *fakeDriver) Store() vm.Store { return d.store }
func (d *fakeDriver) Close() error    { return nil }

type fakeStore struct {
	dir    string
	mu     sync.Mutex
	images map[string]string
	snaps  map[string]proto.SnapshotRef
}

func newFakeStore(t *testing.T) *fakeStore {
	return &fakeStore{dir: t.TempDir(), images: map[string]string{}, snaps: map[string]proto.SnapshotRef{}}
}

func (s *fakeStore) HasImage(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.images[id]
	return ok
}

func (s *fakeStore) HasSnapshot(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.snaps[id]
	return ok
}

func (s *fakeStore) ImportImage(id, path string) error {
	dst := filepath.Join(s.dir, "img-"+id)
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	s.mu.Lock()
	s.images[id] = dst
	s.mu.Unlock()
	return nil
}

func (s *fakeStore) ImportSnapshot(ref proto.SnapshotRef, path string) error {
	if ref.ParentID != "" && !s.HasSnapshot(ref.ParentID) {
		return fmt.Errorf("parent %s missing", ref.ParentID)
	}
	dst := filepath.Join(s.dir, "snap-"+ref.ID)
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	fi, _ := os.Stat(dst)
	ref.SizeBytes = fi.Size()
	s.mu.Lock()
	s.snaps[ref.ID] = ref
	s.mu.Unlock()
	return nil
}

func (s *fakeStore) SnapshotPath(id string) (string, error) {
	if !s.HasSnapshot(id) {
		return "", fmt.Errorf("no snapshot %s", id)
	}
	return filepath.Join(s.dir, "snap-"+id), nil
}

func (s *fakeStore) ImagePath(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.images[id]
	if !ok {
		return "", fmt.Errorf("no image %s", id)
	}
	return p, nil
}

func (s *fakeStore) SnapshotRef(id string) (proto.SnapshotRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.snaps[id]
	return r, ok
}

func (s *fakeStore) DeleteSnapshot(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snaps, id)
	return nil
}

func (s *fakeStore) Snapshots() []proto.SnapshotRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.SnapshotRef
	for _, r := range s.snaps {
		out = append(out, r)
	}
	return out
}

func (s *fakeStore) Images() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.images {
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fake server
// ---------------------------------------------------------------------------

type blob struct {
	data []byte
	sha  string
}

type fakeServer struct {
	t        *testing.T
	srv      *httptest.Server
	sessions chan *yamux.Session
	mu       sync.Mutex
	beats    []proto.Heartbeat
	events   []proto.Event
	blobs    map[string]blob // "snapshot/<id>", "image/<id>"
	snaps    map[string]proto.SnapshotRef
	hbResp   proto.HeartbeatResponse
	connects int
	holdFull chan struct{} // when set, full blob downloads wait for it
	holdPut  chan struct{} // when set, blob uploads wait for it
	failPut  bool          // blob uploads fail (503)
	gone     map[string]bool
	puts     int
	reported map[string]proto.SnapshotRef // POST /node/v1/snapshots
	putOrder []string
}

func newFakeServer(t *testing.T) *fakeServer {
	s := &fakeServer{t: t, sessions: make(chan *yamux.Session, 10), blobs: map[string]blob{}, snaps: map[string]proto.SnapshotRef{}, reported: map[string]proto.SnapshotRef{}}
	nodeAPI := http.NewServeMux()
	nodeAPI.HandleFunc("POST /node/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var hb proto.Heartbeat
		json.NewDecoder(r.Body).Decode(&hb)
		s.mu.Lock()
		s.beats = append(s.beats, hb)
		resp := s.hbResp
		s.mu.Unlock()
		json.NewEncoder(w).Encode(resp)
	})
	nodeAPI.HandleFunc("POST /node/v1/events", func(w http.ResponseWriter, r *http.Request) {
		var evs []proto.Event
		json.NewDecoder(r.Body).Decode(&evs)
		s.mu.Lock()
		s.events = append(s.events, evs...)
		s.mu.Unlock()
	})
	nodeAPI.HandleFunc("GET /node/v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		ref, ok := s.snaps[r.PathValue("id")]
		pending, reported := s.reported[r.PathValue("id")]
		s.mu.Unlock()
		if !ok && reported { // recorded when taken, not stored yet
			json.NewEncoder(w).Encode(map[string]any{"id": pending.ID, "parentId": pending.ParentID, "uploaded": false})
			return
		}
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		json.NewEncoder(w).Encode(ref)
	})
	nodeAPI.HandleFunc("POST /node/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		var ref proto.SnapshotRef
		json.NewDecoder(r.Body).Decode(&ref)
		s.mu.Lock()
		s.reported[ref.ID] = ref
		s.mu.Unlock()
	})
	nodeAPI.HandleFunc("PUT /node/v1/blobs/{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		hold, fail := s.holdPut, s.failPut
		s.puts++
		s.mu.Unlock()
		if fail {
			http.Error(w, "unavailable", 503)
			return
		}
		if hold != nil {
			<-hold
		}
		s.mu.Lock()
		gone := s.gone[r.PathValue("id")]
		s.mu.Unlock()
		if gone { // deleted on the server meanwhile
			io.Copy(io.Discard, r.Body)
			http.Error(w, `{"error":"snapshot_deleted"}`, http.StatusGone)
			return
		}
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if h := r.Header.Get(HeaderSHA256); h != hex.EncodeToString(sum[:]) {
			http.Error(w, "sha mismatch", 400)
			return
		}
		s.mu.Lock()
		s.putOrder = append(s.putOrder, r.PathValue("id"))
		s.blobs[r.PathValue("kind")+"/"+r.PathValue("id")] = blob{b, r.Header.Get(HeaderSHA256)}
		if r.PathValue("kind") == "snapshot" {
			var ref proto.SnapshotRef
			json.Unmarshal([]byte(r.Header.Get("X-Crowdvm-Snapshot")), &ref)
			s.snaps[ref.ID] = ref
		}
		s.mu.Unlock()
	})
	nodeAPI.HandleFunc("GET /node/v1/blobs/{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, ok := s.blobs[r.PathValue("kind")+"/"+r.PathValue("id")]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set(HeaderSHA256, b.sha)
		if r.Header.Get("Range") != "" { // lazy reads through the node's blob proxy
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b.data))
			return
		}
		s.mu.Lock()
		hold := s.holdFull
		s.mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b.data)))
		w.Write(b.data)
	})
	nodeAPI.HandleFunc("GET /node/v1/bench/download", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		buf := make([]byte, 32<<10)
		for n > 0 {
			m := min(n, int64(len(buf)))
			if _, err := w.Write(buf[:m]); err != nil {
				return
			}
			n -= m
		}
	})
	nodeAPI.HandleFunc("POST /node/v1/bench/upload", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	})

	m := http.NewServeMux()
	m.HandleFunc("POST /node/v1/register", func(w http.ResponseWriter, r *http.Request) {
		var req proto.RegisterRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.JoinToken != "join-tok" {
			http.Error(w, "bad token", 401)
			return
		}
		json.NewEncoder(w).Encode(proto.RegisterResponse{NodeID: "node_test1", NodeToken: "node-tok"})
	})
	m.HandleFunc("GET /node/v1/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		sess, err := mux.Accept(w, r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connects++
		s.mu.Unlock()
		go mux.Serve(sess, nodeAPI)
		s.sessions <- sess
	})
	s.srv = httptest.NewServer(m)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeServer) putSnapshot(ref proto.SnapshotRef, data []byte) {
	sum := sha256.Sum256(data)
	ref.SHA256 = hex.EncodeToString(sum[:])
	s.mu.Lock()
	s.snaps[ref.ID] = ref
	s.blobs["snapshot/"+ref.ID] = blob{data, ref.SHA256}
	s.mu.Unlock()
}

func (s *fakeServer) putImage(id string, data []byte) {
	sum := sha256.Sum256(data)
	s.mu.Lock()
	s.blobs["image/"+id] = blob{data, hex.EncodeToString(sum[:])}
	s.mu.Unlock()
}

func (s *fakeServer) eventsOf(typ string) []proto.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.Event
	for _, e := range s.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (s *fakeServer) nextSession() *yamux.Session {
	select {
	case sess := <-s.sessions:
		return sess
	case <-time.After(10 * time.Second):
		s.t.Fatal("node did not connect")
		return nil
	}
}

// call performs a server -> node request over sess.
func call(t *testing.T, sess *yamux.Session, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://node"+path, r)
	resp, err := mux.Client(sess).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
