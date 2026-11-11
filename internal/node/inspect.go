package node

// Inspector VMs: throwaway VMs booted from a snapshot (proto.BootInspect) so
// the server can read files out of a snapshot while its sandbox is stopped.
// They are not sandboxes: no vm.state events, heartbeat entries, periodic
// snapshots, direct ports or capacity accounting, and their overlay is
// deleted with them. One inspector per snapshot; destroyed after InspectIdle
// without guest calls.
//
//	POST   /v1/inspect {snapshotId, imageId?, token?}  → InspectStatus (blocks until the guest is up)
//	GET    /v1/inspect                                 → []InspectStatus
//	DELETE /v1/inspect/{id}
//	ANY    /v1/inspect/{id}/guest/{rest...}            → guest /v1/{rest} (Bearer token added)

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

const inspectPrefix = "insp_"

var (
	// InspectIdle: an inspector without guest calls for this long is destroyed.
	InspectIdle        = 5 * time.Minute
	inspectTick        = 30 * time.Second
	maxInspectors      = 4
	inspectBootTimeout = 30 * time.Minute // includes pulling the chain
	inspectPoll        = 100 * time.Millisecond
)

// InspectRequest is the body of POST /v1/inspect.
type InspectRequest struct {
	SnapshotID string `json:"snapshotId"`
	ImageID    string `json:"imageId,omitempty"`
	Token      string `json:"token,omitempty"` // guest bearer token (generated when empty)
}

// InspectStatus describes one inspector VM.
type InspectStatus struct {
	ID         string    `json:"id"`
	SnapshotID string    `json:"snapshotId"`
	Ready      bool      `json:"ready"`
	Reused     bool      `json:"reused,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
}

type inspector struct {
	id, snap string
	spec     proto.VMSpec
	created  time.Time
	ready    chan struct{} // closed when booted (err nil) or failed
	err      error
	last     atomic.Int64 // unix nanos of the last use
	active   atomic.Int32 // guest calls in flight
}

func (in *inspector) touch() { in.last.Store(time.Now().UnixNano()) }

func (in *inspector) isReady() bool {
	select {
	case <-in.ready:
		return in.err == nil
	default:
		return false
	}
}

func (in *inspector) status() InspectStatus {
	return InspectStatus{ID: in.id, SnapshotID: in.snap, Ready: in.isReady(), CreatedAt: in.created, LastUsedAt: time.Unix(0, in.last.Load()).UTC()}
}

// inspectors is the node's inspector registry (zero value ready to use).
type inspectors struct {
	mu     sync.Mutex
	byID   map[string]*inspector
	bySnap map[string]*inspector
	reaper sync.Once
}

var errInspectorBusy = errors.New("too many inspector VMs on this node")

func (n *Node) inspectRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /v1/inspect", func(w http.ResponseWriter, r *http.Request) {
		var req InspectRequest
		if !decode(w, r, &req) {
			return
		}
		in, reused, err := n.Inspect(r.Context(), req)
		switch {
		case errors.Is(err, errInspectorBusy):
			api.WriteError(w, api.NewError(503, "inspector_busy", err.Error(), map[string]any{"retryAfterSeconds": 10}))
		case err != nil && in == nil:
			api.WriteError(w, api.NewError(400, "invalid_request", err.Error(), nil))
		case err != nil:
			api.WriteError(w, api.NewError(502, "inspector_failed", err.Error(), nil))
		default:
			st := in.status()
			st.Reused = reused
			api.WriteJSON(w, 200, st)
		}
	})
	m.HandleFunc("GET /v1/inspect", func(w http.ResponseWriter, r *http.Request) {
		n.insp.mu.Lock()
		out := []InspectStatus{}
		for _, in := range n.insp.byID {
			out = append(out, in.status())
		}
		n.insp.mu.Unlock()
		api.WriteJSON(w, 200, out)
	})
	m.HandleFunc("DELETE /v1/inspect/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !n.DestroyInspector(r.PathValue("id")) {
			api.WriteError(w, api.NewError(404, "inspector_not_found", "no such inspector", nil))
			return
		}
		api.WriteJSON(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("/v1/inspect/{id}/guest/{rest...}", n.handleInspectGuest)
}

// Inspect returns the running inspector of req.SnapshotID, booting one if
// needed, and waits until its guest answers (or ctx ends).
func (n *Node) Inspect(ctx context.Context, req InspectRequest) (*inspector, bool, error) {
	if req.SnapshotID == "" {
		return nil, false, errors.New("snapshotId is required")
	}
	reg := &n.insp
	reg.mu.Lock()
	if reg.byID == nil {
		reg.byID, reg.bySnap = map[string]*inspector{}, map[string]*inspector{}
	}
	in, reused := reg.bySnap[req.SnapshotID], true
	if in == nil {
		reused = false
		if len(reg.byID) >= maxInspectors && !n.evictIdleLocked() {
			reg.mu.Unlock()
			return nil, false, errInspectorBusy
		}
		id := inspectPrefix + api.NewID("", 16)
		tok := req.Token
		if tok == "" {
			tok = api.NewID("", 32)
		}
		in = &inspector{id: id, snap: req.SnapshotID, created: time.Now().UTC(), ready: make(chan struct{}),
			spec: proto.VMSpec{SandboxID: id, ImageID: req.ImageID, ParentID: req.SnapshotID,
				Size: proto.Size{Name: "inspect", VCPU: 1, MemoryMiB: 2048},
				Guest: proto.GuestConfig{SandboxID: id, Token: tok, ListenAddr: fmt.Sprintf(":%d", proto.GuestPort),
					Hostname: "crowdvm-inspect", Boot: proto.BootInspect}}}
		in.touch()
		reg.byID[id], reg.bySnap[req.SnapshotID] = in, in
		reg.reaper.Do(func() { go n.inspectReaper(n.ctx) })
		go n.bootInspector(in)
	}
	in.touch()
	reg.mu.Unlock()
	select {
	case <-in.ready:
		return in, reused, in.err
	case <-ctx.Done():
		return in, reused, ctx.Err()
	}
}

func (n *Node) bootInspector(in *inspector) {
	ctx, cancel := context.WithTimeout(n.ctx, inspectBootTimeout)
	defer cancel()
	err := n.ensureChain(ctx, "", in.spec.ImageID, in.spec.ParentID)
	if err == nil {
		err = n.drv.Start(n.ctx, in.spec)
		if err == nil {
			err = n.waitGuestUp(ctx, in.id)
		}
		if err != nil {
			_ = n.drv.Destroy(context.Background(), in.id)
		}
	}
	if err != nil {
		n.log.Warn("inspector failed", "snapshot", in.snap, "err", err)
		in.err = err
		n.insp.mu.Lock()
		n.forgetLocked(in)
		n.insp.mu.Unlock()
	} else {
		n.log.Info("inspector up", "snapshot", in.snap, "id", in.id)
	}
	in.touch()
	close(in.ready)
}

// waitGuestUp waits until the driver reports the VM running with its guest up.
func (n *Node) waitGuestUp(ctx context.Context, id string) error {
	if w, ok := n.drv.(vm.Waiter); ok {
		return w.Wait(ctx, id)
	}
	for {
		st, ok := n.drv.Status(id)
		switch {
		case !ok:
			return errors.New("inspector vm vanished")
		case st.State == proto.VMError || st.State == proto.VMStopped:
			return fmt.Errorf("inspector vm %s: %s", st.State, st.Error)
		case st.State == proto.VMRunning && st.GuestReady:
			return nil
		}
		if !sleepCtx(ctx, inspectPoll) {
			return ctx.Err()
		}
	}
}

// forgetLocked (n.insp.mu held) drops in from the registry.
func (n *Node) forgetLocked(in *inspector) {
	if n.insp.byID[in.id] == in {
		delete(n.insp.byID, in.id)
	}
	if n.insp.bySnap[in.snap] == in {
		delete(n.insp.bySnap, in.snap)
	}
}

// evictIdleLocked destroys the least recently used idle inspector.
func (n *Node) evictIdleLocked() bool {
	var lru *inspector
	for _, in := range n.insp.byID {
		if in.isReady() && in.active.Load() == 0 && (lru == nil || in.last.Load() < lru.last.Load()) {
			lru = in
		}
	}
	if lru == nil {
		return false
	}
	n.forgetLocked(lru)
	go func() { _ = n.drv.Destroy(context.Background(), lru.id) }()
	return true
}

// DestroyInspector kills an inspector VM and deletes its overlay.
func (n *Node) DestroyInspector(id string) bool {
	n.insp.mu.Lock()
	in := n.insp.byID[id]
	if in != nil {
		n.forgetLocked(in)
	}
	n.insp.mu.Unlock()
	if in == nil {
		return false
	}
	go func() {
		<-in.ready // never destroy under a booting VM
		_ = n.drv.Destroy(context.Background(), id)
	}()
	return true
}

func (n *Node) inspectReaper(ctx context.Context) {
	t := time.NewTicker(inspectTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			n.insp.mu.Lock()
			var all []string
			for id := range n.insp.byID {
				all = append(all, id)
			}
			n.insp.mu.Unlock()
			for _, id := range all {
				n.DestroyInspector(id)
			}
			return
		case <-t.C:
		}
		cut := time.Now().Add(-InspectIdle).UnixNano()
		var idle []string
		n.insp.mu.Lock()
		for id, in := range n.insp.byID {
			if in.isReady() && in.active.Load() == 0 && in.last.Load() < cut {
				idle = append(idle, id)
			}
		}
		n.insp.mu.Unlock()
		for _, id := range idle {
			n.log.Info("inspector idle, destroying", "id", id)
			n.DestroyInspector(id)
		}
	}
}

// handleInspectGuest proxies to the inspector's guest agent.
func (n *Node) handleInspectGuest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n.insp.mu.Lock()
	in := n.insp.byID[id]
	n.insp.mu.Unlock()
	if in == nil {
		api.WriteError(w, api.NewError(404, "inspector_not_found", "no such inspector (idle ones are destroyed)", nil))
		return
	}
	if !in.isReady() {
		api.WriteError(w, api.NewError(409, "inspector_starting", "the inspector is still booting", nil))
		return
	}
	in.active.Add(1)
	in.touch()
	defer func() { in.touch(); in.active.Add(-1) }()
	addr, err := n.drv.GuestAddr(id)
	if err != nil {
		api.WriteError(w, api.NewError(409, "machine_not_running", err.Error(), nil))
		return
	}
	prefix := "/v1/inspect/" + id + "/guest"
	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if rest == r.URL.EscapedPath() {
		rest = "/" + r.PathValue("rest")
	}
	rp := &httputil.ReverseProxy{
		Transport:     n.guestHTTP,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u, err := url.Parse("http://" + addr + "/v1" + rest)
			if err != nil {
				u = &url.URL{Scheme: "http", Host: addr, Path: "/v1/"}
			}
			u.RawQuery = r.URL.RawQuery
			pr.Out.URL, pr.Out.Host = u, addr
			pr.Out.Header.Set("Authorization", "Bearer "+in.spec.Guest.Token)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			api.WriteError(w, api.NewError(502, "guest_unreachable", err.Error(), nil))
		},
	}
	rp.ServeHTTP(w, r)
}
