package guest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxFileRW     = 5 << 20
	maxArtifact   = 50 << 20
	maxListItems  = 10000
	maxFindMatch  = 1000
	maxFindLine   = 500
	maxReplaceNum = 100
)

// FileEntry is Boat's FileEntry.
type FileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	IsDir      bool      `json:"isDir"`
	IsSymlink  bool      `json:"isSymlink"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

func entryOf(virt string, fi fs.FileInfo) FileEntry {
	return FileEntry{Name: filepath.Base(virt), Path: virt, IsDir: fi.IsDir(),
		IsSymlink: fi.Mode()&fs.ModeSymlink != 0, Size: fi.Size(),
		Mode: fmt.Sprintf("%04o", uint32(fi.Mode().Perm())|modeBits(fi.Mode())), ModifiedAt: fi.ModTime().UTC()}
}

func modeBits(m fs.FileMode) uint32 {
	var b uint32
	if m&fs.ModeSetuid != 0 {
		b |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		b |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		b |= 0o1000
	}
	return b
}

func parseMode(s string) (fs.FileMode, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil || n > 0o7777 {
		return 0, errors.New("mode must be octal permission bits, for example \"755\"")
	}
	m := fs.FileMode(n & 0o777)
	if n&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if n&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if n&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m, nil
}

// fsErr maps filesystem errors onto the error envelope.
func (a *Agent) fsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidPath):
		writeErr(w, 400, "invalid_path", err.Error())
	case errors.Is(err, fs.ErrNotExist):
		writeErr(w, 404, "file_not_found", a.scrub(err.Error()))
	case errors.Is(err, fs.ErrPermission):
		writeErr(w, 403, "permission_denied", a.scrub(err.Error()))
	case errors.Is(err, syscall.ENOTDIR):
		writeErr(w, 400, "not_a_directory", a.scrub(err.Error()))
	case errors.Is(err, syscall.EISDIR):
		writeErr(w, 400, "is_directory", a.scrub(err.Error()))
	case errors.Is(err, syscall.ENOTEMPTY):
		writeErr(w, 400, "directory_not_empty", a.scrub(err.Error()))
	default:
		writeErr(w, 400, "file_error", a.scrub(err.Error()))
	}
}

// scrub rewrites real paths in messages to virtual ones (unprivileged mode).
func (a *Agent) scrub(s string) string {
	if !a.unpriv {
		return s
	}
	s = strings.ReplaceAll(s, a.tmp, "/tmp")
	return strings.ReplaceAll(s, a.home, "/home/user")
}

func (a *Agent) resolveQ(w http.ResponseWriter, r *http.Request, def string, follow bool) (string, string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = def
	}
	real, virt, err := a.resolve(p, follow)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return "", "", false
	}
	return real, virt, true
}

func encodingOf(s string) (string, bool) {
	if s == "" {
		return "utf8", true
	}
	return s, s == "utf8" || s == "base64"
}

// metricFiles are readable through GET /v1/files although they are outside
// /home/user and /tmp: the server's OpenTelemetry exporter reads VM CPU,
// memory, IO pressure and disk usage from them (OTEL-005).
var metricFiles = map[string]bool{"/proc/stat": true, "/proc/meminfo": true, "/proc/pressure/io": true, "/run/crowdvm/df": true}

// GET /v1/files
func (a *Agent) handleFileRead(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Query().Get("path"); metricFiles[p] {
		real := p
		if p == "/run/crowdvm/df" {
			real = a.dfPath()
		}
		b, err := os.ReadFile(real)
		if err != nil {
			a.fsErr(w, err)
			return
		}
		ok(w, "file.read", map[string]any{"success": true, "path": p, "encoding": "utf8", "size": len(b), "content": string(b)})
		return
	}
	real, virt, okk := a.resolveQ(w, r, "", true)
	if !okk {
		return
	}
	enc, good := encodingOf(r.URL.Query().Get("encoding"))
	if !good {
		writeErr(w, 400, "invalid_encoding", "encoding must be utf8 or base64")
		return
	}
	fi, err := os.Stat(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	if fi.IsDir() {
		writeErr(w, 400, "is_directory", virt+" is a directory")
		return
	}
	if fi.Size() > maxFileRW {
		writeErr(w, 400, "file_too_large", "files over 5 MiB need files/download-url")
		return
	}
	b, err := os.ReadFile(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	content := string(b)
	if enc == "base64" {
		content = base64.StdEncoding.EncodeToString(b)
	}
	ok(w, "file.read", map[string]any{"success": true, "path": virt, "encoding": enc, "size": len(b), "content": content})
}

// PUT /v1/files
func (a *Agent) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string  `json:"path"`
		Content  *string `json:"content"`
		Encoding string  `json:"encoding"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	enc, good := encodingOf(req.Encoding)
	if !good {
		writeErr(w, 400, "invalid_encoding", "encoding must be utf8 or base64")
		return
	}
	if req.Content == nil {
		writeErr(w, 400, "invalid_body", "content is required")
		return
	}
	data := []byte(*req.Content)
	if enc == "base64" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(*req.Content); err != nil {
			writeErr(w, 400, "invalid_content", "content is not valid base64")
			return
		}
	}
	if len(data) > maxFileRW {
		writeErr(w, 400, "file_too_large", "files over 5 MiB need files/upload-url")
		return
	}
	real, virt, err := a.resolve(req.Path, true)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return
	}
	if err := a.writeUserFile(real, bytes.NewReader(data)); err != nil {
		a.fsErr(w, err)
		return
	}
	ok(w, "file.written", map[string]any{"success": true, "path": virt, "encoding": enc, "size": len(data)})
}

// writeUserFile atomically writes src to real (following a final symlink),
// creating parent directories owned by the user.
func (a *Agent) writeUserFile(real string, src io.Reader) error {
	if ev, err := filepath.EvalSymlinks(real); err == nil {
		real = ev
	}
	if fi, err := os.Stat(real); err == nil && fi.IsDir() {
		return &os.PathError{Op: "write", Path: real, Err: syscall.EISDIR}
	}
	if err := a.mkdirAll(filepath.Dir(real), 0o755); err != nil {
		return err
	}
	var mode fs.FileMode = 0o644
	uid, gid := a.uid, a.gid
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode().Perm()
		if s, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(s.Uid), int(s.Gid)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(real), ".crowdvm-upload-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil && !a.unpriv {
		_ = os.Lchown(tmp, uid, gid)
	}
	if err == nil {
		err = os.Rename(tmp, real)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// DELETE /v1/files
func (a *Agent) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	real, virt, okk := a.resolveQ(w, r, "", false)
	if !okk {
		return
	}
	if virt == "/home/user" || virt == "/tmp" {
		writeErr(w, 400, "invalid_path", virt+" cannot be deleted")
		return
	}
	fi, err := os.Lstat(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	if fi.IsDir() {
		if !qBool(r, "recursive") {
			writeErr(w, 400, "is_directory", virt+" is a directory; pass recursive=true")
			return
		}
		err = os.RemoveAll(real)
	} else {
		err = os.Remove(real)
	}
	if err != nil {
		a.fsErr(w, err)
		return
	}
	ok(w, "file.deleted", map[string]any{"success": true, "path": virt})
}

// GET /v1/files/list
func (a *Agent) handleFileList(w http.ResponseWriter, r *http.Request) {
	real, virt, okk := a.resolveQ(w, r, "/home/user", true)
	if !okk {
		return
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	truncated := len(ents) > maxListItems
	if truncated {
		ents = ents[:maxListItems]
	}
	files := make([]FileEntry, 0, len(ents))
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, entryOf(filepath.Join(virt, e.Name()), fi))
	}
	ok(w, "file.list", map[string]any{"success": true, "path": virt, "files": files, "truncated": truncated})
}

// GET /v1/files/info
func (a *Agent) handleFileInfo(w http.ResponseWriter, r *http.Request) {
	real, virt, okk := a.resolveQ(w, r, "", false)
	if !okk {
		return
	}
	fi, err := os.Lstat(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	e := entryOf(virt, fi)
	ok(w, "file.info", infoFields(e))
}

func infoFields(e FileEntry) map[string]any {
	return map[string]any{"success": true, "name": e.Name, "path": e.Path, "isDir": e.IsDir,
		"isSymlink": e.IsSymlink, "size": e.Size, "mode": e.Mode, "modifiedAt": e.ModifiedAt}
}

// GET /v1/files/search
func (a *Agent) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	real, _, okk := a.resolveQ(w, r, "/home/user", true)
	if !okk {
		return
	}
	pattern := r.URL.Query().Get("pattern")
	if _, err := filepath.Match(pattern, ""); err != nil || pattern == "" {
		writeErr(w, 400, "invalid_pattern", "pattern must be a valid glob")
		return
	}
	files := []string{}
	var errs []string
	truncated := false
	err := filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == real {
				return err
			}
			errs = append(errs, a.scrub(err.Error()))
			return nil
		}
		if p == real {
			return nil
		}
		if m, _ := filepath.Match(pattern, d.Name()); m {
			if len(files) == maxListItems {
				truncated = true
				return filepath.SkipAll
			}
			files = append(files, a.toVirt(p))
		}
		return nil
	})
	if err != nil {
		a.fsErr(w, err)
		return
	}
	res := map[string]any{"success": true, "files": files, "truncated": truncated}
	if len(errs) > 0 {
		res["errors"] = errs
	}
	ok(w, "file.search", res)
}

type findMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// GET /v1/files/find
func (a *Agent) handleFileFind(w http.ResponseWriter, r *http.Request) {
	real, _, okk := a.resolveQ(w, r, "/home/user", true)
	if !okk {
		return
	}
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		writeErr(w, 400, "invalid_pattern", "pattern is required")
		return
	}
	match := func(s string) bool { return strings.Contains(s, pattern) }
	if qBool(r, "regex") {
		re, err := regexp.Compile(pattern)
		if err != nil {
			writeErr(w, 400, "invalid_regex", "invalid regular expression: "+err.Error())
			return
		}
		match = re.MatchString
	}
	matches := []findMatch{}
	var errs []string
	truncated := false
	walkErr := filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == real {
				return err
			}
			errs = append(errs, a.scrub(err.Error()))
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			errs = append(errs, a.scrub(err.Error()))
			return nil
		}
		defer f.Close()
		br := bufio.NewReaderSize(f, 64<<10)
		if head, _ := br.Peek(8000); bytes.IndexByte(head, 0) >= 0 {
			return nil // binary
		}
		virt := a.toVirt(p)
		for n := 1; ; n++ {
			line, err := br.ReadString('\n')
			if len(line) > 0 && match(strings.TrimRight(line, "\r\n")) {
				if len(matches) == maxFindMatch {
					truncated = true
					return filepath.SkipAll
				}
				matches = append(matches, findMatch{File: virt, Line: n, Content: cutLine(strings.TrimRight(line, "\r\n"))})
			}
			if err != nil {
				return nil
			}
		}
	})
	if walkErr != nil {
		a.fsErr(w, walkErr)
		return
	}
	res := map[string]any{"success": true, "matches": matches, "truncated": truncated}
	if len(errs) > 0 {
		res["errors"] = errs
	}
	ok(w, "file.find", res)
}

func cutLine(s string) string {
	if len(s) <= maxFindLine {
		return s
	}
	b := []byte(s[:maxFindLine])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// POST /v1/files/folder
func (a *Agent) handleFolder(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path, Mode string }
	if !decodeJSON(w, r, &req) {
		return
	}
	var mode fs.FileMode = 0o755
	if req.Mode != "" {
		var err error
		if mode, err = parseMode(req.Mode); err != nil {
			writeErr(w, 400, "invalid_mode", err.Error())
			return
		}
	}
	real, virt, err := a.resolve(req.Path, true)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return
	}
	_, statErr := os.Stat(real)
	if err := a.mkdirAll(real, mode); err != nil {
		a.fsErr(w, err)
		return
	}
	if statErr != nil && req.Mode != "" {
		_ = os.Chmod(real, mode) // undo umask
	}
	fi, err := os.Lstat(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	ok(w, "folder.created", infoFields(entryOf(virt, fi)))
}

// POST /v1/files/move
func (a *Agent) handleMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source, Destination string
		Overwrite           bool
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	src, srcV, err := a.resolve(req.Source, false)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return
	}
	dst, dstV, err := a.resolve(req.Destination, false)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return
	}
	for _, v := range []string{srcV, dstV} {
		if v == "/home/user" || v == "/tmp" {
			writeErr(w, 400, "invalid_path", v+" cannot be moved or replaced")
			return
		}
	}
	si, err := os.Lstat(src)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	// Overwrite removes the destination first: never when that is the
	// source itself, holds it, or lies inside it (bugs-r3-sol 2).
	if di, err := os.Lstat(dst); err == nil && os.SameFile(si, di) {
		ok(w, "file.moved", map[string]any{"success": true, "path": srcV, "destination": dstV})
		return
	}
	if rs, rd := realPath(src), realPath(dst); within(rs, rd) || within(rd, rs) { // through symlinks too (bugs-r4-sol 1)
		writeErr(w, 400, "invalid_path", "cannot move "+srcV+" onto "+dstV+": one contains the other")
		return
	}
	if _, err := os.Lstat(dst); err == nil {
		if !req.Overwrite {
			writeErr(w, 409, "destination_exists", dstV+" exists; pass overwrite=true")
			return
		}
		if err := os.RemoveAll(dst); err != nil {
			a.fsErr(w, err)
			return
		}
	}
	if err := a.mkdirAll(filepath.Dir(dst), 0o755); err != nil {
		a.fsErr(w, err)
		return
	}
	if err := os.Rename(src, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			a.fsErr(w, err)
			return
		}
		if out, err := exec.Command("mv", "-T", "--", src, dst).CombinedOutput(); err != nil {
			writeErr(w, 400, "file_error", a.scrub(strings.TrimSpace(string(out))))
			return
		}
	}
	ok(w, "file.moved", map[string]any{"success": true, "path": srcV, "destination": dstV})
}

// realPath resolves symlinks in p's parent folders (p itself may be a
// link: a move moves the link).
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return filepath.Clean(p)
}

// within: path p lies inside directory dir.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

// POST /v1/files/permissions
func (a *Agent) handlePermissions(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path, Mode string }
	if !decodeJSON(w, r, &req) {
		return
	}
	mode, err := parseMode(req.Mode)
	if err != nil {
		writeErr(w, 400, "invalid_mode", err.Error())
		return
	}
	real, virt, err := a.resolve(req.Path, true)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return
	}
	if err := os.Chmod(real, mode); err != nil {
		a.fsErr(w, err)
		return
	}
	fi, err := os.Lstat(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	ok(w, "file.permissions_set", infoFields(entryOf(virt, fi)))
}

// POST /v1/files/replace
func (a *Agent) handleReplace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths    []string `json:"paths"`
		Pattern  string   `json:"pattern"`
		NewValue *string  `json:"newValue"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Paths) < 1 || len(req.Paths) > maxReplaceNum {
		writeErr(w, 400, "invalid_paths", "paths must hold 1-100 entries")
		return
	}
	if req.Pattern == "" || req.NewValue == nil {
		writeErr(w, 400, "invalid_body", "pattern and newValue are required")
		return
	}
	type result struct {
		File         string `json:"file"`
		Success      bool   `json:"success"`
		Replacements int    `json:"replacements"`
		Error        string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(req.Paths))
	for _, p := range req.Paths {
		res := result{File: p}
		err := func() error {
			real, virt, err := a.resolve(p, true)
			if err != nil {
				return err
			}
			res.File = virt
			fi, err := os.Stat(real)
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return errors.New("is a directory")
			}
			if fi.Size() > maxFileRW {
				return errors.New("file is larger than 5 MiB")
			}
			b, err := os.ReadFile(real)
			if err != nil {
				return err
			}
			n := bytes.Count(b, []byte(req.Pattern))
			res.Replacements = n
			if n == 0 {
				return nil
			}
			return a.writeUserFile(real, bytes.NewReader(bytes.ReplaceAll(b, []byte(req.Pattern), []byte(*req.NewValue))))
		}()
		res.Success = err == nil
		if err != nil {
			res.Error = a.scrub(err.Error())
		}
		results = append(results, res)
	}
	ok(w, "file.replaced", map[string]any{"success": true, "results": results})
}

// GET /v1/artifacts: a file (<= 50 MiB) as raw bytes, or a directory as a
// streamed .tar.gz (CrowdVM extension).
func (a *Agent) handleArtifact(w http.ResponseWriter, r *http.Request) {
	real, virt, okk := a.resolveQ(w, r, "", true)
	if !okk {
		return
	}
	fi, err := os.Stat(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	if fi.IsDir() {
		name := filepath.Base(virt)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".tar.gz"))
		_ = writeTarGz(w, real, name)
		return
	}
	if fi.Size() > maxArtifact {
		writeErr(w, 400, "file_too_large", "artifacts are limited to 50 MiB; use files/download-url")
		return
	}
	f, err := os.Open(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(virt)))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func writeTarGz(w io.Writer, dir, prefix string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(filepath.Join(prefix, rel))
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			if _, err := io.CopyN(tw, f, fi.Size()); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	return err
}

// GET /v1/files/raw?path= : raw bytes, no size limit, Range support. Used by
// the server behind signed files/download-url links.
func (a *Agent) handleRawGet(w http.ResponseWriter, r *http.Request) {
	real, virt, okk := a.resolveQ(w, r, "", true)
	if !okk {
		return
	}
	f, err := os.Open(real)
	if err != nil {
		a.fsErr(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		a.fsErr(w, err)
		return
	}
	if fi.IsDir() {
		writeErr(w, 400, "is_directory", virt+" is a directory")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(virt), fi.ModTime(), f)
}

// PUT /v1/files/raw?path= : body is the file, no size limit; parents created;
// an existing file is replaced only when the upload completes.
func (a *Agent) handleRawPut(w http.ResponseWriter, r *http.Request) {
	real, virt, okk := a.resolveQ(w, r, "", true)
	if !okk {
		return
	}
	if err := a.writeUserFile(real, r.Body); err != nil {
		a.fsErr(w, err)
		return
	}
	ok(w, "file.written", map[string]any{"success": true, "path": virt, "size": fileSize(real)})
}
