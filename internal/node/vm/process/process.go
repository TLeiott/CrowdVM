// Package process is a VM driver without isolation, for tests and
// development. It runs crowdvm-guest as a host process
// (--config FILE --home DIR --unprivileged), and a snapshot is a copy of the
// sandbox home directory (reflinked where the filesystem supports it).
// Snapshots upload and download as tar files. Base images are not needed:
// any image id is accepted.
package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/node/vm/qemu/guestconn"
	"github.com/crowdvm/crowdvm/internal/proto"
)

type Options struct {
	DataDir            string        // required
	GuestBinary        string        // crowdvm-guest for the host OS (required to Start)
	BootTimeout        time.Duration // launch -> guest healthy (default 60s)
	StopTimeout        time.Duration // SIGTERM grace before SIGKILL (default 10s)
	PrepareStopTimeout time.Duration // default 60s
	Logf               func(format string, args ...any)
}

type Driver struct {
	opt     Options
	st      *store
	ctx     context.Context
	cancel  context.CancelFunc
	closed  atomic.Bool
	nmu     sync.Mutex
	mu      sync.Mutex
	vms     map[string]*sandbox
	changed chan struct{}
	notify  func(proto.VMStatus)
}

var (
	_ vm.Driver   = (*Driver)(nil)
	_ vm.Dialer   = (*Driver)(nil)
	_ vm.Waiter   = (*Driver)(nil)
	_ vm.Notifier = (*Driver)(nil)

	_ vm.ChangeTracker = (*Driver)(nil)
)

type record struct {
	Spec      proto.VMSpec `json:"spec"`
	Port      int          `json:"port"`
	PID       int          `json:"pid"`
	StartedAt time.Time    `json:"startedAt"`
}

type sandbox struct {
	id string
	op sync.Mutex
	// guarded by Driver.mu:
	rec  record
	st   proto.VMStatus
	run  *run
	gone bool
	// stamp of the home tree at the last live snapshot ("" = unknown)
	stamp string
}

type run struct {
	proc     *os.Process
	exited   chan struct{}
	intended atomic.Bool
	launched time.Time
}

func (r *run) alive() bool {
	select {
	case <-r.exited:
		return false
	default:
		return true
	}
}

func New(opt Options) (*Driver, error) {
	if opt.DataDir == "" {
		return nil, errors.New("process: DataDir required")
	}
	if opt.BootTimeout <= 0 {
		opt.BootTimeout = time.Minute
	}
	if opt.StopTimeout <= 0 {
		opt.StopTimeout = 10 * time.Second
	}
	if opt.PrepareStopTimeout <= 0 {
		opt.PrepareStopTimeout = time.Minute
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	st, err := openStore(opt.DataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opt.DataDir, "vms"), 0o700); err != nil {
		return nil, err
	}
	d := &Driver{opt: opt, st: st, vms: map[string]*sandbox{}, changed: make(chan struct{})}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.adoptAll()
	return d, nil
}

func (d *Driver) Name() string                      { return "process" }
func (d *Driver) Store() vm.Store                   { return d.st }
func (d *Driver) SetNotify(fn func(proto.VMStatus)) { d.mu.Lock(); d.notify = fn; d.mu.Unlock() }

// Close stops watching. Guest processes keep running; a new Driver adopts them.
func (d *Driver) Close() error {
	d.closed.Store(true)
	d.cancel()
	return nil
}

func (d *Driver) dir(id string) string  { return filepath.Join(d.opt.DataDir, "vms", id) }
func (d *Driver) home(id string) string { return filepath.Join(d.dir(id), "home") }

func (d *Driver) Start(ctx context.Context, spec proto.VMSpec) error {
	id := spec.SandboxID
	if err := checkID(id); err != nil {
		return err
	}
	if _, err := os.Stat(d.opt.GuestBinary); d.opt.GuestBinary == "" || err != nil {
		return fmt.Errorf("process: guest binary %q: %v", d.opt.GuestBinary, err)
	}
	d.mu.Lock()
	if d.vms[id] != nil {
		d.mu.Unlock()
		return fmt.Errorf("%s: %w", id, vm.ErrExists)
	}
	s := &sandbox{id: id, rec: record{Spec: spec}, st: proto.VMStatus{SandboxID: id, State: proto.VMStarting}}
	d.vms[id] = s
	s.op.Lock()
	d.mu.Unlock()
	defer s.op.Unlock()
	err := os.MkdirAll(d.dir(id), 0o700)
	if err == nil && spec.ParentID != "" {
		src, ok := d.st.snapDir(spec.ParentID)
		if !ok {
			err = fmt.Errorf("snapshot %q: %w", spec.ParentID, vm.ErrNoParent)
		} else {
			err = copyTree(src, d.home(id))
		}
	} else if err == nil {
		err = os.MkdirAll(d.home(id), 0o755)
	}
	if err == nil {
		err = d.launch(s, spec.Guest)
	}
	if err != nil {
		d.remove(s)
	}
	return err
}

func (d *Driver) launch(s *sandbox, cfg proto.GuestConfig) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", port)
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(d.dir(s.id), "config.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(d.dir(s.id), "guest.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(d.opt.GuestBinary, "--config", cfgPath, "--home", d.home(s.id), "--unprivileged")
	cmd.Dir = d.home(s.id)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = sysProcAttr()
	err = cmd.Start()
	logf.Close()
	if err != nil {
		return err
	}
	r := &run{proc: cmd.Process, exited: make(chan struct{}), launched: time.Now()}
	go func() { cmd.Wait(); close(r.exited) }()
	d.mu.Lock()
	s.rec.Port, s.rec.PID, s.rec.StartedAt = port, cmd.Process.Pid, time.Now().UTC()
	s.run = r
	d.mu.Unlock()
	d.saveRec(s)
	d.setState(s, proto.VMStarting, "")
	d.watch(s, r)
	return nil
}

func (d *Driver) watch(s *sandbox, r *run) {
	go func() {
		<-r.exited
		if r.intended.Load() || d.closed.Load() {
			return
		}
		d.mu.Lock()
		cur := s.run == r && !s.gone
		d.mu.Unlock()
		if cur {
			d.setState(s, proto.VMError, "guest process exited")
		}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, d.opt.BootTimeout)
		defer cancel()
		err := d.guest(s).WaitHealthy(ctx, r.exited)
		d.mu.Lock()
		cur := s.run == r && s.st.State == proto.VMStarting && !s.gone
		d.mu.Unlock()
		if !cur || !r.alive() || d.closed.Load() {
			return
		}
		if err == nil {
			d.setState(s, proto.VMRunning, "")
			return
		}
		d.setState(s, proto.VMError, "boot timeout: guest agent not healthy after "+d.opt.BootTimeout.String())
		d.kill(r)
	}()
}

// kill terminates the guest process group: SIGTERM, then SIGKILL.
func (d *Driver) kill(r *run) {
	r.intended.Store(true)
	if r.alive() {
		terminate(r.proc)
		select {
		case <-r.exited:
		case <-time.After(d.opt.StopTimeout):
		}
	}
	killGroup(r.proc)
	select {
	case <-r.exited:
	case <-time.After(5 * time.Second):
	}
}

func (d *Driver) Stop(ctx context.Context, id, snapID string, force bool) (proto.SnapshotRef, error) {
	if snapID != "" {
		if err := checkID(snapID); err != nil {
			return proto.SnapshotRef{}, err
		}
		if d.st.HasSnapshot(snapID) {
			return proto.SnapshotRef{}, fmt.Errorf("snapshot %s already exists", snapID)
		}
	}
	s, err := d.lock(id)
	if err != nil {
		return proto.SnapshotRef{}, err
	}
	defer s.op.Unlock()
	d.mu.Lock()
	r, ready := s.run, s.st.GuestReady
	d.mu.Unlock()
	d.setState(s, proto.VMStopping, "")
	if r != nil {
		if !force && ready {
			if err := d.guest(s).Post(ctx, "/v1/internal/prepare-stop", nil, d.opt.PrepareStopTimeout); err != nil {
				d.opt.Logf("process: %s prepare-stop: %v", id, err)
			}
		}
		d.kill(r)
	}
	var ref proto.SnapshotRef
	if snapID != "" {
		ref, err = d.st.add(id, snapID, s.rec.Spec.ParentID, s.rec.Spec.ImageID, d.home(id), true)
		if err != nil {
			d.setState(s, proto.VMError, "stop: "+err.Error())
			return ref, err
		}
	}
	d.remove(s)
	d.setState(s, proto.VMStopped, "")
	return ref, nil
}

func (d *Driver) Snapshot(ctx context.Context, id, snapID string) (proto.SnapshotRef, error) {
	if err := checkID(snapID); err != nil {
		return proto.SnapshotRef{}, err
	}
	if d.st.HasSnapshot(snapID) {
		return proto.SnapshotRef{}, fmt.Errorf("snapshot %s already exists", snapID)
	}
	s, err := d.lock(id)
	if err != nil {
		return proto.SnapshotRef{}, err
	}
	defer s.op.Unlock()
	d.mu.Lock()
	r, ready, parent := s.run, s.st.GuestReady, s.rec.Spec.ParentID
	d.mu.Unlock()
	if r == nil || !r.alive() {
		return proto.SnapshotRef{}, fmt.Errorf("%s: %w", id, vm.ErrNotRunning)
	}
	if ready {
		if err := d.guest(s).Post(ctx, "/v1/internal/sync", nil, time.Minute); err != nil {
			d.opt.Logf("process: %s sync: %v", id, err)
		}
	}
	stamp := treeStamp(d.home(id))
	ref, err := d.st.add(id, snapID, parent, s.rec.Spec.ImageID, d.home(id), false)
	if err != nil {
		return ref, err
	}
	d.mu.Lock()
	s.rec.Spec.ParentID = snapID // next snapshot's parent
	s.stamp = stamp
	d.mu.Unlock()
	d.saveRec(s)
	return ref, nil
}

// DiskChanged compares the home tree (file count, sizes, newest mtime) with
// its state at the last live snapshot (vm.ChangeTracker).
func (d *Driver) DiskChanged(id string) (bool, error) {
	d.mu.Lock()
	s := d.vms[id]
	d.mu.Unlock()
	if s == nil {
		return false, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	d.mu.Lock()
	last := s.stamp
	d.mu.Unlock()
	return last == "" || treeStamp(d.home(id)) != last, nil
}

func (d *Driver) Restart(ctx context.Context, id string) error {
	s, err := d.lock(id)
	if err != nil {
		return err
	}
	defer s.op.Unlock()
	d.mu.Lock()
	r, cfg := s.run, s.rec.Spec.Guest
	d.mu.Unlock()
	if r != nil {
		d.kill(r)
	}
	cfg.Boot, cfg.FirstBoot = proto.BootRecover, false
	if err := d.launch(s, cfg); err != nil {
		d.setState(s, proto.VMError, err.Error())
		return err
	}
	return nil
}

func (d *Driver) Destroy(ctx context.Context, id string) error {
	s, err := d.lock(id)
	if errors.Is(err, vm.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	defer s.op.Unlock()
	d.mu.Lock()
	r := s.run
	d.mu.Unlock()
	if r != nil {
		d.kill(r)
	}
	d.remove(s)
	return nil
}

func (d *Driver) Status(id string) (proto.VMStatus, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.vms[id]; s != nil {
		return s.st, true
	}
	return proto.VMStatus{}, false
}

func (d *Driver) List() []proto.VMStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]proto.VMStatus, 0, len(d.vms))
	for _, s := range d.vms {
		out = append(out, s.st)
	}
	return out
}

func (d *Driver) GuestAddr(id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.vms[id]
	if s == nil {
		return "", fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	if s.run == nil || !s.run.alive() {
		return "", fmt.Errorf("%s: %w", id, vm.ErrNotRunning)
	}
	return fmt.Sprintf("127.0.0.1:%d", s.rec.Port), nil
}

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

func (d *Driver) Wait(ctx context.Context, id string) error {
	for {
		d.mu.Lock()
		s, ch := d.vms[id], d.changed
		var st proto.VMStatus
		if s != nil {
			st = s.st
		}
		d.mu.Unlock()
		switch {
		case s == nil:
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

// adoptAll picks up sandboxes of a previous node process: still-healthy
// guests are adopted; others become error with their home kept.
func (d *Driver) adoptAll() {
	ents, _ := os.ReadDir(filepath.Join(d.opt.DataDir, "vms"))
	for _, e := range ents {
		var rec record
		b, err := os.ReadFile(filepath.Join(d.dir(e.Name()), "vm.json"))
		if err == nil {
			err = json.Unmarshal(b, &rec)
		}
		if err != nil || rec.Spec.SandboxID != e.Name() {
			os.RemoveAll(d.dir(e.Name()))
			continue
		}
		s := &sandbox{id: rec.Spec.SandboxID, rec: rec, st: proto.VMStatus{SandboxID: rec.Spec.SandboxID, StartedAt: rec.StartedAt}}
		d.vms[s.id] = s
		ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
		healthy := d.guest(s).Healthy(ctx)
		cancel()
		proc, perr := os.FindProcess(rec.PID)
		if !healthy || perr != nil {
			if perr == nil && pidIsOurs(rec.PID, d.dir(s.id)) { // hung; never kill a reused pid
				killGroup(proc)
			}
			s.st.State, s.st.Error = proto.VMError, "guest not running after node restart"
			continue
		}
		r := &run{proc: proc, exited: make(chan struct{}), launched: time.Now()}
		go func() { waitExit(proc, d.ctx.Done()); close(r.exited) }() // not our child
		s.run = r
		s.st.State = proto.VMStarting
		d.watch(s, r)
	}
}

func (d *Driver) lock(id string) (*sandbox, error) {
	d.mu.Lock()
	s := d.vms[id]
	d.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	s.op.Lock()
	d.mu.Lock()
	gone := s.gone
	d.mu.Unlock()
	if gone {
		s.op.Unlock()
		return nil, fmt.Errorf("%s: %w", id, vm.ErrNotFound)
	}
	return s, nil
}

func (d *Driver) remove(s *sandbox) {
	d.mu.Lock()
	s.gone = true
	if d.vms[s.id] == s {
		delete(d.vms, s.id)
	}
	d.mu.Unlock()
	removeAll(d.dir(s.id))
}

func (d *Driver) guest(s *sandbox) guestconn.Guest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return guestconn.Guest{Addr: fmt.Sprintf("127.0.0.1:%d", s.rec.Port), Token: s.rec.Spec.Guest.Token}
}

func (d *Driver) saveRec(s *sandbox) {
	d.mu.Lock()
	rec := s.rec
	d.mu.Unlock()
	writeJSON(filepath.Join(d.dir(s.id), "vm.json"), rec)
}

func (d *Driver) setState(s *sandbox, state, msg string) {
	d.mu.Lock()
	s.st.State, s.st.Error = state, msg
	s.st.GuestReady = state == proto.VMRunning
	s.st.StartedAt = s.rec.StartedAt
	st, fn := s.st, d.notify
	close(d.changed)
	d.changed = make(chan struct{})
	d.nmu.Lock()
	d.mu.Unlock()
	defer d.nmu.Unlock()
	if fn != nil && !d.closed.Load() {
		fn(st)
	}
}

func checkID(id string) error {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, `/\:*?"<>|`) || id[0] == '.' {
		return fmt.Errorf("invalid id %q", id)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
