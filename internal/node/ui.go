package node

import (
	"context"
	_ "embed"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
)

//go:embed ui.html
var uiHTML []byte

// UIHeader must be present on state-changing local API calls: browsers can't
// send it cross-origin without a CORS preflight (which we never answer).
const UIHeader = "X-Crowdvm-Ui"

// Status is the local status document (GET /api/status).
type Status struct {
	NodeID              string                   `json:"nodeId"`
	Name                string                   `json:"name"`
	Version             string                   `json:"version"`
	Server              string                   `json:"server"`
	Driver              string                   `json:"driver"`
	OS                  string                   `json:"os"`
	Arch                string                   `json:"arch"`
	DataDir             string                   `json:"dataDir"`
	DirectHost          string                   `json:"directHost,omitempty"`
	Connection          connState                `json:"connection"`
	Paused              bool                     `json:"paused"`
	Drained             bool                     `json:"drained"` // by the server's admin
	Benchmark           proto.Benchmark          `json:"benchmark"`
	BenchmarkRunning    bool                     `json:"benchmarkRunning"`
	Capacity            proto.Capacity           `json:"capacity"`
	Usage               proto.Usage              `json:"usage"`
	VMs                 []VMView                 `json:"vms"`
	Transfers           []proto.TransferProgress `json:"transfers"`
	Images              []string                 `json:"images"`
	SnapshotIntervalSec int                      `json:"snapshotIntervalSec"`
	Disk                DiskState                `json:"disk"`
}

type VMView struct {
	proto.VMStatus
	Size proto.Size `json:"size"`
}

func (n *Node) Status() Status {
	c := n.config()
	info := n.Info()
	n.mu.Lock()
	cs := n.connState
	vms := make([]VMView, 0, len(n.vms))
	for _, r := range n.vms {
		vms = append(vms, VMView{VMStatus: r.status, Size: r.spec.Size})
	}
	n.mu.Unlock()
	sort.Slice(vms, func(i, j int) bool { return vms[i].SandboxID < vms[j].SandboxID })
	return Status{
		NodeID: c.NodeID, Name: info.Name, Version: Version, Server: c.Server, Driver: info.Driver,
		OS: info.OS, Arch: info.Arch, DataDir: c.DataDir, DirectHost: c.DirectHost,
		Connection: cs, Paused: c.Paused, Drained: n.drained.Load(), Benchmark: info.Bench, BenchmarkRunning: n.benching.Load(),
		Capacity: info.Capacity, Usage: n.Usage(), VMs: vms, Transfers: n.Transfers(), Images: info.Images,
		SnapshotIntervalSec: int(max(n.snapshotInterval(), 0) / time.Second), Disk: n.Disk(),
	}
}

// SetPaused toggles accepting new sandboxes (persisted). Only "paused"
// changes in config.json: `crowdvm-node config set` may have changed other
// keys there since the daemon started, and saving the daemon's copy undid
// them (node-UI review).
func (n *Node) SetPaused(p bool) error {
	n.cfgMu.Lock()
	n.cfg.Paused = p
	disk, err := LoadConfig(n.cfg.DataDir)
	if err == nil {
		disk.Paused = p
		err = disk.Save()
	}
	n.cfgMu.Unlock()
	n.markInfoDirty()
	return err
}

// UIHandler serves the local status page and its small JSON API.
func (n *Node) UIHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		_, _ = w.Write(uiHTML)
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, n.Status())
	})
	m.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paused bool `json:"paused"`
		}
		if !decode(w, r, &req) {
			return
		}
		reply(w, map[string]bool{"paused": req.Paused}, n.SetPaused(req.Paused))
	})
	m.HandleFunc("POST /api/bench", func(w http.ResponseWriter, r *http.Request) {
		quick := r.URL.Query().Get("quick") != ""
		if r.URL.Query().Get("wait") == "" {
			go n.RunBenchmark(n.ctx, quick)
			api.WriteJSON(w, 202, map[string]bool{"ok": true})
			return
		}
		b, err := n.RunBenchmark(r.Context(), quick)
		reply(w, b, err)
	})
	m.HandleFunc("POST /api/images/{id}/push", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]bool{"ok": true}, n.PushImage(r.Context(), r.PathValue("id")))
	})
	m.HandleFunc("POST /api/images/{id}/pull", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]bool{"ok": true}, n.PullImage(r.Context(), "", r.PathValue("id")))
	})
	m.HandleFunc("POST /api/images/{id}/import", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if !decode(w, r, &req) {
			return
		}
		err := n.drv.Store().ImportImage(r.PathValue("id"), req.Path)
		if err == nil {
			n.markInfoDirty()
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	return localOnly(m)
}

// localOnly rejects DNS-rebinding hosts and cross-site writes.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		if host != "localhost" && net.ParseIP(strings.Trim(host, "[]")) == nil {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != "GET" && r.Header.Get(UIHeader) == "" {
			http.Error(w, "missing "+UIHeader+" header", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ServeUI listens on addr until ctx is done.
func (n *Node) ServeUI(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: n.UIHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	n.log.Info("status UI", "url", "http://"+ln.Addr().String())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
