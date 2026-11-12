package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Warm pool (SIZE-012, ADAPTED: optional, per node, off by default). With
// `crowdvm-node config set warm-pool N` the node keeps N VMs booted with
// the image, size and nested setting of the last plain create (no
// snapshot, no per-sandbox net policy; remembered in <data dir>/warm.json).
// A create of that shape takes one: the guest is adopted (POST
// /v1/internal/adopt with the sandbox's GuestConfig, then it boots as that
// sandbox: hostname, machine-id, SSH host keys, env, repos) and the driver
// renames the VM, so the sandbox is ready in about a second instead of a
// boot. Pool VMs only use room the owner shares; they are not reported to
// the server (neither as VMs nor in the usage), are destroyed first when a
// create needs their room, and when the node restarts. Needs a driver that
// implements vm.Renamer (qemu).

const warmPrefix = "warm_"

var (
	warmTick  = 5 * time.Second
	warmToken = func() string { return api.NewID("", 32) }
)

type warmVM struct {
	id    string
	spec  proto.VMSpec
	ready bool
}

func (n *Node) warmShapePath() string { return filepath.Join(n.cfg.DataDir, "warm.json") }

func warmMatch(shape *proto.VMSpec, spec proto.VMSpec) bool {
	return shape != nil && spec.ParentID == "" && spec.Net == nil && !spec.GPU && spec.ImageID == shape.ImageID &&
		spec.Size == shape.Size && spec.Nested == shape.Nested
}

// noteWarmShapeLocked (n.mu held) remembers a plain create's shape.
func (n *Node) noteWarmShapeLocked(spec proto.VMSpec) {
	if spec.Guest.Channel != nil {
		// A channel sandbox never takes a pool VM (its config must not pass
		// through this node): while the server sends those, keep no pool
		// (bugs-r14-opus 7).
		if n.warmShape != nil {
			n.warmShape = nil
			_ = os.Remove(n.warmShapePath())
		}
		return
	}
	if spec.ParentID != "" || spec.Net != nil || spec.GPU || spec.ImageID == "" || warmMatch(n.warmShape, spec) {
		return
	}
	n.warmShape = &proto.VMSpec{ImageID: spec.ImageID, Size: spec.Size, Nested: spec.Nested}
	b, _ := json.Marshal(n.warmShape)
	_ = os.WriteFile(n.warmShapePath(), b, 0o644)
}

// takeWarmLocked (n.mu held) removes a ready pool VM that fits spec.
func (n *Node) takeWarmLocked(spec proto.VMSpec) *warmVM {
	for id, w := range n.warm {
		if w.ready && warmMatch(&w.spec, spec) {
			delete(n.warm, id)
			return w
		}
	}
	return nil
}

// evictWarmLocked (n.mu held) removes one pool VM (booting ones first) for
// the caller to destroy.
func (n *Node) evictWarmLocked() *warmVM {
	var pick *warmVM
	for _, w := range n.warm {
		if pick == nil || (!w.ready && pick.ready) {
			pick = w
		}
	}
	if pick != nil {
		delete(n.warm, pick.id)
	}
	return pick
}

func (n *Node) destroyWarm(w *warmVM) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := n.drv.Destroy(ctx, w.id); err != nil {
		n.log.Warn("warm pool: destroy", "vm", w.id, "err", err)
	}
}

// warmLoop keeps the pool at the configured size.
func (n *Node) warmLoop(ctx context.Context) {
	if _, ok := n.drv.(vm.Renamer); !ok {
		return
	}
	if b, err := os.ReadFile(n.warmShapePath()); err == nil {
		var sh proto.VMSpec
		if json.Unmarshal(b, &sh) == nil && sh.ImageID != "" {
			n.mu.Lock()
			n.warmShape = &sh
			n.mu.Unlock()
		}
	}
	var backoff time.Time
	for {
		want := n.config().WarmPool
		if n.config().EffectivePause(time.Now()).Paused {
			want = 0 // paused or outside the schedule: give the machine back
		}
		n.mu.Lock()
		shape := n.warmShape
		var drop []*warmVM
		for id, w := range n.warm {
			if !warmMatch(shape, w.spec) || len(n.warm)-len(drop) > max(want, 0) {
				drop = append(drop, w)
				delete(n.warm, id)
			}
		}
		drop = append(drop, n.warmOverCapacityLocked()...) // bugs-r13-opus 8
		var add *warmVM
		if shape != nil && len(n.warm) < want && time.Now().After(backoff) && n.warmFitsLocked(*shape) {
			id := warmPrefix + api.NewID("", 12)
			sp := *shape
			sp.SandboxID = id
			sp.Guest = proto.GuestConfig{SandboxID: id, Token: warmToken(), Boot: proto.BootWarm,
				ListenAddr: fmt.Sprintf(":%d", proto.GuestPort)}
			add = &warmVM{id: id, spec: sp}
			n.warm[id] = add
		}
		n.mu.Unlock()
		for _, w := range drop {
			n.destroyWarm(w)
		}
		if add != nil {
			if err := n.bootWarm(ctx, add); err != nil {
				n.log.Warn("warm pool: boot failed", "err", err)
				n.mu.Lock()
				delete(n.warm, add.id)
				n.mu.Unlock()
				n.destroyWarm(add)
				backoff = time.Now().Add(time.Minute)
			}
		}
		if !sleepCtx(ctx, warmTick) {
			return
		}
	}
}

// warmOverCapacityLocked (n.mu held) removes pool VMs, unready ones first,
// until real usage plus the pool fits the current cpus, memory and
// max-sandboxes (the owner may have lowered them) and returns them.
// bugs-r13-opus 8
func (n *Node) warmOverCapacityLocked() []*warmVM {
	cp := n.config().Capacity()
	var drop []*warmVM
	for len(n.warm) > 0 {
		u := n.usageWithWarmLocked()
		if u.VCPU <= cp.VCPU && u.MemoryMiB <= cp.MemoryMiB && u.VMs <= cp.MaxSandboxes {
			break
		}
		var victim *warmVM
		for _, w := range n.warm {
			if victim == nil || (victim.ready && !w.ready) {
				victim = w
			}
		}
		delete(n.warm, victim.id)
		drop = append(drop, victim)
	}
	return drop
}

// warmFitsLocked: one more pool VM leaves room (and real disk) for a create.
func (n *Node) warmFitsLocked(shape proto.VMSpec) bool {
	if n.config().EffectivePause(time.Now()).Paused {
		return false
	}
	cp, disk := n.capacity()
	u := n.usageWithWarmLocked()
	return disk.HeadroomBytes >= 2*minDiskHeadroom && u.VMs+1 < cp.MaxSandboxes &&
		u.VCPU+shape.Size.VCPU <= cp.VCPU && u.MemoryMiB+shape.Size.MemoryMiB <= cp.MemoryMiB &&
		u.DiskGiB+shape.Size.DiskGiB <= cp.DiskGiB
}

func (n *Node) bootWarm(ctx context.Context, w *warmVM) error {
	bctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := n.ensureChain(bctx, "", w.spec.ImageID, ""); err != nil {
		return err
	}
	if err := n.drv.Start(n.ctx, w.spec); err != nil {
		return err
	}
	if err := n.waitGuestUp(bctx, w.id); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.warm[w.id] != w {
		return nil // evicted meanwhile (its evictor destroys it)
	}
	w.ready = true
	n.log.Info("warm pool: vm ready", "vm", w.id)
	return nil
}

// adoptWarm turns pool VM w into sandbox r; on failure it boots r normally.
func (n *Node) adoptWarm(r *vmRec, w *warmVM) {
	r.op.Lock()
	defer r.op.Unlock()
	defer r.abort()
	if r.bootCtx.Err() != nil {
		n.destroyWarm(w)
		return
	}
	t0 := time.Now()
	if err := n.adoptGuest(r, w); err != nil {
		n.log.Warn("warm pool: adoption failed; booting normally", "sandbox", r.id, "err", err)
		n.destroyWarm(w)
		n.bootLocked(r)
		return
	}
	n.log.Info("warm pool: sandbox adopted a pool vm", "sandbox", r.id, "vm", w.id, "took", time.Since(t0).Round(time.Millisecond))
	n.setState(r, func(s *proto.VMStatus) { s.StartedAt = time.Now().UTC() })
	n.openDirect(r)
	n.mu.Lock()
	n.startWatch(r)
	n.mu.Unlock()
}

func (n *Node) adoptGuest(r *vmRec, w *warmVM) error {
	addr, err := n.drv.GuestAddr(w.id)
	if err != nil {
		return err
	}
	cfg := r.spec.Guest
	cfg.ListenAddr = fmt.Sprintf(":%d", proto.GuestPort)
	ctx, cancel := context.WithTimeout(r.bootCtx, time.Minute)
	defer cancel()
	c := &http.Client{Transport: n.guestHTTP}
	resp, err := doReq(ctx, c, "POST", "http://"+addr+"/v1/internal/adopt", jsonBody(cfg),
		http.Header{"Authorization": {"Bearer " + w.spec.Guest.Token}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	// The agent restarts as the sandbox: wait until it answers as such.
	for {
		if resp, err := doReq(ctx, c, "GET", "http://"+addr+"/v1/health", nil, http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}); err == nil {
			var h struct {
				SandboxID string `json:"sandboxId"`
			}
			json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
			if h.SandboxID == r.id {
				break
			}
		}
		if !sleepCtx(ctx, 50*time.Millisecond) {
			return fmt.Errorf("adopted guest did not answer: %w", ctx.Err())
		}
	}
	return n.drv.(vm.Renamer).Rename(ctx, w.id, r.spec)
}
