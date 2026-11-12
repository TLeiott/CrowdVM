package node

// Self-update: `crowdvm-node update`, the local page's "Update node", and
// the daemon itself (autoupdate.go). The server's GET /downloads lists its
// binaries with checksums and the release version (scripts/dist.sh's
// manifest). crowdvm-node and the companions installed next to it
// (crowdvm-guest, crowdvm-linux-amd64; the crowdvm CLI updates itself) are
// downloaded to temp files next to them, checked (size, sha256, the new
// node answers `version`), then swapped in: <name> is hard-linked to
// <name>.old and the download renamed over <name>, so <name> exists at
// every moment (Windows renames <name> away first). The daemon keeps
// running the old code until it restarts; its VMs survive the restart.
// Rollback swaps the .old files back. A lock file next to the binaries
// keeps the CLI and the daemon from updating at once. The new build
// confirms itself by reaching the server, or is rolled back
// (update_pending.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/crowdvm/crowdvm/internal/version"
)

// UpdateOptions for SelfUpdate.
type UpdateOptions struct {
	Check    bool   // only report whether an update is available
	Rollback bool   // swap the .old files from the last update back
	Force    bool   // install the server's build even when it is not newer, or was rolled back
	Exe      string // the crowdvm-node to replace (default: this executable)
}

// UpdateResult says what SelfUpdate found and did.
type UpdateResult struct {
	Server     string   `json:"server"`
	Current    string   `json:"current"` // this build
	Latest     string   `json:"latest"`  // the server's release ("" when it has no manifest)
	Available  bool     `json:"available"`
	Updated    bool     `json:"updated"`         // files were swapped (update or rollback)
	Files      []string `json:"files,omitempty"` // the paths replaced
	Restarting bool     `json:"restarting,omitempty"`
}

// updateFile is one binary SelfUpdate manages.
type updateFile struct {
	path string
	want client.DownloadFile
}

var errUpdating = errors.New("another crowdvm-node update is running; try again when it is done")

// SelfUpdate installs the newer build cfg.Server offers (see the file
// comment). Newer means a higher release version; when either side is not
// a release (a "dev" build, or a server without a manifest), any file that
// differs from the server's counts. Force installs a differing build even
// when it is older. A checksum mismatch or a node binary that does not run
// leaves every file as it was.
func SelfUpdate(ctx context.Context, cfg *Config, o UpdateOptions) (UpdateResult, error) {
	res := UpdateResult{Server: cfg.Server, Current: Version}
	exe := o.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return res, err
		}
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
	}
	paths := updatePaths(exe)
	if !o.Check { // a check reads only, so it never waits for an update
		unlock, err := lockUpdate(paths)
		if err != nil {
			return res, err
		}
		defer unlock()
	}
	if o.Rollback {
		return res, rollback(ctx, cfg.DataDir, paths, &res)
	}
	if cfg.Server == "" {
		return res, errors.New("this node has not joined a server (crowdvm-node join)")
	}
	cl := client.New(cfg.Server, "")
	cl.UserAgent = "crowdvm-node/" + Version
	list, err := cl.Downloads(ctx)
	if err != nil {
		return res, fmt.Errorf("list the server's downloads: %w", err)
	}
	res.Latest = list.Version
	var todo []updateFile
	for i, p := range paths {
		want, ok := findDownload(list, filepath.Base(p))
		switch {
		case !ok && i == 0:
			return res, fmt.Errorf("%s offers no %s for %s/%s (an admin adds it with crowdvm-server downloads import)", cfg.Server, filepath.Base(p), runtime.GOOS, runtime.GOARCH)
		case !ok:
			continue // the server does not have this companion: leave it
		case want.SHA256 == "":
			return res, fmt.Errorf("%s lists no checksums for its downloads; it needs a newer crowdvm-server", cfg.Server)
		}
		if sum, _, _ := fileSHA256(p); sum != want.SHA256 {
			todo = append(todo, updateFile{p, want})
		}
	}
	// The companions follow the node: they come with a newer node, or alone
	// when the node already is the server's (so a crash between two swaps
	// heals on the next update), never with an older node.
	nodeDiffers := len(todo) > 0 && todo[0].path == exe
	res.Available = len(todo) > 0
	if cmp, comparable := version.Compare(list.Version, Version); nodeDiffers && comparable && cmp <= 0 {
		res.Available = false
	}
	if o.Check || len(todo) == 0 || (!res.Available && !o.Force) {
		return res, nil
	}
	if nodeDiffers && !o.Force && isSkipped(cfg.DataDir, list.Version) {
		return res, errSkipped(list.Version)
	}

	var news []string
	defer func() {
		for _, n := range news {
			os.Remove(n)
		}
	}()
	for _, f := range todo {
		n, err := download(ctx, cl, f.want, f.path)
		if err != nil {
			return res, err
		}
		news = append(news, n)
	}
	if nodeDiffers {
		got := binaryVersion(ctx, news[0])
		if got == "" {
			return res, fmt.Errorf("the crowdvm-node from %s does not run on this machine (`version` failed); keeping %s", cfg.Server, exe)
		}
		if c, ok := version.Compare(got, Version); ok && c <= 0 && !o.Force {
			return res, fmt.Errorf("the server's crowdvm-node is %s, not newer than this one (%s); use --force to install it anyway", got, Version)
		}
		if !o.Force && isSkipped(cfg.DataDir, got) {
			return res, errSkipped(got)
		}
		res.Latest = got
	}
	// Only this update's files may have a .old afterwards, so a rollback
	// returns exactly to the state before it.
	for _, p := range paths {
		if err := os.Remove(p + ".old"); err != nil && !os.IsNotExist(err) {
			return res, fmt.Errorf("remove %s.old (restart the node once, then update again): %w", p, err)
		}
	}
	if nodeDiffers { // before the swap: a crash right after it still leaves the marker
		if err := savePending(cfg.DataDir, pendingUpdate{From: Version, To: res.Latest}); err != nil {
			return res, err
		}
	}
	var done []string
	for i, f := range todo {
		if err := swapIn(f.path, news[i]); err != nil {
			for _, p := range done {
				_ = os.Rename(p+".old", p)
			}
			removePending(cfg.DataDir)
			return res, err
		}
		done = append(done, f.path)
	}
	syncDir(filepath.Dir(exe))
	news = nil
	res.Updated, res.Files = true, done
	return res, nil
}

// lockUpdate takes the update lock next to the binaries (one update at a
// time across processes: the CLI and the daemon) and clears the temp files
// a killed update left behind.
func lockUpdate(paths []string) (unlock func(), err error) {
	dir := filepath.Dir(paths[0])
	f, err := lockFile(filepath.Join(dir, ".crowdvm-node-update.lock"))
	if errors.Is(err, errLocked) {
		return nil, errUpdating
	}
	if err != nil {
		return nil, fmt.Errorf("lock the update: %w", err)
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			for _, p := range paths {
				if b := filepath.Base(p); strings.HasPrefix(e.Name(), strings.TrimSuffix(b, ".exe")+".new-") || e.Name() == b+".swap" {
					os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	return func() { f.Close() }, nil
}

// updatePaths: crowdvm-node first, then the companions that sit next to it
// as regular files. A symlink belongs to another installer.
func updatePaths(exe string) []string {
	paths := []string{exe}
	for _, n := range []string{"crowdvm-guest", "crowdvm-linux-amd64"} {
		p := filepath.Join(filepath.Dir(exe), n)
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
			paths = append(paths, p)
		}
	}
	return paths
}

// findDownload maps a file next to the node to its download: crowdvm-node
// for this platform, the guest and in-sandbox CLI from linux/amd64 (any
// platform's copy of those is the same file).
func findDownload(l *client.DownloadList, base string) (client.DownloadFile, bool) {
	switch base {
	case "crowdvm-guest", "crowdvm-linux-amd64":
		if f, ok := l.Find("linux", "amd64", base); ok {
			return f, true
		}
	default: // this crowdvm-node, whatever its file is called
		base = "crowdvm-node"
		if runtime.GOOS == "windows" {
			base += ".exe"
		}
	}
	return l.Find(runtime.GOOS, runtime.GOARCH, base)
}

// download fetches want into a new temp file next to p (Windows runs only
// .exe files) and returns its path. A body longer than the listed size
// fails without filling the disk.
func download(ctx context.Context, cl *client.Client, want client.DownloadFile, p string) (string, error) {
	body, err := cl.DownloadBinary(ctx, want.OS, want.Arch, want.Name)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", want.Name, err)
	}
	defer body.Close()
	pattern := filepath.Base(p) + ".new-*"
	if strings.HasSuffix(p, ".exe") {
		pattern = strings.TrimSuffix(filepath.Base(p), ".exe") + ".new-*.exe"
	}
	f, err := os.CreateTemp(filepath.Dir(p), pattern)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, want.Size+1))
	if err == nil {
		err = f.Chmod(0o755)
	}
	if err == nil {
		err = f.Sync() // on disk before it replaces a working binary
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	got := hex.EncodeToString(h.Sum(nil))
	switch {
	case err != nil:
		err = fmt.Errorf("download %s: %w", want.Name, err)
	case n > want.Size:
		err = fmt.Errorf("the downloaded %s is larger than the server's %d bytes; nothing was installed", want.Name, want.Size)
	case got != want.SHA256 || n != want.Size:
		err = fmt.Errorf("the downloaded %s does not match the server's checksum (sha256 %s, %d bytes; want %s, %d bytes); nothing was installed",
			want.Name, got, n, want.SHA256, want.Size)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// keepAs makes dst name p's file: a hard link, so p stays in place, or on
// Windows (and file systems without links) a rename. renamed says which.
func keepAs(p, dst string) (renamed bool, err error) {
	if runtime.GOOS != "windows" {
		if err := os.Link(p, dst); err == nil || os.IsExist(err) {
			return false, err
		}
	}
	return true, os.Rename(p, dst)
}

// swapIn keeps p as p.old and renames next over p (undone when that fails).
func swapIn(p, next string) error {
	renamed, err := keepAs(p, p+".old")
	if err != nil {
		return err
	}
	if err := os.Rename(next, p); err != nil {
		if renamed {
			_ = os.Rename(p+".old", p)
		} else {
			_ = os.Remove(p + ".old")
		}
		return err
	}
	return nil
}

// swapOld exchanges p and p.old, p in place throughout like swapIn.
func swapOld(p string) error {
	tmp := p + ".swap"
	_ = os.Remove(tmp)
	renamed, err := keepAs(p, tmp)
	if err != nil {
		return err
	}
	if err := os.Rename(p+".old", p); err != nil {
		if renamed {
			_ = os.Rename(tmp, p)
		} else {
			_ = os.Remove(tmp)
		}
		return err
	}
	return os.Rename(tmp, p+".old")
}

// syncDir makes the renames in dir durable (Windows has no directory fsync).
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// rollback swaps each file with its .old, so a second rollback goes
// forward again. The build it leaves is skipped by auto-update.
func rollback(ctx context.Context, dataDir string, paths []string, res *UpdateResult) error {
	from := binaryVersion(ctx, paths[0])
	var done []string
	for _, p := range paths {
		if _, err := os.Stat(p + ".old"); err != nil {
			continue
		}
		if err := swapOld(p); err != nil {
			for _, d := range done {
				_ = swapOld(d)
			}
			return err
		}
		done = append(done, p)
	}
	if len(done) == 0 {
		return fmt.Errorf("no previous version to roll back to (no %s.old)", paths[0])
	}
	syncDir(filepath.Dir(paths[0]))
	if done[0] == paths[0] {
		skipVersion(dataDir, from)
		removePending(dataDir)
	}
	res.Updated, res.Files, res.Latest = true, done, ""
	return nil
}

// binaryVersion runs `path version` ("crowdvm-node v1.2.3 linux/amd64 go1.x");
// "" when it does not run. Older releases have no --version flag. A file
// just written may be busy for a moment (another fork still holds it open
// for writing).
func binaryVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []byte
	var err error
	for i := 0; i < 3; i++ {
		if out, err = exec.CommandContext(ctx, path, "version").Output(); !errors.Is(err, syscall.ETXTBSY) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "crowdvm-node" {
		return ""
	}
	return f[1]
}

// Tailscale's addresses: WireGuard authenticates the peer.
var tailnet4, tailnet6 = netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// TrustedUpdateServer reports whether a download from server is
// authenticated: https, this machine, or a Tailscale address. Over plain
// http on a network anyone on the path could swap the binary, so the
// daemon does not update itself from such a server (the checksums come
// from the same server).
func TrustedUpdateServer(server string) bool {
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	if u.Scheme == "https" || u.Hostname() == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	return a.IsLoopback() || tailnet4.Contains(a) || tailnet6.Contains(a)
}

// Update is SelfUpdate for the running daemon (the local page, and
// auto-update): after a swap, when a service manager starts the daemon
// again, it restarts; the new build brings its service unit up to date as
// it starts. Sandboxes keep running through the restart.
func (n *Node) Update(ctx context.Context, o UpdateOptions) (UpdateResult, error) {
	cfg := n.config()
	res, err := SelfUpdate(ctx, &cfg, o)
	if err != nil || !res.Updated {
		return res, err
	}
	n.mu.Lock()
	fn := n.restarter
	n.mu.Unlock()
	if fn != nil {
		n.log.Info("updated; restarting", "files", res.Files, "version", res.Latest)
		res.Restarting = true
		go func() { time.Sleep(300 * time.Millisecond); fn() }() // let a reply go out first
	}
	return res, nil
}

// updateRoutes serves the local page's update control: GET /api/update
// checks (cached for a while, as the page polls; ?refresh=1 skips the
// cache for an authorized caller only, so nobody can make the node hammer
// its server), POST /api/update installs and restarts the node.
func (n *Node) updateRoutes(m, ctl *http.ServeMux, authorized func(*http.Request) bool) {
	const ttl = 10 * time.Minute
	var mu sync.Mutex
	var at time.Time
	var last map[string]any
	m.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) {
		refresh := r.URL.Query().Get("refresh") != "" && authorized(r)
		mu.Lock()
		defer mu.Unlock()
		if last == nil || time.Since(at) > ttl || refresh {
			cfg := n.config()
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			res, err := SelfUpdate(ctx, &cfg, UpdateOptions{Check: true})
			cancel()
			last, at = map[string]any{"current": res.Current, "latest": res.Latest, "available": res.Available}, time.Now()
			if err != nil {
				last["error"] = err.Error()
			}
		}
		api.WriteJSON(w, 200, last)
	})
	ctl.HandleFunc("POST /api/update", func(w http.ResponseWriter, r *http.Request) {
		res, err := n.Update(r.Context(), UpdateOptions{})
		mu.Lock()
		last = nil
		mu.Unlock()
		if err != nil {
			api.WriteError(w, api.NewError(502, "update_failed", err.Error(), nil))
			return
		}
		api.WriteJSON(w, 200, res)
	})
}
