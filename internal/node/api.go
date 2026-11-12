package node

import (
	"bufio"
	"encoding/json"
	"errors"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// oneShotHandler serves h without the machine routes (/v1/vms...): a CLI
// command's short-lived session (WithSession) runs no machines. The server
// must not start one in a process that exits after its command; and a
// machine of the stopped daemon may still run (QEMU outlives it), so "not
// found" would be false. out_of_capacity makes the server place a boot
// elsewhere; other machine requests fail and are retried later.
func oneShotHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms" || strings.HasPrefix(r.URL.Path, "/v1/vms/") {
			api.WriteError(w, api.NewError(503, "out_of_capacity", "this node runs no machines: only a CLI command of it is connected", nil))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Handler is the node API served to the server on yamux streams.
func (n *Node) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, struct {
			proto.NodeInfo
			Usage proto.Usage `json:"usage"`
		}{n.Info(), n.Usage()})
	})
	m.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, n.Statuses())
	})
	m.HandleFunc("POST /v1/vms", n.handleCreate)
	m.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec := n.rec(r.PathValue("id"))
		if rec == nil {
			writeErr(w, errNotFound)
			return
		}
		n.mu.Lock()
		st, last := rec.status, rec.lastHealthy
		n.mu.Unlock()
		// Not confirmed by the monitor lately: ask the guest now, so recover
		// sees a hung guest at once (LIFE-023).
		if st.State == proto.VMRunning && st.GuestReady && time.Since(last) > healthInterval+time.Second &&
			!probeOK(n.probeGuest(r.Context(), rec, 3*time.Second)) {
			st.GuestReady = false
		}
		api.WriteJSON(w, 200, st)
	})
	m.HandleFunc("POST /v1/vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		var req proto.StopRequest
		if !decode(w, r, &req) {
			return
		}
		ref, err := n.StopVM(r.Context(), r.PathValue("id"), req)
		reply(w, ref, err)
	})
	m.HandleFunc("POST /v1/vms/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req proto.SnapshotRequest
		if !decode(w, r, &req) {
			return
		}
		ref, err := n.SnapshotVM(r.Context(), r.PathValue("id"), req.SnapshotID)
		reply(w, ref, err)
	})
	m.HandleFunc("POST /v1/vms/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		st, err := n.RestartVM(r.Context(), r.PathValue("id"))
		reply(w, st, err)
	})
	m.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := n.DestroyVM(r.Context(), r.PathValue("id"))
		reply(w, map[string]bool{"ok": true}, err)
	})
	m.HandleFunc("PUT /v1/vms/{id}/direct-ports", func(w http.ResponseWriter, r *http.Request) {
		var req proto.DirectPortsRequest
		if !decode(w, r, &req) {
			return
		}
		st, err := n.SetDirectPorts(r.PathValue("id"), req.Ports)
		if e := directStatus(err); e != nil {
			api.WriteError(w, e)
			return
		}
		if err != nil && st.SandboxID != "" { // some ports could not be published
			api.WriteError(w, api.NewError(409, "direct_port_unavailable", err.Error(), map[string]any{"status": st}))
			return
		}
		reply(w, st, err)
	})
	m.HandleFunc("/v1/vms/{id}/guest/{rest...}", n.handleGuest)
	m.HandleFunc("GET /v1/vms/{id}/tunnel", n.handleTunnel)
	m.HandleFunc("POST /v1/images/{id}/fetch", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		n.fetchHandler(w, r, func() error { return n.PullImage(n.ctx, "", id) })
	})
	m.HandleFunc("POST /v1/snapshots/{id}/fetch", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		n.fetchHandler(w, r, func() error { return n.PullSnapshot(n.ctx, id) })
	})
	m.HandleFunc("POST /v1/snapshots/{id}/upload", func(w http.ResponseWriter, r *http.Request) {
		if !n.RequestUpload(r.PathValue("id")) {
			api.WriteError(w, api.NewError(404, "not_found", "snapshot not in the local store", nil))
			return
		}
		api.WriteJSON(w, 202, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, map[string]any{"snapshots": n.LocalSnapshots(r.URL.Query().Get("uploading") == "true")})
	})
	m.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := n.drv.Store().DeleteSnapshot(r.PathValue("id"))
		reply(w, map[string]bool{"ok": true}, err)
	})
	n.inspectRoutes(m)
	return m
}

func (n *Node) handleCreate(w http.ResponseWriter, r *http.Request) {
	var spec proto.VMSpec
	if !decode(w, r, &spec) {
		return
	}
	st, err := n.CreateVM(spec)
	var ce *capacityError
	switch {
	case errors.As(err, &ce):
		u := n.Usage()
		cp, disk := n.capacity()
		api.WriteError(w, api.NewError(503, "out_of_capacity", ce.msg, map[string]any{"usage": u, "capacity": cp, "disk": disk}))
	case err != nil && st.SandboxID != "":
		api.WriteError(w, api.NewError(409, "vm_exists", err.Error(), nil))
	case err != nil:
		api.WriteError(w, api.NewError(400, "invalid_request", err.Error(), nil))
	default:
		api.WriteJSON(w, 202, st)
	}
}

// fetchHandler runs fn synchronously (200 when done), or in the background
// with ?async=1 (202).
func (n *Node) fetchHandler(w http.ResponseWriter, r *http.Request, fn func() error) {
	if r.URL.Query().Get("async") != "" {
		go func() {
			if err := fn(); err != nil {
				n.log.Warn("fetch failed", "path", r.URL.Path, "err", err)
			}
		}()
		api.WriteJSON(w, 202, map[string]bool{"ok": true})
		return
	}
	reply(w, map[string]bool{"ok": true}, fn())
}

// handleGuest reverse-proxies /v1/vms/{id}/guest/{rest} to guest /v1/{rest},
// flushing immediately (NDJSON streams) and passing Upgrade through.
func (n *Node) handleGuest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := n.rec(id)
	if rec == nil {
		writeErr(w, errNotFound)
		return
	}
	addr, err := n.drv.GuestAddr(id)
	if err != nil {
		api.WriteError(w, api.NewError(409, "machine_not_running", err.Error(), nil))
		return
	}
	prefix := "/v1/vms/" + id + "/guest"
	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if rest == r.URL.EscapedPath() { // escaped id; fall back to decoded path
		rest = "/" + r.PathValue("rest")
	}
	token := rec.spec.Guest.Token
	rp := &httputil.ReverseProxy{
		Transport:     n.guestHTTP,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u, err := url.Parse("http://" + addr + "/v1" + rest)
			if err != nil {
				u = &url.URL{Scheme: "http", Host: addr, Path: "/v1/"}
			}
			u.RawQuery = r.URL.RawQuery
			pr.Out.URL, pr.Out.Host = u, addr
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			api.WriteError(w, api.NewError(502, "guest_unreachable", err.Error(), nil))
		},
	}
	rp.ServeHTTP(w, r)
}

// handleTunnel: GET /v1/vms/{id}/tunnel?port=N with Upgrade: crowdvm-tunnel.
func (n *Node) handleTunnel(w http.ResponseWriter, r *http.Request) {
	rec := n.rec(r.PathValue("id"))
	if rec == nil {
		writeErr(w, errNotFound)
		return
	}
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port <= 0 || port > 65535 {
		api.WriteError(w, api.NewError(400, "invalid_request", "port must be 1..65535", nil))
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), proto.TunnelUpgrade) {
		api.WriteError(w, api.NewError(400, "invalid_request", "Upgrade: "+proto.TunnelUpgrade+" required", nil))
		return
	}
	g, err := n.dialGuestPort(r.Context(), rec, port)
	if err != nil {
		api.WriteError(w, api.NewError(502, "tunnel_failed", err.Error(), nil))
		return
	}
	c, rw, err := mux.HijackUpgrade(w)
	if err != nil {
		g.Close()
		return
	}
	mux.Join(withBuffered(c, rw.Reader), g)
}

func probeOK(_ guestHealth, ok bool) bool { return ok }

// withBuffered returns c, reading first whatever br already buffered.
func withBuffered(c net.Conn, br *bufio.Reader) net.Conn {
	if br == nil || br.Buffered() == 0 {
		return c
	}
	return &readerConn{Conn: c, r: io.MultiReader(io.LimitReader(br, int64(br.Buffered())), c)}
}

type readerConn struct {
	net.Conn
	r io.Reader
}

// Unwrap gives mux.Join the conn below, to abort it (bugs-r20-sonnet 4).
func (c *readerConn) Unwrap() net.Conn { return c.Conn }

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(v); err != nil && err != io.EOF {
		api.WriteError(w, api.NewError(400, "invalid_request", "invalid JSON: "+err.Error(), nil))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	api.WriteJSON(w, 200, v)
}

func writeErr(w http.ResponseWriter, err error) {
	// The driver's "vm not found" too: after a restart the server retries a
	// stop whose VM is already gone, and must read that as no machine.
	if errors.Is(err, errNotFound) || errors.Is(err, vm.ErrNotFound) {
		api.WriteError(w, api.NewError(404, "not_found", err.Error(), nil))
		return
	}
	if errors.Is(err, errNotConnected) {
		api.WriteError(w, api.NewError(503, "not_connected", err.Error(), nil))
		return
	}
	api.WriteError(w, api.NewError(500, "node_error", err.Error(), nil))
}
