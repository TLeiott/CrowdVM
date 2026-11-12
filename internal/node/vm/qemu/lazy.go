package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Lazy chain fetch (LIFE-013). A VM whose snapshot chain is not fully on
// the node boots at once on an explicit -blockdev graph:
//
//	overlay (disk0) -> [layers snapshotted during this run] -> lz-cor
//	lz-cor  = copy-on-read filter over lz-cache (bottom = lowest remote layer)
//	lz-cache (vms/<id>/lazy-cache.qcow2) -> lz0 -> lz1 -> ... -> lz-img
//	lzN = qcow2 over a local file, or over QEMU's http driver reading the
//	      node's blob proxy (Range requests to the server's blob store)
//
// Data read from remote layers is copied into lz-cache, so each block
// crosses the network once; the guest's own writes still go to the
// overlay, so snapshots hold only them. The overlay's header names the
// store path the top layer will have (disks/snap-<id>.qcow2). When the
// node has imported every layer it calls Hydrated: each http file node is
// swapped for the local file (blockdev-reopen) and later boots use the
// plain header chain. Live chain compaction (block-stream) is paused
// while the graph has lazy nodes; a stop of a hydrated VM compacts
// offline as usual.

type lazyLayer struct {
	ID   string `json:"id"`
	File string `json:"file"`          // store path, relative (present or future)
	URL  string `json:"url,omitempty"` // read from here until hydrated
}

var formatsRe = regexp.MustCompile(`(?s)Supported (?:image )?formats:(.*)`)

// detectLazy reports whether qemu-img (and so qemu) has the http and
// copy-on-read block drivers.
func detectLazy(qimg string) bool {
	out, _ := exec.Command(qimg, "--help").Output()
	m := formatsRe.FindSubmatch(out)
	if m == nil {
		return false
	}
	has := map[string]bool{}
	for _, f := range regexp.MustCompile(`\S+`).FindAll(m[1], -1) {
		has[string(f)] = true
	}
	return has["http"] && has["copy-on-read"] && httpDriverLoads(qimg)
}

// httpDriverLoads: modular QEMU builds (Arch, CachyOS: the qemu-block-curl
// package) list "http" in --help even when the curl module is missing, and
// every lazy boot then fails with "Unknown protocol 'http'". Open a closed
// local port: a missing driver says so, a present one fails to connect.
func httpDriverLoads(qimg string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, qimg, "info", "http://127.0.0.1:1/probe").CombinedOutput()
	s := string(out)
	return ctx.Err() == nil && !strings.Contains(s, "Unknown protocol") && !strings.Contains(s, "Unknown driver")
}

func (d *Driver) LazyChain() bool { return d.lazyOK }

func (d *Driver) StartLazy(ctx context.Context, spec proto.VMSpec, chain []proto.SnapshotRef, remote map[string]string) error {
	if !d.lazyOK {
		return fmt.Errorf("qemu: lazy chains need the http and copy-on-read block drivers")
	}
	if len(chain) == 0 || chain[0].ID != spec.ParentID {
		return fmt.Errorf("qemu: lazy start: chain must start at %s", spec.ParentID)
	}
	return d.start(spec, func(m *machine) error { return d.createLazy(m, spec, chain, remote) })
}

func (d *Driver) createLazy(m *machine, spec proto.VMSpec, chain []proto.SnapshotRef, remote map[string]string) error {
	imageID := spec.ImageID
	if imageID == "" {
		imageID = chain[len(chain)-1].ImageID
	}
	img, err := d.store.backing("", imageID)
	if err != nil {
		return err
	}
	var lz []lazyLayer
	for _, ref := range chain {
		if err := checkID(ref.ID); err != nil {
			return err
		}
		// The overlay's header names l.File for good: a layer still on
		// disk is used from there, a missing one is imported at that path.
		l := lazyLayer{ID: ref.ID}
		var local bool
		if l.File, local = d.store.useLocal(ref.ID); !local {
			if l.URL = remote[ref.ID]; l.URL == "" {
				return fmt.Errorf("qemu: lazy start: no URL for missing layer %s", ref.ID)
			}
		}
		lz = append(lz, l)
	}
	top := d.store.abs(lz[0].File)
	if lz[0].URL != "" {
		top = lz[0].URL
	}
	ti, err := d.store.info(top) // qemu-img reads the header over http
	if err != nil {
		return err
	}
	want := int64(spec.Size.DiskGiB)<<30 + roundUp(d.store.imageData(img.imageID), 1<<30)
	size := max(want, ti.VirtualSize)
	rel := "disks/" + m.id + "-" + randHex(4) + ".qcow2"
	d.mu.Lock()
	m.rec = vmRecord{Spec: spec, ImageID: img.imageID, Disk: rel, Parent: spec.ParentID, Lazy: lz}
	m.rec.Spec.Guest = spec.Guest.WithoutTicket() // the ticket stays in RAM (launch)
	d.mu.Unlock()
	topFile := d.store.abs(lz[0].File)
	if err := d.store.run("create", "-q", "-f", "qcow2", "-u", "-b", topFile, "-F", "qcow2", d.store.abs(rel), strconv.FormatInt(size, 10)); err != nil {
		return err
	}
	if err := os.MkdirAll(d.vmDir(m.id), 0o700); err != nil {
		return err
	}
	cache := filepath.Join(d.vmDir(m.id), "lazy-cache.qcow2")
	if err := d.store.run("create", "-q", "-f", "qcow2", "-u", "-b", topFile, "-F", "qcow2", cache, strconv.FormatInt(ti.VirtualSize, 10)); err != nil {
		return err
	}
	d.opt.Logf("qemu: %s: lazy boot, %d of %d chain layers remote", m.id, countRemote(lz), len(lz))
	return d.launch(m, spec.Guest)
}

func countRemote(lz []lazyLayer) (n int) {
	for _, l := range lz {
		if l.URL != "" {
			n++
		}
	}
	return n
}

// lazyDiskArgs returns the -blockdev graph under the overlay and the node
// name the overlay's backing is.
func (d *Driver) lazyDiskArgs(rec vmRecord, dir string, esc func(string) string) ([]string, string, error) {
	img, err := d.store.backing("", rec.ImageID)
	if err != nil {
		return nil, "", err
	}
	a := []string{
		"-blockdev", "driver=file,node-name=lz-img-f,filename=" + esc(img.path) + ",read-only=on",
		"-blockdev", "driver=" + img.format + ",node-name=lz-img,file=lz-img-f,read-only=on",
	}
	below, lowest := "lz-img", ""
	for i := len(rec.Lazy) - 1; i >= 0; i-- {
		l, name := rec.Lazy[i], fmt.Sprintf("lz%d", i)
		if l.URL != "" {
			a = append(a, "-blockdev", "driver=http,node-name="+name+"-f,url="+esc(l.URL)+",read-only=on,timeout=120,readahead=1048576")
			if lowest == "" {
				lowest = name
			}
		} else {
			a = append(a, "-blockdev", "driver=file,node-name="+name+"-f,filename="+esc(d.store.abs(l.File))+",read-only=on")
		}
		a = append(a, "-blockdev", "driver=qcow2,node-name="+name+",file="+name+"-f,backing="+below+",read-only=on")
		below = name
	}
	if lowest != "" {
		a = append(a,
			"-blockdev", "driver=file,node-name=lz-cache-f,filename="+esc(filepath.Join(dir, "lazy-cache.qcow2")),
			"-blockdev", "driver=qcow2,node-name=lz-cache,file=lz-cache-f,backing="+below,
			"-blockdev", "driver=copy-on-read,node-name=lz-cor,file=lz-cache,bottom="+lowest)
		below = "lz-cor"
	}
	for i := len(rec.LazyTop) - 1; i >= 0; i-- {
		l, name := rec.LazyTop[i], fmt.Sprintf("lzt%d", i)
		a = append(a,
			"-blockdev", "driver=file,node-name="+name+"-f,filename="+esc(d.store.abs(l.File))+",read-only=on",
			"-blockdev", "driver=qcow2,node-name="+name+",file="+name+"-f,backing="+below+",read-only=on")
		below = name
	}
	return a, below, nil
}

// Lazy reports whether the VM still reads layers from URLs.
func (d *Driver) Lazy(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.vms[id]
	return m != nil && m.rec.Lazy != nil
}

// Hydrated switches the VM's remote layers to the store's files.
func (d *Driver) Hydrated(ctx context.Context, id string) error {
	m, err := d.lock(id)
	if err != nil {
		return err
	}
	defer m.op.Unlock()
	d.mu.Lock()
	rec, r := m.rec, m.run
	d.mu.Unlock()
	if rec.Lazy == nil {
		return nil
	}
	for _, l := range rec.Lazy {
		if !d.store.HasSnapshot(l.ID) {
			return fmt.Errorf("%s: layer %s: %w", id, l.ID, vm.ErrNoParent)
		}
	}
	if r != nil && r.alive() && rec.LazyGraph {
		sfx := randHex(3)
		for i, l := range rec.Lazy {
			if l.URL == "" {
				continue
			}
			name, below := fmt.Sprintf("lz%d", i), "lz-img"
			if i+1 < len(rec.Lazy) {
				below = fmt.Sprintf("lz%d", i+1)
			}
			p, err := d.store.SnapshotPath(l.ID)
			if err != nil {
				return err
			}
			if err := r.q.cmd(ctx, "blockdev-add", map[string]any{"driver": "file", "node-name": name + "-l" + sfx, "filename": p, "read-only": true}, nil); err != nil {
				return fmt.Errorf("hydrate %s: %w", l.ID, err)
			}
			if err := r.q.cmd(ctx, "blockdev-reopen", map[string]any{"options": []map[string]any{{
				"driver": "qcow2", "node-name": name, "file": name + "-l" + sfx, "backing": below, "read-only": true}}}, nil); err != nil {
				return fmt.Errorf("hydrate %s: %w", l.ID, err)
			}
			_ = r.q.cmd(ctx, "blockdev-del", map[string]any{"node-name": name + "-f"}, nil)
		}
	}
	d.mu.Lock()
	m.rec.Lazy, m.rec.LazyTop = nil, nil // the overlay headers describe the chain now
	d.mu.Unlock()
	d.saveRec(m)
	d.opt.Logf("qemu: %s: chain hydrated (%d layers)", id, len(rec.Lazy))
	return nil
}
