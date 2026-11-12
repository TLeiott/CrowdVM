package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// snapshotDataExpiredErr: the stopped sandbox's snapshot data is gone
// (deleted, or expired under the retention policy).
func snapshotDataExpiredErr(sb *Sandbox) error {
	d := map[string]any{"retryable": false, "sandboxId": sb.ID}
	if v, ok := sb.Extra["snapshotDataExpiredAt"].(float64); ok && v > 0 {
		d["dataExpiredAt"] = tsMs(int64(v))
	}
	return ErrD(409, "snapshot_data_expired", "The snapshot data of this sandbox no longer exists (its snapshots were deleted or expired under the retention policy), so it cannot be resumed or forked. Deploy a saved snapshot or create a new sandbox.", d)
}

// chainRestorable (API-127 resume_failed): every layer of the disk chain
// still exists (not purged, parent present).
func (s *Server) chainRestorable(chain []string) error {
	for i, id := range chain {
		var parent sql.NullString
		var purged sql.NullInt64
		err := s.DB.QueryRow(`SELECT parent_id, purged_at FROM snapshots WHERE id=?`, id).Scan(&parent, &purged)
		if err == nil && purged.Valid { // erased for good
			return ErrD(409, "resume_failed", "The sandbox's disk cannot be restored: snapshot "+id+" no longer exists. Deploy a saved snapshot or create a new sandbox.",
				map[string]any{"retryable": false, "snapshotId": id})
		}
		if err != nil || (i == len(chain)-1 && parent.String != "") {
			// Not recorded yet: a node's report of a layer can arrive late
			// (e.g. after a server restart); it registers with its upload.
			missingID := id
			if err == nil {
				missingID = parent.String
			}
			return ErrD(409, "snapshot_unavailable", "Snapshot "+missingID+" of this sandbox is not registered yet (its node is still reporting or uploading it). Retry in a minute.",
				map[string]any{"retryable": true, "retryAfterSeconds": 30, "snapshotId": missingID})
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// State transitions
// ---------------------------------------------------------------------------

var startingStates = []string{api.StateProvisioning, api.StateProvisioned, api.StateCloning}
var usableStates = []string{api.StateReady, api.StateIdle, api.StateRunning}

// transition moves sandbox id to state `to` when its current state is in
// from (nil = any), applying sets. It publishes sandbox.state on a change
// and every event in evs (Type/Reason/Data are kept, the rest is filled).
func (s *Server) transition(id string, from []string, to string, sets map[string]any, evs ...Event) (*Sandbox, bool) {
	return s.transitionIf(id, func(state, _ string) bool { return from == nil || slices.Contains(from, state) }, to, sets, evs...)
}

// transitionIf is transition with a condition on the current state and
// node, checked in the same transaction.
func (s *Server) transitionIf(id string, cond func(state, nodeID string) bool, to string, sets map[string]any, evs ...Event) (*Sandbox, bool) {
	var prev, node string
	ok := false
	for i := range evs {
		if evs[i].ID == "" {
			evs[i].ID = "evt_" + randHex(16)
		}
	}
	err := s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT state, COALESCE(node_id,'') FROM sandboxes WHERE id=? AND deleted_at IS NULL`, id).Scan(&prev, &node); err != nil {
			return err
		}
		if !cond(prev, node) {
			return nil
		}
		parts := []string{"state=?", "updated_at=?"}
		args := []any{to, store.Now()}
		for k, v := range sets {
			parts = append(parts, k+"=?")
			args = append(args, v)
		}
		args = append(args, id)
		if _, err := tx.Exec(`UPDATE sandboxes SET `+strings.Join(parts, ",")+` WHERE id=?`, args...); err != nil {
			return err
		}
		for _, ev := range evs {
			for _, fn := range s.Hooks.EventTx {
				if err := fn(tx, Event{ID: ev.ID, Type: ev.Type, SandboxID: id, PreviousState: prev, State: to, Reason: ev.Reason, Data: ev.Data}); err != nil {
					return err
				}
			}
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return nil, false
	}
	sb, err := s.SandboxByID(id)
	if err != nil {
		return nil, false
	}
	base := Event{SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, NodeID: sb.NodeID, PreviousState: prev, State: to}
	if prev != to {
		e := base
		e.Type = EvSandboxState
		s.Bus.Publish(e)
	}
	for _, ev := range evs {
		e := base
		e.ID, e.Type, e.Reason, e.Data = ev.ID, ev.Type, ev.Reason, ev.Data
		s.Bus.Publish(e)
	}
	return sb, true
}

// readyState is the usable state matching the agent activity.
func readyState(sb *Sandbox) string {
	switch sb.AgentState {
	case "running":
		return api.StateRunning
	case "idle":
		return api.StateIdle
	}
	return api.StateReady
}

// failSandbox moves a starting/usable sandbox to error.
func (s *Server) failSandbox(id, msg string) {
	if _, ok := s.transition(id, append(append([]string{}, startingStates...), usableStates...), api.StateError,
		map[string]any{"error": msg}, Event{Type: EvSandboxError, Reason: msg}); ok {
		s.closeRun(id)
		s.Log.Warn("sandbox failed", "sandbox", id, "err", msg)
	}
}

// stateWaiters wakes callers waiting for a sandbox state change.
type stateWaiters struct {
	mu sync.Mutex
	m  map[string][]chan struct{}
}

func newStateWaiters() *stateWaiters { return &stateWaiters{m: map[string][]chan struct{}{}} }

func (w *stateWaiters) wait(id string) <-chan struct{} {
	ch := make(chan struct{})
	w.mu.Lock()
	w.m[id] = append(w.m[id], ch)
	w.mu.Unlock()
	return ch
}

func (w *stateWaiters) onEvent(e Event) {
	if e.Type != EvSandboxState && e.Type != EvSandboxDeleted {
		return
	}
	w.mu.Lock()
	chs := w.m[e.SandboxID]
	delete(w.m, e.SandboxID)
	w.mu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
}

// WaitUsable waits up to d while sb is starting. It returns the sandbox in
// its latest state (usable or not).
func (s *Server) WaitUsable(ctx context.Context, id string, d time.Duration) (*Sandbox, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	tick := time.NewTicker(time.Second) // safety net for missed events
	defer tick.Stop()
	ch := s.waiters.wait(id)
	for {
		sb, err := s.SandboxByID(id)
		if err != nil || sb.DeletedAt != nil {
			return nil, errNotFound("Sandbox")
		}
		if !sb.starting() {
			return sb, nil
		}
		select {
		case <-ch:
			ch = s.waiters.wait(id)
		case <-tick.C:
		case <-t.C:
			return sb, nil
		case <-ctx.Done():
			return sb, ctx.Err()
		}
	}
}

// ---------------------------------------------------------------------------
// Environments hook types
// ---------------------------------------------------------------------------

// EnvRequest asks the environments feature which environment a sandbox gets.
type EnvRequest struct {
	User    *User
	Org     *Org
	Name    string // requested environment ("" = default)
	NoEnv   bool
	Op      string   // "create", "resume", "fork"
	Sandbox *Sandbox // resume/fork source
}

// ResolvedEnv pins a sandbox to an environment version.
type ResolvedEnv struct {
	Name, ID    string
	Version     *int
	HoldsLogins bool // passes its creator's personal logins
}

// GuestPrep is what PrepareGuest hooks may change before a boot.
type GuestPrep struct {
	Config *proto.GuestConfig
	Spec   *proto.VMSpec
	Boot   proto.BootKind
}

func (s *Server) resolveEnv(c *Ctx, req EnvRequest) (*ResolvedEnv, error) {
	if s.Hooks.ResolveEnvironment != nil {
		r, err := s.Hooks.ResolveEnvironment(c.R.Context(), req)
		if err != nil || r != nil {
			return r, err
		}
	}
	if req.Name != "" && req.Name != "base" {
		return nil, Err(400, "unknown_environment", "Unknown environment "+req.Name+".")
	}
	if req.Sandbox != nil && req.Name == "" {
		return &ResolvedEnv{Name: req.Sandbox.Environment, ID: req.Sandbox.EnvironmentID, Version: req.Sandbox.EnvironmentVersion, HoldsLogins: req.Sandbox.HoldsCreatorLogins && !req.NoEnv}, nil
	}
	return &ResolvedEnv{}, nil
}

// ---------------------------------------------------------------------------
// Boot pipeline
// ---------------------------------------------------------------------------

type bootOpts struct {
	Cleanup   string // node that may still hold a dead VM of this sandbox (resume from error)
	Kind      proto.BootKind
	FirstBoot bool
	Parent    string       // snapshot to boot from ("" = base image)
	Env       *ResolvedEnv // unused by the core; kept for hooks
	LiveFrom  string       // fork: take a live snapshot of this running sandbox first
	FailFast  bool         // a node that refuses for capacity fails the boot instead of queueing it
	Retried   bool         // failFast: already placed again once after a refusal
}

func (s *Server) startBoot(sb *Sandbox, o bootOpts) {
	s.Go(func(ctx context.Context) {
		if err := s.boot(ctx, sb.ID, o); err != nil {
			var nf *nodeFullError
			if errors.As(err, &nf) && s.requeueBoot(sb.ID, o, nf) {
				return
			}
			s.failSandbox(sb.ID, err.Error())
		}
	})
}

// nodeFullError: the node picked for a boot refused it for capacity (its
// real disk budget is short although its advertised capacity fit).
type nodeFullError struct {
	parent, node, msg string
	offline           bool // the node died under the boot
}

func (e *nodeFullError) Error() string { return "out_of_capacity: " + e.msg }

// requeueBoot handles a node's capacity refusal like a full pool (SIZE-011):
// the boot waits for a node with room, or fails at once with failFast.
func (s *Server) requeueBoot(id string, o bootOpts, nf *nodeFullError) bool {
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil {
		return true
	}
	state := sb.State
	if nf.offline && sb.State == api.StateError && sb.Error == "node_offline" && sb.NodeID == nf.node {
		// The server noticed the node's death first: the boot never ran, so
		// it is placed again rather than left in error.
		state = api.StateProvisioning
		_, _ = s.DB.Exec(`UPDATE sandboxes SET error=NULL, health='ok', health_reason=NULL, degraded_since=NULL WHERE id=? AND state='error' AND error='node_offline'`, id)
	} else if !sb.starting() {
		return true
	}
	if o.FailFast && !nf.offline && !o.Retried {
		// The node refused before the server learned it was paused or full
		// (its next heartbeat): try once more on another node with room now
		// (bugs-r13-opus 11).
		spec, _ := api.SizeByName(sb.Size)
		spec = diskSpec(sb, spec)
		req := schedReq{Size: spec, Placement: sb.Placement, Chain: s.snapshotChain(nf.parent), ImageID: sb.ImageID, Exclude: nf.node, Owners: s.ownersFor(sb.OwnerID, sb.OrgID), Encrypted: sb.Disk == "luks"}
		if _, err := s.placeSandbox(id, state, req); err == nil {
			o.Retried = true
			if nf.parent != "" {
				o.Parent, o.LiveFrom = nf.parent, "" // a fork's live snapshot is taken
			}
			if sb, err := s.SandboxByID(id); err == nil {
				s.startBoot(sb, o)
				return true
			}
		}
	}
	if o.FailFast {
		msg := "no_ready_machine: the node had no room for this sandbox (" + nf.msg + ")"
		if nf.offline {
			msg = "no_ready_machine: the node went offline while it started this sandbox"
		}
		s.abortBoot(sb, o.Kind, nf.parent, msg)
		return true
	}
	reason := "capacity"
	if nf.offline {
		reason = "node_offline"
	}
	s.Log.Info("node could not take the boot; placing it again", "sandbox", id, "node", nf.node, "reason", reason)
	cleanup := o.Cleanup
	if nf.offline && cleanup == "" {
		cleanup = nf.node // it may still serve the boot late: its machine goes first
	}
	s.queueBoot(id, pendingBoot{Kind: o.Kind, FirstBoot: o.FirstBoot, Parent: nf.parent, Cleanup: cleanup, State: state,
		Reason: reason, NotBefore: store.Now() + NodeFullBackoff.Milliseconds()})
	return true
}

// NodeFullBackoff keeps a refused boot from being placed again before the
// node's next heartbeat reports its real free disk (tests shorten it).
var NodeFullBackoff = 5 * time.Second

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

func hostnameFor(sb *Sandbox) string {
	h := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(sb.Name), "-"), "-")
	if h == "" || len(h) > 40 || strings.HasPrefix(h, "sandbox-") {
		h = strings.ReplaceAll(sb.ID, "_", "-")
	}
	return h
}

func (s *Server) stillStarting(id string) (*Sandbox, error) {
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil {
		return nil, errors.New("sandbox deleted")
	}
	if !sb.starting() {
		return nil, errAborted
	}
	return sb, nil
}

var errAborted = errors.New("aborted")

func (s *Server) boot(ctx context.Context, id string, o bootOpts) (err error) {
	defer func() {
		if errors.Is(err, errAborted) {
			err = nil
		}
	}()
	sb, err := s.stillStarting(id)
	if err != nil {
		return err
	}
	if o.Cleanup != "" {
		if c := s.nodes.get(o.Cleanup); c != nil {
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			_ = c.Do(cctx, "DELETE", "/v1/vms/"+id, nil, nil)
			cancel()
		}
	}
	parent := o.Parent
	if o.LiveFrom != "" {
		if src, err := s.SandboxByID(o.LiveFrom); err != nil {
			// The source was deleted while this fork waited for a node: boot
			// from its last snapshot (kept for the fork by the snapshot GC).
			var snap string
			if s.DB.QueryRow(`SELECT COALESCE(head_snapshot_id,'') FROM sandboxes WHERE id=?`, o.LiveFrom).Scan(&snap) != nil || snap == "" {
				return fmt.Errorf("fork_failed: source vanished")
			}
			parent = snap
		} else {
			snap, err := s.takeLiveSnapshot(ctx, src)
			if err != nil {
				if src.HeadSnapshotID == "" {
					return fmt.Errorf("fork_failed: %v", err)
				}
				s.Log.Warn("live snapshot failed, forking from the last snapshot", "source", src.ID, "err", err)
				snap = src.HeadSnapshotID
			}
			parent = snap
			_, _ = s.DB.Exec(`UPDATE sandboxes SET source_id=? WHERE id=? AND source_kind='fork'`, src.ID, id)
			if sb, err = s.stillStarting(id); err != nil {
				return err
			}
		}
	}
	node := s.nodes.get(sb.NodeID)
	if node == nil {
		return fmt.Errorf("node_offline: the node picked for this sandbox went offline")
	}
	n, _ := s.NodeByID(node.ID)
	// Base image.
	if sb.ImageID != "" && n != nil && !slices.Contains(n.Info.Images, sb.ImageID) {
		if _, err := s.Blobs.Stat(store.BlobImage, sb.ImageID); err == nil {
			fctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
			err := node.Do(fctx, "POST", "/v1/images/"+sb.ImageID+"/fetch", map[string]any{"imageId": sb.ImageID}, nil)
			cancel()
			if err != nil {
				return fmt.Errorf("image_fetch_failed: %v", err)
			}
		}
	}
	// Snapshot chain: the sandbox is not hydrated until its data is on the
	// node (ready sets hydrated again).
	if parent != "" {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET hydrated=0, hydrated_at=NULL WHERE id=?`, id)
		// LIFE-013: a lazy-chain node boots at once and reads missing
		// layers from the server; only their upload is awaited.
		lazy := n != nil && n.Info.LazyChain
		s.grantChain(node.ID, parent) // the node may download these layers (mayFetch)
		if err := s.ensureChain(ctx, id, node, parent, lazy); err != nil {
			return err
		}
	}
	if sb, err = s.stillStarting(id); err != nil {
		return err
	}
	spec, _ := api.SizeByName(sb.Size)
	spec = diskSpec(sb, spec) // the node never shrinks a disk (bugs-r17-opus 9)
	if prev, _ := sb.Extra["diskGiB"].(float64); int(prev) < spec.DiskGiB {
		_ = s.SetSandboxExtra(sb.ID, "diskGiB", spec.DiskGiB)
	}
	token := randToken(40)
	env := map[string]string{}
	for k, v := range sb.Env {
		env[k] = v
	}
	env["CROWDVM_SANDBOX_ID"] = sb.ID
	env["CROWDVM_API_URL"] = s.Cfg.PublicURL + "/api/v1"
	if !sb.NoEnv && !sb.WipePending {
		if key, err := s.rotateSandboxKey(sb); err == nil {
			env["CROWDVM_API_KEY"] = key
		}
	}
	if tok, err := s.sandboxToken(sb); err == nil {
		env["ASCII_TOKEN"], env["CROWDVM_TOKEN"] = tok, tok
	}
	g := proto.GuestConfig{
		SandboxID: sb.ID, Token: token, ListenAddr: fmt.Sprintf(":%d", proto.GuestPort), Hostname: hostnameFor(sb),
		Boot: o.Kind, FirstBoot: o.FirstBoot, Env: env,
		Desktop: proto.DesktopConfig{Width: 1920, Height: 1080},
	}
	if o.FirstBoot {
		g.Files, g.SetupScript = sb.Files, sb.SetupScript
	}
	g.NoMonitoring = !sb.Monitoring
	vs := proto.VMSpec{SandboxID: sb.ID, ImageID: sb.ImageID, ParentID: parent, Nested: sb.Nested, GPU: sb.Placement.GPU, NoSnapshots: !sb.SnapshotsEnabled,
		Size: proto.Size{Name: spec.Name, VCPU: spec.VCPU, MemoryMiB: spec.MemoryMiB, DiskGiB: spec.DiskGiB}}
	prep := &GuestPrep{Config: &g, Spec: &vs, Boot: o.Kind}
	for _, h := range s.Hooks.PrepareGuest {
		if err := h(ctx, sb, prep); err != nil {
			return fmt.Errorf("prepare_failed: %v", err)
		}
	}
	// Credentials of a previous owner (or withheld by the environment) go
	// before the sandbox is reachable.
	s.scrubForBoot(id, &g)
	// An encrypted disk always boots through the channel (its key comes
	// that way), also with --no-guest-channel or after a rollback of it.
	encrypted := sb.Disk == "luks" || (parent != "" && s.encryptedChain(parent))
	useChannel := s.channelNode(n) || (encrypted && n != nil && n.Info.GuestChannel)
	if encrypted && !useChannel {
		return fmt.Errorf("node_unsuitable: this sandbox's disk is encrypted and its node cannot fetch the key (crowdvm-node too old)")
	}
	if useChannel && !s.Cfg.NodeEgress {
		// Internet through the server; the node's netstack lets the VM
		// reach nothing but the server (a kill switch if the guest drops
		// its egress rules).
		g.Egress = "server"
		vs.Net = &proto.NetPolicy{Deny: []string{"0.0.0.0/0"}, NoDNS: true}
	}
	vs.Guest = g
	if useChannel {
		// Secure layer: the node gets a boot stub, the guest fetches the
		// config from the server (guest_channel.go), and the key of its
		// encrypted data partition.
		if _, err := s.ensureDiskKey(sb.ID, parent); err != nil {
			return fmt.Errorf("prepare_failed: disk key: %v", err)
		}
		if vs.Guest, err = s.issueGuestChannel(sb, node.ID, g); err != nil {
			return fmt.Errorf("prepare_failed: guest channel: %v", err)
		}
	} else {
		_, _ = s.DB.Exec(`DELETE FROM guest_channels WHERE sandbox_id=?`, id)
	}
	if _, err := s.DB.Exec(`UPDATE sandboxes SET guest_token=?, boot_kind=?, head_snapshot_id=COALESCE(head_snapshot_id, NULLIF(?,'')) WHERE id=?`, token, string(o.Kind), parent, id); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	var st proto.VMStatus
	err = node.Do(cctx, "POST", "/v1/vms", vs, &st)
	if ne := (*NodeError)(nil); errors.As(err, &ne) && ne.Status == 409 && strings.Contains(ne.Body, "vm_exists") {
		// A machine of this sandbox is already there: an earlier POST the
		// node served late (bugs-r2-opus 2), or one left behind. It may not
		// run this boot's disk, so it goes and the boot runs once more.
		_ = node.Do(cctx, "DELETE", "/v1/vms/"+id, nil, nil)
		st = proto.VMStatus{}
		err = node.Do(cctx, "POST", "/v1/vms", vs, &st)
	}
	cancel()
	if err != nil {
		var ne *NodeError
		if errors.As(err, &ne) && ne.Status == 503 && strings.Contains(ne.Body, "out_of_capacity") {
			return &nodeFullError{parent: parent, node: node.ID, msg: strings.TrimSpace(ne.Body)}
		}
		if !errors.As(err, &ne) && ctx.Err() == nil {
			// No answer at all: the node died under the boot (the server
			// notices a moment later). Place it again (opus-23 m).
			return &nodeFullError{parent: parent, node: node.ID, msg: "the node went offline", offline: true}
		}
		return fmt.Errorf("start_failed: %v", err)
	}
	_, _ = s.DB.Exec(`UPDATE sandboxes SET started_at=? WHERE id=?`, store.Now(), id)
	_ = s.SetSandboxExtra(id, "bootParent", parent) // the disk this machine runs on (afterSnapshotStored)
	if st.State != "" {
		if st.SandboxID == "" {
			st.SandboxID = id
		}
		s.reconcileVM(node.ID, st, true)
	}
	return nil
}

// ensureChain makes the node hold snapshot parent and its ancestors,
// waiting for uploads when another node made them.
func (s *Server) ensureChain(ctx context.Context, sbID string, node *NodeConn, parent string, lazy bool) error {
	chain := s.snapshotChain(parent) // newest first
	var missing []string
	for _, id := range chain {
		if s.hasLocation(id, node.ID) {
			break
		}
		missing = append(missing, id)
	}
	slices.Reverse(missing)
	defer layerWaits.Delete(sbID)
	for _, id := range missing {
		deadline := time.Now().Add(30 * time.Minute)
		var asked, offlineSince time.Time
		for {
			sn, err := s.SnapshotByID(id)
			if err != nil {
				return fmt.Errorf("snapshot_not_found: %s", id)
			}
			if sn.UploadedAt != nil {
				break
			}
			layerWaits.Store(sbID, id)
			if time.Now().After(deadline) {
				return fmt.Errorf("snapshot_unavailable: %s was never uploaded", id)
			}
			// Only the node that took it holds it: ask that node to upload
			// it (a no-op while it does), and give up soon when it is
			// offline.
			switch {
			case !s.holderOnline(id):
				if offlineSince.IsZero() {
					offlineSince = time.Now()
				}
				if time.Since(offlineSince) > HolderOfflineGrace {
					return fmt.Errorf("snapshot_unavailable: snapshot %s is not on the server yet and the node that holds it is offline; retry once that node is back online", id)
				}
			case time.Since(asked) >= 10*time.Second:
				offlineSince, asked = time.Time{}, time.Now()
				s.requestUpload(ctx, id)
			default:
				offlineSince = time.Time{}
			}
			if _, err := s.stillStarting(sbID); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		if lazy {
			// The node fetches it in the background. It is a location from
			// now on, so a deletion erases it there too (a location the node
			// never got is a harmless no-op delete) (opus-16 B1).
			s.addLocation(id, node.ID)
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
		err := node.Do(fctx, "POST", "/v1/snapshots/"+id+"/fetch", map[string]any{"snapshotId": id}, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("snapshot_fetch_failed: %v", err)
		}
		s.addLocation(id, node.ID)
	}
	return nil
}

func (s *Server) takeLiveSnapshot(ctx context.Context, sb *Sandbox) (string, error) {
	node := s.nodes.get(sb.NodeID)
	if node == nil || !sb.Usable() {
		return "", errors.New("the source sandbox is not running")
	}
	id := api.NewID(api.PrefixSnapshot, 20)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var ref proto.SnapshotRef
	_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_at=?, last_snapshot_status='in_progress' WHERE id=?`, store.Now(), sb.ID)
	if err := node.Do(cctx, "POST", "/v1/vms/"+sb.ID+"/snapshot", proto.SnapshotRequest{SnapshotID: id}, &ref); err != nil {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_status='failed' WHERE id=?`, sb.ID)
		return "", err
	}
	if ref.ID == "" {
		ref.ID = id
	}
	s.recordSnapshot(sb, ref, node.ID, "live")
	return ref.ID, nil
}

// ---------------------------------------------------------------------------
// Node state reconciliation
// ---------------------------------------------------------------------------

// vmStarts: per node and sandbox, the newest VM start time it reported (node
// clock), to drop a previous machine's late stopped/error report.
var vmStarts sync.Map

func (s *Server) reconcileVM(nodeID string, st proto.VMStatus, fromEvent bool) {
	if st.SandboxID == "" {
		return
	}
	if !st.StartedAt.IsZero() {
		// Per node: times on one node's clock are compared with each other
		// only, and another node's report cannot hide this node's crash
		// (bugs-r8-sonnet 3).
		key := nodeID + "/" + st.SandboxID
		switch st.State {
		case proto.VMRunning, proto.VMStarting:
			if prev, ok := vmStarts.Load(key); !ok || st.StartedAt.After(prev.(time.Time)) {
				vmStarts.Store(key, st.StartedAt)
			}
		case proto.VMStopped, proto.VMError:
			if prev, ok := vmStarts.Load(key); ok && st.StartedAt.Before(prev.(time.Time)) {
				return // about an older machine of this sandbox
			}
		}
	}
	sb, err := s.SandboxByID(st.SandboxID)
	if err == nil && sb.DeletedAt == nil && sb.NodeID == "" {
		if s.adoptQueuedMachine(sb, nodeID, st) {
			sb.NodeID = nodeID
		} else if cur, e := s.SandboxByID(st.SandboxID); e == nil {
			sb = cur // another report of this machine adopted it meanwhile (bugs-r3-opus 1)
		}
	}
	// A sandbox without a node has no machine: a report of one is an orphan
	// (bugs-r2-opus 1), unless adopted above.
	if err != nil || sb.DeletedAt != nil || sb.NodeID != nodeID {
		// Orphan VM (deleted sandbox, or the sandbox moved to another node).
		// One left in error goes too: it only holds disk (sonnet-21).
		if (err == nil || store.IsNoRows(err)) && (st.State == proto.VMRunning || st.State == proto.VMStarting || st.State == proto.VMError) {
			if c := s.nodes.get(nodeID); c != nil && (err != nil || sb.State != api.StateArchiving) {
				s.Go(func(ctx context.Context) {
					cctx, cancel := context.WithTimeout(ctx, time.Minute)
					defer cancel()
					_ = c.Do(cctx, "DELETE", "/v1/vms/"+st.SandboxID, nil, nil)
				})
			}
		}
		return
	}
	// A forced stop gave up on a node that was offline; the machine it kept
	// running there goes now, so a resume there does not hit vm_exists
	// (sonnet-22 m1).
	if sb.State == api.StateArchived && (st.State == proto.VMRunning || st.State == proto.VMStarting || st.State == proto.VMError) && s.forcedStopLeftover(sb) {
		if c := s.nodes.get(nodeID); c != nil {
			s.Go(func(ctx context.Context) {
				cctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				_ = c.Do(cctx, "DELETE", "/v1/vms/"+st.SandboxID, nil, nil)
			})
		}
		return
	}
	if len(st.Direct) > 0 && mustJSON(st.Direct) != mustJSON(sb.Direct) {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET direct=? WHERE id=?`, mustJSON(st.Direct), sb.ID)
	}
	if st.State == proto.VMRunning && !st.Hydrating && sb.Usable() && !sb.Hydrated {
		s.markHydrated(sb, nodeID) // LIFE-013: a lazy chain is now local
	}
	now := store.Now()
	switch st.State {
	case proto.VMRunning:
		if st.GuestReady {
			ok := map[string]any{"error": nil, "health": "ok", "health_reason": nil, "degraded_since": nil}
			switch {
			case sb.starting() && isSecuring(sb):
				// Machine up, previous owner's credentials not yet removed:
				// metered, but not usable until the scrub finished.
				if sb.State != api.StateProvisioned {
					s.transition(sb.ID, startingStates, api.StateProvisioned, nil)
				}
				s.ensureRun(sb.ID)
				s.secureAsync(sb.ID)
			case sb.starting():
				s.markReady(sb)
			case sb.State == api.StateError && sb.Error == "node_offline" && isSecuring(sb):
				// The node dropped during the scrub of a previous owner's
				// credentials: finish it before the sandbox is usable
				// (bugs-r7-sonnet 7).
				if _, done := s.transition(sb.ID, []string{api.StateError}, api.StateProvisioned, map[string]any{"error": nil, "health": "ok", "health_reason": nil, "degraded_since": nil}); done {
					sb.NodeID = nodeID
					s.ensureRun(sb.ID)
					s.secureAsync(sb.ID)
				}
			case sb.State == api.StateError && sb.Error == "node_offline":
				// Boat: sandbox.ready "includes recovery from an error";
				// sandbox.recovered: fully reachable again.
				if _, done := s.transition(sb.ID, []string{api.StateError}, readyState(sb), ok, Event{Type: EvSandboxReady, Reason: "node_returned"}, Event{Type: EvSandboxRecovered}); done {
					sb.NodeID = nodeID
					_ = s.SetSandboxExtra(sb.ID, "billingPaused", nil)
					s.openRun(sb)
				}
			case sb.Usable() && sb.Health == "degraded":
				if _, done := s.transition(sb.ID, usableStates, sb.State, ok, Event{Type: EvSandboxRecovered}); done {
					s.resumeBilling(sb.ID, pauseNotAnswering)
				}
			}
		} else {
			switch {
			case sb.State == api.StateProvisioning || sb.State == api.StateCloning:
				if _, done := s.transition(sb.ID, []string{api.StateProvisioning, api.StateCloning}, api.StateProvisioned, nil); done {
					s.ensureRun(sb.ID)
				}
			case sb.Usable() && fromEvent && sb.Health != "degraded":
				if cur, done := s.transition(sb.ID, usableStates, sb.State, map[string]any{"health": "degraded", "health_reason": "guest_unreachable", "degraded_since": now},
					Event{Type: EvSandboxDegraded, Reason: "guest_unreachable"}); done {
					s.notAnswering(cur) // LIFE-040: billing pauses, owner notified
				}
			}
		}
	case proto.VMError:
		// A refused stop whose save failed after the pre-flight: the node
		// reports the failed stop, but the machine still answers.
		if billingPause(sb) == pauseStopRefused && strings.HasPrefix(st.Error, "stop:") {
			return
		}
		if sb.starting() || sb.Usable() {
			s.confirmVMDown(nodeID, sb.ID, firstNonEmpty(st.Error, "machine_error"))
		}
		// Its node came back but the machine failed after all, rather than
		// staying node_offline for good (bugs-r8-opus 2). A boot that never
		// came up (a lazy boot whose chain reads broke while the node was
		// down) holds nothing newer: boot it again. Any other failure keeps
		// its disk for recover: just say what happened.
		if sb.State == api.StateError && sb.Error == "node_offline" && sb.NodeID == nodeID {
			if fromEvent && strings.HasPrefix(st.Error, "boot timeout") {
				s.restartLostMachine(sb.ID, nodeID)
			} else {
				_, _ = s.DB.Exec(`UPDATE sandboxes SET error=?, health_reason='machine_error' WHERE id=? AND state='error' AND error='node_offline'`,
					"machine_error: "+firstNonEmpty(st.Error, "the machine stopped while its node was offline"), sb.ID)
			}
		}
	case proto.VMStopped:
		// Not while starting: a booting machine does not stop cleanly (a crash
		// is VMError), but the previous machine's late "stopped" event can
		// arrive after a quick stop + resume and must not fail the new boot.
		if sb.Usable() && fromEvent {
			s.confirmVMDown(nodeID, sb.ID, firstNonEmpty(st.Error, "machine_stopped"))
		}
	}
}

func (s *Server) onNodeOffline(nodeID string) {
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`, nodeID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := store.Now()
	for _, id := range ids {
		from := append(append([]string{}, startingStates...), usableStates...)
		// Still on this node: a boot the node died under may have been
		// placed again meanwhile (requeueBoot).
		if _, ok := s.transitionIf(id, func(st, n string) bool { return n == nodeID && slices.Contains(from, st) }, api.StateError,
			map[string]any{"error": "node_offline", "health": "degraded", "health_reason": "node_offline", "degraded_since": now},
			Event{Type: EvSandboxDegraded, Reason: "node_offline"}, Event{Type: EvSandboxError, Reason: "node_offline"}); ok {
			s.closeRun(id)
		}
	}
}

// ---------------------------------------------------------------------------
// Guest events: setup status and agent activity
// ---------------------------------------------------------------------------

func (s *Server) onGuestEvent(nodeID, sbID string, ge proto.GuestEvent) {
	if sbID == "" {
		return
	}
	// Only the node the sandbox runs on speaks for its guest: another node
	// must not inject notices or agent history into it (bugs-r7-sonnet 4).
	if cur, err := s.SandboxByID(sbID); err != nil || cur.NodeID != nodeID {
		return
	}
	var data map[string]any
	_ = json.Unmarshal(ge.Data, &data)
	s.Bus.Publish(Event{Type: EvGuestEvent, SandboxID: sbID, NodeID: nodeID, Reason: ge.Type, Data: map[string]any{"type": ge.Type, "time": ge.Time, "data": data}})
	switch ge.Type {
	case "file.skipped":
		path, _ := data["path"].(string)
		why, _ := data["error"].(string)
		if sb, err := s.SandboxByID(sbID); err == nil {
			s.notifySandbox(sb, "file_skipped", "warning", 24*time.Hour, "A secret file was not written",
				"The file "+path+" from the environment was not written into "+sb.Name+": "+why+". Pick another path for it.")
		}
	case "setup.status":
		st, _ := data["status"].(string)
		er, _ := data["error"].(string)
		if st != "" {
			_, _ = s.DB.Exec(`UPDATE sandboxes SET setup_status=?, setup_error=? WHERE id=?`, st, nz(er), sbID)
		}
	case "prompt.event":
		s.storeAgentEvent(sbID, ge.Data)
		typ, _ := data["type"].(string)
		inner, _ := data["data"].(map[string]any)
		status, _ := inner["status"].(string)
		pid, _ := data["taskId"].(string)
		if pid == "" {
			pid, _ = data["id"].(string)
		}
		if (typ == "prompt" || typ == "prompt.status") && status != "" && pid != "" {
			s.markPrompt(sbID, pid, status)
		}
	case "disk.usage":
		used, _ := data["usedBytes"].(float64)
		total, _ := data["totalBytes"].(float64)
		s.onDiskUsage(sbID, int64(used), int64(total))
	case "boot.previous":
		// How the previous boot of this machine ended (LIFE-026). A boot the
		// server started (create/resume/fork) is not a reboot unless the
		// previous one crashed; one that happens while usable is.
		reason, _ := data["reason"].(string)
		if sb, err := s.SandboxByID(sbID); err == nil && reason != "" && (reason == "crash" || sb.Usable() || sb.HealthReason == "recovering") {
			at := store.T(ge.Time)
			if at == 0 {
				at = store.Now()
			}
			_, _ = s.DB.Exec(`UPDATE sandboxes SET last_reboot_at=?, last_reboot_reason=? WHERE id=?`, at, reason, sbID)
		}
	case "agent.activity":
		if busy, ok := data["busy"].(bool); ok {
			s.setAgentState(sbID, busy)
		}
	}
}

// markPrompt tracks a prompt's status and derives idle/running.
func (s *Server) markPrompt(sbID, promptID, status string) {
	s.activityMu.Lock()
	set := s.activity[sbID]
	if set == nil {
		set = map[string]bool{}
		s.activity[sbID] = set
	}
	switch status {
	case "sending", "queued", "running", "started":
		set[promptID] = true
	default:
		delete(set, promptID)
	}
	busy := len(set) > 0
	s.activityMu.Unlock()
	s.setAgentState(sbID, busy)
}

func (s *Server) setAgentState(sbID string, busy bool) {
	st, state := "idle", api.StateIdle
	if busy {
		st, state = "running", api.StateRunning
	}
	_, _ = s.DB.Exec(`UPDATE sandboxes SET agent_state=? WHERE id=?`, st, sbID)
	s.transition(sbID, usableStates, state, nil)
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

type stopOp struct {
	ID, SandboxID, Status, Error, SnapshotID string
	RequestedAt                              time.Time
	LastAttemptAt, EndedAt                   *time.Time
	Forced                                   bool
	Attempts                                 int
}

func (s *Server) stopByID(id string) (*stopOp, error) {
	o := &stopOp{}
	var req int64
	var last, ended sql.NullInt64
	err := s.DB.QueryRow(`SELECT id,sandbox_id,status,COALESCE(error,''),COALESCE(snapshot_id,''),requested_at,last_attempt_at,ended_at,forced,attempts FROM stops WHERE id=?`, id).
		Scan(&o.ID, &o.SandboxID, &o.Status, &o.Error, &o.SnapshotID, &req, &last, &ended, &o.Forced, &o.Attempts)
	if err != nil {
		return nil, err
	}
	o.RequestedAt, o.LastAttemptAt, o.EndedAt = store.Time(req), store.NullTime(last), store.NullTime(ended)
	return o, nil
}

func (s *Server) stopJSON(o *stopOp, onSandbox bool) map[string]any {
	var req, last, ended any = ts(o.RequestedAt), nil, nil
	if o.LastAttemptAt != nil {
		last = ts(*o.LastAttemptAt)
	}
	if o.EndedAt != nil {
		ended = ts(*o.EndedAt)
		if onSandbox {
			req = nil
		}
	}
	var er any
	if o.Status == "failing" {
		er = nz(o.Error)
	}
	return map[string]any{"id": o.ID, "status": o.Status, "requestedAt": req, "lastAttemptAt": last, "error": er, "endedAt": ended, "forced": o.Forced}
}

// requestStop starts (or returns) the stop of sb.
func (s *Server) requestStop(sb *Sandbox, force bool, reason string) (*stopOp, error) {
	unlock := s.sbLocks.Lock(sb.ID)
	defer unlock()
	sb, err := s.SandboxByID(sb.ID)
	if err != nil || sb.DeletedAt != nil {
		return nil, errNotFound("Sandbox")
	}
	// Node evacuation (nodes_evacuate.go): only a stop the evacuation itself
	// starts resumes the sandbox elsewhere. Any other stop (the owner's, a
	// TTL, billing, a suspension) keeps it stopped, also when it joins a stop
	// already under way. Under sbLocks, so the two can't race.
	if reason != "evacuated" && sb.Extra["evacuateFrom"] != nil {
		_ = s.SetSandboxExtra(sb.ID, "evacuateFrom", nil)
	}
	if sb.State == api.StateArchiving && sb.StopID != "" {
		o, err := s.stopByID(sb.StopID)
		if err == nil && force && !o.Forced {
			_, _ = s.DB.Exec(`UPDATE stops SET forced=1 WHERE id=?`, o.ID)
			o.Forced = true
			s.Go(func(ctx context.Context) { s.runStop(ctx, sb.ID, o.ID) })
		}
		return o, err
	}
	// A refused stop still waits (the sandbox runs meanwhile): the same stop,
	// forced now when asked.
	if sb.Usable() && sb.StopID != "" {
		if o, err := s.stopByID(sb.StopID); err == nil && o.Status == "failing" {
			if force && !o.Forced {
				_, _ = s.DB.Exec(`UPDATE stops SET forced=1 WHERE id=?`, o.ID)
				o.Forced = true
				s.Go(func(ctx context.Context) { s.runStop(ctx, sb.ID, o.ID) })
			}
			return o, nil
		}
	}
	if reason == "evacuated" && sb.SnapshotsEnabled && sb.NodeID != "" {
		_ = s.SetSandboxExtra(sb.ID, "evacuateFrom", sb.NodeID) // a new stop of the evacuation's own
	}
	now := store.Now()
	id := "stop_" + randHex(8)
	if sb.State == api.StateCancelled {
		return nil, ErrD(409, "sandbox_not_ready", "The sandbox was cancelled before it ever ran; there is nothing to stop. Resume it to try again, or delete it.", map[string]any{"retryable": false, "state": sb.State})
	}
	if sb.State == api.StateArchived {
		if sb.StopID != "" {
			if o, err := s.stopByID(sb.StopID); err == nil {
				return o, nil
			}
		}
		_, err := s.DB.Exec(`INSERT INTO stops(id,sandbox_id,status,requested_at,ended_at,last_attempt_at) VALUES(?,?,?,?,?,?)`, id, sb.ID, "completed", now, now, now)
		if err != nil {
			return nil, err
		}
		_, _ = s.DB.Exec(`UPDATE sandboxes SET stop_id=? WHERE id=?`, id, sb.ID)
		return s.stopByID(id)
	}
	// An errored machine cannot shut down cleanly: snapshot what is there, or
	// keep the last snapshot. Not when only its node is offline: the machine
	// may still run there with newer data, so the stop waits for the node
	// up to NodeLostGrace (sonnet-21); --force, or removing the node, ends
	// it at once.
	if sb.State == api.StateError && sb.Error != "node_offline" {
		force = true
	}
	if reason != "" {
		s.Log.Info("stopping sandbox", "sandbox", sb.ID, "reason", reason)
	}
	if _, err := s.DB.Exec(`INSERT INTO stops(id,sandbox_id,status,requested_at,forced) VALUES(?,?,?,?,?)`, id, sb.ID, "pending", now, boolInt(force)); err != nil {
		return nil, err
	}
	_ = s.SetSandboxExtra(sb.ID, "stopReason", reason)
	// Guest answering at the request: the first attempt runs a pre-flight save.
	_ = s.SetSandboxExtra(sb.ID, "stopPreflight", sb.Usable() && sb.Health != "degraded" && sb.SnapshotsEnabled)
	if _, ok := s.transition(sb.ID, nil, api.StateArchiving, map[string]any{"stop_id": id, "archive_after": nil}); !ok {
		return nil, Err(409, "sandbox_stopping", "The sandbox changed state; retry.")
	}
	s.closeRun(sb.ID) // archiving is not metered (LIFE-005)
	s.Go(func(ctx context.Context) { s.runStop(ctx, sb.ID, id) })
	return s.stopByID(id)
}

var stopRunning sync.Map // stop id -> struct{} while an attempt runs

// nodeLost: node id was removed, or has been offline longer than
// NodeLostGrace (when the server also rebuilds its sandboxes elsewhere).
func (s *Server) nodeLost(id string) bool {
	seen, ok := s.seenAt(id)
	if !ok {
		return true
	}
	return seen > 0 && store.Now()-seen > s.Cfg.NodeLostGrace.Milliseconds()
}

// seenAt is when node id was last connected (0: never), but not before this
// server started: while the server was down the node could not connect.
// ok is false for a removed node.
func (s *Server) seenAt(id string) (int64, bool) {
	var seen sql.NullInt64
	if s.DB.QueryRow(`SELECT last_seen_at FROM nodes WHERE id=?`, id).Scan(&seen) != nil {
		return 0, false
	}
	if !seen.Valid {
		return 0, true
	}
	return max(seen.Int64, s.startedAt), true
}

// markUnconnected (field test): a node that went away while the server was
// restarting never disconnects from this server, so onNodeOffline never ran
// and its sandboxes stayed "ready" for good. Once nodes have had time to
// reconnect (their backoff tops out at 30 s), a node that is still missing is
// offline.
func (s *Server) markUnconnected() {
	if s.startedAt == 0 || store.Now()-s.startedAt < nodeReconnectWindow.Milliseconds() {
		return
	}
	rows, err := s.DB.Query(`SELECT DISTINCT node_id FROM sandboxes WHERE deleted_at IS NULL AND node_id IS NOT NULL AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if s.nodes.get(id) == nil {
			s.onNodeOffline(id)
		}
	}
}

// nodeReconnectWindow: how long after a server start a node has to reconnect.
var nodeReconnectWindow = time.Minute

// adoptQueuedMachine (bugs-r2-opus 1): sb waits to be rebuilt elsewhere
// after its node was lost, and that node came back with its machine still
// running. That machine has the newest disk: the sandbox takes it back
// instead of booting an older snapshot.
func (s *Server) adoptQueuedMachine(sb *Sandbox, nodeID string, st proto.VMStatus) bool {
	pb := pendingBootOf(sb)
	if pb == nil || pb.Cleanup != nodeID || sb.State != api.StateProvisioning || (st.State != proto.VMRunning && st.State != proto.VMStarting) {
		return false
	}
	res, err := s.DB.Exec(`UPDATE sandboxes SET node_id=? WHERE id=? AND node_id IS NULL AND state=? AND deleted_at IS NULL`, nodeID, sb.ID, api.StateProvisioning)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	_ = s.SetSandboxExtra(sb.ID, "pendingBoot", nil)
	s.Log.Info("node came back with the machine of a queued rebuild; adopting it", "sandbox", sb.ID, "node", nodeID)
	return true
}

// forcedStopLeftover: sb's last stop was forced and ended a while ago, so a
// machine of it that still runs was left behind (not a late report about
// the machine a normal stop just stopped).
// ForcedStopLeftoverAge: how long after a forced stop a still-running
// machine counts as left behind (tests shorten it).
var ForcedStopLeftoverAge = 10 * time.Second

func (s *Server) forcedStopLeftover(sb *Sandbox) bool {
	var forced bool
	var ended sql.NullInt64
	if sb.StopID == "" || s.DB.QueryRow(`SELECT forced, ended_at FROM stops WHERE id=? AND status='completed'`, sb.StopID).Scan(&forced, &ended) != nil {
		return false
	}
	return forced && ended.Valid && store.Now()-ended.Int64 >= ForcedStopLeftoverAge.Milliseconds()
}

// noHeadErr (resume, fork): a sandbox that ran has no head. While its
// layers are still being uploaded they are not lost: retryable (sonnet-19,
// sonnet-20); otherwise its snapshot data expired.
func (s *Server) noHeadErr(sb *Sandbox) error {
	var pending, held int
	_ = s.DB.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT CASE WHEN EXISTS (SELECT 1 FROM snapshot_locations l JOIN nodes n ON n.id=l.node_id WHERE l.snapshot_id=s.id) THEN s.id END)
		FROM snapshots s WHERE s.sandbox_id=? AND s.uploaded_at IS NULL AND s.purged_at IS NULL AND s.deleted_at IS NULL`, sb.ID).Scan(&pending, &held)
	switch {
	case held > 0:
		return ErrD(409, "snapshot_unavailable", "This sandbox's snapshot is still being uploaded from its node. Retry in a minute.",
			map[string]any{"retryable": true, "retryAfterSeconds": 30})
	case pending > 0: // only on nodes that were removed (bugs-r2-sol 2)
		return ErrD(409, "snapshot_lost", "This sandbox's disk was only on a node that was removed, so it cannot be restored.",
			map[string]any{"retryable": false})
	}
	return snapshotDataExpiredErr(sb)
}

// runStop performs one attempt of a stop; failures are retried by the reaper.
func (s *Server) runStop(ctx context.Context, sbID, stopID string) {
	if _, busy := stopRunning.LoadOrStore(stopID, struct{}{}); busy {
		return
	}
	defer stopRunning.Delete(stopID)
	sb, err := s.SandboxByID(sbID)
	if err != nil || sb.DeletedAt != nil || sb.StopID != stopID {
		return
	}
	refused := sb.Usable() // a refused stop retried while the sandbox runs
	if sb.State != api.StateArchiving && !refused {
		return
	}
	op, err := s.stopByID(stopID)
	if err != nil || op.Status == "completed" || op.Status == "superseded" || (refused && op.Status != "failing") {
		return
	}
	now := store.Now()
	_, _ = s.DB.Exec(`UPDATE stops SET attempts=attempts+1, last_attempt_at=? WHERE id=?`, now, stopID)
	node := s.nodes.get(sb.NodeID)
	fail := func(msg string) {
		_, _ = s.DB.Exec(`UPDATE stops SET status='failing', error=?, last_attempt_at=? WHERE id=?`, msg, store.Now(), stopID)
		s.Log.Warn("stop attempt failed", "sandbox", sbID, "err", msg)
	}
	toArchiving := func() bool { // a refused stop leaves the usable state only now
		if !refused {
			return true
		}
		_, ok := s.transition(sb.ID, usableStates, api.StateArchiving, nil)
		return ok
	}
	if node == nil && !op.Forced && sb.NodeID != "" && s.nodeLost(sb.NodeID) {
		op.Forced = true // the node is not coming back (soon): keep the last snapshot
		_, _ = s.DB.Exec(`UPDATE stops SET forced=1 WHERE id=?`, op.ID)
	}
	if sb.NodeID == "" || (node == nil && op.Forced) {
		if toArchiving() {
			if node == nil && sb.NodeID != "" {
				s.fallBackToStoredHead(sb)
			}
			s.finishStop(sb, op, "", op.Forced)
		}
		return
	}
	if node == nil {
		fail("node_offline: the node hosting this sandbox is offline")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	// Pre-flight save (LIFE-034): refuse before prepare-stop changes the guest.
	preflight, _ := sb.Extra["stopPreflight"].(bool)
	if sb.SnapshotsEnabled && !op.Forced && (refused || preflight) {
		if err := s.preflightSave(cctx, node, sb); err != nil {
			s.refuseStop(sb, op, "save_failed: "+err.Error())
			return
		}
	}
	if !toArchiving() {
		return
	}
	if !sb.SnapshotsEnabled {
		if !op.Forced {
			s.historyFlush(cctx, sb)
		}
		err := node.Do(cctx, "DELETE", "/v1/vms/"+sb.ID, nil, nil)
		var ne *NodeError
		if err != nil && !(errors.As(err, &ne) && ne.Status == 404) {
			fail(err.Error())
			return
		}
		s.finishStop(sb, op, "", false)
		return
	}
	// One layer id per stop, kept across attempts and server restarts: after
	// a 404 the node stopped the VM for this stop exactly when it holds that
	// layer (opus-22 B1/B2).
	snapID := op.SnapshotID
	if snapID == "" {
		snapID = api.NewID(api.PrefixSnapshot, 20)
		_, _ = s.DB.Exec(`UPDATE stops SET snapshot_id=? WHERE id=?`, snapID, op.ID)
	}
	var ref proto.SnapshotRef
	if !op.Forced {
		s.historyFlush(cctx, sb)
	}
	_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_at=?, last_snapshot_status='in_progress' WHERE id=?`, store.Now(), sbID)
	err = node.Do(cctx, "POST", "/v1/vms/"+sb.ID+"/stop", proto.StopRequest{SnapshotID: snapID, Force: op.Forced}, &ref)
	if err != nil {
		var ne *NodeError
		if errors.As(err, &ne) && ne.Status == 404 {
			// The machine is gone. If it stopped into this stop's layer and the
			// answer never got here (a server restart mid-stop, the node
			// restarted too), adopt that layer rather than fall back to an
			// older one (opus-21 N1, sonnet-19).
			if l, ok := s.nodeLayer(cctx, node, sb.ID, snapID); ok {
				_, _ = s.DB.Exec(`UPDATE snapshots SET deleted_at=NULL WHERE id=? AND purged_at IS NULL`, snapID)
				ref := proto.SnapshotRef{ID: snapID, ParentID: l.ParentID, ImageID: l.ImageID, SizeBytes: l.SizeBytes, Supersedes: l.Supersedes}
				if l.CreatedAt != nil {
					ref.CreatedAt = *l.CreatedAt
				}
				s.recordSnapshot(sb, ref, node.ID, "stop")
				s.finishStop(sb, op, snapID, false)
				return
			}
			s.finishStop(sb, op, "", true) // machine already gone: keep the last snapshot
			return
		}
		_, _ = s.DB.Exec(`UPDATE sandboxes SET last_snapshot_status='failed' WHERE id=?`, sbID)
		if op.Forced {
			if s.nodes.get(sb.NodeID) == nil {
				s.fallBackToStoredHead(sb) // the node went away meanwhile
			}
			s.finishStop(sb, op, "", true) // forced: keep the last snapshot
			return
		}
		if s.machineAnswers(cctx, node, sb.ID) {
			if cur, cerr := s.SandboxByID(sbID); cerr == nil {
				s.refuseStop(cur, op, err.Error()) // the machine still runs: keep it usable
				return
			}
		}
		fail(err.Error())
		return
	}
	if ref.ID == "" {
		ref.ID = snapID
	}
	s.recordSnapshot(sb, ref, node.ID, "stop")
	s.finishStop(sb, op, ref.ID, false)
}

// nodeLayer: layer id of sandbox sbID, when node holds it.
func (s *Server) nodeLayer(ctx context.Context, node *NodeConn, sbID, id string) (proto.NodeSnapshot, bool) {
	var list struct {
		Snapshots []proto.NodeSnapshot `json:"snapshots"`
	}
	if node.Do(ctx, "GET", "/v1/snapshots", nil, &list) != nil {
		return proto.NodeSnapshot{}, false
	}
	for _, n := range list.Snapshots {
		if n.ID == id && n.SandboxID == sbID {
			var purged bool
			if s.DB.QueryRow(`SELECT purged_at IS NOT NULL FROM snapshots WHERE id=?`, id).Scan(&purged) == nil && purged {
				return proto.NodeSnapshot{}, false
			}
			return n, true
		}
	}
	return proto.NodeSnapshot{}, false
}

func (s *Server) finishStop(sb *Sandbox, op *stopOp, snapID string, forced bool) {
	now := store.Now()
	_, _ = s.DB.Exec(`UPDATE stops SET status='completed', ended_at=?, error=NULL, snapshot_id=?, forced=? WHERE id=?`, now, nz(snapID), boolInt(forced || op.Forced), op.ID)
	s.closeRun(sb.ID)
	s.activityMu.Lock()
	delete(s.activity, sb.ID)
	s.activityMu.Unlock()
	_ = s.SetSandboxExtra(sb.ID, "billingPaused", nil)
	s.transition(sb.ID, []string{api.StateArchiving}, api.StateArchived,
		map[string]any{"stopped_at": now, "agent_state": "", "health": "ok", "health_reason": nil, "degraded_since": nil, "error": nil},
		Event{Type: EvSandboxArchived})
	s.afterStopResume(sb.ID) // LIFE-014: a resume arrived while it stopped
}

// ---------------------------------------------------------------------------
// Reaper: TTL auto-stop, stop retries, boot timeouts
// ---------------------------------------------------------------------------

func (s *Server) reaperLoop(ctx context.Context) {
	s.resumePendingStops()
	every(ctx, s.Cfg.ReaperInterval, func() { s.Reap() })
}

func (s *Server) resumePendingStops() {
	rows, err := s.DB.Query(`SELECT id,stop_id FROM sandboxes WHERE state='archiving' AND deleted_at IS NULL AND stop_id IS NOT NULL`)
	if err != nil {
		return
	}
	type p struct{ sb, stop string }
	var list []p
	for rows.Next() {
		var x p
		if rows.Scan(&x.sb, &x.stop) == nil {
			list = append(list, x)
		}
	}
	rows.Close()
	for _, x := range list {
		x := x
		s.Go(func(ctx context.Context) { s.runStop(ctx, x.sb, x.stop) })
	}
}

// Reap runs one reaper pass (exported for tests).
func (s *Server) Reap() {
	now := store.Now()
	// TTL.
	rows, err := s.DB.Query(`SELECT id,COALESCE(ttl_seconds,0),agent_state FROM sandboxes WHERE deleted_at IS NULL AND archive_after IS NOT NULL AND archive_after<=? AND state IN ('provisioned','ready','idle','running')`, now)
	if err == nil {
		type t struct {
			id    string
			ttl   int64
			agent string
		}
		var due []t
		for rows.Next() {
			var x t
			if rows.Scan(&x.id, &x.ttl, &x.agent) == nil {
				due = append(due, x)
			}
		}
		rows.Close()
		for _, x := range due {
			if x.agent == "running" && x.ttl > 0 {
				_, _ = s.DB.Exec(`UPDATE sandboxes SET archive_after=? WHERE id=?`, now+x.ttl*1000, x.id)
				continue
			}
			if sb, err := s.SandboxByID(x.id); err == nil {
				s.Log.Info("auto-stop", "sandbox", x.id)
				_, _ = s.requestStop(sb, false, "auto_stop")
			}
		}
	}
	// Retry failing stops with backoff.
	rows, err = s.DB.Query(`SELECT s.id,st.id,st.attempts,COALESCE(st.last_attempt_at,0) FROM sandboxes s JOIN stops st ON st.id=s.stop_id WHERE s.deleted_at IS NULL
AND ((s.state='archiving' AND st.status IN ('failing','pending')) OR (s.state IN ('ready','idle','running') AND st.status='failing'))`)
	if err == nil {
		type r struct {
			sb, stop string
			n        int
			last     int64
		}
		var list []r
		for rows.Next() {
			var x r
			if rows.Scan(&x.sb, &x.stop, &x.n, &x.last) == nil {
				list = append(list, x)
			}
		}
		rows.Close()
		for _, x := range list {
			backoff := int64(2000) << min(x.n, 5)
			if x.last == 0 || now-x.last < backoff {
				continue
			}
			x := x
			s.Go(func(ctx context.Context) { s.runStop(ctx, x.sb, x.stop) })
		}
	}
	// Waiting boots, lost nodes, long error holds and failing stops.
	s.reapEvacuations()
	s.placeWaiting()
	s.markUnconnected()
	s.rebuildLost()
	s.reapHolds()
	// Boot timeout (boots waiting for capacity have their own deadline).
	rows, err = s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND node_id IS NOT NULL AND state IN ('provisioning','provisioned','cloning') AND updated_at<?`, now-int64(20*time.Minute/time.Millisecond))
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			s.failSandbox(id, "boot_timeout")
		}
	}
}

// ---------------------------------------------------------------------------
// Handlers: stop, resume, fork, recover, share, delete
// ---------------------------------------------------------------------------

func (s *Server) handleStop(c *Ctx) error {
	var req struct {
		Force bool `json:"force"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := c.Sandbox("sandbox.stop", false)
	if err != nil {
		return err
	}
	if (req.Force || !sb.SnapshotsEnabled) && sb.OwnerID != c.P.User.ID {
		if sb.OrgID == "" || s.memberRole(sb.OrgID, c.P.User.ID) != "owner" {
			return Err(403, "owner_only", "Only the creator (or the organization owner) can run a stop that deletes data.")
		}
	}
	op, err := s.requestStop(sb, req.Force, "") // clears an evacuation's move: it stays stopped
	if err != nil {
		return err
	}
	sb, _ = s.SandboxByID(sb.ID)
	status := sb.State
	return c.OK(202, "sandbox.stopping", map[string]any{"id": sb.ID, "status": status, "stop": s.stopJSON(op, false), "sandbox": s.SandboxJSON(sb, c.P)})
}

func (s *Server) handleGetStop(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.read", false)
	if err != nil {
		return err
	}
	op, err := s.stopByID(c.Param("stopId"))
	if err != nil || op.SandboxID != sb.ID {
		return errNotFound("Stop")
	}
	return c.OK(200, "stop.status", map[string]any{"id": sb.ID, "stop": s.stopJSON(op, false)})
}

func (s *Server) handleResume(c *Ctx) error {
	var req struct {
		FailFast    bool               `json:"failFast"`
		Type        string             `json:"type"`
		Env         *map[string]string `json:"env"`
		Environment string             `json:"environment"`
		NoEnv       OptBool            `json:"noEnv"`
		TTLSeconds  OptInt             `json:"ttlSeconds"`
		Placement   *Placement         `json:"placement"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := c.Sandbox("sandbox.resume", false)
	if err != nil {
		return err
	}
	if err := validateTTL(&req.TTLSeconds); err != nil {
		return err
	}
	if req.Type != "" {
		if _, ok := api.SizeByName(req.Type); !ok {
			return Err(400, "invalid_type", "type must be small, default, large or xlarge.")
		}
	}
	if req.Env != nil {
		if err := validateEnv(*req.Env); err != nil {
			return err
		}
	}
	if req.Placement != nil {
		if err := req.Placement.validate(); err != nil {
			return err
		}
	}
	unlock := s.sbLocks.Lock(sb.ID)
	defer unlock()
	sb, _ = s.SandboxByID(sb.ID)
	newSize := sb.Size
	if req.Type != "" {
		sp, _ := api.SizeByName(req.Type)
		newSize = sp.Name
	}
	// A new placement (--node, --gpu, ...) is a change too: cancelling a stop
	// would keep the machine where it is and silently drop it (field test).
	moved := req.Placement != nil && !samePlacement(*req.Placement, sb.Placement)
	changes := newSize != sb.Size || req.Environment != "" || (req.NoEnv.Set && req.NoEnv.Value && !sb.NoEnv) || req.Env != nil || moved
	switch {
	case sb.Usable() || sb.State == api.StateProvisioned:
		if changes {
			return Err(409, "sandbox_running_stop_first", "The sandbox is running. Stop it, then resume with the new size, environment or placement.")
		}
		if req.TTLSeconds.Set {
			sets := map[string]any{"ttl_seconds": nil, "archive_after": nil}
			if !req.TTLSeconds.Null {
				sets["ttl_seconds"] = req.TTLSeconds.Value
				sets["archive_after"] = store.Now() + int64(req.TTLSeconds.Value)*1000
			}
			_ = s.updateSandbox(sb.ID, sets)
			sb, _ = s.SandboxByID(sb.ID)
		}
		s.cancelRefusedStop(sb) // Boat: a resume cancels a refused stop
		sb, _ = s.SandboxByID(sb.ID)
		return c.OK(202, "sandbox.resuming", map[string]any{"id": sb.ID, "status": sb.State, "sandbox": s.SandboxJSON(sb, c.P)})
	case sb.State == api.StateArchiving && changes:
		return ErrD(409, "sandbox_stopping", "The sandbox is stopping. Wait until it is stopped, then resume it with the new size, environment or placement: crowdvm resume "+sb.ID, map[string]any{"retryable": true, "retryAfterSeconds": 5})
	case sb.State == api.StateArchiving:
		if !sb.SnapshotsEnabled {
			return ErrD(409, "sandbox_stopping", "The sandbox has snapshots off and is being erased; it cannot be resumed.", map[string]any{"retryable": false})
		}
		if err := s.resumeStartChecks(sb); err != nil {
			return err
		}
		return s.resumeDuringStop(c, sb, req.TTLSeconds)
	case sb.starting():
		return Err(409, "sandbox_resuming", "The sandbox is already starting.")
	}
	// Its node only just went away: the machine may still run there with
	// newer data than the last snapshot. Booting it elsewhere now would
	// discard that when the node returns (bugs-r9-opus 1). Wait like stop
	// and the rebuild do, unless the user forces the stop first.
	if sb.State == api.StateError && sb.Error == "node_offline" && sb.NodeID != "" && s.nodes.get(sb.NodeID) == nil && !s.nodeLost(sb.NodeID) {
		wait := s.Cfg.NodeLostGrace.Milliseconds()
		if seen, _ := s.seenAt(sb.NodeID); seen > 0 {
			wait -= store.Now() - seen
		}
		return ErrD(409, "node_offline", "The node running this sandbox went offline moments ago; its machine may hold newer data than the last snapshot. Retry once the node is back (or after "+fmt.Sprint(max(wait/1000, 1))+" s, when it is given up), or run `crowdvm stop --force "+sb.ID+"` first to resume from the last snapshot now.",
			map[string]any{"retryable": true, "retryAfterSeconds": max(wait/1000, 1)})
	}
	if !sb.SnapshotsEnabled && sb.FirstBootDone {
		return Err(409, "snapshots_disabled", "This sandbox has snapshots off; its disk was not kept.")
	}
	// A machine that failed (a crash, an unannounced reboot) may have left
	// a snapshot newer than the head that its node still uploads: boot from
	// that one, not the older head (bugs-r14-opus 5). The boot waits for it.
	// It is written with the other changes once the resume is accepted: a
	// refused resume changes nothing (bugs-r15-sonnet 2).
	newHead := ""
	if sb.State == api.StateError && sb.NodeID != "" {
		if snap, _ := s.newerSnapshotOn(sb, sb.NodeID); snap != "" {
			newHead, sb.HeadSnapshotID = snap, snap
		}
	}
	// Never boot a blank disk for a sandbox that already ran.
	if sb.FirstBootDone && sb.HeadSnapshotID == "" {
		if sb.State == api.StateError {
			return ErrD(409, "resume_failed", "The sandbox never completed a snapshot, so a resume would boot an empty disk. Recover it instead (crowdvm recover "+sb.ID+"): that keeps the disk on its machine.",
				map[string]any{"retryable": false})
		}
		return s.noHeadErr(sb)
	}
	parent := sb.HeadSnapshotID
	if parent == "" && !sb.FirstBootDone && (sb.SourceKind == "saved" || sb.SourceKind == "share") {
		// Created from a saved snapshot, stopped before it ever booted: its
		// first boot is still the saved snapshot's disk, not an empty one.
		_ = s.DB.QueryRow(`SELECT COALESCE(snapshot_id,'') FROM saved_snapshots WHERE id=?`, sb.SourceID).Scan(&parent)
		if parent == "" { // never an empty disk in place of the saved snapshot
			return ErrD(409, "snapshot_not_found", "This sandbox never booted, and the saved snapshot it was created from was removed. Delete it and create a new one.", map[string]any{"retryable": false})
		}
	}
	u := c.P.User
	var org *Org
	if sb.OrgID != "" {
		org, _ = s.OrgByID(sb.OrgID)
	}
	owner, err := s.UserByID(sb.OwnerID)
	if err != nil {
		return err
	}
	unlockLimits := s.lockLimits() // bugs-r10-sonnet 3
	defer unlockLimits()
	if err := s.checkCredits(owner, org); err != nil {
		return err
	}
	if err := s.checkConcurrent(owner, org, 1); err != nil {
		return err
	}
	if err := s.checkStarts(sb.Wallet(), 1); err != nil {
		return err
	}
	noEnv := sb.NoEnv || (req.NoEnv.Set && req.NoEnv.Value)
	renv, err := s.resolveEnv(c, EnvRequest{User: u, Org: org, Name: req.Environment, NoEnv: noEnv, Op: "resume", Sandbox: sb})
	if err != nil {
		return err
	}
	placement := sb.Placement
	if req.Placement != nil {
		placement = *req.Placement
	}
	spec, _ := api.SizeByName(newSize)
	sets := map[string]any{"no_env": boolInt(noEnv), "placement": mustJSON(placement), "error": nil, "stopped_at": nil,
		"environment": nz(renv.Name), "environment_id": nz(renv.ID), "holds_creator_logins": boolInt(renv.HoldsLogins && !noEnv && !sb.WipePending)}
	if renv.Version != nil {
		sets["environment_version"] = *renv.Version
	}
	if req.Env != nil {
		sets["env"] = mustJSON(*req.Env)
	}
	if newHead != "" {
		sets["head_snapshot_id"] = newHead
	}
	ttl := sb.TTLSeconds
	if req.TTLSeconds.Set {
		ttl = ttlPtr(req.TTLSeconds, nil)
		sets["ttl_seconds"] = ttlOut(ttl)
	}
	if ttl != nil {
		sets["archive_after"] = store.Now() + int64(*ttl)*1000
	} else {
		sets["archive_after"] = nil
	}
	if err := s.chainRestorable(s.snapshotChain(sb.HeadSnapshotID)); err != nil {
		return err
	}
	if err := s.chainReachable(s.snapshotChain(sb.HeadSnapshotID)); err != nil {
		return err
	}
	// Supersede an unfinished stop.
	if sb.StopID != "" {
		_, _ = s.DB.Exec(`UPDATE stops SET status='superseded', ended_at=? WHERE id=? AND status IN ('pending','failing')`, store.Now(), sb.StopID)
	}
	chain := s.snapshotChain(sb.HeadSnapshotID)
	prevState, prevNode := sb.State, ""
	if sb.State == api.StateError {
		prevNode = sb.NodeID
	}
	// Placed by the disk it really has: it does not shrink (bugs-r17-opus 9).
	// The type_too_small check below keeps the type's own size.
	sreq := schedReq{Size: diskSpec(sb, spec), Placement: placement, Chain: chain, ImageID: sb.ImageID, Owners: s.ownersFor(sb.OwnerID, sb.OrgID), Encrypted: sb.Disk == "luks"}
	// Shrinking below the data on the disk is refused (SIZE-010).
	if used := diskUsedBytes(sb); used > 0 && newSize != sb.Size && used > int64(spec.DiskGiB)<<30 {
		return ErrD(409, "type_too_small", fmt.Sprintf("The sandbox holds %.1f GB of data, more than the %d GB disk of %s. Free space first, or pick a larger type.",
			float64(used)/1e9, spec.DiskGiB, spec.Name), map[string]any{"usedBytes": used, "diskGB": spec.DiskGiB, "type": spec.Name})
	}
	kind := proto.BootResume
	first := false
	switch {
	case !sb.FirstBootDone && sb.SourceKind == "fork" && sb.HeadSnapshotID != "":
		kind = proto.BootFork // a cancelled fork: new identity on the source's disk
	case !sb.FirstBootDone && sb.SourceKind == "fork":
		return ErrD(409, "resume_failed", "This fork was cancelled before its source had a snapshot; fork the source again.", map[string]any{"retryable": false})
	case sb.HeadSnapshotID == "" && !sb.FirstBootDone:
		kind, first = proto.BootCreate, true
	}
	waiting := false
	if _, err := s.placeSandbox(sb.ID, api.StateProvisioning, sreq); err != nil {
		if req.FailFast {
			return ErrD(503, "no_ready_machine", "No machine is ready right now; the sandbox stays stopped.", map[string]any{"retryAfterSeconds": 5})
		}
		if !s.couldEverPlace(sreq) {
			return s.neverFitsErr(sreq, err)
		}
		waiting = true // SIZE-011: wait for capacity
	}
	_ = s.updateSandbox(sb.ID, sets)
	if waiting {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET size=? WHERE id=?`, spec.Name, sb.ID)
		s.queueBoot(sb.ID, pendingBoot{Kind: kind, FirstBoot: first, Parent: parent, Cleanup: prevNode, Reason: "capacity"})
	}
	s.Bus.Publish(Event{Type: EvSandboxState, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, PreviousState: prevState, State: api.StateProvisioning})
	s.recordStart(sb.Wallet(), sb.OwnerID)
	unlockLimits()
	sb, _ = s.SandboxByID(sb.ID)
	if !waiting {
		s.startBoot(sb, bootOpts{Kind: kind, FirstBoot: first, Parent: parent, Env: renv, Cleanup: prevNode, FailFast: req.FailFast})
	}
	return c.OK(202, "sandbox.resuming", map[string]any{"id": sb.ID, "status": "resuming", "sandbox": s.SandboxJSON(sb, c.P)})
}

func (s *Server) handleFork(c *Ctx) error {
	raw, err := readBody(c)
	if err != nil {
		return err
	}
	var req struct {
		Labels      map[string]string  `json:"labels"`
		FailFast    bool               `json:"failFast"`
		Monitoring  OptBool            `json:"monitoring"`
		Snapshots   OptBool            `json:"snapshots"`
		Env         *map[string]string `json:"env"`
		Environment string             `json:"environment"`
		NoEnv       bool               `json:"noEnv"`
		Type        string             `json:"type"`
		TTLSeconds  OptInt             `json:"ttlSeconds"`
		Name        string             `json:"name"`
		Placement   *Placement         `json:"placement"`
		NewDiskKey  bool               `json:"newDiskKey"` // see CreateSandboxRequest
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return Err(400, "invalid_json", "Request body must be valid JSON: "+err.Error())
		}
	}
	src, err := c.Sandbox("sandbox.fork", true)
	if err != nil {
		return err
	}
	if err := validateTTL(&req.TTLSeconds); err != nil {
		return err
	}
	if err := validateLabels(req.Labels); err != nil {
		return err
	}
	if req.Env != nil {
		if err := validateEnv(*req.Env); err != nil {
			return err
		}
	}
	if req.Type != "" {
		if _, ok := api.SizeByName(req.Type); !ok {
			return Err(400, "invalid_type", "type must be small, default, large or xlarge.")
		}
	}
	if req.Placement != nil {
		if err := req.Placement.validate(); err != nil {
			return err
		}
	}
	if !src.SnapshotsEnabled {
		return Err(409, "snapshots_disabled", "This sandbox has snapshots off and cannot be forked.")
	}
	if s.sandboxRekeyPending(src.ID) {
		return errRekeyPending
	}
	live := src.Usable() && src.NodeID != "" && s.nodes.get(src.NodeID) != nil
	if !live { // a running source gets a live snapshot of its own
		if err := s.copyRekeyErr(src.HeadSnapshotID); err != nil {
			return err
		}
	}
	if !live && src.HeadSnapshotID == "" {
		if src.FirstBootDone && src.State != api.StateError {
			return s.noHeadErr(src)
		}
		return Err(409, "fork_failed", "The sandbox has no snapshot to fork from yet.")
	}
	if !live {
		if err := s.chainReachable(s.snapshotChain(src.HeadSnapshotID)); err != nil {
			return err
		}
	}
	if req.Name != "" {
		if req.Name, err = validName(req.Name); err != nil {
			return err
		}
	}
	u := c.P.User
	var org *Org
	if src.OrgID != "" {
		org, _ = s.OrgByID(src.OrgID)
	}
	idemKey := strings.TrimSpace(c.R.Header.Get("Idempotency-Key"))
	if idemKey != "" {
		done, err := s.idemBegin(c, idemKey, "POST /sandboxes/"+src.ID+"/fork", raw)
		if done || err != nil {
			return err
		}
	}
	fail := func(err error) error {
		if idemKey != "" {
			s.idemRelease(u.ID, idemKey)
		}
		return err
	}
	noEnv := src.NoEnv || req.NoEnv
	renv, err := s.resolveEnv(c, EnvRequest{User: u, Org: org, Name: req.Environment, NoEnv: noEnv, Op: "fork", Sandbox: src})
	if err != nil {
		return fail(err)
	}
	unlockLimits := s.lockLimits() // bugs-r10-sonnet 3
	defer unlockLimits()
	if err := s.checkCredits(u, org); err != nil {
		return fail(err)
	}
	if err := s.checkConcurrent(u, org, 1); err != nil {
		return fail(err)
	}
	if err := s.checkStarts(walletOf(u, org), 1); err != nil {
		return fail(err)
	}
	size := src.Size
	if req.Type != "" {
		sp, _ := api.SizeByName(req.Type)
		size = sp.Name
	}
	spec, _ := api.SizeByName(size)
	if used := diskUsedBytes(src); used > 0 && size != src.Size && used > int64(spec.DiskGiB)<<30 {
		return fail(ErrD(409, "type_too_small", fmt.Sprintf("The sandbox holds %.1f GB of data, more than the %d GB disk of %s. Pick a larger type.",
			float64(used)/1e9, spec.DiskGiB, spec.Name), map[string]any{"usedBytes": used, "diskGB": spec.DiskGiB, "type": spec.Name}))
	}
	env := src.Env
	if req.Env != nil {
		env = *req.Env
	}
	placement := src.Placement
	if req.Placement != nil {
		placement = *req.Placement
	}
	ttl := 3600
	ttlp := ttlPtr(req.TTLSeconds, &ttl)
	now := time.Now().UTC()
	name := req.Name
	if name == "" {
		name = src.Name + " (fork)"
		if len(name) > 120 {
			name = name[:120]
		}
	}
	vis, priv := "", false
	if org != nil {
		vis = "shared"
		if src.Visibility == "private" || org.NewSandboxesPrivate || renv.HoldsLogins {
			vis, priv = "private", src.PrivateByCreator || org.NewSandboxesPrivate
		}
	}
	monOn := src.Monitoring
	if req.Monitoring.Set {
		monOn = req.Monitoring.Value
	}
	snapsOn := !req.Snapshots.Set || req.Snapshots.Value // fork --no-snapshots
	pol := &SandboxPolicy{Op: "fork", Owner: u, SnapshotsReq: req.Snapshots, MonitorReq: req.Monitoring, Snapshots: snapsOn, Monitoring: monOn}
	for _, fn := range s.Hooks.NewSandbox {
		if err := fn(c, pol); err != nil {
			return err
		}
	}
	monOn = pol.Monitoring
	fk := &Sandbox{
		ID: api.NewID(api.PrefixSandbox, 8), Name: name, OwnerID: u.ID, OrgID: src.OrgID, State: api.StateCloning, Size: spec.Name,
		TTLSeconds: ttlp, CreatedAt: now, UpdatedAt: now, ImageID: src.ImageID, Labels: req.Labels, Env: env, Nested: src.Nested,
		Placement: placement, Visibility: vis, PrivateByCreator: priv, HoldsCreatorLogins: renv.HoldsLogins && !noEnv, NoEnv: noEnv,
		SnapshotsEnabled: pol.Snapshots, Monitoring: monOn, Environment: renv.Name, EnvironmentID: renv.ID, EnvironmentVersion: renv.Version,
		CreatedByKey: keyID(c.P), SourceKind: "fork", SourceID: src.ID, BootKind: string(proto.BootFork),
	}
	if ttlp != nil {
		t := now.Add(time.Duration(*ttlp) * time.Second)
		fk.ArchiveAfter = &t
	}
	if err := s.insertSandbox(fk); err != nil {
		return fail(err)
	}
	if req.NewDiskKey {
		_ = s.SetSandboxExtra(fk.ID, "newDiskKey", true) // see ensureDiskKey
	}
	if d, _ := src.Extra["diskGiB"].(float64); d > 0 {
		_ = s.SetSandboxExtra(fk.ID, "diskGiB", int(d)) // its source's disk, never shrunk (bugs-r18-sonnet 4)
	}
	chain := s.snapshotChain(src.HeadSnapshotID)
	sreq := schedReq{Size: spec, Placement: placement, Chain: chain, ImageID: src.ImageID, Owners: s.ownersFor(fk.OwnerID, fk.OrgID), Encrypted: src.Disk == "luks"}
	var placeErr error
	if live {
		sreq.Only = src.NodeID
		if _, placeErr = s.placeSandbox(fk.ID, api.StateCloning, sreq); placeErr != nil {
			sreq.Only = ""
			_, placeErr = s.placeSandbox(fk.ID, api.StateCloning, sreq)
		}
	} else {
		_, placeErr = s.placeSandbox(fk.ID, api.StateCloning, sreq)
	}
	forkWaiting := false
	if placeErr != nil {
		if req.FailFast || !s.couldEverPlace(sreq) {
			_, _ = s.DB.Exec(`DELETE FROM sandboxes WHERE id=?`, fk.ID)
			if req.FailFast {
				placeErr = ErrD(503, "no_ready_machine", "No machine is ready right now; no fork was created.", map[string]any{"retryAfterSeconds": 5})
			} else {
				placeErr = s.neverFitsErr(sreq, placeErr)
			}
			return fail(placeErr)
		}
		forkWaiting = true // SIZE-011: wait for capacity
	}
	s.recordStart(walletOf(u, org), u.ID)
	unlockLimits()
	if idemKey != "" {
		s.idemFinish(u.ID, idemKey, fk.ID)
	}
	fk, _ = s.SandboxByID(fk.ID)
	s.Bus.Publish(Event{Type: EvSandboxCreated, SandboxID: fk.ID, SandboxName: fk.Name, OwnerID: fk.OwnerID, OrgID: fk.OrgID, NodeID: fk.NodeID, State: fk.State, Data: map[string]any{"forkedFrom": src.ID}})
	o := bootOpts{Kind: proto.BootFork, Parent: src.HeadSnapshotID, Env: renv, FailFast: req.FailFast}
	if live {
		o.LiveFrom = src.ID
	}
	if forkWaiting {
		if o.LiveFrom != "" {
			// The fork copies the source as it is now, not when a node frees
			// up (bugs-r5-opus 2): take the live snapshot at once.
			if snap, err := s.takeLiveSnapshot(c.R.Context(), src); err == nil {
				o.Parent, o.LiveFrom = snap, ""
			} else {
				s.Log.Warn("live snapshot for a queued fork failed; it copies the source when placed", "source", src.ID, "err", err)
			}
		}
		s.queueBoot(fk.ID, pendingBoot{Kind: o.Kind, Parent: o.Parent, LiveFrom: o.LiveFrom, State: api.StateCloning, Reason: "capacity"})
		fk, _ = s.SandboxByID(fk.ID)
	} else {
		s.startBoot(fk, o)
	}
	return c.OK(202, "sandbox.forking", map[string]any{"id": fk.ID, "status": "forking", "sandbox": s.SandboxJSON(fk, c.P), "sourceId": src.ID})
}

func (s *Server) handleRecover(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.update", false)
	if err != nil {
		return err
	}
	if sb.NodeID == "" || !(sb.Usable() || sb.State == api.StateProvisioned || sb.State == api.StateError) {
		return Err(409, "no_machine", "The sandbox has no machine. Resume it instead.")
	}
	if sb.LastRecoverAt != nil && time.Since(*sb.LastRecoverAt) < 10*time.Minute {
		left := int(math.Ceil((10*time.Minute - time.Since(*sb.LastRecoverAt)).Minutes()))
		return ErrD(409, "too_soon", fmt.Sprintf("A recover started less than 10 minutes ago. Give it %d more minute(s).", left), map[string]any{"retryable": true, "retryAfterSeconds": left * 60})
	}
	node := s.nodes.get(sb.NodeID)
	if node == nil {
		return Err(409, "recover_unsupported", "The node hosting this sandbox is offline, so its machine cannot be power-cycled. Wait for the node, or stop it with force and resume it from its last snapshot.")
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 15*time.Second)
	defer cancel()
	var st proto.VMStatus
	if err := node.Do(ctx, "GET", "/v1/vms/"+sb.ID, nil, &st); err != nil {
		var ne *NodeError
		if errors.As(err, &ne) && ne.Status == 404 {
			return Err(409, "no_machine", "The node has no machine for this sandbox. Resume it instead.")
		}
	} else if st.State == proto.VMRunning && st.GuestReady && sb.Health != "degraded" && sb.State != api.StateError {
		return Err(409, "sandbox_is_reachable", "The sandbox answers normally.")
	}
	rctx, rcancel := context.WithTimeout(c.R.Context(), 2*time.Minute)
	defer rcancel()
	s.guests.expectReboot(sb.ID) // the node's new-ticket request is ours
	if err := node.Do(rctx, "POST", "/v1/vms/"+sb.ID+"/restart", map[string]any{}, nil); err != nil {
		return Err(409, "reset_failed", "The power-cycle failed: "+err.Error())
	}
	now := store.Now()
	s.transition(sb.ID, nil, api.StateProvisioned, map[string]any{"last_recover_at": now, "last_reboot_at": now, "last_reboot_reason": "unknown",
		"health": "degraded", "health_reason": "recovering", "degraded_since": now, "error": nil})
	if sb.State == api.StateError {
		sb.NodeID = node.ID
		s.openRun(sb)
	}
	return c.OK(200, "sandbox.recovering", map[string]any{"id": sb.ID, "status": "recovering", "message": "The machine is power-cycling. sandbox.recovered fires when it answers again."})
}

func (s *Server) handleShare(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.update", true)
	if err != nil {
		return err
	}
	if sb.OrgID == "" {
		return Err(400, "not_org_sandbox", "Only sandboxes billed to an organization can be shared.")
	}
	restart := false
	sets := map[string]any{"visibility": "shared", "private_by_creator": 0}
	if sb.HoldsCreatorLogins {
		sets["wipe_pending"], sets["no_env"] = 1, 1
		if sb.Active() {
			restart = true
		} else {
			sets["holds_creator_logins"] = 0
		}
		_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE sandbox_id=? AND preset<>'sandbox-token' AND revoked_at IS NULL`, store.Now(), sb.ID)
	}
	if sb.Visibility == "private" {
		// The private period ends: commands run in it stay private to the
		// creator, also those pulled from the guest later (bugs-r3-opus 15,
		// bugs-r4-sol 4).
		since, _ := sb.Extra["privateSince"].(float64)
		spans, _ := sb.Extra["privateSpans"].([]any)
		_ = s.SetSandboxExtra(sb.ID, "privateSpans", append(spans, []any{since, store.Now()}))
		_ = s.SetSandboxExtra(sb.ID, "privateSince", nil)
	}
	if err := s.updateSandbox(sb.ID, sets); err != nil {
		return err
	}
	sb, _ = s.SandboxByID(sb.ID)
	msg := "The sandbox is shared with the organization."
	if restart {
		msg = "Shared. It holds your logins in memory, so teammates can only view it until you stop and resume it."
	}
	s.Bus.Publish(Event{Type: EvSandboxUpdated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, State: sb.State, Reason: "shared"})
	return c.OK(200, "sandbox.shared", map[string]any{"sandbox": s.SandboxJSON(sb, c.P), "restartRequired": restart, "message": msg})
}

func (s *Server) handleUnshare(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.update", true)
	if err != nil {
		return err
	}
	if sb.OrgID == "" {
		return Err(400, "not_org_sandbox", "Only organization sandboxes can be made private.")
	}
	if sb.Visibility != "private" {
		_ = s.SetSandboxExtra(sb.ID, "privateSince", store.Now()) // a private period starts
	}
	if err := s.updateSandbox(sb.ID, map[string]any{"visibility": "private", "private_by_creator": 1}); err != nil {
		return err
	}
	sb, _ = s.SandboxByID(sb.ID)
	s.Bus.Publish(Event{Type: EvSandboxUpdated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, State: sb.State, Reason: "unshared"})
	return c.OK(200, "sandbox.unshared", map[string]any{"sandbox": s.SandboxJSON(sb, c.P), "message": "Only you can use this sandbox now."})
}

func confirmHeader(c *Ctx) string {
	if v := c.R.Header.Get("X-CrowdVM-Confirm-Delete"); v != "" {
		return v
	}
	return c.R.Header.Get("X-Ascii-Confirm-Delete")
}

func (s *Server) handleDeleteSandbox(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.delete", true)
	if err != nil {
		return err
	}
	if confirmHeader(c) != sb.ID {
		return Err(409, "confirmation_required", "Set X-Ascii-Confirm-Delete (or X-CrowdVM-Confirm-Delete) to the sandbox id to delete it.")
	}
	op, err := s.DeleteSandbox(sb, "explicit")
	if err != nil {
		return err
	}
	return c.OK(202, "sandbox.deleting", map[string]any{"operation": op})
}

// DeleteSandbox removes sb at once from every list and purges its machine
// and snapshots in the background. It returns the deletion operation JSON.
func (s *Server) DeleteSandbox(sb *Sandbox, reason string) (map[string]any, error) {
	unlock := s.sbLocks.Lock(sb.ID)
	defer unlock()
	opID, err := s.newDeletionOp(sb.OwnerID, "sandbox", sb.ID, reason)
	if err != nil {
		return nil, err
	}
	now := store.Now()
	if _, err := s.DB.Exec(`UPDATE sandboxes SET deleted_at=?, updated_at=? WHERE id=?`, now, now, sb.ID); err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE sandbox_id=? AND revoked_at IS NULL`, now, sb.ID)
	_, _ = s.DB.Exec(`UPDATE snapshots SET deleted_at=? WHERE sandbox_id=? AND deleted_at IS NULL`, now, sb.ID)
	s.dropGuestChannel(sb.ID) // the sealed config of its boot, and its channel
	s.closeRun(sb.ID)
	if sb.StopID != "" {
		_, _ = s.DB.Exec(`UPDATE stops SET status='superseded', ended_at=? WHERE id=? AND status IN ('pending','failing')`, now, sb.StopID)
	}
	s.Bus.Publish(Event{Type: EvSandboxDeleted, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, PreviousState: sb.State})
	// The operation completes once the node confirmed the machine is gone
	// and the snapshots are purged (deletion_ops.go).
	_, _ = s.DB.Exec(`UPDATE deletion_ops SET node_id=? WHERE id=?`, nz(sb.NodeID), opID)
	s.Go(s.processDeletionOps)
	return s.deletionOpJSON(opID)
}

// confirmVMDown fails a sandbox only after its node confirms the VM is down
// right now. Events are batched and can arrive late: a "stopped" event from a
// finished stop must not fail the resume that already followed it.
func (s *Server) confirmVMDown(nodeID, sbID, reason string) {
	c := s.nodes.get(nodeID)
	if c == nil {
		return // node offline: onNodeOffline handles it
	}
	s.Go(func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var cur proto.VMStatus
		if err := c.Do(cctx, "GET", "/v1/vms/"+sbID, nil, &cur); err == nil &&
			(cur.State == proto.VMStarting || cur.State == proto.VMRunning || cur.State == proto.VMStopping) {
			return // stale event for an earlier run
		}
		// A stop in flight powers the VM off while the sandbox still looks
		// usable (refused-stop pre-flight); its own result decides the state.
		var inFlight int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM stops WHERE sandbox_id=? AND ended_at IS NULL AND status IN ('pending','in_progress')`, sbID).Scan(&inFlight)
		if inFlight > 0 {
			return
		}
		if sb, err := s.SandboxByID(sbID); err == nil && sb.NodeID == nodeID && (sb.starting() || sb.Usable()) {
			s.failSandbox(sbID, reason)
		}
	})
}
