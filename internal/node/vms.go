package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// vmRec is the node's record of one sandbox VM. status is guarded by Node.mu;
// op serializes lifecycle operations on the VM.
type vmRec struct {
	id       string
	spec     proto.VMSpec
	status   proto.VMStatus
	op       sync.Mutex
	cancel   context.CancelFunc // watcher + guest events
	bootCtx  context.Context    // canceled by DestroyVM to abort a pull/boot
	abort    context.CancelFunc
	direct   *directPort            // ssh on DirectHost
	ports    map[string]*directPort // user ports ("tcp/3000") on DirectHost (URL-012)
	lastSnap time.Time
	// stopRef: the snapshot of the stop that finished, answered again to a
	// repeated stop (a server that restarted mid-stop retries it; a 404
	// there would make it fall back to an older stored layer).
	stopRef proto.SnapshotRef

	// Guest health (guarded by Node.mu): healthFails consecutive failed
	// probes; unhealthy turns GuestReady off while the driver says running
	// (server: degraded, recover allowed) until a probe answers again.
	healthFails int
	unhealthy   bool
	gate        int // clone gate (LIFE-009): gateNone, gateChecking, gateOpen
	lastHealthy time.Time
	lastEvent   time.Time // last guest event received
	lastGuestEv time.Time // guest timestamp of the last forwarded event (replay cursor)
}

func active(state string) bool {
	return state == proto.VMStarting || state == proto.VMRunning || state == proto.VMStopping
}

// Usage sums the sizes of active VMs.
func (n *Node) Usage() proto.Usage {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.usageLocked()
}

func (n *Node) usageLocked() proto.Usage {
	var u proto.Usage
	for _, r := range n.vms {
		if active(r.status.State) {
			u.VCPU += r.spec.Size.VCPU
			u.MemoryMiB += r.spec.Size.MemoryMiB
			u.DiskGiB += r.spec.Size.DiskGiB
			u.VMs++
			if gpuOf(r.spec) != "" {
				u.GPUs++
			}
		}
	}
	return u
}

// usageWithWarmLocked (n.mu held) also counts warm pool VMs: they hold
// resources on this machine, but are not reported to the server (a create
// that needs their room evicts them).
func (n *Node) usageWithWarmLocked() proto.Usage {
	u := n.usageLocked()
	for _, w := range n.warm {
		u.VCPU += w.spec.Size.VCPU
		u.MemoryMiB += w.spec.Size.MemoryMiB
		u.DiskGiB += w.spec.Size.DiskGiB
		u.VMs++
	}
	return u
}

// Statuses lists every VM known to the node.
func (n *Node) Statuses() []proto.VMStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]proto.VMStatus, 0, len(n.vms))
	for _, r := range n.vms {
		out = append(out, r.status)
	}
	return out
}

func (n *Node) rec(id string) *vmRec {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.vms[id]
}

// setState updates a VM's status and emits vm.state when it changed.
func (n *Node) setState(r *vmRec, mut func(s *proto.VMStatus)) {
	n.mu.Lock()
	old := r.status
	mut(&r.status)
	r.status.Direct = r.directMap()
	r.status.GPU = gpuOf(r.spec)
	st := r.status
	n.mu.Unlock()
	if old.State != st.State || old.GuestReady != st.GuestReady || old.Error != st.Error ||
		old.GuestVersion != st.GuestVersion || old.Hydrating != st.Hydrating || !maps.Equal(old.Direct, st.Direct) {
		n.log.Info("vm state", "sandbox", r.id, "state", st.State, "ready", st.GuestReady, "err", st.Error)
		n.emit(r.id, proto.EvVMState, st)
	}
}

type capacityError struct{ msg string }

func (e *capacityError) Error() string { return e.msg }

// CreateVM reserves capacity and boots the VM asynchronously.
func (n *Node) CreateVM(spec proto.VMSpec) (proto.VMStatus, error) {
	if spec.SandboxID == "" {
		return proto.VMStatus{}, errors.New("sandboxId is required")
	}
	c := n.config()
	cp, disk := n.capacity()
	n.mu.Lock()
	if old, ok := n.vms[spec.SandboxID]; ok && active(old.status.State) {
		st := old.status // copied under n.mu (bugs-r3-sol 3)
		n.mu.Unlock()
		return st, fmt.Errorf("vm %s already exists (%s)", spec.SandboxID, st.State)
	}
	if strings.HasPrefix(spec.SandboxID, warmPrefix) || strings.HasPrefix(spec.SandboxID, inspectPrefix) {
		n.mu.Unlock()
		return proto.VMStatus{}, fmt.Errorf("sandbox id %q is reserved", spec.SandboxID)
	}
	n.noteWarmShapeLocked(spec)
	w := n.takeWarmLocked(spec) // SIZE-012: its resources become the sandbox's
	u := n.usageWithWarmLocked()
	var why string
	switch {
	case disk.HeadroomBytes < minDiskHeadroom:
		why = fmt.Sprintf("not enough free disk (%.1f GiB usable, %d GiB kept free)", float64(disk.HeadroomBytes)/(1<<30), disk.ReserveGiB)
	case c.Paused:
		why = "node is paused by its owner"
	case u.VMs+1 > cp.MaxSandboxes:
		why = fmt.Sprintf("max sandboxes reached (%d)", cp.MaxSandboxes)
	case u.VCPU+spec.Size.VCPU > cp.VCPU:
		why = fmt.Sprintf("not enough vCPUs (%d/%d used, %d requested)", u.VCPU, cp.VCPU, spec.Size.VCPU)
	case u.MemoryMiB+spec.Size.MemoryMiB > cp.MemoryMiB:
		why = fmt.Sprintf("not enough memory (%d/%d MiB used, %d requested)", u.MemoryMiB, cp.MemoryMiB, spec.Size.MemoryMiB)
	case u.DiskGiB+spec.Size.DiskGiB > cp.DiskGiB:
		why = fmt.Sprintf("not enough disk (%d/%d GiB allocated incl. %gx overcommit, %d requested)", u.DiskGiB, cp.DiskGiB, disk.Overcommit, spec.Size.DiskGiB)
	}
	if why != "" && w == nil {
		if ev := n.evictWarmLocked(); ev != nil { // pool VMs make room for real sandboxes
			n.mu.Unlock()
			n.destroyWarm(ev)
			return n.CreateVM(spec)
		}
	}
	if why != "" {
		if w != nil {
			n.warm[w.id] = w
		}
		n.mu.Unlock()
		return proto.VMStatus{}, &capacityError{why}
	}
	if err := n.takeGPULocked(&spec); err != nil { // one VM per passthrough GPU
		n.mu.Unlock()
		return proto.VMStatus{}, err
	}
	r := &vmRec{id: spec.SandboxID, spec: spec, lastSnap: time.Now(),
		status: proto.VMStatus{SandboxID: spec.SandboxID, State: proto.VMStarting, GPU: gpuOf(spec)}}
	r.bootCtx, r.abort = context.WithCancel(n.ctx)
	n.vms[spec.SandboxID] = r
	n.saveSpecsLocked()
	st := r.status
	n.mu.Unlock()

	n.emit(r.id, proto.EvVMState, st)
	if w != nil {
		go n.adoptWarm(r, w)
	} else {
		go n.boot(r)
	}
	return st, nil
}

func (n *Node) boot(r *vmRec) {
	r.op.Lock()
	defer r.op.Unlock()
	defer r.abort()
	n.bootLocked(r)
}

// bootLocked (r.op held) pulls the chain (or boots lazily) and starts r.
func (n *Node) bootLocked(r *vmRec) {
	ctx := r.bootCtx
	if ctx.Err() != nil { // destroyed before it booted
		return
	}
	fail := func(err error) {
		n.log.Error("vm start failed", "sandbox", r.id, "err", err)
		n.setState(r, func(s *proto.VMStatus) { s.State, s.Error, s.GuestReady = proto.VMError, err.Error(), false })
	}
	if lazy, err := n.bootLazy(ctx, r); lazy && err != nil { // LIFE-013: boot before the chain is local
		fail(err)
		return
	} else if !lazy {
		if err := n.ensureChain(ctx, r.id, r.spec.ImageID, r.spec.ParentID); err != nil {
			fail(fmt.Errorf("pull disk chain: %w", err))
			return
		}
		// A previous (stopped) incarnation may still be known to the driver.
		if _, ok := n.drv.Status(r.id); ok {
			_ = n.drv.Destroy(ctx, r.id)
		}
		if err := n.drv.Start(n.ctx, r.spec); err != nil {
			fail(err)
			return
		}
	}
	n.setState(r, func(s *proto.VMStatus) { s.StartedAt = time.Now().UTC() })
	n.openDirect(r)
	n.mu.Lock()
	n.startWatch(r)
	n.mu.Unlock()
}

// startWatch (n.mu held) polls the driver status and forwards guest events
// until the VM is stopped/destroyed.
func (n *Node) startWatch(r *vmRec) {
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithCancel(n.ctx)
	r.cancel = cancel
	go n.watch(ctx, r)
	go n.guestEvents(ctx, r)
}

func (n *Node) stopWatch(r *vmRec) {
	n.mu.Lock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	n.mu.Unlock()
	n.closeDirect(r)
}

var (
	watchInterval = 1 * time.Second
	// Guest health monitor: while the driver reports the VM running, GET
	// /v1/health every healthInterval; healthFailures misses in a row turn
	// guestReady off (server: sandbox degraded, recover allowed), the next
	// answer turns it back on (recovered).
	healthInterval = 5 * time.Second
	healthTimeout  = 5 * time.Second
	healthFailures = 3
)

func (n *Node) watch(ctx context.Context, r *vmRec) {
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	var lastProbe time.Time
	for {
		if st, ok := n.drv.Status(r.id); ok {
			n.onDriverStatus(st)
			if st.State == proto.VMRunning && st.GuestReady && time.Since(lastProbe) >= healthInterval {
				lastProbe = time.Now()
				n.checkHealth(ctx, r)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// guestHealth is the part of the guest's GET /v1/health the node uses.
type guestHealth struct {
	Version string `json:"version"`
	// ReposCloning: the guest still clones this boot's environment
	// repositories; ready waits for them (LIFE-009).
	ReposCloning bool `json:"reposCloning"`
}

// probeGuest reports whether the guest agent answers GET /v1/health (and
// records its version).
func (n *Node) probeGuest(ctx context.Context, r *vmRec, timeout time.Duration) (guestHealth, bool) {
	var h guestHealth
	addr, err := n.drv.GuestAddr(r.id)
	if err != nil {
		return h, false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}
	resp, err := doReq(ctx, &http.Client{Transport: n.guestHTTP}, "GET", "http://"+addr+"/v1/health", nil, hdr)
	if err != nil {
		return h, false
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h)
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	n.mu.Lock()
	r.lastHealthy = time.Now()
	n.mu.Unlock()
	if h.Version != "" {
		n.setState(r, func(s *proto.VMStatus) { s.GuestVersion = h.Version })
	}
	return h, true
}

// checkHealth runs one monitor probe and flips guestReady on a change.
func (n *Node) checkHealth(ctx context.Context, r *vmRec) {
	_, ok := n.probeGuest(ctx, r, healthTimeout)
	if ctx.Err() != nil {
		return
	}
	n.mu.Lock()
	was := r.unhealthy
	if ok {
		r.healthFails, r.unhealthy = 0, false
	} else if r.healthFails++; r.healthFails >= healthFailures {
		r.unhealthy = true
	}
	now, fails := r.unhealthy, r.healthFails
	n.mu.Unlock()
	if now == was {
		return
	}
	if now {
		n.log.Warn("guest not answering; sandbox degraded", "sandbox", r.id, "failedProbes", fails)
	} else {
		n.log.Info("guest answers again", "sandbox", r.id)
	}
	n.setState(r, func(s *proto.VMStatus) {
		if s.State == proto.VMRunning {
			s.GuestReady = !now && r.gate == gateOpen
		}
	})
}

// Clone gate (LIFE-009, Boat: "at ready the environment's repositories are
// cloned"): the first time a VM's guest answers, the node keeps reporting
// running with guestReady false (server: provisioned) until the guest says
// its boot-time repository clones are done, at most cloneGateMax.
const (
	gateNone     = iota // not checked yet
	gateChecking        // awaitClones runs
	gateOpen
)

var (
	cloneGateMax  = 10 * time.Minute
	cloneGatePoll = 500 * time.Millisecond
)

func (n *Node) awaitClones(r *vmRec) {
	start := time.Now()
	for logged := false; ; {
		n.mu.Lock()
		cur := n.vms[r.id] == r && r.gate == gateChecking
		n.mu.Unlock()
		if !cur || n.ctx.Err() != nil {
			return
		}
		h, ok := n.probeGuest(n.ctx, r, 3*time.Second)
		if ok && !h.ReposCloning || time.Since(start) > cloneGateMax {
			break
		}
		if ok && !logged {
			logged = true
			n.log.Info("waiting for the environment's repositories before ready", "sandbox", r.id)
		}
		if !sleepCtx(n.ctx, cloneGatePoll) {
			return
		}
	}
	st, ok := n.drv.Status(r.id)
	n.setState(r, func(s *proto.VMStatus) {
		r.gate = gateOpen
		if ok && s.State == proto.VMRunning && st.State == proto.VMRunning {
			s.GuestReady = st.GuestReady && !r.unhealthy
		}
	})
}

// onDriverStatus applies a status reported by the driver to a VM the node
// is tracking (ignored while the node drives a stop).
func (n *Node) onDriverStatus(st proto.VMStatus) {
	r := n.rec(st.SandboxID)
	if r == nil {
		return
	}
	n.setState(r, func(s *proto.VMStatus) {
		if s.State == proto.VMStopping || (s.State == proto.VMStopped && st.State != proto.VMStopped) {
			return
		}
		if st.State != proto.VMRunning || !st.GuestReady { // new boot: health starts over
			r.healthFails, r.unhealthy = 0, false
		}
		ready := st.GuestReady && !r.unhealthy
		if ready && r.gate != gateOpen {
			if r.gate == gateNone {
				r.gate = gateChecking
				go n.awaitClones(r)
			}
			ready = false
		}
		s.State, s.GuestReady, s.Error = st.State, ready, st.Error
		if !st.StartedAt.IsZero() {
			s.StartedAt = st.StartedAt
		}
	})
}

func (n *Node) guestReady(r *vmRec) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return r.status.GuestReady || r.status.State == proto.VMRunning
}

// guestEvents follows GET /v1/internal/events/stream and forwards each line
// as an EvGuest event; reconnects while the VM lives.
func (n *Node) guestEvents(ctx context.Context, r *vmRec) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		if !n.guestReady(r) {
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		start := time.Now()
		err := n.followGuestEvents(ctx, r)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 10*time.Second {
			backoff = 500 * time.Millisecond
		}
		n.log.Debug("guest event stream ended", "sandbox", r.id, "err", err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (n *Node) followGuestEvents(ctx context.Context, r *vmRec) error {
	addr, err := n.drv.GuestAddr(r.id)
	if err != nil {
		return err
	}
	hdr := http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}
	// Replay what the guest emitted before (re)connecting, e.g. setup.status
	// events sent during boot before the first subscription.
	n.mu.Lock()
	since := r.lastGuestEv
	n.mu.Unlock()
	if since.IsZero() {
		since = time.Unix(1, 0)
	}
	resp, err := doReq(ctx, &http.Client{Transport: n.guestHTTP}, "GET", "http://"+addr+"/v1/internal/events/stream?since="+url.QueryEscape(since.UTC().Format(time.RFC3339Nano)), nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	go n.syncGuestFirewall(ctx, r) // the agent (re)started: its boot reset the firewall
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ge proto.GuestEvent
		if err := json.Unmarshal(line, &ge); err != nil {
			continue
		}
		if ge.Time.IsZero() {
			ge.Time = time.Now().UTC()
		}
		n.mu.Lock()
		r.lastEvent = time.Now()
		if ge.Time.After(r.lastGuestEv) {
			r.lastGuestEv = ge.Time
		}
		n.mu.Unlock()
		n.emitEvent(proto.Event{Time: ge.Time, SandboxID: r.id, Type: proto.EvGuest, Data: append(json.RawMessage(nil), line...)})
	}
	return sc.Err()
}

// StopVM powers the VM off into snapshot snapID and uploads it in the background.
func (n *Node) StopVM(ctx context.Context, id string, req proto.StopRequest) (proto.SnapshotRef, error) {
	r := n.rec(id)
	if r == nil {
		return proto.SnapshotRef{}, errNotFound
	}
	r.op.Lock()
	defer r.op.Unlock()
	n.mu.Lock()
	done, prev := r.status.State == proto.VMStopped, r.stopRef
	n.mu.Unlock()
	if done && prev.ID != "" { // stopped already: the same answer as the first time
		return prev, nil
	}
	if req.SnapshotID == "" && !r.spec.NoSnapshots { // snapshots off: the disk is erased
		req.SnapshotID = api.NewID(api.PrefixSnapshot, 20)
	}
	n.setState(r, func(s *proto.VMStatus) { s.State = proto.VMStopping })
	// Boat stop: pre-stop hook, docker stop, cleanup, sync, freeze; the
	// guest's last events (command output flush) are forwarded before the
	// VM halts. The driver then halts it without an OS shutdown.
	n.prepareStop(ctx, r, req.Force)
	n.stopWatch(r)
	ref, err := n.drv.Stop(ctx, id, req.SnapshotID, true)
	if err != nil {
		if addr, aerr := n.drv.GuestAddr(id); aerr == nil { // still running: undo the freeze now
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if resp, err := doReq(tctx, &http.Client{Transport: n.guestHTTP}, "POST", "http://"+addr+"/v1/internal/thaw", nil,
				http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}); err == nil {
				resp.Body.Close()
			}
			cancel()
		}
		n.setState(r, func(s *proto.VMStatus) { s.State, s.Error = proto.VMError, "stop: "+err.Error() })
		return ref, err
	}
	n.mu.Lock()
	r.stopRef = ref
	n.mu.Unlock()
	n.setState(r, func(s *proto.VMStatus) {
		s.State, s.GuestReady, s.Error, s.Hydrating = proto.VMStopped, false, "", false
	})
	if ref.ID != "" {
		n.uploadAsync(id, ref, false)
	}
	return ref, nil
}

var (
	prepareStopTimeout       = 120 * time.Second // hook 60 s + docker stop 30 s + cleanup
	forcedPrepareStopTimeout = 15 * time.Second
	unreadyProbeTimeout      = 2 * time.Second
	eventDrainIdle           = 300 * time.Millisecond
	eventDrainMax            = 3 * time.Second
)

// prepareStop runs the guest's prepare-stop and then waits until its event
// stream has gone quiet. A guest that is not known to answer (degraded,
// still booting) gets a short forced prepare-stop: no hook and no Docker
// stop, but its filesystems are still cleaned, synced and frozen.
func (n *Node) prepareStop(ctx context.Context, r *vmRec, force bool) {
	addr, err := n.drv.GuestAddr(r.id)
	if err != nil {
		return
	}
	n.mu.Lock()
	healthy := !r.unhealthy
	n.mu.Unlock()
	st, _ := n.drv.Status(r.id)
	timeout := prepareStopTimeout
	if !healthy || st.State != proto.VMRunning || !st.GuestReady {
		if _, ok := n.probeGuest(ctx, r, unreadyProbeTimeout); !ok {
			return // hung or not booted: nothing would answer
		}
		force = true
	}
	if force {
		timeout = forcedPrepareStopTimeout
	}
	// Only the qemu driver halts the VM right after; the process driver's
	// guest is a host process.
	body := proto.PrepareStopRequest{Force: force, Freeze: n.drv.Name() == "qemu", FreezeTimeoutSeconds: 120}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	hdr := http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}
	t0 := time.Now()
	resp, err := doReq(cctx, &http.Client{Transport: n.guestHTTP}, "POST", "http://"+addr+"/v1/internal/prepare-stop", jsonBody(body), hdr)
	cancel()
	if err != nil {
		n.log.Warn("guest prepare-stop failed; stopping anyway", "sandbox", r.id, "err", err)
	} else {
		resp.Body.Close()
		n.log.Info("guest prepared for stop", "sandbox", r.id, "took", time.Since(t0).Round(time.Millisecond))
	}
	// Drain: let events emitted before the reply arrive (they travel on
	// another connection), until the stream is idle or eventDrainMax.
	sleepCtx(ctx, eventDrainIdle)
	for deadline := time.Now().Add(eventDrainMax); time.Now().Before(deadline); {
		n.mu.Lock()
		idle := time.Since(r.lastEvent)
		n.mu.Unlock()
		if idle >= eventDrainIdle || !sleepCtx(ctx, 50*time.Millisecond) {
			return
		}
	}
}

// SnapshotVM takes a live snapshot and uploads it in the background.
func (n *Node) SnapshotVM(ctx context.Context, id, snapID string) (proto.SnapshotRef, error) {
	r := n.rec(id)
	if r == nil {
		return proto.SnapshotRef{}, errNotFound
	}
	r.op.Lock()
	defer r.op.Unlock()
	periodic := snapID == "" // the server records the others itself
	if periodic {
		snapID = api.NewID(api.PrefixSnapshot, 20)
	}
	ref, err := n.drv.Snapshot(ctx, id, snapID) // the driver syncs/freezes the guest
	if err != nil {
		return ref, err
	}
	n.mu.Lock()
	r.lastSnap = time.Now()
	n.mu.Unlock()
	n.uploadAsync(id, ref, periodic) // reports a periodic layer while r.op is held
	return ref, nil
}

// guestSync asks the guest to flush filesystems before a live snapshot.
func (n *Node) guestSync(ctx context.Context, r *vmRec) error {
	addr, err := n.drv.GuestAddr(r.id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}
	resp, err := doReq(ctx, &http.Client{Transport: n.guestHTTP}, "POST", "http://"+addr+"/v1/internal/sync", nil, hdr)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (n *Node) RestartVM(ctx context.Context, id string) (proto.VMStatus, error) {
	r := n.rec(id)
	if r == nil {
		return proto.VMStatus{}, errNotFound
	}
	r.op.Lock()
	defer r.op.Unlock()
	if err := n.drv.Restart(ctx, id); err != nil {
		return proto.VMStatus{}, err
	}
	n.setState(r, func(s *proto.VMStatus) {
		s.State, s.GuestReady, s.Error, s.StartedAt = proto.VMStarting, false, "", time.Now().UTC()
		r.healthFails, r.unhealthy = 0, false
	})
	n.mu.Lock()
	n.startWatch(r)
	st, hasDirect := r.status, r.direct != nil
	n.mu.Unlock()
	if !hasDirect {
		n.openDirect(r)
	}
	return st, nil
}

// DestroyVM kills the VM and deletes its overlay. Idempotent.
func (n *Node) DestroyVM(ctx context.Context, id string) error {
	r := n.rec(id)
	if r != nil {
		if r.abort != nil {
			r.abort()
		}
		r.op.Lock()
		defer r.op.Unlock()
		n.stopWatch(r)
	}
	err := n.drv.Destroy(ctx, id)
	n.mu.Lock()
	delete(n.vms, id)
	n.saveSpecsLocked()
	n.mu.Unlock()
	if r != nil {
		// The owner should see why a VM vanished, e.g. a stale copy dropped
		// after this node slept and the sandbox moved on (field test).
		n.log.Info("vm removed (the server asked: deleted, or now running elsewhere)", "sandbox", id, "err", err)
		n.emit(id, proto.EvVMState, proto.VMStatus{SandboxID: id, State: proto.VMStopped})
	}
	return err
}

// snapshotLoop takes periodic live snapshots of running VMs.
func (n *Node) snapshotLoop(ctx context.Context) {
	t := time.NewTicker(snapshotTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		every := n.snapshotInterval()
		if every <= 0 {
			continue
		}
		var due []*vmRec
		n.mu.Lock()
		for _, r := range n.vms {
			// Not for snapshots-off sandboxes (never saved) nor for a guest
			// that does not answer (it could not sync or freeze).
			if r.status.State == proto.VMRunning && r.status.GuestReady && !r.spec.NoSnapshots && time.Since(r.lastSnap) >= every {
				due = append(due, r)
			}
		}
		n.mu.Unlock()
		for _, r := range due {
			id := r.id
			if !n.diskChanged(ctx, r) { // idle VM: nothing new to save or upload
				n.mu.Lock()
				r.lastSnap = time.Now()
				n.mu.Unlock()
				continue
			}
			if _, err := n.SnapshotVM(ctx, id, ""); err != nil {
				n.log.Warn("periodic snapshot failed", "sandbox", id, "err", err)
				if r := n.rec(id); r != nil {
					n.mu.Lock()
					r.lastSnap = time.Now() // retry after the next interval
					n.mu.Unlock()
				}
			}
		}
	}
}

var snapshotTick = 10 * time.Second

// diskChanged: whether the VM's disk changed since its last snapshot, after
// the guest flushed its page cache (true when the driver cannot tell).
func (n *Node) diskChanged(ctx context.Context, r *vmRec) bool {
	ct, ok := n.drv.(vm.ChangeTracker)
	if !ok {
		return true
	}
	if err := n.guestSync(ctx, r); err != nil {
		return true
	}
	changed, err := ct.DiskChanged(r.id)
	return err != nil || changed
}

var errNotFound = errors.New("vm not found")

// ---------------------------------------------------------------------------
// Guest connections (tunnel, direct ports)
// ---------------------------------------------------------------------------

// dialGuestPort opens a raw TCP stream to port inside the guest.
func (n *Node) dialGuestPort(ctx context.Context, r *vmRec, port int) (net.Conn, error) {
	if d, ok := n.drv.(vm.Dialer); ok {
		return d.DialGuest(ctx, r.id, port)
	}
	addr, err := n.drv.GuestAddr(r.id)
	if err != nil {
		return nil, err
	}
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}
	return mux.DialUpgrade(conn, addr, "/v1/tunnel?port="+strconv.Itoa(port), hdr)
}
