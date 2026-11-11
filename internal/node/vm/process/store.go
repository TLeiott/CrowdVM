package process

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// store: snapshots/<id>/ is a full copy of a sandbox home with its ref in
// snapshots/<id>.json. snapshots/<id>.tar is the upload form (made on demand).
// images/<id> holds imported image files, which this driver never reads.
type store struct {
	dir   string
	mu    sync.Mutex
	snaps map[string]proto.SnapshotRef
}

func openStore(dir string) (*store, error) {
	s := &store{dir: dir, snaps: map[string]proto.SnapshotRef{}}
	for _, d := range []string{"images", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	ents, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".json"):
			var ref proto.SnapshotRef
			b, err := os.ReadFile(filepath.Join(dir, "snapshots", name))
			if err == nil && json.Unmarshal(b, &ref) == nil && ref.ID != "" {
				s.snaps[ref.ID] = ref
			}
		case strings.Contains(name, ".tmp"):
			removeAll(filepath.Join(dir, "snapshots", name)) // interrupted copy
		}
	}
	return s, nil
}

func (s *store) path(id, ext string) string { return filepath.Join(s.dir, "snapshots", id+ext) }

func (s *store) snapDir(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.snaps[id]
	return s.path(id, ""), ok
}

// add records home (moved when move, else copied) as snapshot id.
func (s *store) add(sandboxID, id, parent, imageID, home string, move bool) (proto.SnapshotRef, error) {
	tmp := s.path(id, ".tmp-"+randHex(4))
	var err error
	if move {
		err = os.Rename(home, tmp)
	} else {
		err = copyTree(home, tmp)
	}
	if err == nil {
		err = os.Rename(tmp, s.path(id, ""))
	}
	if err != nil {
		removeAll(tmp)
		return proto.SnapshotRef{}, err
	}
	ref := proto.SnapshotRef{ID: id, ParentID: parent, ImageID: imageID, SizeBytes: treeSize(s.path(id, "")), CreatedAt: time.Now().UTC(), SandboxID: sandboxID}
	return ref, s.save(ref)
}

func (s *store) save(ref proto.SnapshotRef) error {
	if err := writeJSON(s.path(ref.ID, ".json"), ref); err != nil {
		return err
	}
	s.mu.Lock()
	s.snaps[ref.ID] = ref
	s.mu.Unlock()
	return nil
}

func (s *store) HasImage(id string) bool { return true } // images are not used

func (s *store) Images() []string {
	ents, _ := os.ReadDir(filepath.Join(s.dir, "images"))
	var ids []string
	for _, e := range ents {
		ids = append(ids, e.Name())
	}
	return ids
}

func (s *store) ImportImage(id, path string) error {
	if err := checkID(id); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(s.dir, "images", id))
}

func (s *store) ImagePath(id string) (string, error) {
	p := filepath.Join(s.dir, "images", id)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func (s *store) HasSnapshot(id string) bool {
	_, ok := s.SnapshotRef(id)
	return ok
}

func (s *store) SnapshotRef(id string) (proto.SnapshotRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.snaps[id]
	return ref, ok
}

func (s *store) Snapshots() []proto.SnapshotRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.SnapshotRef, 0, len(s.snaps))
	for _, ref := range s.snaps {
		out = append(out, ref)
	}
	return out
}

// SnapshotPath returns the snapshot as a tar file, creating it on first use.
func (s *store) SnapshotPath(id string) (string, error) {
	if !s.HasSnapshot(id) {
		return "", fmt.Errorf("snapshot %s: %w", id, os.ErrNotExist)
	}
	p := s.path(id, ".tar")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	tmp := p + ".tmp-" + randHex(4)
	if err := writeTar(s.path(id, ""), tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// ImportSnapshot unpacks a tar made by SnapshotPath. Snapshots are full
// copies, so the parent need not be present.
func (s *store) ImportSnapshot(ref proto.SnapshotRef, path string) error {
	if err := checkID(ref.ID); err != nil {
		return err
	}
	if s.HasSnapshot(ref.ID) {
		return nil
	}
	tmp := s.path(ref.ID, ".tmp-"+randHex(4))
	err := extractTar(path, tmp)
	if err == nil {
		err = os.Rename(tmp, s.path(ref.ID, ""))
	}
	if err != nil {
		removeAll(tmp)
		return err
	}
	if err := os.Rename(path, s.path(ref.ID, ".tar")); err != nil {
		os.Remove(path)
	}
	if ref.SizeBytes == 0 {
		ref.SizeBytes = treeSize(s.path(ref.ID, ""))
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}
	return s.save(ref)
}

func (s *store) DeleteSnapshot(id string) error {
	s.mu.Lock()
	delete(s.snaps, id)
	s.mu.Unlock()
	os.Remove(s.path(id, ".json"))
	os.Remove(s.path(id, ".tar"))
	return removeAll(s.path(id, ""))
}

// ---------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------

// copyTree copies src to dst like cp -a: regular files (reflinked when
// possible), directories and symlinks; sockets and devices are skipped.
func copyTree(src, dst string) error {
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			dirs = append(dirs, dirMode{out, info.Mode().Perm()})
			return os.MkdirAll(out, 0o700) // writable while copying
		case e.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, out)
		case e.Type().IsRegular():
			if err := copyFile(p, out, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(out, info.ModTime(), info.ModTime())
		}
		return nil
	})
	for _, d := range slices.Backward(dirs) {
		os.Chmod(d.path, d.mode)
	}
	return err
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o200)
	if err != nil {
		return err
	}
	if cloneFile(out, in) != nil {
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func treeSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// treeStamp fingerprints a tree: entries, total size, newest mtime.
func treeStamp(dir string) string {
	var n, size int64
	var newest time.Time
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := e.Info(); err == nil {
			n++
			if e.Type().IsRegular() {
				size += info.Size()
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%d/%d/%d", n, size, newest.UnixNano())
}

// removeAll also removes trees with read-only directories (Go module cache).
func removeAll(p string) error {
	if os.RemoveAll(p) == nil {
		return nil
	}
	filepath.WalkDir(p, func(q string, e fs.DirEntry, err error) error {
		if err == nil {
			os.Chmod(q, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}

func writeTar(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		link := ""
		if e.Type()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !e.IsDir() && !e.Type().IsRegular() {
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h.Name = filepath.ToSlash(rel)
		h.Uname, h.Gname = "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !e.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, in)
		in.Close()
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// extractTar unpacks into dst, refusing entries that escape it (including
// through symlinks unpacked earlier).
func extractTar(path, dst string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return err
	}
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	defer func() {
		for _, d := range slices.Backward(dirs) {
			os.Chmod(d.path, d.mode)
		}
	}()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("tar: unsafe path %q", h.Name)
		}
		p := filepath.Join(dst, name)
		parent, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil && h.Typeflag != tar.TypeDir {
			return err
		}
		if err == nil && parent != root && !strings.HasPrefix(parent, root+string(filepath.Separator)) {
			return fmt.Errorf("tar: %q escapes through a symlink", h.Name)
		}
		mode := h.FileInfo().Mode().Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{p, mode})
		case tar.TypeReg:
			out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o200)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			os.Chmod(p, mode)
			os.Chtimes(p, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		}
	}
}
