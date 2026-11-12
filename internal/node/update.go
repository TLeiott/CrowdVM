package node

// Self-update: `crowdvm-node update`, the local page's "Update node", and
// later the daemon itself. The server's GET /downloads lists its binaries
// with checksums and the release version (scripts/dist.sh's manifest).
// crowdvm-node and the companions installed next to it are downloaded to
// <name>.new, checked (size, sha256, the new node answers `version`), then
// swapped in: <name> becomes <name>.old and <name>.new becomes <name>.
// Renaming works for a running executable on Linux and Windows, so the
// daemon keeps running the old code until it restarts; its VMs survive the
// restart. Rollback swaps the .old files back.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/crowdvm/crowdvm/internal/node/service"
	"github.com/crowdvm/crowdvm/internal/version"
)

// UpdateOptions for SelfUpdate.
type UpdateOptions struct {
	Check    bool   // only report whether an update is available
	Rollback bool   // swap the .old files from the last update back
	Force    bool   // install the server's build even when it is not newer
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

var updateMu sync.Mutex // one update at a time in this process

// SelfUpdate installs the newer build cfg.Server offers (see the file
// comment). Newer means a higher release version; when either side is not
// a release (a "dev" build, or a server without a manifest), any file that
// differs from the server's counts. Force installs a differing build even
// when it is older. A checksum mismatch or a node binary that does not run
// leaves every file as it was.
func SelfUpdate(ctx context.Context, cfg *Config, o UpdateOptions) (UpdateResult, error) {
	updateMu.Lock()
	defer updateMu.Unlock()
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
	if o.Rollback {
		return res, rollback(paths, &res)
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
	cmp, comparable := version.Compare(list.Version, Version)
	res.Available = len(todo) > 0 && (!comparable || cmp > 0)
	if o.Check || len(todo) == 0 || (!res.Available && !o.Force) {
		return res, nil
	}

	var news []string
	defer func() {
		for _, n := range news {
			os.Remove(n)
		}
	}()
	for _, f := range todo {
		n := newPath(f.path)
		news = append(news, n)
		if err := download(ctx, cl, f.want, n); err != nil {
			return res, err
		}
	}
	if todo[0].path == exe {
		got := binaryVersion(ctx, news[0])
		if got == "" {
			return res, fmt.Errorf("the crowdvm-node from %s does not run on this machine (`version` failed); keeping %s", cfg.Server, exe)
		}
		if c, ok := version.Compare(got, Version); ok && c <= 0 && !o.Force {
			return res, fmt.Errorf("the server's crowdvm-node is %s, not newer than this one (%s); use --force to install it anyway", got, Version)
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
	var done []string
	for i, f := range todo {
		if err := swapIn(f.path, news[i]); err != nil {
			for _, p := range done {
				_ = os.Rename(p+".old", p)
			}
			return res, err
		}
		done = append(done, f.path)
	}
	news = nil
	res.Updated, res.Files = true, done
	return res, nil
}

// updatePaths: crowdvm-node first, then the companions that sit next to it
// as regular files. A symlink (npm's crowdvm) belongs to another installer.
func updatePaths(exe string) []string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	paths := []string{exe}
	for _, n := range []string{"crowdvm-guest", "crowdvm-linux-amd64", "crowdvm" + ext} {
		p := filepath.Join(filepath.Dir(exe), n)
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
			paths = append(paths, p)
		}
	}
	return paths
}

// findDownload maps a file next to the node to its download: crowdvm-node
// and crowdvm for this platform, the guest and in-sandbox CLI from
// linux/amd64 (any platform's copy of those is the same file).
func findDownload(l *client.DownloadList, base string) (client.DownloadFile, bool) {
	switch base {
	case "crowdvm-guest", "crowdvm-linux-amd64":
		if f, ok := l.Find("linux", "amd64", base); ok {
			return f, true
		}
	case "crowdvm", "crowdvm.exe":
	default: // this crowdvm-node, whatever its file is called
		base = "crowdvm-node"
		if runtime.GOOS == "windows" {
			base += ".exe"
		}
	}
	return l.Find(runtime.GOOS, runtime.GOARCH, base)
}

// newPath is where the download goes; Windows runs only .exe files.
func newPath(p string) string {
	if strings.HasSuffix(p, ".exe") {
		return strings.TrimSuffix(p, ".exe") + ".new.exe"
	}
	return p + ".new"
}

func download(ctx context.Context, cl *client.Client, want client.DownloadFile, dst string) error {
	body, err := cl.DownloadBinary(ctx, want.OS, want.Arch, want.Name)
	if err != nil {
		return fmt.Errorf("download %s: %w", want.Name, err)
	}
	defer body.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", want.Name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 || n != want.Size {
		return fmt.Errorf("the downloaded %s does not match the server's checksum (sha256 %s, %d bytes; want %s, %d bytes); nothing was installed",
			want.Name, got, n, want.SHA256, want.Size)
	}
	return nil
}

// swapIn moves p to p.old and next to p (undone when the second step fails).
func swapIn(p, next string) error {
	if err := os.Rename(p, p+".old"); err != nil {
		return err
	}
	if err := os.Rename(next, p); err != nil {
		_ = os.Rename(p+".old", p)
		return err
	}
	return nil
}

// rollback swaps each file with its .old, so a second rollback goes
// forward again.
func rollback(paths []string, res *UpdateResult) error {
	var done []string
	swap := func(p string) error {
		tmp := newPath(p)
		_ = os.Remove(tmp)
		if err := os.Rename(p, tmp); err != nil {
			return err
		}
		if err := os.Rename(p+".old", p); err != nil {
			_ = os.Rename(tmp, p)
			return err
		}
		return os.Rename(tmp, p+".old")
	}
	for _, p := range paths {
		if _, err := os.Stat(p + ".old"); err != nil {
			continue
		}
		if err := swap(p); err != nil {
			for _, d := range done {
				_ = swap(d)
			}
			return err
		}
		done = append(done, p)
	}
	if len(done) == 0 {
		return fmt.Errorf("no previous version to roll back to (no %s.old)", paths[0])
	}
	res.Updated, res.Files, res.Latest = true, done, ""
	return nil
}

// binaryVersion runs `path version` ("crowdvm-node v1.2.3 linux/amd64 go1.x");
// "" when it does not run. Older releases have no --version flag.
func binaryVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "crowdvm-node" {
		return ""
	}
	return f[1]
}

// Update is SelfUpdate for the running daemon (the local page, and
// auto-update): after a swap it refreshes the service unit and, when a
// service manager starts the daemon again, restarts it. Sandboxes keep
// running through the restart.
func (n *Node) Update(ctx context.Context, o UpdateOptions) (UpdateResult, error) {
	cfg := n.config()
	res, err := SelfUpdate(ctx, &cfg, o)
	if err != nil || !res.Updated {
		return res, err
	}
	if _, err := service.Refresh(io.Discard); err != nil {
		n.log.Warn("update: cannot refresh the service unit", "err", err)
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
// checks (cached for a while, as the page polls), POST /api/update installs
// and restarts the node.
func (n *Node) updateRoutes(m, ctl *http.ServeMux) {
	const ttl = 10 * time.Minute
	var mu sync.Mutex
	var at time.Time
	var last map[string]any
	m.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if last == nil || time.Since(at) > ttl || r.URL.Query().Get("refresh") != "" {
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
