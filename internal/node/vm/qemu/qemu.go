// Package qemu is the real VM driver. It runs one qemu-system-x86_64 per
// sandbox (KVM on Linux, WHPX on Windows, TCG as fallback) on a qcow2 overlay
// chain. Each boot gets a fresh CROWDVM config drive. The VM's NIC is a QEMU
// stream socket served by a node-side userspace network (package netstack)
// that enforces the node's egress policy; the guest agent port is forwarded
// to loopback through it. The driver uses QMP to halt VMs (no OS shutdown),
// for live snapshots and for chain compaction.
//
// VMs survive a restart of the node process: a new Driver adopts running
// qemu processes from <DataDir>/vms and marks dead ones as error (their
// overlay is kept, so Stop still saves it and Restart boots it again).
package qemu

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/node/vm/netstack"
	"github.com/crowdvm/crowdvm/internal/node/vm/qemu/guestconn"
	"github.com/crowdvm/crowdvm/internal/node/vm/qemu/iso"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Options configure the driver. Zero values mean the documented defaults.
type Options struct {
	DataDir     string // required: store, overlays, VM runtime dirs
	GuestBinary string // linux/amd64 crowdvm-guest for the config drive (required to Start)
	QEMU        string // qemu-system-x86_64 (default: PATH, then next to QEMUImg)
	QEMUImg     string // qemu-img (default: PATH, then next to QEMU)
	Accel       string // "kvm" | "whpx" | "tcg"; default: kvm if /dev/kvm usable, whpx on Windows, else tcg
	CPU         string // -cpu override; default "host" on KVM, "max" otherwise

	// CLIBinary is the linux/amd64 in-sandbox `crowdvm` CLI. When set (and
	// present) it goes on the config drive as "crowdvm"; the guest installs
	// it with host/boat/box links. Optional.
	CLIBinary string

	BootTimeout        time.Duration // launch -> guest healthy, else error (default 3m)
	PrepareStopTimeout time.Duration // guest prepare-stop call (pre-stop hook, docker stop, cleanup; default 120s)
	// MaxChain is the most snapshot layers a chain may have. A snapshot that
	// would exceed it is flattened onto the base image (ParentID ""). Default 32.
	MaxChain int
	// MergeFanout drives tiered chain compaction (see mergePlan): a run of
	// MergeFanout-1 layers of one level is merged into the next snapshot.
	// Default 4; < 0 disables (only MaxChain flattens).
	MergeFanout int
	// NoNested hides vmx/svm unless a sandbox asks for nested
	// virtualization. Default: exposed (KVM passes them only when the host
	// has nested virtualization enabled).
	NoNested bool

	// Net selects guest networking. NetNetstack (default): each VM gets a
	// node-side userspace network that enforces NetPolicy. NetSlirp: QEMU user
	// networking, under which guests reach this machine's loopback services
	// (as 10.0.2.2) and its LANs/tailnets; insecure, a fallback only.
	Net       string
	NetPolicy netstack.Policy
	// ServerAddr (host:port) is the CrowdVM server. Guests can always reach
	// it, also as 10.0.2.2:<port> (like slirp's host alias).
	ServerAddr string

	// VirGL: "on" or a /dev/dri/renderD* path gives every VM an OpenGL
	// virtio-gpu rendered on this machine's GPU (egl-headless); checked in New.
	VirGL string

	Logf func(format string, args ...any) // default: discard
}

// Guest networking modes (Options.Net).
const (
	NetNetstack = "netstack"
	NetSlirp    = "slirp"
)

// Driver implements vm.Driver, vm.Dialer, vm.Waiter and vm.Notifier.
type Driver struct {
	opt     Options
	virgl   string // render node for the OpenGL virtio-gpu ("" off)
	qemu    string
	accel   string
	seccomp bool
	lazyOK  bool // http + copy-on-read block drivers (lazy chains)
	rules   *netstack.Rules
	store   *store
	ctx     context.Context
	cancel  context.CancelFunc
	closed  atomic.Bool
	nmu     sync.Mutex // orders notifications
	mu      sync.Mutex
	vms     map[string]*machine
	changed chan struct{} // closed and replaced on every state change
	notify  func(proto.VMStatus)
	ticket  func(ctx context.Context, id string) (*proto.GuestChannel, error)
	fds     *fdStore // nil: path sockets (no systemd fd store)
}

var (
	_ vm.Driver        = (*Driver)(nil)
	_ vm.Dialer        = (*Driver)(nil)
	_ vm.Waiter        = (*Driver)(nil)
	_ vm.Notifier      = (*Driver)(nil)
	_ vm.ChangeTracker = (*Driver)(nil)
	_ vm.NICDialer     = (*Driver)(nil)
	_ vm.LazyStarter   = (*Driver)(nil)
	_ vm.Renamer       = (*Driver)(nil)
	_ vm.Reticketer    = (*Driver)(nil)
)

// vmRecord is persisted as vms/<id>/vm.json (0600: holds the guest token).
type vmRecord struct {
	Spec      proto.VMSpec `json:"spec"`
	ImageID   string       `json:"imageId"`
	Disk      string       `json:"disk"`              // active overlay, relative to DataDir
	Parent    string       `json:"parent"`            // snapshot under Disk ("" = base image)
	Port      int          `json:"port"`              // 127.0.0.1:Port -> guest :7070
	Net       string       `json:"net,omitempty"`     // NetNetstack | NetSlirp ("" = slirp, older records)
	NetSock   string       `json:"netSock,omitempty"` // qemu's stream netdev socket (netstack)
	QMPNet    string       `json:"qmpNet"`
	QMPAddr   string       `json:"qmpAddr"`
	PID       int          `json:"pid"`
	StartedAt time.Time    `json:"startedAt"`
	// Lazy chain (lazy.go, LIFE-013): Lazy = the chain under the overlay
	// (top first) while some layer is read from a URL, LazyTop = layers
	// snapshotted during such a run (top first); both nil once hydrated.
	// LazyGraph: the running qemu has the lazy -blockdev graph (until its
	// next launch), so live compaction waits.
	Lazy      []lazyLayer `json:"lazy,omitempty"`
	LazyTop   []lazyLayer `json:"lazyTop,omitempty"`
	LazyGraph bool        `json:"lazyGraph,omitempty"`
}

type machine struct {
	id string
	op sync.Mutex // serializes lifecycle operations
	// guarded by Driver.mu:
	rec  vmRecord
	st   proto.VMStatus
	run  *run
	gone bool
	// snapWr: guest disk write+discard ops counted when the current overlay
	// started (-1 = unknown), for DiskChanged.
	snapWr int64
	resets []time.Time // guest reboots of a channel guest, for the loop breaker
	ticket string      // the current boot's ticket: in RAM only, served on the VM's network
}

// run is one qemu process incarnation.
type run struct {
	q        *qmp
	proc     *os.Process
	exited   chan struct{}
	intended atomic.Bool // we asked it to exit: not an error
	poweroff atomic.Bool // guest powered itself off
	reset    atomic.Bool // channel guest rebooted (qemu exits instead): relaunch
	launched time.Time
	log      string
	waited   bool              // waitErr is cmd.Wait's result (not set for adopted runs)
	waitErr  error             // set before exited closes
	net      *netstack.Network // nil with slirp
}

func (r *run) closeNet() {
	if r.net != nil {
		r.net.Close()
	}
}

func (r *run) alive() bool {
	select {
	case <-r.exited:
		return false
	default:
		return true
	}
}

// New opens the store under opt.DataDir and adopts VMs left by a previous
// node process.
func New(opt Options) (*Driver, error) {
	if opt.DataDir == "" {
		return nil, errors.New("qemu: DataDir required")
	}
	if opt.BootTimeout <= 0 {
		opt.BootTimeout = 3 * time.Minute
	}
	if opt.PrepareStopTimeout <= 0 {
		opt.PrepareStopTimeout = 120 * time.Second
	}
	if opt.MaxChain <= 0 {
		opt.MaxChain = 32
	}
	if opt.MergeFanout == 0 {
		opt.MergeFanout = 4
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	p := opt.NetPolicy // compiled in slirp mode too: adopted netstack VMs need it
	if opt.ServerAddr != "" {
		p.Pinholes = append(append([]string(nil), p.Pinholes...), opt.ServerAddr)
	}
	rules, err := netstack.NewRules(p)
	if err != nil {
		return nil, fmt.Errorf("qemu: net policy: %w", err)
	}
	switch opt.Net {
	case "", NetNetstack:
		opt.Net = NetNetstack
	case NetSlirp:
		opt.Logf("qemu: WARNING: --net slirp: guests can reach this machine's localhost services (as 10.0.2.2), " +
			"its LAN and its tailnet. Untrusted sandboxes are NOT contained. Use only for debugging.")
	default:
		return nil, fmt.Errorf("qemu: unknown net mode %q (netstack | slirp)", opt.Net)
	}
	qemuBin, qimg, err := findBinaries(opt.QEMU, opt.QEMUImg)
	if err != nil {
		return nil, err
	}
	st, err := openStore(opt.DataDir, qimg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opt.DataDir, "vms"), 0o700); err != nil {
		return nil, err
	}
	d := &Driver{opt: opt, qemu: qemuBin, accel: opt.Accel, rules: rules, store: st, vms: map[string]*machine{}, changed: make(chan struct{})}
	if d.accel == "" {
		d.accel = defaultAccel()
	}
	if runtime.GOOS == "linux" { // seccomp: limit what a compromised qemu can do
		out, _ := exec.Command(qemuBin, "-help").Output()
		d.seccomp = bytes.Contains(out, []byte("\n-sandbox "))
	}
	if opt.VirGL != "" && opt.VirGL != "off" {
		if d.virgl, err = checkVirGL(qemuBin, opt.VirGL); err != nil {
			return nil, err
		}
	}
	d.lazyOK = detectLazy(qimg)
	d.ctx, d.cancel = context.WithCancel(context.Background())
	st.inUse = d.inUse
	d.fds = openFDStore()
	d.adoptAll()
	st.gcOrphans(true)
	return d, nil
}

// Accel reports the accelerator in use ("kvm", "whpx" or "tcg").
func (d *Driver) Accel() string { return d.accel }

func (d *Driver) Name() string                      { return "qemu" }
func (d *Driver) Store() vm.Store                   { return d.store }
func (d *Driver) SetNotify(fn func(proto.VMStatus)) { d.mu.Lock(); d.notify = fn; d.mu.Unlock() }
func (d *Driver) SetReticket(fn func(context.Context, string) (*proto.GuestChannel, error)) {
	d.mu.Lock()
	d.ticket = fn
	d.mu.Unlock()
}

// Close releases QMP connections. VMs keep running; a new Driver adopts them.
func (d *Driver) Close() error {
	d.closed.Store(true)
	d.cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.vms {
		if m.run != nil && m.run.q != nil {
			m.run.q.close()
		}
		if m.run != nil {
			m.run.closeNet() // qemu keeps its socket; the next node process attaches again
		}
	}
	return nil
}

// NetMode reports the guest networking mode (NetNetstack or NetSlirp).
func (d *Driver) NetMode() string { return d.opt.Net }

func (d *Driver) vmDir(id string) string { return filepath.Join(d.opt.DataDir, "vms", id) }

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (d *Driver) Start(ctx context.Context, spec proto.VMSpec) error {
	return d.start(spec, func(m *machine) error { return d.create(m, spec) })
}

func (d *Driver) start(spec proto.VMSpec, create func(*machine) error) error {
	id := spec.SandboxID
	if err := checkID(id); err != nil {
		return err
	}
	if d.opt.GuestBinary == "" {
		return errors.New("qemu: no guest binary configured")
	}
	if _, err := os.Stat(d.opt.GuestBinary); err != nil {
		return fmt.Errorf("qemu: guest binary: %w", err)
	}
	if _, err := sandboxPolicy(spec.Net); err != nil {
		return err
	}
	if len(spec.GPUDevices) > 0 && d.accel != "kvm" {
		return fmt.Errorf("qemu: GPU passthrough needs KVM (accel is %s)", d.accel)
	}
	d.mu.Lock()
	if d.vms[id] != nil {
		d.mu.Unlock()
		return fmt.Errorf("%s: %w", id, vm.ErrExists)
	}
	m := &machine{id: id, st: proto.VMStatus{SandboxID: id, State: proto.VMStarting}, snapWr: -1}
	d.vms[id] = m
	m.op.Lock()
	d.mu.Unlock()
	defer m.op.Unlock()
	if err := create(m); err != nil {
		d.remove(m)
		return err
	}
	return nil
}

func (d *Driver) create(m *machine, spec proto.VMSpec) error {
	base, err := d.store.backing(spec.ParentID, spec.ImageID)
	if err != nil {
		return err
	}
	// The size counts user data only (Boat): add what the OS image uses.
	// A disk never shrinks: a smaller size on resume keeps the parent's
	// (the guest's filesystem cannot be shrunk while mounted).
	want := int64(spec.Size.DiskGiB)<<30 + roundUp(d.store.imageData(base.imageID), 1<<30)
	size := max(want, base.vsize)
	if size > want && spec.Size.DiskGiB > 0 {
		d.opt.Logf("qemu: %s: disk kept at %d GiB (parent), %d GiB requested", m.id, size>>30, want>>30)
	}
	rel := "disks/" + m.id + "-" + randHex(4) + ".qcow2"
	d.mu.Lock()
	m.rec = vmRecord{Spec: spec, ImageID: base.imageID, Disk: rel, Parent: spec.ParentID}
	m.rec.Spec.Guest = spec.Guest.WithoutTicket() // the ticket stays in RAM (launch)
	d.mu.Unlock()
	if err := d.store.run("create", "-q", "-f", "qcow2", "-u", "-b", base.path, "-F", base.format,
		d.store.abs(rel), strconv.FormatInt(size, 10)); err != nil {
		return err
	}
	if err := os.MkdirAll(d.vmDir(m.id), 0o700); err != nil {
		return err
	}
	return d.launch(m, spec.Guest)
}

func (d *Driver) Stop(ctx context.Context, id, snapID string, force bool) (proto.SnapshotRef, error) {
	if snapID != "" {
		if err := checkID(snapID); err != nil {
			return proto.SnapshotRef{}, err
		}
		if d.store.HasSnapshot(snapID) {
			return proto.SnapshotRef{}, fmt.Errorf("snapshot %s already exists", snapID)
		}
	}
	m, err := d.lock(id)
	if err != nil {
		return proto.SnapshotRef{}, err
	}
	defer m.op.Unlock()
	start := time.Now()
	d.mu.Lock()
	r, ready := m.run, m.st.GuestReady
	d.mu.Unlock()
	d.setState(m, proto.VMStopping, "")
	if r != nil && r.alive() {
		r.intended.Store(true)
		if !force && ready {
			d.prepareStop(ctx, m)
		}
		d.kill(r) // QMP stop + quit: no OS shutdown (Boat)
	}
	if snapID == "" {
		d.remove(m)
		d.setState(m, proto.VMStopped, "")
		d.store.gc()
		return proto.SnapshotRef{}, nil
	}
	ref, err := d.freeze(m, snapID)
	if err != nil {
		d.setState(m, proto.VMError, "stop: "+err.Error())
		return ref, err
	}
	d.remove(m)
	d.setState(m, proto.VMStopped, "")
	d.store.gc()
	d.opt.Logf("qemu: %s stopped as %s in %s", id, snapID, time.Since(start).Round(time.Millisecond))
	return ref, nil
}

// prepareStop asks the guest to run the Boat stop steps (pre-stop hook,
// docker stop, cleanup, sync) and to freeze its filesystems; the caller
// terminates the VM right after.
func (d *Driver) prepareStop(ctx context.Context, m *machine) {
	req := proto.PrepareStopRequest{Freeze: true}
	if err := d.guest(m).Post(ctx, "/v1/internal/prepare-stop", req, d.opt.PrepareStopTimeout); err != nil {
		d.opt.Logf("qemu: %s prepare-stop: %v", m.id, err)
	}
}

// freeze turns the (stopped) VM's overlay into immutable snapshot snapID,
// first merging older layers into it when the compaction policy says so.
func (d *Driver) freeze(m *machine, snapID string) (proto.SnapshotRef, error) {
	d.mu.Lock()
	rec := m.rec
	d.mu.Unlock()
	step := mergeStep{parent: rec.Parent}
	var err error
	if rec.Lazy == nil { // a chain still read from URLs is not merged
		if step, err = d.planMerge(rec.Parent, rec.ImageID); err != nil {
			return proto.SnapshotRef{}, err
		}
	}
	if step.n > 0 { // offline: qemu-img copies what the absorbed layers held
		if err := d.store.run("rebase", "-q", "-f", "qcow2", "-b", step.base.path, "-F", step.base.format, d.store.abs(rec.Disk)); err != nil {
			return proto.SnapshotRef{}, err
		}
		d.opt.Logf("qemu: %s: merged %d layers into the stopped overlay", m.id, step.n)
		d.mu.Lock()
		m.rec.Parent = step.parent
		d.mu.Unlock()
		d.saveRec(m)
	}
	ref, err := d.addLayer(m.id, snapID, rec.Disk, step.parent, rec.ImageID, step.level, step.supersedes)
	if err != nil {
		return ref, err
	}
	d.mu.Lock()
	m.rec.Disk, m.rec.Parent = "", snapID // the disk now belongs to the store
	d.mu.Unlock()
	return ref, nil
}

func (d *Driver) addLayer(sandboxID, snapID, rel, parent, imageID string, level int, supersedes []string) (proto.SnapshotRef, error) {
	p := d.store.abs(rel)
	info, err := d.store.info(p)
	if err != nil {
		return proto.SnapshotRef{}, err
	}
	os.Chmod(p, 0o444)
	ref := proto.SnapshotRef{ID: snapID, ParentID: parent, ImageID: imageID, SizeBytes: fileSize(p), CreatedAt: time.Now().UTC(), Supersedes: supersedes, SandboxID: sandboxID}
	return ref, d.store.addSnapshot(ref, rel, info.VirtualSize, level)
}

// mergePlan is the tiered chain compaction policy. levels are the levels
// of a VM's snapshot layers from the top (its parent) down. The next
// snapshot absorbs the run of fanout-1 level-0 layers at the top, then a
// run of fanout-1 level-1 layers below them, and so on; it gets the next
// level. So the chain stays O(fanout*log(snapshots)) deep, and each byte is
// uploaded again only once per level (a 60 s interval: level 1 every 4 min,
// level 2 every 16 min, ...). Returns the layers to absorb and the level.
func mergePlan(levels []int, fanout int) (n, level int) {
	if fanout < 2 {
		return 0, 0
	}
	for {
		run := 0
		for n+run < len(levels) && levels[n+run] == level {
			run++
		}
		if run < fanout-1 {
			return n, level
		}
		n += run
		level++
	}
}

// mergeStep is the compaction a new snapshot of a VM on parent does.
type mergeStep struct {
	n          int    // top layers absorbed (0 = none)
	parent     string // the new snapshot's parent ("" = image)
	base       layer  // what the overlay is rebased/streamed onto (n > 0)
	level      int
	supersedes []string // absorbed server snapshots
}

func (d *Driver) planMerge(parent, imageID string) (mergeStep, error) {
	chain := d.store.chain(parent)
	levels := make([]int, len(chain))
	top := 0
	for i, c := range chain {
		levels[i] = c.Level
		if c.Level < levelUnknown {
			top = max(top, c.Level+1)
		}
	}
	n, level := mergePlan(levels, d.opt.MergeFanout)
	if len(chain)-n+1 > d.opt.MaxChain { // hard cap: flatten onto the image
		n, level = len(chain), max(level, top)
	}
	st := mergeStep{n: n, parent: parent, level: level}
	if n == 0 {
		return st, nil
	}
	for _, c := range chain[:n] {
		if !strings.HasPrefix(c.Ref.ID, "_") { // hidden layers are not server snapshots
			st.supersedes = append(st.supersedes, c.Ref.ID)
		}
	}
	if n < len(chain) { // a layer may be hidden/deleted yet in use: no backing() check
		b := chain[n]
		st.parent, st.base = b.Ref.ID, layer{path: d.store.abs(b.File), format: "qcow2", imageID: imageID}
		return st, nil
	}
	var err error
	st.parent = ""
	st.base, err = d.store.backing("", imageID)
	return st, err
}

// Snapshot takes a live snapshot: the guest syncs, the current overlay
// becomes snapshot snapID and the VM continues on a fresh overlay. Another
// VM may start from snapID at once (fork).
func (d *Driver) Snapshot(ctx context.Context, id, snapID string) (proto.SnapshotRef, error) {
	if err := checkID(snapID); err != nil {
		return proto.SnapshotRef{}, err
	}
	if d.store.HasSnapshot(snapID) {
		return proto.SnapshotRef{}, fmt.Errorf("snapshot %s already exists", snapID)
	}
	m, err := d.lock(id)
	if err != nil {
		return proto.SnapshotRef{}, err
	}
	defer m.op.Unlock()
	d.mu.Lock()
	r, ready, rec := m.run, m.st.GuestReady, m.rec
	d.mu.Unlock()
	if r == nil || !r.alive() {
		return proto.SnapshotRef{}, fmt.Errorf("%s: %w", id, vm.ErrNotRunning)
	}
	// Compaction first (block-stream runs while the guest keeps working);
	// not across a lazy graph.
	step := mergeStep{parent: rec.Parent}
	if !rec.LazyGraph {
		if step, err = d.planMerge(rec.Parent, rec.ImageID); err != nil {
			return proto.SnapshotRef{}, err
		}
	}
	if step.n > 0 {
		t0 := time.Now()
		if err := d.stream(ctx, r, step.base.path); err != nil {
			return proto.SnapshotRef{}, fmt.Errorf("merge %d layers: %w", step.n, err)
		}
		d.opt.Logf("qemu: %s: merged %d layers into the overlay in %s", id, step.n, time.Since(t0).Round(time.Millisecond))
		d.mu.Lock()
		m.rec.Parent = step.parent
		d.mu.Unlock()
		d.saveRec(m)
	}
	// Consistent point: the guest syncs and freezes its filesystems for the
	// instant of the overlay switch (sync only with older guests). Not for a
	// guest that stalled them before: crash-consistent instead (bugs-r11-opus 7).
	ready = ready && !vm.NoQuiesce(ctx)
	frozen := ready && d.guest(m).Post(ctx, "/v1/internal/freeze", proto.FreezeRequest{TimeoutSeconds: 30}, time.Minute) == nil
	if ready && !frozen {
		if err := d.guest(m).Post(ctx, "/v1/internal/sync", nil, time.Minute); err != nil {
			d.opt.Logf("qemu: %s sync before snapshot: %v", id, err)
		}
	}
	wr, wrOK := d.writeOps(ctx, r)
	newRel := "disks/" + id + "-" + randHex(4) + ".qcow2"
	mode := "absolute-paths"
	if rec.LazyGraph {
		// qemu would name the lazy graph (json:{...}) as the new overlay's
		// backing file; create it with the old overlay's plain path.
		mode = "existing"
		if err = d.createOverlayOn(rec.Disk, newRel); err != nil {
			return proto.SnapshotRef{}, err
		}
	}
	// Not cancelled with the request: qemu switches to the new overlay even
	// when the caller goes away (a server restart mid-stop), and the switch
	// must then be recorded as a layer, never left as an untracked file.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	err = r.q.cmd(sctx, "blockdev-snapshot-sync", map[string]any{
		"device": "disk0", "snapshot-file": d.store.abs(newRel), "format": "qcow2", "mode": mode,
	}, nil)
	scancel()
	if frozen {
		if terr := d.guest(m).Post(context.WithoutCancel(ctx), "/v1/internal/thaw", nil, 30*time.Second); terr != nil {
			d.opt.Logf("qemu: %s thaw: %v (the guest auto-thaws)", id, terr)
		}
	}
	if err != nil {
		removeFile(d.store.abs(newRel))
		return proto.SnapshotRef{}, err
	}
	ref, err := d.addLayer(m.id, snapID, rec.Disk, step.parent, rec.ImageID, step.level, step.supersedes)
	d.mu.Lock()
	if m.rec.Lazy != nil { // relaunch while lazy: the frozen layer sits above the lazy graph
		m.rec.LazyTop = append([]lazyLayer{{ID: snapID, File: rec.Disk}}, m.rec.LazyTop...)
	}
	m.rec.Disk = newRel
	m.rec.Parent = snapID
	m.snapWr = -1
	if wrOK {
		m.snapWr = wr
	}
	if err != nil { // keep the frozen layer as a hidden snapshot so the chain stays valid
		m.rec.Parent = d.hideLayer(rec.Disk, step.parent, rec.ImageID)
		if m.rec.Lazy != nil {
			m.rec.LazyTop[0].ID = m.rec.Parent
		}
	}
	d.mu.Unlock()
	d.saveRec(m)
	return ref, err
}

// writeOps returns the guest's write+discard request count on its disk.
func (d *Driver) writeOps(ctx context.Context, r *run) (int64, bool) {
	var stats []struct {
		Device string `json:"device"`
		Stats  struct {
			Wr    int64 `json:"wr_operations"`
			Unmap int64 `json:"unmap_operations"`
		} `json:"stats"`
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if r.q.cmd(ctx, "query-blockstats", nil, &stats) != nil {
		return 0, false
	}
	for _, s := range stats {
		if s.Device == "disk0" {
			return s.Stats.Wr + s.Stats.Unmap, true
		}
	}
	return 0, false
}

// DiskChanged reports whether the guest wrote to its disk since the last
// live snapshot (true when unknown). Periodic snapshots skip idle VMs.
func (d *Driver) DiskChanged(id string) (bool, error) {
	d.mu.Lock()
	m := d.vms[id]
	var r *run
	var last int64 = -1
	if m != nil {
		r, last = m.run, m.snapWr
	}
	d.mu.Unlock()
	if m == nil {
		return false, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	if r == nil || !r.alive() || last < 0 {
		return true, nil
	}
	wr, ok := d.writeOps(d.ctx, r)
	return !ok || wr != last, nil
}

// hideLayer registers a frozen overlay that has no server snapshot id as a
// deleted (hidden) snapshot, which gc removes once nothing builds on it.
func (d *Driver) hideLayer(rel, parent, imageID string) string {
	id := "_layer-" + randHex(6)
	ref := proto.SnapshotRef{ID: id, ParentID: parent, ImageID: imageID, CreatedAt: time.Now().UTC()}
	m := &snapMeta{Ref: ref, File: rel, Deleted: true, Level: levelUnknown}
	d.store.mu.Lock()
	d.store.snaps[id] = m
	d.store.saveSnap(m)
	d.store.mu.Unlock()
	return id
}

// stream pulls the snapshot layers above basePath (a snapshot file or the
// base image) into the running VM's active overlay (QMP block-stream), which
// then backs directly onto basePath.
func (d *Driver) stream(ctx context.Context, r *run, basePath string) error {
	var nodes []struct {
		NodeName string `json:"node-name"`
		Drv      string `json:"drv"`
		File     string `json:"file"`
	}
	if err := r.q.cmd(ctx, "query-named-block-nodes", map[string]any{"flat": true}, &nodes); err != nil {
		return err
	}
	base := ""
	for _, n := range nodes {
		if n.Drv != "file" && sameFile(n.File, basePath) {
			base = n.NodeName
		}
	}
	if base == "" {
		return fmt.Errorf("block node for %s not found", basePath)
	}
	job := "stream-" + randHex(3)
	ev, err := r.q.cmdWaitEvent(ctx, "block-stream", map[string]any{"job-id": job, "device": "disk0", "base-node": base},
		func(e qmpEvent) bool {
			var data struct{ Device string }
			json.Unmarshal(e.Data, &data)
			return data.Device == job && (e.Name == "BLOCK_JOB_COMPLETED" || e.Name == "BLOCK_JOB_CANCELLED")
		})
	if err != nil {
		return err
	}
	var data struct{ Error string }
	json.Unmarshal(ev.Data, &data)
	if ev.Name == "BLOCK_JOB_CANCELLED" || data.Error != "" {
		return fmt.Errorf("block-stream: %s %s", ev.Name, data.Error)
	}
	return nil
}

// Restart hard-restarts the VM on the same overlay (recover).
func (d *Driver) Restart(ctx context.Context, id string) error {
	m, err := d.lock(id)
	if err != nil {
		return err
	}
	defer m.op.Unlock()
	d.mu.Lock()
	r, cfg := m.run, m.rec.Spec.Guest
	d.mu.Unlock()
	if r != nil {
		d.kill(r)
	}
	if err := d.reticket(ctx, m, &cfg); err != nil {
		d.setState(m, proto.VMError, err.Error())
		return err
	}
	cfg.Boot, cfg.FirstBoot = proto.BootRecover, false
	if err := d.launch(m, cfg); err != nil {
		d.setState(m, proto.VMError, err.Error())
		return err
	}
	return nil
}

// Destroy kills the VM and deletes its overlay. Unknown ids are a no-op.
func (d *Driver) Destroy(ctx context.Context, id string) error {
	m, err := d.lock(id)
	if errors.Is(err, vm.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	defer m.op.Unlock()
	d.mu.Lock()
	r := m.run
	d.mu.Unlock()
	if r != nil {
		d.kill(r)
	}
	d.remove(m)
	d.store.gc()
	return nil
}

func (d *Driver) Status(id string) (proto.VMStatus, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.vms[id]
	if m == nil {
		return proto.VMStatus{}, false
	}
	return m.st, true
}

func (d *Driver) List() []proto.VMStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]proto.VMStatus, 0, len(d.vms))
	for _, m := range d.vms {
		out = append(out, m.st)
	}
	return out
}

func (d *Driver) GuestAddr(id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.vms[id]
	if m == nil {
		return "", fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	if m.run == nil || !m.run.alive() {
		return "", fmt.Errorf("%s: %w", id, vm.ErrNotRunning)
	}
	return fmt.Sprintf("127.0.0.1:%d", m.rec.Port), nil
}

// DialGuest opens a TCP connection to 127.0.0.1:port inside the guest via
// the guest agent's /v1/tunnel.
func (d *Driver) DialGuest(ctx context.Context, id string, port int) (net.Conn, error) {
	addr, err := d.GuestAddr(id)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	tok := d.vms[id].rec.Spec.Guest.Token
	d.mu.Unlock()
	return guestconn.Guest{Addr: addr, Token: tok}.Dial(ctx, port)
}

// Rename hands running VM fromID to spec.SandboxID (warm pool adoption):
// its runtime dir moves and its record takes spec.
func (d *Driver) Rename(ctx context.Context, fromID string, spec proto.VMSpec) error {
	to := spec.SandboxID
	if err := checkID(to); err != nil {
		return err
	}
	m, err := d.lock(fromID)
	if err != nil {
		return err
	}
	defer m.op.Unlock()
	d.mu.Lock()
	if d.vms[to] != nil {
		d.mu.Unlock()
		return fmt.Errorf("%s: %w", to, vm.ErrExists)
	}
	oldDir, newDir := d.vmDir(fromID), d.vmDir(to)
	if err := os.Rename(oldDir, newDir); err != nil {
		d.mu.Unlock()
		return err
	}
	move := func(p string) string {
		if rest, ok := strings.CutPrefix(p, oldDir); ok {
			return newDir + rest
		}
		return p
	}
	delete(d.vms, fromID)
	d.vms[to] = m
	m.id = to
	m.st.SandboxID = to
	m.rec.Spec = spec
	m.rec.Spec.Guest = spec.Guest.WithoutTicket()
	m.rec.QMPAddr, m.rec.NetSock = move(m.rec.QMPAddr), move(m.rec.NetSock)
	state, msg := m.st.State, m.st.Error
	d.mu.Unlock()
	d.saveRec(m)
	d.setState(m, state, msg)
	return nil
}

// DialGuestNIC connects to the guest's NIC (netstack networking only): the
// guest sees the gateway as the peer, so its firewall applies.
func (d *Driver) DialGuestNIC(ctx context.Context, id, network string, port int) (net.Conn, error) {
	d.mu.Lock()
	m := d.vms[id]
	var r *run
	if m != nil {
		r = m.run
	}
	d.mu.Unlock()
	switch {
	case m == nil:
		return nil, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	case r == nil || !r.alive():
		return nil, fmt.Errorf("%s: %w", id, vm.ErrNotRunning)
	case r.net == nil:
		return nil, fmt.Errorf("%s (slirp): %w", id, vm.ErrNoNIC)
	case port < 1 || port > 65535:
		return nil, fmt.Errorf("invalid port %d", port)
	case network == "udp":
		return r.net.DialGuestUDP(uint16(port))
	default:
		return r.net.DialGuest(ctx, uint16(port))
	}
}

// Wait blocks until the VM is running (nil) or no longer starting (error).
func (d *Driver) Wait(ctx context.Context, id string) error {
	for {
		d.mu.Lock()
		m, ch := d.vms[id], d.changed
		var st proto.VMStatus
		if m != nil {
			st = m.st
		}
		d.mu.Unlock()
		switch {
		case m == nil:
			return fmt.Errorf("%s: %w", id, vm.ErrNotFound)
		case st.State == proto.VMRunning:
			return nil
		case st.State != proto.VMStarting:
			return fmt.Errorf("vm %s is %s: %s", id, st.State, st.Error)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ---------------------------------------------------------------------------
// Process management
// ---------------------------------------------------------------------------

// lock returns the machine with its op lock held.
func (d *Driver) lock(id string) (*machine, error) {
	d.mu.Lock()
	m := d.vms[id]
	d.mu.Unlock()
	if m == nil {
		return nil, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	m.op.Lock()
	d.mu.Lock()
	gone := m.gone
	d.mu.Unlock()
	if gone {
		m.op.Unlock()
		return nil, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	return m, nil
}

// remove forgets the VM and deletes its runtime dir and active overlay.
func (d *Driver) remove(m *machine) {
	d.mu.Lock()
	m.gone = true
	if d.vms[m.id] == m {
		delete(d.vms, m.id)
	}
	disk, qnet, qaddr, nsock := m.rec.Disk, m.rec.QMPNet, m.rec.QMPAddr, m.rec.NetSock
	m.rec.Disk = ""
	d.mu.Unlock()
	if disk != "" {
		removeFile(d.store.abs(disk))
	}
	if qnet == "unix" {
		os.Remove(qaddr)
	}
	if nsock != "" {
		os.Remove(nsock)
	}
	os.RemoveAll(d.vmDir(m.id))
}

func (d *Driver) guest(m *machine) guestconn.Guest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return guestconn.Guest{Addr: fmt.Sprintf("127.0.0.1:%d", m.rec.Port), Token: m.rec.Spec.Guest.Token}
}

func (d *Driver) saveRec(m *machine) {
	d.mu.Lock()
	rec := m.rec
	d.mu.Unlock()
	if err := writeJSON(filepath.Join(d.vmDir(m.id), "vm.json"), rec); err != nil {
		d.opt.Logf("qemu: %s: save vm.json: %v", m.id, err)
	}
}

func (d *Driver) setState(m *machine, state, msg string) {
	d.mu.Lock()
	m.st.State, m.st.Error = state, msg
	m.st.GuestReady = state == proto.VMRunning
	m.st.StartedAt = m.rec.StartedAt
	st, fn := m.st, d.notify
	close(d.changed)
	d.changed = make(chan struct{})
	d.nmu.Lock()
	d.mu.Unlock()
	defer d.nmu.Unlock()
	if fn != nil && !d.closed.Load() {
		fn(st)
	}
}

// launch writes the config drive and starts qemu, retrying when the random
// host port for the guest agent forward was taken in the meantime.
func (d *Driver) launch(m *machine, cfg proto.GuestConfig) error {
	dir := d.vmDir(m.id)
	cfg.ListenAddr = fmt.Sprintf(":%d", proto.GuestPort)
	if cfg.Channel != nil && d.opt.Net == NetNetstack && d.opt.ServerAddr != "" {
		// The guest dials the server through the gateway alias: no DNS
		// before its channel (which carries its DNS) is up.
		if _, port, err := net.SplitHostPort(d.opt.ServerAddr); err == nil {
			ch := *cfg.Channel
			ch.Addr = net.JoinHostPort(netstack.Gateway.String(), port)
			cfg.Channel = &ch
		}
	}
	if cfg.Channel != nil && cfg.Channel.Ticket != "" && d.opt.Net == NetNetstack {
		// The ticket stays in this process: the guest reads it from its
		// network (TicketPort), never from a file the node's user can read.
		d.mu.Lock()
		m.ticket = cfg.Channel.Ticket
		d.mu.Unlock()
		cfg = cfg.WithoutTicket()
		ch := *cfg.Channel
		ch.TicketFrom = net.JoinHostPort(netstack.Gateway.String(), strconv.Itoa(proto.TicketPort))
		cfg.Channel = &ch
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	files := []iso.File{{Name: "config.json", Data: cfgJSON}, {Name: "crowdvm-guest", Path: d.opt.GuestBinary}}
	if p := d.opt.CLIBinary; p != "" && fileSize(p) > 0 { // in-sandbox CLI (optional)
		files = append(files, iso.File{Name: "crowdvm", Path: p})
	}
	if err := iso.WriteFile(filepath.Join(dir, "config.iso"), proto.ConfigDriveLabel, files); err != nil {
		return fmt.Errorf("config drive: %w", err)
	}
	for attempt := 0; ; attempt++ {
		err = d.spawn(m, dir)
		if err == nil || attempt == 2 || !strings.Contains(err.Error(), "forward") {
			return err
		}
	}
}

func (d *Driver) spawn(m *machine, dir string) (err error) {
	d.mu.Lock()
	rec := m.rec
	d.mu.Unlock()
	// QMP and the NIC: socketpairs kept in systemd's fd store when the node
	// runs under it (no path anyone could connect to), else path sockets.
	var qmpArgs []string
	var qnet, qaddr string
	var qmpNode, nicNode *os.File
	var extra []*os.File
	defer func() {
		// qemu's ends (qemu has them now) and the node's (duplicated into
		// the fd store and the connections).
		for _, f := range append(extra, qmpNode, nicNode) {
			if f != nil {
				f.Close()
			}
		}
	}()
	if d.fds != nil {
		a, b, err := socketPair()
		if err != nil {
			return err
		}
		qmpNode, extra = a, append(extra, b)
		qmpArgs = []string{"-chardev", fmt.Sprintf("socket,id=qmp,fd=%d,server=off", 2+len(extra)), "-mon", "chardev=qmp,mode=control"}
		qnet = "fd"
		if d.opt.Net == NetNetstack {
			a, b, err := socketPair()
			if err != nil {
				return err
			}
			nicNode, extra = a, append(extra, b)
		}
	} else {
		qarg, n, a, err := qmpEndpoint(dir)
		if err != nil {
			return err
		}
		qmpArgs, qnet, qaddr = []string{"-qmp", qarg}, n, a
	}
	// netstack: the guest agent forward listens before qemu starts (no port
	// race); slirp: qemu binds a port that was free a moment ago.
	var port int
	var gl net.Listener
	nsock := ""
	if d.opt.Net == NetNetstack {
		if gl, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				gl.Close()
			}
		}()
		port = gl.Addr().(*net.TCPAddr).Port
		if nicNode != nil {
			nsock = fmt.Sprintf("fd:%d", 2+len(extra))
		} else {
			nsock = unixSocket(dir, "net.sock")
			os.Remove(nsock)
		}
	} else if port, err = freePort(); err != nil {
		return err
	}
	args, err := d.args(rec, dir, port, nsock, qmpArgs)
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "qemu.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintf(logf, "--- launch %s: %s %s\n", time.Now().Format(time.RFC3339), d.qemu, strings.Join(args, " "))
	// The serial console carries the guest's boot and service output; keep
	// only the tail across boots (secure layer: less on the node's disk).
	trimTail(filepath.Join(d.vmDir(m.id), "console.log"), 256<<10)
	cmd := exec.Command(d.qemu, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = sysProcAttr()
	cmd.ExtraFiles = extra // fd 3 (QMP), fd 4 (NIC)
	err = cmd.Start()
	logf.Close()
	if err != nil {
		return err
	}
	r := &run{proc: cmd.Process, exited: make(chan struct{}), launched: time.Now(), log: logPath}
	go func() { r.waitErr, r.waited = cmd.Wait(), true; close(r.exited) }() // bugs-r13-opus 12

	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	if qmpNode != nil {
		if err = d.storeFD("qmp-"+m.id, qmpNode); err == nil {
			var c net.Conn
			if c, err = net.FileConn(qmpNode); err == nil {
				r.q, err = startQMP(ctx, c, true)
			}
		}
		if err != nil {
			d.kill(r)
			return fmt.Errorf("qemu: QMP: %w: %s", err, logTail(logPath))
		}
	}
	for r.q == nil {
		r.q, err = dialQMP(ctx, qnet, qaddr)
		if err == nil {
			break
		}
		select {
		case <-r.exited:
			return fmt.Errorf("qemu exited at startup: %s", logTail(logPath))
		case <-ctx.Done():
			d.kill(r)
			return fmt.Errorf("qemu: no QMP after 30s: %s", logTail(logPath))
		case <-time.After(20 * time.Millisecond):
		}
	}
	if nicNode != nil {
		var c net.Conn
		if err = d.storeFD("net-"+m.id, nicNode); err == nil {
			c, err = net.FileConn(nicNode)
		}
		if err == nil {
			r.net, err = d.attachNet(ctx, m.id, rec.Spec.Net, "", c, gl)
		}
		if err != nil {
			d.kill(r)
			return fmt.Errorf("qemu: guest network: %w: %s", err, logTail(logPath))
		}
	} else if nsock != "" {
		if r.net, err = d.attachNet(ctx, m.id, rec.Spec.Net, nsock, nil, gl); err != nil {
			d.kill(r)
			return fmt.Errorf("qemu: guest network: %w: %s", err, logTail(logPath))
		}
	}
	d.mu.Lock()
	m.rec.Port, m.rec.QMPNet, m.rec.QMPAddr, m.rec.PID = port, qnet, qaddr, cmd.Process.Pid
	m.rec.Net, m.rec.NetSock = d.opt.Net, nsock
	m.rec.LazyGraph = rec.Lazy != nil
	m.rec.StartedAt = time.Now().UTC()
	m.run = r
	m.snapWr = -1
	d.mu.Unlock()
	d.saveRec(m)
	d.setState(m, proto.VMStarting, "")
	d.watch(m, r)
	return nil
}

// attachNet creates the VM's userspace network, connects it to qemu's
// stream netdev socket and forwards gl to the guest agent.
func (d *Driver) attachNet(ctx context.Context, id string, pol *proto.NetPolicy, sock string, conn net.Conn, gl net.Listener) (*netstack.Network, error) {
	sb, err := sandboxPolicy(pol)
	if err != nil {
		return nil, err
	}
	fwd := map[uint16]string{}
	if _, port, err := net.SplitHostPort(d.opt.ServerAddr); err == nil {
		if p, err := strconv.ParseUint(port, 10, 16); err == nil && p > 0 {
			fwd[uint16(p)] = d.opt.ServerAddr // 10.0.2.2:<server port> -> server
		}
	}
	svc := map[uint16]func(net.Conn){proto.TicketPort: func(c net.Conn) {
		d.mu.Lock()
		var t string
		if m := d.vms[id]; m != nil {
			t = m.ticket
		}
		d.mu.Unlock()
		b, _ := json.Marshal(map[string]string{"ticket": t})
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(append(b, '\n'))
	}}
	n, err := netstack.New(netstack.Config{Rules: d.rules, Sandbox: sb, GatewayForwards: fwd, GatewayServices: svc,
		Logf: func(f string, a ...any) { d.opt.Logf("qemu: "+id+": "+f, a...) }})
	if err != nil {
		return nil, err
	}
	if conn != nil { // the node's end of the NIC socketpair
		n.Attach(conn)
		n.Forward(gl, proto.GuestPort)
		return n, nil
	}
	var dl net.Dialer
	for {
		c, err := dl.DialContext(ctx, "unix", sock)
		if err == nil {
			n.Attach(c)
			n.Forward(gl, proto.GuestPort)
			return n, nil
		}
		select {
		case <-ctx.Done():
			n.Close()
			return nil, fmt.Errorf("connect %s: %w", sock, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// sandboxPolicy converts the server's per-sandbox egress rules.
func sandboxPolicy(p *proto.NetPolicy) (*netstack.Sandbox, error) {
	if p == nil {
		return nil, nil
	}
	allow, err := netstack.ParsePrefixes(p.Allow)
	if err != nil {
		return nil, fmt.Errorf("sandbox net policy: %w", err)
	}
	deny, err := netstack.ParsePrefixes(p.Deny)
	if err != nil {
		return nil, fmt.Errorf("sandbox net policy: %w", err)
	}
	return &netstack.Sandbox{Allow: allow, Deny: deny, NoDNS: p.NoDNS}, nil
}

func (d *Driver) args(rec vmRecord, dir string, port int, netSock string, qmpArgs []string) ([]string, error) {
	esc := func(p string) string { return strings.ReplaceAll(p, ",", ",,") }
	mem := rec.Spec.Size.MemoryMiB
	if mem <= 0 {
		mem = 2048
	}
	display := "none"
	if d.virgl != "" {
		display = "egl-headless,rendernode=" + esc(d.virgl)
	}
	a := []string{
		"-name", esc("crowdvm-" + rec.Spec.SandboxID),
		"-nodefaults", "-no-user-config", "-display", display,
		"-machine", "q35,smm=off,vmport=off",
		"-smp", strconv.Itoa(max(rec.Spec.Size.VCPU, 1)),
		"-m", strconv.Itoa(mem),
	}
	cpu := d.opt.CPU
	switch d.accel {
	case "kvm":
		a = append(a, "-accel", "kvm")
		if cpu == "" {
			cpu = "host" // vmx/svm included when the host has nested KVM (SIZE-014)
			if !rec.Spec.Nested && d.opt.NoNested {
				cpu += ",-vmx,-svm"
			}
		}
	case "whpx":
		a = append(a, "-accel", "whpx,kernel-irqchip=off", "-accel", "tcg")
	default:
		a = append(a, "-accel", "tcg")
	}
	if cpu == "" {
		cpu = "max"
	}
	if rec.Spec.Guest.Channel != nil {
		// A channel guest's reboot is a new boot with a new ticket (relaunch).
		a = append(a, "-action", "reboot=shutdown")
	}
	if d.seccomp {
		a = append(a, "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny")
	}
	netdev := fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:%d", port, proto.GuestPort)
	nic := "virtio-net-pci,netdev=net0"
	if fd, ok := strings.CutPrefix(netSock, "fd:"); ok {
		netdev = "stream,id=net0,server=off,addr.type=fd,addr.str=" + fd
		nic += fmt.Sprintf(",mac=%s,host_mtu=%d", netstack.GuestMAC, netstack.DefaultMTU)
	} else if netSock != "" {
		netdev = "stream,id=net0,server=on,addr.type=unix,addr.path=" + esc(netSock)
		nic += fmt.Sprintf(",mac=%s,host_mtu=%d", netstack.GuestMAC, netstack.DefaultMTU)
	}
	drive := "file=" + esc(d.store.abs(rec.Disk)) + ",if=none,id=disk0,format=qcow2,discard=unmap,detect-zeroes=unmap"
	if rec.Lazy != nil {
		lz, top, err := d.lazyDiskArgs(rec, dir, esc)
		if err != nil {
			return nil, err
		}
		a = append(a, lz...)
		drive += ",backing=" + top
	}
	a = append(a,
		"-cpu", cpu,
		"-drive", drive,
		"-device", "virtio-blk-pci,drive=disk0,bootindex=1",
		"-drive", "file="+esc(filepath.Join(dir, "config.iso"))+",if=none,id=cfg,format=raw,readonly=on",
		"-device", "virtio-blk-pci,drive=cfg",
		"-netdev", netdev,
		"-device", nic,
		"-device", "virtio-rng-pci",
	)
	if len(rec.Spec.GPUDevices) > 0 {
		// VFIO pins all guest RAM: no balloon (QEMU inhibits it anyway).
		a = append(a, gpuArgs(rec.Spec.GPUDevices)...)
	} else {
		a = append(a, "-device", "virtio-balloon-pci,free-page-reporting=on")
	}
	if d.virgl != "" {
		a = append(a, "-device", "virtio-gpu-gl-pci")
	}
	a = append(a,
		"-chardev", "file,id=con0,path="+esc(filepath.Join(dir, "console.log")),
		"-serial", "chardev:con0",
	)
	return append(a, qmpArgs...), nil
}

// createOverlayOn creates rel as an empty overlay backed by the file base
// (relative store paths), the size of base.
func (d *Driver) createOverlayOn(base, rel string) error {
	info, err := d.store.info(d.store.abs(base))
	if err != nil {
		return err
	}
	return d.store.run("create", "-q", "-f", "qcow2", "-u", "-b", d.store.abs(base), "-F", "qcow2",
		d.store.abs(rel), strconv.FormatInt(info.VirtualSize, 10))
}

// watch follows one qemu incarnation: guest readiness, reboots, exit.
func (d *Driver) watch(m *machine, r *run) {
	cancel := r.q.subscribe(func(e qmpEvent) {
		switch e.Name {
		case "SHUTDOWN":
			var data struct {
				Guest  bool
				Reason string
			}
			json.Unmarshal(e.Data, &data)
			if data.Reason == "guest-reset" {
				r.reset.Store(true)
			} else if data.Guest {
				r.poweroff.Store(true)
			}
		case "RESET": // guest reboot: the agent restarts too
			go func() {
				d.mu.Lock()
				cur := m.run == r && m.st.State == proto.VMRunning
				d.mu.Unlock()
				if cur {
					if r.net != nil {
						r.net.CloseGuestConns() // bugs-r13-opus 6: calls in flight to the old guest
					}
					d.setState(m, proto.VMStarting, "")
					d.waitBoot(m, r)
				}
			}()
		}
	})
	go func() {
		<-r.exited
		cancel()
		r.q.close()
		if !d.closed.Load() {
			r.closeNet()
		}
		if r.intended.Load() || d.closed.Load() {
			return
		}
		if r.reset.Load() {
			d.relaunch(m, r)
			return
		}
		msg := "qemu exited unexpectedly"
		if r.poweroff.Load() {
			msg = "guest powered off"
		} else {
			msg += exitDetail(r) // bugs-r13-opus 12
		}
		d.mu.Lock()
		cur := m.run == r && !m.gone
		d.mu.Unlock()
		if cur {
			d.opt.Logf("qemu: %s: %s", m.id, msg)
			d.setState(m, proto.VMError, msg)
		}
	}()
	go d.waitBoot(m, r)
}

// storeFD keeps the node's end of a VM socket in systemd's fd store.
func (d *Driver) storeFD(name string, f *os.File) error {
	if err := d.fds.put(name, f); err != nil {
		return fmt.Errorf("fd store: %w", err)
	}
	return nil
}

func (d *Driver) takeFD(name string) *os.File {
	if d.fds == nil {
		return nil
	}
	return d.fds.take(name)
}

// relaunch starts a channel guest again after it rebooted (qemu runs with
// -action reboot=shutdown). Its channel session lived in RAM, so the new boot
// gets a new ticket. A third reboot within 2 minutes is a boot loop: error.
func (d *Driver) relaunch(m *machine, r *run) {
	if _, err := d.lock(m.id); err != nil {
		return
	}
	defer m.op.Unlock()
	now := time.Now()
	d.mu.Lock()
	cur := m.run == r
	cfg := m.rec.Spec.Guest
	m.resets = append(slices.DeleteFunc(m.resets, func(t time.Time) bool { return now.Sub(t) > 2*time.Minute }), now)
	loop := len(m.resets) > 2
	d.mu.Unlock()
	if !cur || d.closed.Load() {
		return
	}
	if loop {
		d.opt.Logf("qemu: %s: reboot loop; serial console tail:\n%s", m.id, consoleTail(filepath.Join(d.vmDir(m.id), "console.log"), 4000))
		d.setState(m, proto.VMError, "the machine rebooted 3 times within 2 minutes")
		return
	}
	d.opt.Logf("qemu: %s: guest rebooted, relaunching", m.id)
	d.setState(m, proto.VMStarting, "")
	if err := d.reticket(d.ctx, m, &cfg); err != nil {
		d.setState(m, proto.VMError, err.Error())
		return
	}
	cfg.Boot, cfg.FirstBoot = proto.BootRecover, false
	if err := d.launch(m, cfg); err != nil {
		d.setState(m, proto.VMError, err.Error())
	}
}

// reticket gives a channel guest's next boot a new ticket (its session was
// in the RAM of the previous boot). Legacy guests keep their config.
func (d *Driver) reticket(ctx context.Context, m *machine, cfg *proto.GuestConfig) error {
	if cfg.Channel == nil {
		return nil
	}
	d.mu.Lock()
	fn := d.ticket
	d.mu.Unlock()
	if fn == nil {
		return errors.New("qemu: no way to get a new boot ticket")
	}
	ch, err := fn(ctx, m.id)
	if err != nil {
		return fmt.Errorf("new boot ticket: %w", err)
	}
	cfg.Channel = ch
	d.mu.Lock()
	m.rec.Spec.Guest = cfg.WithoutTicket()
	d.mu.Unlock()
	d.saveRec(m)
	return nil
}

func (d *Driver) waitBoot(m *machine, r *run) {
	ctx, cancel := context.WithTimeout(d.ctx, d.opt.BootTimeout)
	defer cancel()
	err := d.guest(m).WaitHealthy(ctx, r.exited)
	d.mu.Lock()
	cur := m.run == r && m.st.State == proto.VMStarting && !m.gone
	d.mu.Unlock()
	if !cur || !r.alive() || d.closed.Load() {
		return
	}
	if err == nil {
		d.opt.Logf("qemu: %s guest healthy %s after launch", m.id, time.Since(r.launched).Round(time.Millisecond))
		d.setState(m, proto.VMRunning, "")
		return
	}
	d.opt.Logf("qemu: %s boot timeout; serial console tail:\n%s", m.id, consoleTail(filepath.Join(d.vmDir(m.id), "console.log"), 4000))
	d.setState(m, proto.VMError, fmt.Sprintf("boot timeout: guest agent not healthy after %s", d.opt.BootTimeout))
	d.kill(r)
}

// kill makes qemu exit: QMP stop (vCPUs halt at once) and quit (clean
// qcow2 close), then SIGKILL. The guest OS is not shut down.
func (d *Driver) kill(r *run) {
	r.intended.Store(true)
	if !r.alive() {
		return
	}
	if r.q != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		r.q.cmd(ctx, "stop", nil, nil)
		r.q.cmd(ctx, "quit", nil, nil)
		cancel()
		select {
		case <-r.exited:
			r.closeNet()
			return
		case <-time.After(5 * time.Second):
		}
	}
	r.proc.Kill()
	select {
	case <-r.exited:
	case <-time.After(10 * time.Second):
	}
	r.closeNet()
}

// adoptAll picks up VMs left by a previous node process: live qemu
// processes are adopted, dead ones become error with the overlay kept.
func (d *Driver) adoptAll() {
	ents, _ := os.ReadDir(filepath.Join(d.opt.DataDir, "vms"))
	for _, e := range ents {
		dir := filepath.Join(d.opt.DataDir, "vms", e.Name())
		var rec vmRecord
		b, err := os.ReadFile(filepath.Join(dir, "vm.json"))
		if err == nil {
			err = json.Unmarshal(b, &rec)
		}
		if err != nil || rec.Spec.SandboxID != e.Name() || rec.Disk == "" {
			os.RemoveAll(dir)
			continue
		}
		m := &machine{id: rec.Spec.SandboxID, rec: rec, st: proto.VMStatus{SandboxID: rec.Spec.SandboxID, StartedAt: rec.StartedAt}, snapWr: -1}
		d.vms[m.id] = m
		ours := rec.PID > 0 && pidIsOurs(rec.PID, dir)
		q, err := d.dialAdopt(rec, ours)
		if err != nil {
			if ours { // alive but unreachable: hung
				if p, err := os.FindProcess(rec.PID); err == nil {
					p.Kill()
				}
			}
			m.st.State, m.st.Error = proto.VMError, "qemu not running after node restart"
			d.opt.Logf("qemu: %s: %s (overlay kept)", m.id, m.st.Error)
			continue
		}
		proc, _ := os.FindProcess(rec.PID)
		r := &run{q: q, proc: proc, exited: make(chan struct{}), launched: time.Now(), log: filepath.Join(dir, "qemu.log")}
		go func() { <-q.done; close(r.exited) }()
		if rec.Net == NetNetstack {
			if err := d.adoptNet(m, r); err != nil {
				d.kill(r)
				m.st.State, m.st.Error = proto.VMError, "guest network not reattached after node restart: "+err.Error()
				d.opt.Logf("qemu: %s: %s (overlay kept)", m.id, m.st.Error)
				continue
			}
		} else if d.opt.Net == NetNetstack {
			d.opt.Logf("qemu: WARNING: adopted VM %s still uses slirp networking until it restarts", m.id)
		}
		d.reconcileDisk(m, q)
		m.run = r
		m.st.State = proto.VMStarting
		d.opt.Logf("qemu: adopted running VM %s (pid %d)", m.id, rec.PID)
		d.watch(m, r)
	}
}

// adoptNet gives an adopted netstack VM a new network (guest connections
// through the old one are lost) and a new guest agent port.
func (d *Driver) adoptNet(m *machine, r *run) error {
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	var conn net.Conn
	if strings.HasPrefix(m.rec.NetSock, "fd:") {
		f := d.takeFD("net-" + m.id)
		if f == nil {
			gl.Close()
			return errors.New("its NIC socket was not passed back (systemd fd store)")
		}
		conn, err = net.FileConn(f)
		f.Close()
		if err != nil {
			gl.Close()
			return err
		}
	}
	if r.net, err = d.attachNet(ctx, m.id, m.rec.Spec.Net, m.rec.NetSock, conn, gl); err != nil {
		gl.Close()
		return err
	}
	m.rec.Port = gl.Addr().(*net.TCPAddr).Port
	writeJSON(filepath.Join(d.vmDir(m.id), "vm.json"), m.rec)
	return nil
}

// dialAdopt connects to a surviving qemu's QMP socket. A qemu known to be
// alive (ours) gets up to 10s, as a loaded host can be slow to answer.
func (d *Driver) dialAdopt(rec vmRecord, ours bool) (*qmp, error) {
	if rec.QMPNet == "fd" {
		f := d.takeFD("qmp-" + rec.Spec.SandboxID)
		if f == nil {
			return nil, errors.New("its QMP socket was not passed back (systemd fd store)")
		}
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		defer cancel()
		return startQMP(ctx, c, false)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
		q, err := dialQMP(ctx, rec.QMPNet, rec.QMPAddr)
		cancel()
		if err == nil || !ours || time.Now().After(deadline) {
			return q, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// reconcileDisk fixes vm.json when the node died between a live snapshot and
// recording it: qemu's active overlay is the truth.
func (d *Driver) reconcileDisk(m *machine, q *qmp) {
	var blocks []struct {
		Device   string `json:"device"`
		Inserted *struct {
			File string `json:"file"`
		} `json:"inserted"`
	}
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	defer cancel()
	if q.cmd(ctx, "query-block", nil, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Device != "disk0" || b.Inserted == nil || sameFile(b.Inserted.File, d.store.abs(m.rec.Disk)) {
			continue
		}
		rel, err := filepath.Rel(d.opt.DataDir, b.Inserted.File)
		if err != nil {
			return
		}
		m.rec.Parent = d.hideLayer(m.rec.Disk, m.rec.Parent, m.rec.ImageID)
		m.rec.Disk = filepath.ToSlash(rel)
		writeJSON(filepath.Join(d.vmDir(m.id), "vm.json"), m.rec)
	}
}

func (d *Driver) inUse() (parents, disks []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.vms {
		if m.rec.Parent != "" {
			parents = append(parents, m.rec.Parent)
		}
		if m.rec.Disk != "" {
			disks = append(disks, m.rec.Disk)
		}
	}
	return
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func findBinaries(qemuBin, qimg string) (string, string, error) {
	find := func(given, name, sibling string) (string, error) {
		if given != "" {
			return given, nil
		}
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
		dirs := []string{}
		if sibling != "" {
			dirs = append(dirs, filepath.Dir(sibling))
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			dirs = append(dirs, filepath.Join(pf, "qemu"))
		}
		for _, dir := range dirs {
			if p := filepath.Join(dir, name); fileSize(p) > 0 {
				return p, nil
			}
		}
		return "", fmt.Errorf("qemu: %s not found", name)
	}
	q, err1 := find(qemuBin, "qemu-system-x86_64", qimg)
	i, err2 := find(qimg, "qemu-img", q)
	if err1 != nil {
		return "", "", err1
	}
	return q, i, err2
}

// unixSocket returns a socket path in dir, or in the temp dir when dir is
// too long for sun_path (108 bytes on Linux and Windows).
func unixSocket(dir, name string) string {
	p := filepath.Join(dir, name)
	if len(p) > 100 {
		sum := sha256.Sum256([]byte(dir))
		p = filepath.Join(os.TempDir(), fmt.Sprintf("crowdvm-%x-%s", sum[:8], name))
	}
	return p
}

func roundUp(n, unit int64) int64 { return (n + unit - 1) / unit * unit }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// consoleTail returns the last n bytes of the serial console log, without
// carriage returns and blank lines.
func consoleTail(path string, n int) string {
	b, _ := os.ReadFile(path)
	if len(b) > n {
		b = b[len(b)-n:]
	}
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// logTail returns the end of the last launch's qemu output.
// exitDetail describes how a run ended: its exit status or signal, then a tail
// of what qemu itself wrote. bugs-r13-opus 12: qemu also prints "Formatting"
// lines for the overlays it creates, and its log names the node's paths; the
// sandbox owner sees neither.
func exitDetail(r *run) string {
	var parts []string
	if r.waited { // adopted runs have no wait status
		s := "exit status 0"
		if r.waitErr != nil {
			s = r.waitErr.Error()
		}
		if strings.HasPrefix(s, "signal: ") {
			s = "killed by " + s
		}
		parts = append(parts, s)
	}
	if t := cleanLogTail(r.log); t != "" {
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, ": ")
}

var pathRe = regexp.MustCompile(`(/[\w.@+=-]+){2,}`)

// cleanLogTail is logTail without the overlay "Formatting" lines and with
// absolute paths replaced.
func cleanLogTail(path string) string {
	var keep []string
	for _, l := range strings.Split(string(lastLaunchLog(path)), "\n") {
		if strings.Contains(l, "Formatting '") || strings.Contains(l, "backing_file=") {
			continue
		}
		keep = append(keep, pathRe.ReplaceAllString(l, "<path>"))
	}
	s := strings.TrimSpace(strings.Join(keep, "\n"))
	if len(s) > 800 {
		s = strings.TrimSpace(s[len(s)-800:])
	}
	return s
}

// lastLaunchLog is the log of the latest launch, without its "--- launch" line.
func lastLaunchLog(path string) []byte {
	b, _ := os.ReadFile(path)
	if i := bytes.LastIndex(b, []byte("--- launch ")); i >= 0 {
		b = b[i:]
		if j := bytes.IndexByte(b, '\n'); j >= 0 {
			b = b[j+1:]
		}
	}
	return b
}

func logTail(path string) string {
	b := lastLaunchLog(path)
	if len(b) > 800 {
		b = b[len(b)-800:]
	}
	return strings.TrimSpace(string(b))
}

// trimTail keeps only the last max bytes of file path.
func trimTail(path string, max int64) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= max {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	buf := make([]byte, max)
	n, _ := f.ReadAt(buf, st.Size()-max)
	f.Close()
	_ = os.WriteFile(path, buf[:n], 0o600)
}
