// Package vm defines the VM driver interface used by the node daemon.
// Implementations: internal/node/vm/qemu (real VMs, Linux KVM / Windows WHPX / TCG)
// and internal/node/vm/process (no isolation: runs crowdvm-guest as a host
// process with a fake root; for tests and development only).
package vm

import (
	"context"
	"errors"
	"net"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Driver manages VMs and their disk snapshot chains on one node.
//
// Disk model: every VM runs on a writable overlay on top of an immutable chain
// snapshot -> ... -> snapshot -> base image. Stop/Snapshot turn the current
// overlay into an immutable snapshot (id chosen by the server) and, for live
// snapshots, continue on a fresh overlay. Snapshots and images are files in the
// node's store; the node uploads/downloads them to/from the server by id.
type Driver interface {
	Name() string // "qemu" | "process"

	// Start boots a VM. It returns once the hypervisor is launched; Wait/Status
	// report guest readiness. spec.ParentID ("" = base image) and every ancestor
	// must be present locally (see Store).
	Start(ctx context.Context, spec proto.VMSpec) error
	// Stop runs the guest's prepare-stop (unless force: hung guest, or the
	// caller already ran it), halts the VM without an OS shutdown, then
	// freezes the overlay as snapshot snapID ("" = discard the disk).
	// Returns the new snapshot.
	Stop(ctx context.Context, sandboxID, snapID string, force bool) (proto.SnapshotRef, error)
	// Snapshot takes a live snapshot; the VM keeps running on a new overlay.
	Snapshot(ctx context.Context, sandboxID, snapID string) (proto.SnapshotRef, error)
	// Restart hard-restarts a hung VM on the same overlay (recover).
	Restart(ctx context.Context, sandboxID string) error
	// Destroy kills the VM (if running) and deletes its overlay.
	Destroy(ctx context.Context, sandboxID string) error
	Status(sandboxID string) (proto.VMStatus, bool)
	List() []proto.VMStatus

	// GuestAddr is the host-side address of the guest agent (e.g. 127.0.0.1:41234).
	GuestAddr(sandboxID string) (string, error)

	Store() Store
	Close() error
}

// Store holds base images and snapshots (immutable disk layers) on the node.
type Store interface {
	HasImage(id string) bool
	HasSnapshot(id string) bool
	// ImportImage/ImportSnapshot move a downloaded file into the store. For a
	// snapshot the parent chain must already be present; the store rebases it.
	ImportImage(id string, path string) error
	ImportSnapshot(ref proto.SnapshotRef, path string) error
	// SnapshotPath / ImagePath return the file to upload.
	SnapshotPath(id string) (string, error)
	ImagePath(id string) (string, error)
	SnapshotRef(id string) (proto.SnapshotRef, bool)
	DeleteSnapshot(id string) error
	Images() []string
	// Snapshots lists the snapshots in the store (not deleted ones), with
	// their sandbox when known.
	Snapshots() []proto.SnapshotRef
}

// Compactor is optionally implemented by Stores whose snapshot files can be
// sparse (a guest's discards punch holes in them). CompactSnapshot writes a
// copy of snapshot id into dir that holds only its allocated data, with the
// same guest view and backing reference, and returns its path; "" when the
// file is not sparse. The caller removes the copy.
type Compactor interface {
	CompactSnapshot(id, dir string) (string, error)
}

// Dialer is optionally implemented by drivers that can reach guest ports
// without going through the guest agent tunnel.
type Dialer interface {
	DialGuest(ctx context.Context, sandboxID string, port int) (net.Conn, error)
}

// NICDialer is optionally implemented by drivers whose guests have a NIC
// the node can reach directly (qemu with netstack networking). network is
// "tcp" or "udp"; the guest sees the connection come from its gateway, so
// its firewall applies (direct ports, URL-012). Errors wrapping ErrNoNIC
// mean the VM has no such NIC (e.g. slirp): use the agent tunnel for TCP.
type NICDialer interface {
	DialGuestNIC(ctx context.Context, sandboxID, network string, port int) (net.Conn, error)
}

// LazyStarter is optionally implemented by drivers that can boot a VM
// before its snapshot chain is local (LIFE-013 lazy chain fetch): layers
// not in the Store are read on demand from a URL (HTTP Range requests)
// and the data read is cached next to the VM, until the node has imported
// every layer and calls Hydrated.
type LazyStarter interface {
	// LazyChain reports whether lazy boots work here (e.g. qemu with its
	// http block driver).
	LazyChain() bool
	// StartLazy is Start for a spec whose snapshot chain is not fully in
	// the Store. chain lists every layer from spec.ParentID down (top
	// first); remote maps the id of each layer that is not in the Store to
	// its URL. The base image must be in the Store.
	StartLazy(ctx context.Context, spec proto.VMSpec, chain []proto.SnapshotRef, remote map[string]string) error
	// Hydrated switches a running VM from the URLs to the Store's files
	// once every layer of its chain is there.
	Hydrated(ctx context.Context, sandboxID string) error
	// Lazy reports whether the VM still reads layers from URLs (e.g. after
	// a node restart adopted it).
	Lazy(sandboxID string) bool
}

// Renamer is optionally implemented by drivers that can hand a running VM
// to another id (node warm pool, SIZE-012): the VM keeps running, its
// record becomes spec (used for later restarts and config drives).
type Renamer interface {
	Rename(ctx context.Context, fromID string, spec proto.VMSpec) error
}

// Waiter is implemented by drivers whose Start returns before the guest is
// ready (qemu, process). Wait blocks until the VM is running (nil), failed or
// gone (error), or ctx ends.
type Waiter interface {
	Wait(ctx context.Context, sandboxID string) error
}

// Notifier is implemented by drivers that push VM state changes (starting,
// running, stopping, stopped, error), e.g. to emit vm.state events. fn must not
// block; set it before the first Start.
type Notifier interface {
	SetNotify(fn func(proto.VMStatus))
}

// Reticketer is implemented by drivers that relaunch a channel guest after
// it rebooted. Its session lived in RAM, so the relaunch needs a new boot
// ticket from the server; fn gets one. Set it before the first Start.
type Reticketer interface {
	SetReticket(fn func(ctx context.Context, sandboxID string) (*proto.GuestChannel, error))
}

type noQuiesceKey struct{}

// WithoutQuiesce marks ctx: a live Snapshot skips the guest freeze and sync
// (a crash-consistent snapshot) because the guest did not answer them before
// (bugs-r11-opus 7).
func WithoutQuiesce(ctx context.Context) context.Context {
	return context.WithValue(ctx, noQuiesceKey{}, true)
}

// NoQuiesce reports whether ctx came from WithoutQuiesce.
func NoQuiesce(ctx context.Context) bool {
	v, _ := ctx.Value(noQuiesceKey{}).(bool)
	return v
}

// ChangeTracker is optionally implemented by drivers that know whether a
// VM's disk was written since its last snapshot (true when unknown);
// periodic snapshots skip VMs whose disk did not change.
type ChangeTracker interface {
	DiskChanged(sandboxID string) (bool, error)
}

// Errors returned by drivers (wrapped; test with errors.Is).
var (
	ErrNotFound   = errors.New("vm not found")
	ErrExists     = errors.New("vm already exists")
	ErrNotRunning = errors.New("vm not running")
	ErrNoParent   = errors.New("snapshot parent chain not present")
	ErrNoNIC      = errors.New("vm has no node-reachable NIC")
)
