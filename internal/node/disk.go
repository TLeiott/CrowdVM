package node

import (
	"io/fs"
	"path/filepath"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/bench"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Disk accounting with thin provisioning. A sandbox's disk size is a limit,
// not an allocation: its qcow2 overlay only holds what it writes on top of
// the shared base image. So the node
//   - advertises a virtual disk capacity of budget × overcommit, which the
//     server's scheduler sums sandbox sizes against (a 51 GiB "small"
//     sandbox does not need 51 GiB free), and
//   - admits a VM only while real usage leaves headroom: the node's data
//     stays within the budget and the host keeps `disk-reserve` GiB free.
//     When that headroom is gone it advertises no free disk at all.
//
// budget = config `disk` (GiB), or 90% of (free space + what the node
// already stores). A VM that fills the host anyway is paused by QEMU
// (ENOSPC), not corrupted.

const (
	defaultDiskOvercommit = 4.0
	defaultDiskReserveGiB = 10
	minDiskHeadroom       = 2 << 30 // real space a new VM needs at least
	diskStateTTL          = 10 * time.Second
)

// DiskState is the node's real disk accounting (status UI, admission).
type DiskState struct {
	UsedBytes     int64   `json:"usedBytes"` // node data dir: images, snapshots, overlays
	FreeBytes     int64   `json:"freeBytes"` // free on the data dir's filesystem
	BudgetGiB     int     `json:"budgetGiB"` // real disk the owner shares
	ReserveGiB    int     `json:"reserveGiB"`
	Overcommit    float64 `json:"overcommit"`
	HeadroomBytes int64   `json:"headroomBytes"` // real space left for VM growth
}

// Disk returns the current disk accounting (cached for a few seconds).
func (n *Node) Disk() DiskState {
	n.diskMu.Lock()
	defer n.diskMu.Unlock()
	if !n.diskAt.IsZero() && time.Since(n.diskAt) < diskStateTTL {
		return n.disk
	}
	c := n.config()
	used := dirSize(c.DataDir)
	free, _ := bench.DataDirFree(c.DataDir)
	ds := DiskState{UsedBytes: used, FreeBytes: int64(free), ReserveGiB: c.DiskReserve(), Overcommit: c.Overcommit()}
	budget := int64(c.DiskGiB) << 30
	if budget <= 0 {
		budget = (ds.FreeBytes + used) / 10 * 9
	}
	ds.BudgetGiB = int(budget >> 30)
	ds.HeadroomBytes = max(0, min(budget-used, ds.FreeBytes-int64(ds.ReserveGiB)<<30))
	n.disk, n.diskAt = ds, time.Now()
	return ds
}

// capacity is the capacity advertised to the server: the owner's shares,
// with the disk as thin-provisioned virtual capacity (see above).
func (n *Node) capacity() (proto.Capacity, DiskState) {
	cp := n.config().Capacity()
	ds := n.Disk()
	cp.DiskGiB = int(float64(ds.BudgetGiB) * ds.Overcommit)
	if ds.HeadroomBytes < minDiskHeadroom {
		cp.DiskGiB = min(cp.DiskGiB, n.Usage().DiskGiB) // full: place nothing new here
	}
	return cp, ds
}

// dirSize sums the sizes of the files under dir (sparse files count in
// full, which errs on the safe side).
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
