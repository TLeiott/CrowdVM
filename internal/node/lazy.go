package node

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Lazy chain fetch (LIFE-013, Boat: "ready in seconds regardless of disk
// size"). When a VM's snapshot chain is not on the node and the driver can
// (vm.LazyStarter), the node boots it at once: missing layers are read on
// demand through a loopback blob proxy (HTTP Range requests forwarded to
// the server's blob store over the node session) while the node downloads
// them in the background. VMStatus.Hydrating is true until every layer is
// imported and the driver switched to the local files; then the guest's
// startup playbook recording ends (LIFE-018).
//
// The proxy listens on 127.0.0.1 on a port kept in <data dir>/lazy.json
// (so VMs that survive a node restart keep reading), and every URL carries
// an HMAC of the blob id, so other local users cannot read blobs through it.

type lazyProxy struct {
	key  []byte
	base string // http://127.0.0.1:port
}

type lazyState struct {
	Port int    `json:"port"`
	Key  string `json:"key"`
}

// lazyURLs starts the blob proxy on first use.
func (n *Node) lazyURLs() (*lazyProxy, error) {
	n.lazyMu.Lock()
	defer n.lazyMu.Unlock()
	if n.lazy != nil {
		return n.lazy, nil
	}
	p := filepath.Join(n.cfg.DataDir, "lazy.json")
	var st lazyState
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	key, _ := hex.DecodeString(st.Key)
	if len(key) < 16 {
		key = make([]byte, 32)
		rand.Read(key)
	}
	var ln net.Listener
	var err error
	if st.Port > 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(st.Port))
	}
	if ln == nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, err
		}
	}
	st = lazyState{Port: ln.Addr().(*net.TCPAddr).Port, Key: hex.EncodeToString(key)}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	lp := &lazyProxy{key: key, base: "http://" + ln.Addr().String()}
	srv := &http.Server{Handler: n.lazyHandler(lp), ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	go func() { <-n.ctx.Done(); srv.Close() }()
	n.lazy = lp
	return lp, nil
}

func (p *lazyProxy) token(kind, id string) string {
	m := hmac.New(sha256.New, p.key)
	m.Write([]byte(kind + "/" + id))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

func (p *lazyProxy) url(kind, id string) string {
	return p.base + "/b/" + kind + "/" + id + "/" + p.token(kind, id)
}

var lazyRetry = 90 * time.Second // the server may be briefly unreachable

func (n *Node) lazyHandler(p *lazyProxy) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /b/{kind}/{id}/{tok}", func(w http.ResponseWriter, r *http.Request) {
		kind, id := r.PathValue("kind"), r.PathValue("id")
		if kind != "snapshot" || !hmac.Equal([]byte(r.PathValue("tok")), []byte(p.token(kind, id))) {
			http.Error(w, "forbidden", 403)
			return
		}
		hdr := http.Header{}
		if v := r.Header.Get("Range"); v != "" {
			hdr.Set("Range", v)
		}
		var resp *http.Response
		var err error
		for deadline := time.Now().Add(lazyRetry); ; {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			_, err = n.waitConnected(ctx)
			cancel()
			if err == nil {
				resp, err = n.serverDo(r.Context(), r.Method, "/node/v1/blobs/"+kind+"/"+id, nil, hdr)
			}
			var he *httpError
			if err == nil || errors.As(err, &he) || r.Context().Err() != nil || time.Now().After(deadline) {
				break
			}
			sleepCtx(r.Context(), 500*time.Millisecond)
		}
		if err != nil {
			status := 502
			var he *httpError
			if errors.As(err, &he) {
				status = he.Status
			}
			http.Error(w, err.Error(), status)
			return
		}
		defer resp.Body.Close()
		for _, k := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Content-Type", "Last-Modified", "Etag"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		if w.Header().Get("Accept-Ranges") == "" {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			io.Copy(w, resp.Body)
		}
	})
	return m
}

// chainRefs lists snapID's chain (top first) and which layers are not in
// the store. Unlike a plain walk it looks past local layers, which may sit
// on missing ones after a lazy run.
func (n *Node) chainRefs(ctx context.Context, snapID string) ([]proto.SnapshotRef, map[string]bool, error) {
	st := n.drv.Store()
	var chain []proto.SnapshotRef
	missing := map[string]bool{}
	for id := snapID; id != ""; {
		ref, ok := st.SnapshotRef(id)
		if !ok || !st.HasSnapshot(id) {
			var err error
			if ref, err = n.snapshotMeta(ctx, id); err != nil {
				return nil, nil, fmt.Errorf("snapshot %s: %w", id, err)
			}
			missing[id] = true
		}
		chain = append(chain, ref)
		id = ref.ParentID
		if len(chain) > 1000 {
			return nil, nil, fmt.Errorf("snapshot chain of %s too long", snapID)
		}
	}
	return chain, missing, nil
}

// bootLazy starts r on a chain with missing layers through the driver's
// lazy path. ok=false: not possible here (the caller pulls the chain).
func (n *Node) bootLazy(ctx context.Context, r *vmRec) (ok bool, err error) {
	lz, can := n.drv.(vm.LazyStarter)
	if !can || !lz.LazyChain() || n.config().NoLazyChain || r.spec.ParentID == "" {
		return false, nil
	}
	chain, missing, err := n.chainRefs(ctx, r.spec.ParentID)
	if err != nil || len(missing) == 0 {
		return false, nil // the full pull reports errors
	}
	imageID := r.spec.ImageID
	if imageID == "" {
		imageID = chain[len(chain)-1].ImageID
	}
	if imageID == "" {
		return false, nil
	}
	if !n.drv.Store().HasImage(imageID) {
		if err := n.PullImage(ctx, r.id, imageID); err != nil {
			return true, err
		}
	}
	p, err := n.lazyURLs()
	if err != nil {
		n.log.Warn("lazy chain: blob proxy", "err", err)
		return false, nil
	}
	remote := map[string]string{}
	var todo []proto.SnapshotRef // bottom first
	for i := len(chain) - 1; i >= 0; i-- {
		if missing[chain[i].ID] {
			remote[chain[i].ID] = p.url("snapshot", chain[i].ID)
			todo = append(todo, chain[i])
		}
	}
	if _, ok := n.drv.Status(r.id); ok {
		_ = n.drv.Destroy(ctx, r.id)
	}
	spec := r.spec
	spec.ImageID = imageID
	if err := lz.StartLazy(n.ctx, spec, chain, remote); err != nil {
		return true, err
	}
	n.log.Info("lazy boot", "sandbox", r.id, "remoteLayers", len(todo), "layers", len(chain))
	n.setState(r, func(s *proto.VMStatus) { s.Hydrating = true })
	go n.hydrate(r, lz, todo)
	return true, nil
}

// hydrate downloads the missing layers (bottom first), then switches the
// VM to them and ends the guest's playbook recording.
func (n *Node) hydrate(r *vmRec, lz vm.LazyStarter, todo []proto.SnapshotRef) {
	ctx := n.ctx
	for attempt := 0; ; attempt++ {
		var err error
		for _, ref := range todo {
			if err = n.pullSnapshot(ctx, r.id, ref); err != nil {
				break
			}
		}
		if err == nil {
			break
		}
		n.log.Warn("lazy chain: download failed", "sandbox", r.id, "attempt", attempt+1, "err", err)
		if attempt >= 9 || !sleepCtx(ctx, time.Duration(min(1<<attempt, 60))*time.Second) {
			return // the VM keeps reading from the server
		}
	}
	if err := lz.Hydrated(ctx, r.id); err != nil {
		if !errors.Is(err, vm.ErrNotFound) {
			n.log.Warn("lazy chain: switch to local layers failed", "sandbox", r.id, "err", err)
		}
		return
	}
	n.setState(r, func(s *proto.VMStatus) { s.Hydrating = false })
	n.log.Info("chain hydrated", "sandbox", r.id)
	if addr, err := n.drv.GuestAddr(r.id); err == nil { // LIFE-018: the playbook covers the download
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		if resp, err := doReq(cctx, &http.Client{Transport: n.guestHTTP}, "POST", "http://"+addr+"/v1/internal/playbook/finish", nil,
			http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}}); err == nil {
			resp.Body.Close()
		}
		cancel()
	}
}

// resumeLazy continues a lazy chain of a VM adopted after a node restart:
// the blob proxy comes back on its port (lazy.json), missing layers are
// downloaded again, and the driver switches once they are local.
func (n *Node) resumeLazy(r *vmRec) {
	lz, ok := n.drv.(vm.LazyStarter)
	if !ok || !lz.Lazy(r.id) || r.spec.ParentID == "" {
		return
	}
	chain, missing, err := n.chainRefs(n.ctx, r.spec.ParentID)
	if err != nil {
		n.log.Warn("lazy chain: resume", "sandbox", r.id, "err", err)
		return
	}
	if len(missing) == 0 {
		_ = lz.Hydrated(n.ctx, r.id) // no-op unless the switch was still due
		return
	}
	var todo []proto.SnapshotRef
	for i := len(chain) - 1; i >= 0; i-- {
		if missing[chain[i].ID] {
			todo = append(todo, chain[i])
		}
	}
	n.setState(r, func(s *proto.VMStatus) { s.Hydrating = true })
	n.hydrate(r, lz, todo)
}
