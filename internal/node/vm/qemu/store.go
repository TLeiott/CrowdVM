package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// store keeps base images and snapshots as disk files under <dir>/disks with
// metadata in <dir>/images/<id>.json and <dir>/snapshots/<id>.json.
//
// Snapshot files are qcow2 overlays whose backing file is the absolute local
// path of the parent snapshot (or base image). Frozen VM overlays become
// snapshots in place (no rename, so the running VM's backing chain and
// Windows file locks stay valid).
type store struct {
	dir  string
	qimg string

	mu     sync.Mutex
	images map[string]*imageMeta
	snaps  map[string]*snapMeta
	// inUse returns the snapshot ids VMs run on and their active disks
	// (relative paths); set by the driver, used by gc.
	inUse func() (parents, disks []string)
}

type imageMeta struct {
	ID          string `json:"id"`
	File        string `json:"file"` // relative to store dir
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtualSize"`
	// DataBytes: allocated (non-zero) data in the image, i.e. what the OS
	// uses. A VM's disk is its size plus this, so the size counts user data
	// only (Boat: "disk" excludes the OS). 0 = not measured yet.
	DataBytes int64 `json:"dataBytes,omitempty"`
}

type snapMeta struct {
	Ref         proto.SnapshotRef `json:"ref"`
	File        string            `json:"file"`
	VirtualSize int64             `json:"virtualSize"`
	// Deleted snapshots stay on disk while a live snapshot or VM builds on them.
	Deleted bool `json:"deleted,omitempty"`
	// Level for tiered chain compaction (see mergePlan); layers pulled from
	// the server have an unknown level and are never merged by the policy.
	Level int `json:"level,omitempty"`
}

// levelUnknown marks layers whose compaction level is not known.
const levelUnknown = 1 << 20

// layer is a resolved backing for a new overlay.
type layer struct {
	path    string
	format  string
	vsize   int64
	imageID string
	depth   int // snapshot layers in the chain (0 = base image)
}

func openStore(dir, qimg string) (*store, error) {
	s := &store{dir: dir, qimg: qimg, images: map[string]*imageMeta{}, snaps: map[string]*snapMeta{}}
	for _, d := range []string{"images", "snapshots", "disks"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	if err := loadJSONDir(filepath.Join(dir, "images"), func(b []byte) error {
		var m imageMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		s.images[m.ID] = &m
		return nil
	}); err != nil {
		return nil, err
	}
	if err := loadJSONDir(filepath.Join(dir, "snapshots"), func(b []byte) error {
		var m snapMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		s.snaps[m.Ref.ID] = &m
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func loadJSONDir(dir string, fn func([]byte) error) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err == nil {
			err = fn(b)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return nil
}

func (s *store) abs(rel string) string { return filepath.Join(s.dir, filepath.FromSlash(rel)) }

// ---------------------------------------------------------------------------
// vm.Store
// ---------------------------------------------------------------------------

func (s *store) HasImage(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.images[id] != nil
}

func (s *store) HasSnapshot(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.snaps[id]
	return m != nil && !m.Deleted && s.onDisk(m)
}

// onDisk reports whether the snapshot's file exists (s.mu held).
func (s *store) onDisk(m *snapMeta) bool {
	_, err := os.Stat(s.abs(m.File))
	return err == nil
}

// useLocal returns the store path of snapshot id for an overlay to name.
// A record whose file is still on disk (even one deleted but kept because
// something builds on it) is undeleted and used as is (local=true);
// otherwise the layer will be imported at disks/snap-<id>.qcow2.
func (s *store) useLocal(id string) (rel string, local bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.snaps[id]
	if m == nil || !s.onDisk(m) {
		return "disks/snap-" + id + ".qcow2", false
	}
	if m.Deleted {
		m.Deleted = false
		if err := s.saveSnap(m); err != nil {
			return "disks/snap-" + id + ".qcow2", false
		}
	}
	return m.File, true
}

func (s *store) Images() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.images {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *store) ImportImage(id, path string) error {
	if err := checkID(id); err != nil {
		return err
	}
	if s.HasImage(id) {
		return nil // immutable: VMs may be running on the stored file
	}
	info, err := s.info(path)
	if err != nil {
		return err
	}
	rel := "disks/image-" + id + "." + info.Format
	if info.BackingFile != "" { // e.g. a fresh build overlay: store it standalone
		rel = "disks/image-" + id + ".qcow2"
		if err := s.run("convert", "-O", "qcow2", path, s.abs(rel)); err != nil {
			return err
		}
		info.Format = "qcow2"
		os.Remove(path)
	} else if err := moveFile(path, s.abs(rel)); err != nil {
		return err
	}
	os.Chmod(s.abs(rel), 0o444)
	m := &imageMeta{ID: id, File: rel, Format: info.Format, VirtualSize: info.VirtualSize, DataBytes: s.dataBytes(s.abs(rel))}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeJSON(filepath.Join(s.dir, "images", id+".json"), m); err != nil {
		return err
	}
	s.images[id] = m
	return nil
}

func (s *store) ImportSnapshot(ref proto.SnapshotRef, path string) error {
	if err := checkID(ref.ID); err != nil {
		return err
	}
	if _, local := s.useLocal(ref.ID); local {
		return nil // same immutable content, still on disk: resurrected
	}
	s.mu.Lock()
	delete(s.snaps, ref.ID) // a record without its file: import it anew
	s.mu.Unlock()
	parent, err := s.backing(ref.ParentID, ref.ImageID)
	if err != nil {
		return err
	}
	info, err := s.info(path)
	if err != nil {
		return err
	}
	if info.Format != "qcow2" {
		return fmt.Errorf("snapshot %s: format %q, want qcow2", ref.ID, info.Format)
	}
	// The file names the uploader's local parent path; point it at ours.
	if err := s.run("rebase", "-u", "-f", "qcow2", "-b", parent.path, "-F", parent.format, path); err != nil {
		return err
	}
	rel := "disks/snap-" + ref.ID + ".qcow2"
	if err := moveFile(path, s.abs(rel)); err != nil {
		return err
	}
	os.Chmod(s.abs(rel), 0o444)
	if ref.SizeBytes == 0 {
		ref.SizeBytes = fileSize(s.abs(rel))
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}
	if ref.ImageID == "" {
		ref.ImageID = parent.imageID
	}
	return s.addSnapshot(ref, rel, info.VirtualSize, levelUnknown)
}

func (s *store) SnapshotPath(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.snaps[id]
	if m == nil || m.Deleted {
		return "", fmt.Errorf("snapshot %s: %w", id, os.ErrNotExist)
	}
	return s.abs(m.File), nil
}

func (s *store) ImagePath(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.images[id]
	if m == nil {
		return "", fmt.Errorf("image %s: %w", id, os.ErrNotExist)
	}
	return s.abs(m.File), nil
}

func (s *store) SnapshotRef(id string) (proto.SnapshotRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.snaps[id]
	if m == nil || m.Deleted {
		return proto.SnapshotRef{}, false
	}
	return m.Ref, true
}

// Snapshots lists the snapshots that are not deleted. Layers a VM froze in
// place keep its disk name (disks/<sandbox>-<rand>.qcow2), which gives the
// sandbox of snapshots recorded before refs carried it.
func (s *store) Snapshots() []proto.SnapshotRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.SnapshotRef, 0, len(s.snaps))
	for _, m := range s.snaps {
		if m.Deleted {
			continue
		}
		ref := m.Ref
		if base := filepath.Base(filepath.FromSlash(m.File)); ref.SandboxID == "" && !strings.HasPrefix(base, "snap-") {
			if sb, _, ok := strings.Cut(base, "-"); ok {
				ref.SandboxID = sb
			}
		}
		out = append(out, ref)
	}
	return out
}

// DeleteSnapshot hides the snapshot at once; its file goes when nothing
// (child snapshot or VM) builds on it any more.
func (s *store) DeleteSnapshot(id string) error {
	s.mu.Lock()
	m := s.snaps[id]
	if m == nil {
		s.mu.Unlock()
		return nil
	}
	m.Deleted = true
	err := s.saveSnap(m)
	s.mu.Unlock()
	s.gc()
	return err
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// backing resolves the layer a new overlay builds on: snapshot parentID (with
// its whole chain present), or the base image when parentID is "".
func (s *store) backing(parentID, imageID string) (layer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if parentID == "" {
		m := s.images[imageID]
		if m == nil {
			return layer{}, fmt.Errorf("image %q: %w", imageID, vm.ErrNoParent)
		}
		return layer{path: s.abs(m.File), format: m.Format, vsize: m.VirtualSize, imageID: imageID}, nil
	}
	top := s.snaps[parentID]
	if top == nil || top.Deleted {
		return layer{}, fmt.Errorf("snapshot %q: %w", parentID, vm.ErrNoParent)
	}
	depth := 0
	for id := parentID; id != ""; {
		m := s.snaps[id]
		if m == nil {
			return layer{}, fmt.Errorf("snapshot %q (ancestor of %s): %w", id, parentID, vm.ErrNoParent)
		}
		depth++
		if depth > 10000 {
			return layer{}, fmt.Errorf("snapshot %s: parent cycle", parentID)
		}
		id = m.Ref.ParentID
		if id == "" && s.images[m.Ref.ImageID] == nil {
			return layer{}, fmt.Errorf("image %q (base of %s): %w", m.Ref.ImageID, parentID, vm.ErrNoParent)
		}
	}
	return layer{path: s.abs(top.File), format: "qcow2", vsize: top.VirtualSize, imageID: top.Ref.ImageID, depth: depth}, nil
}

// chain returns the snapshot layers from parentID (top) down to the base
// image. The chain must be present (see backing).
func (s *store) chain(parentID string) []snapMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []snapMeta
	for id := parentID; id != "" && len(out) < 10000; {
		m := s.snaps[id]
		if m == nil {
			break
		}
		out = append(out, *m)
		id = m.Ref.ParentID
	}
	return out
}

// imageData returns the image's allocated data bytes, measuring (and
// recording) them on first use for images imported before it was tracked.
func (s *store) imageData(id string) int64 {
	s.mu.Lock()
	m := s.images[id]
	var path string
	if m != nil && m.DataBytes == 0 {
		path = s.abs(m.File)
	}
	s.mu.Unlock()
	if m == nil {
		return 0
	}
	if path != "" {
		n := s.dataBytes(path)
		s.mu.Lock()
		if m.DataBytes == 0 && n > 0 {
			m.DataBytes = n
			writeJSON(filepath.Join(s.dir, "images", id+".json"), m)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.DataBytes
}

// dataBytes sums the data extents of a disk image (qemu-img map; metadata
// only, fast). 0 if unknown.
func (s *store) dataBytes(path string) int64 {
	out, err := exec.Command(s.qimg, "map", "-U", "--output=json", path).Output()
	if err != nil {
		return 0
	}
	var ext []struct {
		Length int64 `json:"length"`
		Data   bool  `json:"data"`
	}
	if json.Unmarshal(out, &ext) != nil {
		return 0
	}
	var n int64
	for _, e := range ext {
		if e.Data {
			n += e.Length
		}
	}
	return n
}

func (s *store) addSnapshot(ref proto.SnapshotRef, rel string, vsize int64, level int) error {
	m := &snapMeta{Ref: ref, File: rel, VirtualSize: vsize, Level: level}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveSnap(m); err != nil {
		return err
	}
	s.snaps[ref.ID] = m
	return nil
}

func (s *store) saveSnap(m *snapMeta) error {
	return writeJSON(filepath.Join(s.dir, "snapshots", m.Ref.ID+".json"), m)
}

// gc removes deleted snapshots nothing builds on. With orphans set (only at
// driver start, when no operation is in flight) it also removes disk files
// no image, snapshot or VM references.
func (s *store) gcOrphans(orphans bool) {
	var parents, disks []string
	if s.inUse != nil {
		parents, disks = s.inUse()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[string]bool{}
	var mark func(id string)
	mark = func(id string) {
		for id != "" && !keep[id] {
			keep[id] = true
			m := s.snaps[id]
			if m == nil {
				return
			}
			id = m.Ref.ParentID
		}
	}
	for id, m := range s.snaps {
		if !m.Deleted {
			mark(id)
		}
	}
	for _, id := range parents {
		mark(id)
	}
	for id, m := range s.snaps {
		if m.Deleted && !keep[id] {
			removeFile(s.abs(m.File))
			os.Remove(filepath.Join(s.dir, "snapshots", id+".json"))
			delete(s.snaps, id)
		}
	}
	if !orphans {
		return
	}
	used := map[string]bool{}
	for _, m := range s.images {
		used[m.File] = true
	}
	for _, m := range s.snaps {
		used[m.File] = true
	}
	for _, d := range disks {
		used[d] = true
	}
	ents, _ := os.ReadDir(filepath.Join(s.dir, "disks"))
	for _, e := range ents {
		if rel := "disks/" + e.Name(); !used[rel] {
			removeFile(s.abs(rel))
		}
	}
}

func (s *store) gc() { s.gcOrphans(false) }

// CompactSnapshot (vm.Compactor): a layer whose file is mostly holes (the
// guest discarded what it wrote, e.g. fstrim after a disk re-encryption)
// is copied with qemu-img convert onto the same backing file, which writes
// its allocated clusters only. The copy is deterministic, so a retried
// upload has the same sha256.
func (s *store) CompactSnapshot(id, dir string) (string, error) {
	src, err := s.SnapshotPath(id)
	if err != nil {
		return "", err
	}
	info, err := s.info(src)
	if err != nil {
		return "", err
	}
	if info.FullBackingFile == "" || info.BackingFormat == "" || info.ActualSize >= fileSize(src)/2 {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "snapshot-"+id+"-*.part") // removed at node start if left behind
	if err != nil {
		return "", err
	}
	f.Close()
	if err := s.run("convert", "-U", "-O", "qcow2", "-B", info.FullBackingFile, "-F", info.BackingFormat, src, f.Name()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	// convert writes the backing file's absolute path (this node's paths);
	// keep the reference the layer itself had (a header-only change).
	if info.BackingFile != info.FullBackingFile {
		if err := s.run("rebase", "-u", "-b", info.BackingFile, "-F", info.BackingFormat, f.Name()); err != nil {
			os.Remove(f.Name())
			return "", err
		}
	}
	return f.Name(), nil
}

type imgInfo struct {
	Format          string `json:"format"`
	VirtualSize     int64  `json:"virtual-size"`
	ActualSize      int64  `json:"actual-size"`
	BackingFile     string `json:"backing-filename"`
	FullBackingFile string `json:"full-backing-filename"`
	BackingFormat   string `json:"backing-filename-format"`
}

func (s *store) info(path string) (imgInfo, error) {
	var out bytes.Buffer
	cmd := exec.Command(s.qimg, "info", "-U", "--output=json", path)
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return imgInfo{}, fmt.Errorf("qemu-img info %s: %v: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	var i imgInfo
	err := json.Unmarshal(out.Bytes(), &i)
	return i, err
}

func (s *store) run(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.qimg, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func checkID(id string) error {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, `/\:*?"<>|`) || id[0] == '.' || id[0] == '_' {
		return fmt.Errorf("invalid id %q", id)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// moveFile renames src to dst, copying across filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst + ".tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		return err
	}
	in.Close()
	return os.Remove(src)
}

// removeFile deletes a file, also a read-only one (undeletable on Windows).
func removeFile(p string) {
	os.Chmod(p, 0o644)
	os.Remove(p)
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
