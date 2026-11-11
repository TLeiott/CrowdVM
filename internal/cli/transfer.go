package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
)

// pushFiles lists the files to push from dir (relative, slash-separated).
// In a git work tree: tracked + untracked-not-ignored files, with git's file
// modes (so Windows keeps executable bits); else every file except .git.
func (a *App) pushFiles(dir string) ([]pushFile, error) {
	if _, err := a.LookPath("git"); err == nil {
		var out bytes.Buffer
		c := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		c.Stdout, c.Stderr = &out, io.Discard
		if a.Run(c) == nil {
			modes := map[string]int64{}
			var st bytes.Buffer
			c2 := exec.Command("git", "-C", dir, "ls-files", "-z", "-s")
			c2.Stdout, c2.Stderr = &st, io.Discard
			if a.Run(c2) == nil {
				for _, rec := range strings.Split(st.String(), "\x00") {
					meta, name, ok := strings.Cut(rec, "\t")
					if !ok {
						continue
					}
					if m, err := strconv.ParseInt(strings.Fields(meta)[0], 8, 64); err == nil {
						modes[name] = m
					}
				}
			}
			var files []pushFile
			seen := map[string]bool{}
			for _, name := range strings.Split(out.String(), "\x00") {
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
					continue // deleted locally
				}
				files = append(files, pushFile{rel: name, gitMode: modes[name]})
			}
			return files, nil
		}
	}
	var files []pushFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if p == dir || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, pushFile{rel: filepath.ToSlash(rel)})
		return nil
	})
	return files, err
}

type pushFile struct {
	rel     string
	gitMode int64
}

// writeTarGz writes the files under root as a gzipped tar. Directories in
// the list are walked recursively. prefix is prepended to entry names.
func writeTarGz(w io.Writer, root string, files []pushFile, prefix string) (n int, size int64, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(full, name string, gitMode int64) error {
		st, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if st.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice|os.ModeCharDevice|os.ModeIrregular) != 0 {
			fmt.Fprintf(os.Stderr, "warning: skipping %s (not a regular file, directory or symlink)\n", full)
			return nil
		}
		link := ""
		if st.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return err
			}
			link = filepath.ToSlash(link)
		}
		hdr, err := tar.FileInfoHeader(st, link)
		if err != nil {
			return err
		}
		hdr.Name = path.Join(prefix, name)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 1000, 1000, "user", "user"
		if gitMode == 0o100755 {
			hdr.Mode |= 0o111
		}
		if st.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if st.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			c, err := io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
			size += c
			n++
		} else if st.Mode()&os.ModeSymlink != 0 {
			n++ // a link is a file of the push too
		}
		return nil
	}
	for _, f := range files {
		full := filepath.Join(root, filepath.FromSlash(f.rel))
		st, err := os.Lstat(full)
		if err != nil {
			return n, size, err
		}
		if st.IsDir() {
			err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(root, p)
				return add(p, filepath.ToSlash(rel), 0)
			})
		} else {
			err = add(full, f.rel, f.gitMode)
		}
		if err != nil {
			return n, size, err
		}
	}
	if err := tw.Close(); err != nil {
		return n, size, err
	}
	return n, size, gz.Close()
}

// uploadTree tars files under root, uploads the archive and extracts it in
// dest. With scpSemantics, an existing directory dest gets dest/<base(root)>
// (like scp -r).
func (a *App) uploadTree(ctx context.Context, c *client.Client, id, root string, files []pushFile, dest string, scpSemantics bool) (int, int64, error) {
	var buf bytes.Buffer
	n, size, err := writeTarGz(&buf, root, files, "")
	if err != nil {
		return 0, 0, err
	}
	archive := "/tmp/crowdvm-upload-" + client.NewIdempotencyKey()[:12] + ".tgz"
	if err := c.Upload(ctx, id, archive, &buf); err != nil {
		return 0, 0, err
	}
	target := shellQuote(dest)
	script := "d=" + target + "; "
	if scpSemantics {
		script += "if [ -d \"$d\" ]; then d=\"$d\"/" + shellQuote(filepath.Base(root)) + "; fi; "
	}
	// Outside /home/user (`push --to /srv/api`) the folder is made with sudo and given to the
	// sandbox user; a folder the user cannot write is extracted with sudo (owners come from the archive).
	script += "if ! mkdir -p \"$d\" 2>/dev/null; then sudo -n mkdir -p \"$d\" && sudo -n chown \"$(id -u):$(id -g)\" \"$d\" || exit 1; fi; " +
		"if [ -w \"$d\" ]; then tar -xzf " + archive + " -C \"$d\"; else sudo -n tar -xzf " + archive + " -C \"$d\"; fi; rc=$?; rm -f " + archive + "; exit $rc"
	res, err := c.Exec(ctx, id, client.CommandRequest{Command: script, TimeoutSeconds: 600})
	if err != nil {
		return 0, 0, err
	}
	if client.ExitCode(res.ExitCode) != 0 {
		return 0, 0, fmt.Errorf("extract on the sandbox failed: %s", strings.TrimSpace(res.Stderr))
	}
	return n, size, nil
}

// --- scp ---

var remoteRe = regexp.MustCompile(`^(?:([a-z_][a-z0-9_-]*)@)?(bx_[A-Za-z0-9_-]+|current|self):(.*)$`)

type location struct {
	remote bool
	user   string
	id     string
	path   string
}

func parseLocation(s string) location {
	if isWindowsDrive(s) {
		return location{path: s}
	}
	if m := remoteRe.FindStringSubmatch(s); m != nil {
		return location{remote: true, user: m[1], id: m[2], path: m[3]}
	}
	return location{path: s}
}

// isWindowsDrive reports "C:\..." style paths, which are local.
func isWindowsDrive(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z')
}

// apiCopy copies with the files API (no ssh/scp needed).
func (a *App) apiCopy(ctx context.Context, c *client.Client, srcs []location, dst location, recursive bool) error {
	if dst.remote {
		dpath := remotePath(dst.path)
		dstIsDir := strings.HasSuffix(dst.path, "/") || len(srcs) > 1
		if !dstIsDir {
			if st, err := c.StatFile(ctx, dst.id, dpath); err == nil && st.IsDir {
				dstIsDir = true
			}
		}
		for _, s := range srcs {
			if isDir(s.path) {
				if !recursive {
					return usagef("%s is a directory (use --recursive)", s.path)
				}
				abs, _ := filepath.Abs(s.path)
				files := []pushFile{{rel: "."}}
				n, size, err := a.uploadTreeDir(ctx, c, dst.id, abs, files, dpath)
				if err != nil {
					return err
				}
				a.info("%s → %s:%s (%d files, %s)\n", s.path, dst.id, dpath, n, humanBytes(size))
				continue
			}
			f, err := os.Open(s.path)
			if err != nil {
				return err
			}
			target := dpath
			if dstIsDir {
				target = path.Join(dpath, filepath.Base(s.path))
			}
			err = c.Upload(ctx, dst.id, target, f)
			f.Close()
			if err != nil {
				return err
			}
			if st, err := os.Stat(s.path); err == nil && st.Mode()&0o111 != 0 {
				_ = c.SetFilePermissions(ctx, dst.id, target, fmt.Sprintf("%o", st.Mode().Perm()))
			}
			a.info("%s → %s:%s\n", s.path, dst.id, target)
		}
		return nil
	}
	for _, s := range srcs {
		if !s.remote {
			return usagef("one side of the copy must be a sandbox path (ID:/path)")
		}
		spath := remotePath(s.path)
		st, err := c.StatFile(ctx, s.id, spath)
		if err != nil {
			return err
		}
		if st.IsDir {
			if !recursive {
				return usagef("%s:%s is a directory (use --recursive)", s.id, spath)
			}
			if err := a.downloadDir(ctx, c, s.id, spath, dst.path); err != nil {
				return err
			}
			continue
		}
		target := dst.path
		if isDir(target) || strings.HasSuffix(target, "/") || strings.HasSuffix(target, string(os.PathSeparator)) || len(srcs) > 1 {
			target = filepath.Join(target, path.Base(spath))
		}
		rc, err := c.Download(ctx, s.id, spath)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if m, err := strconv.ParseUint(strings.TrimPrefix(st.Mode, "0"), 8, 32); err == nil && m&0o111 != 0 {
			mode = 0o755
		}
		err = writeFile(target, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
		a.info("%s:%s → %s\n", s.id, spath, target)
	}
	return nil
}

// uploadTreeDir uploads a local directory with scp -r semantics.
func (a *App) uploadTreeDir(ctx context.Context, c *client.Client, id, abs string, files []pushFile, dest string) (int, int64, error) {
	return a.uploadTree(ctx, c, id, abs, files, dest, true)
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	if dir := filepath.Dir(target); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// A fresh, exclusively created temp file: never a path the sandbox could
	// have planted as a symlink. The rename replaces a symlink at target itself.
	// The name is short and independent of the target's, which may be near NAME_MAX.
	f, err := os.CreateTemp(filepath.Dir(target), ".cvm-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// checkNoSymlinkParents fails when dest or a directory between dest and target
// is a symlink (one the archive made earlier, or a replaced dest), so nothing
// is written through it.
func checkNoSymlinkParents(dest, target string) error {
	rel, err := filepath.Rel(dest, filepath.Dir(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe path in archive: %s", target)
	}
	cur := dest
	for _, part := range append([]string{""}, strings.Split(rel, string(os.PathSeparator))...) {
		if part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe path in archive: %s goes through a symlink", target)
		}
	}
	return nil
}

// linkInside reports whether a symlink at dir pointing to link stays inside
// dest. Absolute targets, targets that climb out of dest and targets that go
// through an existing symlink are refused.
func linkInside(dest, dir, link string) bool {
	if link == "" || path.IsAbs(link) || filepath.IsAbs(link) || strings.HasPrefix(link, `\\`) {
		return false
	}
	inside := func(p string) bool {
		rel, err := filepath.Rel(dest, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	cur, named := dir, false
	for _, part := range strings.FieldsFunc(link, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		switch part {
		case ".":
			continue
		case "..":
			// A ".." is only safe before any name: after one, a link made later
			// could change what the name resolves to (b -> a/.., then a -> .).
			if named {
				return false
			}
			cur = filepath.Dir(cur)
		default:
			named = true
			cur = filepath.Join(cur, part)
			if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
				return false
			}
		}
		if !inside(cur) {
			return false
		}
	}
	return true
}

// downloadDir tars a remote directory on the sandbox, downloads and extracts
// it. An existing local directory receives local/<base>; else local is the copy.
func (a *App) downloadDir(ctx context.Context, c *client.Client, id, rpath, local string) error {
	archive := "/tmp/crowdvm-download-" + client.NewIdempotencyKey()[:12] + ".tgz"
	parent, base := path.Split(strings.TrimRight(rpath, "/"))
	if parent == "" {
		parent = "/"
	}
	res, err := c.Exec(ctx, id, client.CommandRequest{TimeoutSeconds: 600,
		Command: "tar -czf " + archive + " -C " + shellQuote(parent) + " " + shellQuote(base)})
	if err != nil {
		return err
	}
	if client.ExitCode(res.ExitCode) != 0 {
		return fmt.Errorf("archive on the sandbox failed: %s", strings.TrimSpace(res.Stderr))
	}
	defer func() { _ = c.DeleteFile(context.Background(), id, archive, false) }()
	rc, err := c.Download(ctx, id, archive)
	if err != nil {
		return err
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	dest, strip := local, base
	if isDir(local) {
		dest, strip = filepath.Join(local, base), base
	}
	n, err := extractTar(gz, dest, strip, nil)
	if err != nil {
		return err
	}
	a.info("%s:%s → %s (%d files)\n", id, rpath, dest, n)
	return nil
}

// stripName cleans an archive name and removes strip from it. ok is false for
// names outside strip.
func stripName(n, strip string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+n), "/")
	if strip != "" {
		if name != strip && !strings.HasPrefix(name, strip+"/") {
			return "", false
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, strip), "/")
	}
	return name, true
}

// extractTar extracts r into dest. Entries must live under strip (which is
// removed from their names); an empty strip keeps names as they are. When
// only is non-empty, only entries under those prefixes are written.
// The archive comes from a sandbox and may be hostile: an entry for dest
// itself is ignored (a directory) or refused, nothing is written through a
// symlink, and symlinks that point outside dest are skipped. Symlinks that
// fail (Windows without privileges) are counted, not fatal.
func extractTar(r io.Reader, dest, strip string, only []string) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	skippedLinks, unsafeLinks := 0, 0
	dest = filepath.Clean(dest)
	if real, err := filepath.EvalSymlinks(dest); err == nil {
		dest = real // a symlinked dest the user chose; one made by the archive is caught per entry
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		name, ok := stripName(h.Name, strip)
		if !ok || len(only) > 0 && !underAny(name, only) {
			continue
		}
		if name == "" {
			if h.Typeflag == tar.TypeDir {
				continue
			}
			return n, fmt.Errorf("unsafe path in archive: entry %q is the destination itself", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if err := checkNoSymlinkParents(dest, target); err != nil {
			return n, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
		case tar.TypeSymlink:
			if !linkInside(dest, filepath.Dir(target), h.Linkname) {
				unsafeLinks++
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				skippedLinks++
			}
		case tar.TypeReg:
			if err := writeFile(target, tr, os.FileMode(h.Mode).Perm()|0o600); err != nil {
				return n, err
			}
			n++
		case tar.TypeLink:
			// A second name of an archived file: link (or copy) inside dest only.
			src, ok := stripName(h.Linkname, strip)
			if !ok || src == "" {
				continue
			}
			from := filepath.Join(dest, filepath.FromSlash(src))
			if err := checkNoSymlinkParents(dest, from); err != nil {
				return n, err
			}
			if st, err := os.Lstat(from); err != nil || !st.Mode().IsRegular() {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Link(from, target); err != nil {
				in, err := os.Open(from)
				if err != nil {
					return n, err
				}
				err = writeFile(target, in, os.FileMode(h.Mode).Perm()|0o600)
				in.Close()
				if err != nil {
					return n, err
				}
			}
			n++
		}
	}
	if unsafeLinks > 0 {
		fmt.Fprintf(os.Stderr, "warning: skipped %d symlink(s) pointing outside the destination\n", unsafeLinks)
	}
	if skippedLinks > 0 {
		fmt.Fprintf(os.Stderr, "warning: skipped %d symlink(s) this system refused to create\n", skippedLinks)
	}
	return n, nil
}

func underAny(name string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.Trim(p, "/")
		if p == "" || name == p || strings.HasPrefix(name, p+"/") || strings.HasPrefix(p, name+"/") {
			return true
		}
	}
	return false
}
