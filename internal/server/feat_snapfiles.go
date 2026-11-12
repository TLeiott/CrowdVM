package server

// Snapshot file access while the sandbox is stopped:
//
//	GET /snapshots/{id}/tree                → flat entries of /home/user + docker volumes (__dockervol__/<vol>/...)
//	GET /snapshots/{id}/files?path=         → a file (Range ok) or a folder as tar ("" or "/" = everything)
//	GET /snapshots/{id}/download            → signed URLs of every qcow2 layer of the chain (+ base image, inventory)
//
// tree/files boot a throwaway inspector VM from the snapshot on a node
// (node POST /v1/inspect; it reuses the guest files/artifacts/commands API
// and is destroyed by the node when idle). The sandbox's own machine is never
// contacted. Trees are cached (snapshots are immutable). Paths: relative =
// under /home/user ("home/user/..." is also accepted), "/abs" = any path in
// the machine, "__dockervol__/<vol>/..." = a docker named volume. Paths outside
// /home/user are read through a read-only bind mount of / at
// /tmp/.crowdvm-root inside the inspector.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	inspectRoot     = "/tmp/.crowdvm-root"
	dockerVolPrefix = "__dockervol__"
	dockerVolDir    = "/var/lib/docker/volumes"
	maxTreeEntries  = 100000
	maxCachedTrees  = 64
	downloadTTL     = time.Hour
)

func init() {
	RegisterFeature("snapshot-files", func(s *Server) error {
		f := &snapFiles{s: s, insp: map[string]*snapInsp{}, trees: map[string]map[string]any{}}
		s.API("GET /snapshots/{snapshotId}/tree", "snapshot.read", f.handleTree)
		s.API("GET /snapshots/{snapshotId}/files", "snapshot.read", f.handleFiles)
		s.API("GET /snapshots/{snapshotId}/download", "snapshot.read", f.handleDownload)
		s.Router().HandleFunc("GET /api/v1/snapshots/{snapshotId}/download/blob/{kind}/{layer}", f.handleBlob)
		s.Router().HandleFunc("GET /api/v1/snapshots/{snapshotId}/download/inventory", f.handleInventory)
		return nil
	})
}

type snapFiles struct {
	s     *Server
	mu    sync.Mutex
	insp  map[string]*snapInsp      // snapshot id -> inspector
	trees map[string]map[string]any // snapshot id -> tree response fields
	order []string                  // tree cache insertion order
}

// snapInsp is an inspector VM on a node, shared by requests for one snapshot.
type snapInsp struct {
	done   chan struct{}
	err    error
	nodeID string
	id     string

	prepOnce sync.Once
	prepErr  error
}

var (
	errInspectorGone = errors.New("inspector gone")
	volNameRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// ---------------------------------------------------------------------------
// Access
// ---------------------------------------------------------------------------

func (f *snapFiles) snapshot(c *Ctx) (*Snapshot, error) {
	return f.snapForRead(c.P, c.Param("snapshotId"))
}

func (f *snapFiles) snapForRead(p *Principal, id string) (*Snapshot, error) {
	// A sandbox id names its newest snapshot (Boat: snapshot pull <sandboxId>).
	if strings.HasPrefix(id, api.PrefixSandbox) {
		var head string
		_ = f.s.DB.QueryRow(`SELECT id FROM snapshots WHERE sandbox_id=? AND deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT 1`, id).Scan(&head)
		if head == "" {
			return nil, Err(404, "not_found", "The sandbox "+id+" has no snapshot (or does not exist).")
		}
		id = head
	}
	sn, err := f.s.SnapshotByID(id)
	if err != nil {
		return nil, Err(404, "not_found", "Snapshot not found.")
	}
	// A saved snapshot's layer stays readable through its snapshotId (SNAP-032),
	// even after its sandbox's snapshots were deleted. Only for its saver:
	// anyone else deploys it, and the deploy scrubs the saver's logins, keys
	// and history, which the raw files still hold (bugs-r10-opus 1).
	var saved int
	_ = f.s.DB.QueryRow(`SELECT COUNT(*) FROM saved_snapshots WHERE snapshot_id=? AND saved_by=?`, sn.ID, p.User.ID).Scan(&saved)
	if sn.DeletedAt != nil && saved == 0 {
		return nil, Err(404, "not_found", "Snapshot not found.")
	}
	sb, sbErr := f.s.SandboxByID(sn.SandboxID)
	if sn.OwnerID == p.User.ID {
		if p.Key != nil && sbErr == nil && !p.Key.AllowsSandbox(sb, "snapshot.read") {
			return nil, Err(403, "api_key_sandbox_forbidden", "The API key is not allowed on this snapshot's sandbox.")
		}
		return sn, nil
	}
	if saved > 0 || (sbErr == nil && sb.DeletedAt == nil && f.s.authorizeSandbox(p, sb, "snapshot.read", false) == nil) {
		return sn, nil
	}
	return nil, Err(404, "not_found", "Snapshot not found.")
}

// ---------------------------------------------------------------------------
// Inspector VMs
// ---------------------------------------------------------------------------

// inspector returns a ready inspector for sn, booting one when needed.
func (f *snapFiles) inspector(ctx context.Context, sn *Snapshot) (*snapInsp, error) {
	f.mu.Lock()
	in := f.insp[sn.ID]
	if in != nil {
		select {
		case <-in.done:
			if in.err != nil || f.s.nodes.get(in.nodeID) == nil {
				delete(f.insp, sn.ID)
				in = nil
			}
		default:
		}
	}
	if in == nil {
		in = &snapInsp{done: make(chan struct{})}
		f.insp[sn.ID] = in
		go f.boot(sn, in)
	}
	f.mu.Unlock()
	select {
	case <-in.done:
		return in, in.err
	case <-ctx.Done():
		return nil, Err(499, "client_closed", "The client went away.")
	}
}

func (f *snapFiles) drop(snapID string, in *snapInsp) {
	f.mu.Lock()
	if f.insp[snapID] == in {
		delete(f.insp, snapID)
	}
	f.mu.Unlock()
}

func (f *snapFiles) boot(sn *Snapshot, in *snapInsp) {
	defer close(in.done)
	if f.s.encryptedChain(sn.ID) {
		// The inspector VM has no guest channel to fetch the disk key over.
		in.err = Err(409, "snapshot_encrypted", "This snapshot's disk is encrypted, and only the sandbox's own machine gets its key. Resume the sandbox (or fork it) to read its files.")
		return
	}
	ctx, cancel := context.WithTimeout(f.s.ctx, 30*time.Minute)
	defer cancel()
	conn, err := f.pickNode(sn)
	if err != nil {
		in.err = err
		return
	}
	var st struct {
		ID string `json:"id"`
	}
	f.s.grantChain(conn.ID, sn.ID)
	err = conn.Do(ctx, "POST", "/v1/inspect", map[string]any{"snapshotId": sn.ID, "imageId": sn.ImageID, "token": randToken(32)}, &st)
	if err != nil {
		var ne *NodeError
		if errors.As(err, &ne) && (ne.Status == 404 || ne.Status == 405) && !strings.Contains(ne.Body, "inspector") {
			in.err = Err(501, "inspector_unsupported", "The node holding this snapshot is too old to read snapshot files. Update crowdvm-node.")
			return
		}
		f.s.Log.Warn("snapshot inspector failed", "snapshot", sn.ID, "node", conn.ID, "err", err)
		in.err = ErrD(502, "snapshot_inspect_failed", "Could not open the snapshot: "+err.Error(), map[string]any{"retryable": true})
		return
	}
	for _, id := range f.s.snapshotChain(sn.ID) {
		f.s.addLocation(id, conn.ID)
	}
	in.nodeID, in.id = conn.ID, st.ID
}

// pickNode prefers the online node holding most of the chain; the layers it
// lacks must be on the server.
func (f *snapFiles) pickNode(sn *Snapshot) (*NodeConn, error) {
	chain := f.s.snapshotChain(sn.ID) // newest first
	var best *NodeConn
	bestDepth := 0
	online := f.s.nodes.online()
	// The disk only goes to nodes its sandbox's placement.trust allows (SEC-014).
	var trust Placement
	var owners []string
	if sb, err := f.s.SandboxByID(sn.SandboxID); err == nil && (sb.Placement.Trust == "own" || sb.Placement.Trust == "trusted") {
		trust, owners = Placement{Trust: sb.Placement.Trust}, f.s.ownersFor(sb.OwnerID, sb.OrgID)
	}
	ids := make([]string, 0, len(online))
	for id := range online {
		n, err := f.s.NodeByID(id)
		if err == nil && n.Disabled && !f.s.hasLocation(sn.ID, id) {
			continue // disabled nodes only serve snapshots they already hold
		}
		if trust.Trust != "" && (err != nil || matchReason(n, trust, owners) != "") {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		depth := len(chain)
		for i, l := range chain {
			if f.s.hasLocation(l, id) {
				depth = i
				break
			}
		}
		if best == nil || depth < bestDepth {
			best, bestDepth = online[id], depth
		}
	}
	if best == nil {
		return nil, ErrD(503, "no_capacity", "No node is online to open the snapshot. Retry later.", map[string]any{"retryable": true, "retryAfterSeconds": 30})
	}
	for _, l := range chain[:bestDepth] {
		if s2, err := f.s.SnapshotByID(l); err != nil || s2.UploadedAt == nil {
			return nil, ErrD(409, "snapshot_unavailable", "The snapshot is not stored on the server yet and its node is offline. Retry later.",
				map[string]any{"retryable": true, "retryAfterSeconds": 30})
		}
	}
	return best, nil
}

// withInspector runs fn with a ready inspector, retrying once with a fresh
// one when the node destroyed the old one.
func (f *snapFiles) withInspector(ctx context.Context, sn *Snapshot, fn func(in *snapInsp) error) error {
	for try := 0; ; try++ {
		in, err := f.inspector(ctx, sn)
		if err != nil {
			return err
		}
		err = fn(in)
		if errors.Is(err, errInspectorGone) {
			f.drop(sn.ID, in)
			if try == 0 {
				continue
			}
			return ErrD(502, "snapshot_inspect_failed", "The snapshot reader went away. Retry.", map[string]any{"retryable": true})
		}
		return err
	}
}

// guest sends a request to the inspector's guest /v1/<rest>.
func (f *snapFiles) guest(ctx context.Context, in *snapInsp, method, rest string, body any, hdr http.Header) (*http.Response, error) {
	conn := f.s.nodes.get(in.nodeID)
	if conn == nil {
		return nil, errInspectorGone
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node/v1/inspect/"+in.id+"/guest/"+rest, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := conn.HTTP().Do(req)
	if err != nil {
		return nil, ErrD(502, "snapshot_inspect_failed", "The snapshot reader did not answer: "+err.Error(), map[string]any{"retryable": true})
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == 404 && strings.Contains(string(b), "inspector_not_found") {
			return nil, errInspectorGone
		}
		return nil, nodeAPIError(&NodeError{Status: resp.StatusCode, Body: string(b)})
	}
	return resp, nil
}

// run executes a shell script in the inspector (as user, sudo available in VMs).
func (f *snapFiles) run(ctx context.Context, in *snapInsp, script string, timeout int) (string, error) {
	resp, err := f.guest(ctx, in, "POST", "commands", map[string]any{"command": script, "timeoutSeconds": timeout}, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ExitCode *int   `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", ErrD(502, "snapshot_inspect_failed", "Bad answer from the snapshot reader: "+err.Error(), map[string]any{"retryable": true})
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		return out.Stdout, fmt.Errorf("exit %d: %s", *out.ExitCode, strings.TrimSpace(out.Stderr))
	}
	return out.Stdout, nil
}

// sudoPrefix runs the rest of a script line as root when sudo works.
const sudoPrefix = `S=; [ "$(id -u)" = 0 ] || { sudo -n true 2>/dev/null && S="sudo -n"; }; `

// prepare bind-mounts / read-only at inspectRoot (once per inspector).
func (f *snapFiles) prepare(ctx context.Context, in *snapInsp) error {
	in.prepOnce.Do(func() {
		_, err := f.run(ctx, in, sudoPrefix+`R=`+inspectRoot+`
mountpoint -q "$R" 2>/dev/null && exit 0
$S mkdir -p "$R" && $S mount --bind / "$R" && $S mount -o remount,bind,ro "$R" && mountpoint -q "$R"`, 60)
		switch {
		case errors.Is(err, errInspectorGone):
			in.prepErr = errInspectorGone
		case err != nil:
			in.prepErr = Err(409, "snapshot_path_unavailable", "Paths outside /home/user cannot be read from this snapshot: "+err.Error())
		}
	})
	return in.prepErr
}

// ---------------------------------------------------------------------------
// Tree
// ---------------------------------------------------------------------------

const treeScript = sudoPrefix + `cd ~ || exit 3
find . -mindepth 1 \( -type f -o -type d -o -type l \) -printf '%y\t%s\t%P\n' 2>/dev/null | head -n 200000
for v in $($S ls -1 ` + dockerVolDir + ` 2>/dev/null); do
  $S test -d "` + dockerVolDir + `/$v/_data" || continue
  printf 'd\t0\t` + dockerVolPrefix + `/%s\n' "$v"
  $S find "` + dockerVolDir + `/$v/_data" -mindepth 1 \( -type f -o -type d -o -type l \) -printf "%y\t%s\t` + dockerVolPrefix + `/$v/%P\n" 2>/dev/null | head -n 200000
done
exit 0`

// parseTree turns `find -printf '%y\t%s\t%P\n'` output into tree fields.
type treeEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size *int64 `json:"size,omitempty"`
}

func parseTree(out string) map[string]any {
	type entry = treeEntry
	entries := []entry{}
	var files, total int64
	// The inspector's output hit its cap: the inventory is too large to
	// expand (Boat treeAvailable false, reason inventory_too_large); the
	// entries read so far are still returned.
	tooLarge := len(out) >= 8<<20-1024 || strings.Count(out, "\n") >= 200000
	truncated := tooLarge
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[2] == "" || seen[parts[2]] {
			continue
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		e := entry{Path: parts[2]}
		switch parts[0] {
		case "f":
			e.Kind, e.Size = "file", &size
			files++
			total += size
		case "d":
			e.Kind = "dir"
		case "l":
			e.Kind = "symlink"
		default:
			continue
		}
		seen[e.Path] = true
		if len(entries) < maxTreeEntries {
			entries = append(entries, e)
		} else {
			truncated = true
		}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, dockerVolPrefix+"/") && !seen[dockerVolPrefix] {
			seen[dockerVolPrefix] = true
			entries = append(entries, entry{Path: dockerVolPrefix, Kind: "dir"})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.Path, b.Path) })
	var reason any
	if tooLarge {
		reason = "inventory_too_large"
	}
	return map[string]any{"treeAvailable": !tooLarge, "truncated": truncated, "fileCount": files, "totalSizeBytes": total, "entries": entries, "reason": reason}
}

func (f *snapFiles) tree(ctx context.Context, sn *Snapshot) (map[string]any, error) {
	f.mu.Lock()
	t := f.trees[sn.ID]
	f.mu.Unlock()
	if t != nil {
		return t, nil
	}
	var out string
	err := f.withInspector(ctx, sn, func(in *snapInsp) error {
		var err error
		out, err = f.run(ctx, in, treeScript, 600)
		var ae *api.Error
		if err != nil && !errors.Is(err, errInspectorGone) && !errors.As(err, &ae) {
			err = ErrD(502, "snapshot_inspect_failed", "Listing the snapshot failed: "+err.Error(), map[string]any{"retryable": true})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	t = parseTree(out)
	t["snapshotId"], t["sandboxId"], t["generation"], t["root"] = sn.ID, sn.SandboxID, sn.Generation, proto.GuestHome
	f.mu.Lock()
	if _, ok := f.trees[sn.ID]; !ok {
		f.trees[sn.ID] = t
		f.order = append(f.order, sn.ID)
		for len(f.order) > maxCachedTrees {
			delete(f.trees, f.order[0])
			f.order = f.order[1:]
		}
	}
	f.mu.Unlock()
	return t, nil
}

func (f *snapFiles) handleTree(c *Ctx) error {
	sn, err := f.snapshot(c)
	if err != nil {
		return err
	}
	t, err := f.tree(c.R.Context(), sn)
	if err != nil {
		return err
	}
	if c.Query("path") == "" && c.Query("depth") == "" {
		return c.OK(200, "snapshot.tree", t)
	}
	// CrowdVM extension: only entries below path, at most depth levels deep.
	prefix := treePrefix(c.Query("path"))
	depth, _ := strconv.Atoi(c.Query("depth"))
	out := map[string]any{}
	for k, v := range t {
		out[k] = v
	}
	keep := []treeEntry{}
	for _, e := range t["entries"].([]treeEntry) {
		rel, ok := e.Path, prefix == ""
		if !ok {
			rel, ok = strings.CutPrefix(e.Path, prefix+"/")
		}
		if ok && (depth <= 0 || strings.Count(rel, "/") < depth) {
			keep = append(keep, e)
		}
	}
	out["entries"], out["path"], out["depth"] = keep, prefix, depth
	return c.OK(200, "snapshot.tree", out)
}

// treePrefix maps a path to the tree's path space ("" = the root).
func treePrefix(p string) string {
	p = strings.Trim(path.Clean("/"+strings.TrimSpace(p)), "/")
	if p == "home/user" {
		return ""
	}
	if rest, ok := strings.CutPrefix(p, "home/user/"); ok {
		return rest
	}
	return p
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

// snapPath maps an API path to an absolute path in the machine ("" = everything).
func snapPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" || p == "." {
		return "", nil
	}
	if strings.ContainsRune(p, 0) {
		return "", Err(400, "invalid_path", "path is not valid.")
	}
	if strings.HasPrefix(p, "/") {
		return path.Clean(p), nil
	}
	rel := path.Clean(p)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", Err(400, "invalid_path", "path must stay inside the snapshot.")
	}
	switch {
	case rel == "home/user" || strings.HasPrefix(rel, "home/user/"):
		return "/" + rel, nil
	case rel == dockerVolPrefix:
		return dockerVolDir, nil
	case strings.HasPrefix(rel, dockerVolPrefix+"/"):
		vol, rest, _ := strings.Cut(strings.TrimPrefix(rel, dockerVolPrefix+"/"), "/")
		return path.Join(dockerVolDir, vol, "_data", rest), nil
	}
	return path.Join(proto.GuestHome, rel), nil
}

func underHome(abs string) bool {
	return abs == proto.GuestHome || strings.HasPrefix(abs, proto.GuestHome+"/")
}

// guestPathFor returns the path the guest files API reads abs at.
func (f *snapFiles) guestPathFor(ctx context.Context, in *snapInsp, abs string) (string, error) {
	if underHome(abs) {
		return abs, nil
	}
	if err := f.prepare(ctx, in); err != nil {
		return "", err
	}
	return inspectRoot + abs, nil
}

// shq quotes s for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shPath is abs as a shell word (home paths via $HOME, which also works unprivileged).
func shPath(abs string) string {
	if underHome(abs) {
		return `"$HOME"` + shq(strings.TrimPrefix(abs, proto.GuestHome))
	}
	return shq(abs)
}

// dirStats counts files and bytes below the given paths.
func (f *snapFiles) dirStats(ctx context.Context, in *snapInsp, paths ...string) (int64, int64) {
	words := make([]string, len(paths))
	for i, p := range paths {
		words[i] = shPath(p)
	}
	out, err := f.run(ctx, in, sudoPrefix+`$S find `+strings.Join(words, " ")+` -type f -printf '%s\n' 2>/dev/null | awk '{n++; s+=$1} END {printf "%d %d\n", n, s}'`, 600)
	if err != nil {
		return 0, 0
	}
	var n, size int64
	_, _ = fmt.Sscan(strings.TrimSpace(out), &n, &size)
	return n, size
}

func (f *snapFiles) handleFiles(c *Ctx) error {
	sn, err := f.snapshot(c)
	if err != nil {
		return err
	}
	abs, err := snapPath(c.Query("path"))
	if err != nil {
		return err
	}
	ctx := c.R.Context()
	return f.withInspector(ctx, sn, func(in *snapInsp) error {
		if abs == "" {
			return f.streamAll(c, in)
		}
		gp, err := f.guestPathFor(ctx, in, abs)
		if err != nil {
			return err
		}
		resp, err := f.guest(ctx, in, "GET", "files/info?"+url.Values{"path": {gp}}.Encode(), nil, nil)
		if err != nil {
			var ae *api.Error
			if errors.As(err, &ae) && ae.Status == 404 {
				return Err(404, "not_found", "No file or folder "+c.Query("path")+" in this snapshot.")
			}
			return err
		}
		var info struct {
			IsDir     bool  `json:"isDir"`
			IsSymlink bool  `json:"isSymlink"`
			Size      int64 `json:"size"`
		}
		err = json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
		if err != nil {
			return ErrD(502, "snapshot_inspect_failed", "Bad answer from the snapshot reader.", map[string]any{"retryable": true})
		}
		switch {
		case info.IsSymlink:
			return Err(409, "is_symlink", "The path is a symlink. Request its target instead.")
		case info.IsDir:
			return f.streamDir(c, in, abs, gp)
		default:
			return f.streamFile(c, in, abs, gp)
		}
	})
}

func (f *snapFiles) streamFile(c *Ctx, in *snapInsp, abs, gp string) error {
	hdr := http.Header{}
	for _, k := range []string{"Range", "If-Range"} {
		if v := c.R.Header.Get(k); v != "" {
			hdr.Set(k, v)
		}
	}
	resp, err := f.guest(c.R.Context(), in, "GET", "files/raw?"+url.Values{"path": {gp}}.Encode(), nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h := c.W.Header()
	for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(abs)))
	h.Set("X-Snapshot-Entry-Kind", "file")
	c.W.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(c.W, resp.Body)
	return nil
}

func (f *snapFiles) setDirHeaders(c *Ctx, name string, files, size int64) {
	h := c.W.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".tar"))
	h.Set("X-Snapshot-Entry-Kind", "dir")
	h.Set("X-Snapshot-File-Count", strconv.FormatInt(files, 10))
	h.Set("X-Snapshot-Total-Size-Bytes", strconv.FormatInt(size, 10))
	h.Set("X-Snapshot-Skipped-Base-Image-Files", "0")
}

// artifact opens guest /v1/artifacts for a folder as a tar reader.
func (f *snapFiles) artifact(ctx context.Context, in *snapInsp, gp string) (*tar.Reader, func(), error) {
	resp, err := f.guest(ctx, in, "GET", "artifacts?"+url.Values{"path": {gp}}.Encode(), nil, nil)
	if err != nil {
		return nil, nil, err
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, nil, ErrD(502, "snapshot_inspect_failed", "Bad archive from the snapshot reader.", map[string]any{"retryable": true})
	}
	return tar.NewReader(zr), func() { zr.Close(); resp.Body.Close() }, nil
}

// streamDir sends a folder as tar; entries are rooted at the folder's name.
func (f *snapFiles) streamDir(c *Ctx, in *snapInsp, abs, gp string) error {
	ctx := c.R.Context()
	files, size := f.dirStats(ctx, in, abs)
	tr, closeFn, err := f.artifact(ctx, in, gp)
	if err != nil {
		return err
	}
	defer closeFn()
	f.setDirHeaders(c, path.Base(abs), files, size)
	c.W.WriteHeader(200)
	tw := tar.NewWriter(c.W)
	if err := copyTar(tw, tr, func(n string) string { return n }); err == nil {
		_ = tw.Close()
	}
	return nil
}

// streamAll sends the whole snapshot: /home/user at the root of the tar and
// docker volumes under __dockervol__/<vol>/.
func (f *snapFiles) streamAll(c *Ctx, in *snapInsp) error {
	ctx := c.R.Context()
	vols := []string{}
	out, _ := f.run(ctx, in, sudoPrefix+`for v in $($S ls -1 `+dockerVolDir+` 2>/dev/null); do $S test -d "`+dockerVolDir+`/$v/_data" && echo "$v"; done; exit 0`, 60)
	for _, v := range strings.Fields(out) {
		if volNameRe.MatchString(v) {
			vols = append(vols, v)
		}
	}
	if len(vols) > 0 {
		if err := f.prepare(ctx, in); err != nil {
			if errors.Is(err, errInspectorGone) {
				return err
			}
			vols = nil // volumes need root; send the home folder only
		}
	}
	paths := []string{proto.GuestHome}
	for _, v := range vols {
		paths = append(paths, path.Join(dockerVolDir, v, "_data"))
	}
	files, size := f.dirStats(ctx, in, paths...)
	tr, closeFn, err := f.artifact(ctx, in, proto.GuestHome)
	if err != nil {
		return err
	}
	f.setDirHeaders(c, "snapshot", files, size)
	c.W.WriteHeader(200)
	tw := tar.NewWriter(c.W)
	err = copyTar(tw, tr, stripFirst(""))
	closeFn()
	if err != nil {
		return nil
	}
	for _, v := range vols {
		tr, closeFn, err := f.artifact(ctx, in, inspectRoot+path.Join(dockerVolDir, v, "_data"))
		if err != nil {
			return nil
		}
		hdr := &tar.Header{Name: dockerVolPrefix + "/" + v + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()}
		if tw.WriteHeader(hdr) != nil {
			closeFn()
			return nil
		}
		err = copyTar(tw, tr, stripFirst(dockerVolPrefix+"/"+v+"/"))
		closeFn()
		if err != nil {
			return nil
		}
	}
	_ = tw.Close()
	return nil
}

// stripFirst drops the first path component and adds prefix ("" for the root itself = skip).
func stripFirst(prefix string) func(string) string {
	return func(n string) string {
		_, rest, ok := strings.Cut(n, "/")
		if !ok || rest == "" {
			return ""
		}
		return prefix + rest
	}
}

// copyTar copies entries, renaming them; an empty name skips the entry.
func copyTar(tw *tar.Writer, tr *tar.Reader, rename func(string) string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := rename(hdr.Name)
		if name == "" {
			continue
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Download manifest + signed layer URLs
// ---------------------------------------------------------------------------

func (f *snapFiles) blobSig(snapID, kind, layer string, exp int64) string {
	return f.s.sign(fmt.Sprintf("snapblob|%s|%s|%s|%d", snapID, kind, layer, exp))
}

func (f *snapFiles) signedURL(snapID, kind, layer string, exp int64) string {
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {f.blobSig(snapID, kind, layer, exp)}}
	return fmt.Sprintf("%s/api/v1/snapshots/%s/download/blob/%s/%s?%s", f.s.Cfg.PublicURL, snapID, kind, layer, q.Encode())
}

func (f *snapFiles) handleDownload(c *Ctx) error {
	sn, err := f.snapshot(c)
	if err != nil {
		return err
	}
	exp := time.Now().Add(downloadTTL).Unix()
	chain := f.s.snapshotChain(sn.ID)
	slices.Reverse(chain) // base first
	chunks := []map[string]any{}
	for _, id := range chain {
		layer, err := f.s.SnapshotByID(id)
		bi, berr := f.s.Blobs.Stat(store.BlobSnapshot, id)
		if err != nil || berr != nil {
			return Err(409, "snapshot_compacted", "A layer of this snapshot is not stored on the server (not uploaded yet, or cleaned up). Read its files with GET /snapshots/{id}/files instead.")
		}
		chunks = append(chunks, map[string]any{"snapshotId": id, "generation": layer.Generation, "chunkIndex": 0,
			"r2Key": "snapshots/" + id + ".qcow2", "sizeBytes": bi.Size, "sha256": nz(bi.SHA256), "signedUrl": f.signedURL(sn.ID, store.BlobSnapshot, id, exp),
			"parentId": nz(layer.ParentID), "format": "qcow2"})
	}
	var base any
	if bi, err := f.s.Blobs.Stat(store.BlobImage, sn.ImageID); err == nil {
		base = map[string]any{"imageId": sn.ImageID, "sizeBytes": bi.Size, "sha256": nz(bi.SHA256), "signedUrl": f.signedURL(sn.ID, store.BlobImage, sn.ImageID, exp), "format": "qcow2"}
	} else if sn.ImageID != "" {
		base = map[string]any{"imageId": sn.ImageID, "signedUrl": nil}
	}
	kind := "incremental"
	if sn.ParentID == "" {
		kind = "base"
	}
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {f.blobSig(sn.ID, "inventory", sn.ID, exp)}}
	return c.OK(200, "snapshot.download", map[string]any{
		"snapshotId": sn.ID, "sandboxId": sn.SandboxID, "kind": kind, "generation": sn.Generation,
		"expiresInSeconds": int(downloadTTL.Seconds()),
		"reconstruct": "Each chunk is one qcow2 layer (a disk delta). Download baseImage and every chunk, then in ascending generation point each " +
			"layer at the previous one (`qemu-img rebase -u -F qcow2 -b <previous> <layer>`; the first layer's backing file is the base image). " +
			"The last layer is the sandbox disk at this snapshot. For plain files use GET /snapshots/{id}/files (`crowdvm snapshot pull` does).",
		"inventory": map[string]any{"r2Key": "snapshots/" + sn.ID + "/tree.json",
			"signedUrl": f.s.Cfg.PublicURL + "/api/v1/snapshots/" + sn.ID + "/download/inventory?" + q.Encode()},
		"chunks": chunks, "baseImage": base,
	})
}

func (f *snapFiles) checkSig(w http.ResponseWriter, r *http.Request, kind, layer string) bool {
	exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if !tokenEq(r.URL.Query().Get("sig"), f.blobSig(r.PathValue("snapshotId"), kind, layer, exp)) {
		api.WriteError(w, Err(403, "invalid_signature", "The URL signature is not valid."))
		return false
	}
	if time.Now().Unix() > exp {
		api.WriteError(w, Err(403, "url_expired", "The URL has expired."))
		return false
	}
	return true
}

func (f *snapFiles) handleBlob(w http.ResponseWriter, r *http.Request) {
	kind, layer := r.PathValue("kind"), r.PathValue("layer")
	if !f.checkSig(w, r, kind, layer) {
		return
	}
	bi, err := f.s.Blobs.Stat(kind, layer)
	if err != nil {
		api.WriteError(w, Err(409, "snapshot_compacted", "This layer is no longer stored."))
		return
	}
	fh, err := os.Open(bi.Path)
	if err != nil {
		api.WriteError(w, Err(409, "snapshot_compacted", "This layer is no longer stored."))
		return
	}
	defer fh.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", layer+".qcow2"))
	w.Header().Set("X-Crowdvm-Sha256", bi.SHA256)
	http.ServeContent(w, r, "", time.Time{}, fh)
}

// handleInventory serves the snapshot tree (the download's inventory).
func (f *snapFiles) handleInventory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("snapshotId")
	if !f.checkSig(w, r, "inventory", id) {
		return
	}
	sn, err := f.s.SnapshotByID(id)
	var saved int
	if err == nil && sn.DeletedAt != nil { // a saved snapshot's layer stays readable (SNAP-032, bugs-r19-sonnet 4)
		_ = f.s.DB.QueryRow(`SELECT COUNT(*) FROM saved_snapshots WHERE snapshot_id=?`, sn.ID).Scan(&saved)
	}
	if err != nil || (sn.DeletedAt != nil && saved == 0) {
		api.WriteError(w, Err(404, "not_found", "Snapshot not found."))
		return
	}
	t, err := f.tree(r.Context(), sn)
	if err != nil {
		api.WriteError(w, toAPIError(err))
		return
	}
	api.WriteOK(w, 200, "snapshot.tree", t)
}
