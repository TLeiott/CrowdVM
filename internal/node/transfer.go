package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// HeaderSHA256 carries the hex sha256 of a blob on PUT/GET /node/v1/blobs/...
const HeaderSHA256 = "X-Crowdvm-Sha256"

// fetchCall deduplicates concurrent transfers of the same blob. The transfer
// runs on its own context, cancelled once no caller waits for it any more.
type fetchCall struct {
	done    chan struct{}
	err     error
	waiters int
	// abandoned: cancelled when the last waiter left; fn may still be running.
	abandoned bool
	cancel    context.CancelFunc
}

// once runs fn for key, or joins the run already in flight. fn's context does
// not end with any single caller's; each caller returns when its own ends.
func (n *Node) once(ctx context.Context, m map[string]*fetchCall, key string, fn func(context.Context) error) error {
	n.mu.Lock()
	c, ok := m[key]
	for ok && c.abandoned {
		// Cancelled with no waiter left, but fn has not returned yet: wait for it
		// so two runs of one transfer never overlap, then start afresh.
		n.mu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		n.mu.Lock()
		c, ok = m[key]
	}
	if !ok {
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c = &fetchCall{done: make(chan struct{}), cancel: cancel}
		m[key] = c
		go func() {
			c.err = fn(fctx)
			n.mu.Lock()
			if m[key] == c {
				delete(m, key)
			}
			n.mu.Unlock()
			cancel()
			close(c.done)
		}()
	}
	c.waiters++
	n.mu.Unlock()
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		n.mu.Lock()
		if c.waiters--; c.waiters == 0 {
			c.cancel()
			c.abandoned = true // stays in m until fn returns
		}
		n.mu.Unlock()
		return ctx.Err()
	}
}

// ensureChain makes snapID's whole chain (and its base image) available
// locally, pulling missing layers from the server.
func (n *Node) ensureChain(ctx context.Context, sandboxID, imageID, snapID string) error {
	st := n.drv.Store()
	var chain []proto.SnapshotRef
	var missing map[string]bool
	if snapID != "" {
		var err error
		if chain, missing, err = n.chainRefs(ctx, snapID); err != nil {
			return err
		}
		if imageID == "" {
			imageID = chain[len(chain)-1].ImageID
		}
	}
	if imageID != "" && !st.HasImage(imageID) {
		if err := n.PullImage(ctx, sandboxID, imageID); err != nil {
			return err
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !missing[chain[i].ID] {
			continue
		}
		if err := n.pullSnapshot(ctx, sandboxID, chain[i]); err != nil {
			return err
		}
	}
	return nil
}

// snapshotMeta: GET /node/v1/snapshots/{id}.
func (n *Node) snapshotMeta(ctx context.Context, id string) (proto.SnapshotRef, error) {
	var ref proto.SnapshotRef
	if _, err := n.waitConnected(ctx); err != nil {
		return ref, err
	}
	resp, err := n.serverDo(ctx, "GET", "/node/v1/snapshots/"+id, nil, nil)
	if err != nil {
		return ref, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		return ref, err
	}
	if ref.ID == "" {
		ref.ID = id
	}
	return ref, nil
}

// PullImage downloads base image id into the store (no-op when present).
func (n *Node) PullImage(ctx context.Context, sandboxID, id string) error {
	return n.once(ctx, n.fetches, "image/"+id, func(ctx context.Context) error {
		if n.drv.Store().HasImage(id) {
			return nil
		}
		path, err := n.download(ctx, sandboxID, "image", id, "")
		if err != nil {
			return err
		}
		defer os.Remove(path)
		if err := n.drv.Store().ImportImage(id, path); err != nil {
			return fmt.Errorf("import image %s: %w", id, err)
		}
		n.markInfoDirty() // Images changed
		return nil
	})
}

// PullSnapshot downloads snapshot id and its missing ancestors.
func (n *Node) PullSnapshot(ctx context.Context, id string) error {
	return n.ensureChain(ctx, "", "", id)
}

func (n *Node) pullSnapshot(ctx context.Context, sandboxID string, ref proto.SnapshotRef) error {
	return n.once(ctx, n.fetches, "snapshot/"+ref.ID, func(ctx context.Context) error {
		if n.drv.Store().HasSnapshot(ref.ID) {
			return nil
		}
		path, err := n.download(ctx, sandboxID, "snapshot", ref.ID, ref.SHA256)
		if err != nil {
			return err
		}
		defer os.Remove(path)
		if err := n.drv.Store().ImportSnapshot(ref, path); err != nil {
			return fmt.Errorf("import snapshot %s: %w", ref.ID, err)
		}
		return nil
	})
}

// download fetches /node/v1/blobs/{kind}/{id} into <data>/tmp, verifying the
// sha256 (wantSHA, else the response's X-Crowdvm-Sha256 header, if any).
func (n *Node) download(ctx context.Context, sandboxID, kind, id, wantSHA string) (string, error) {
	if _, err := n.waitConnected(ctx); err != nil {
		return "", err
	}
	resp, err := n.serverDo(ctx, "GET", "/node/v1/blobs/"+kind+"/"+id, nil, nil)
	if err != nil {
		return "", fmt.Errorf("download %s %s: %w", kind, id, err)
	}
	defer resp.Body.Close()
	if wantSHA == "" {
		wantSHA = resp.Header.Get(HeaderSHA256)
	}
	// Restores need the chain on local disk (LIFE-016): refuse a layer that
	// would eat into the reserve instead of failing half-way.
	if ds := n.Disk(); resp.ContentLength > 0 && resp.ContentLength > ds.FreeBytes-int64(ds.ReserveGiB)<<30 {
		return "", fmt.Errorf("download %s %s: not enough free disk (%d MiB needed, %d MiB free, %d GiB kept free)",
			kind, id, resp.ContentLength>>20, ds.FreeBytes>>20, ds.ReserveGiB)
	}
	dir := filepath.Join(n.cfg.DataDir, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, kind+"-"+id+"-*.part")
	if err != nil {
		return "", err
	}
	tp := &proto.TransferProgress{Kind: kind, ID: id, Direction: "download", Total: resp.ContentLength}
	h := sha256.New()
	pw := n.progress(sandboxID, tp)
	_, err = io.Copy(io.MultiWriter(f, h, pw), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && wantSHA != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
			err = fmt.Errorf("sha256 mismatch for %s %s: got %s, want %s", kind, id, got, wantSHA)
		}
	}
	pw.finish(err)
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// uploadAsync uploads a new snapshot (with retries) and emits
// snapshot.uploaded / snapshot.failed. The snapshot counts as uploading from
// now on (before the caller returns), until the upload ends. A periodic
// layer (one the server did not ask for) is also reported to the server
// before the caller returns, so it is recorded before a stop, save or fork
// can build on it; when that fails (offline), again before the upload.
func (n *Node) uploadAsync(sandboxID string, ref proto.SnapshotRef, periodic bool) {
	n.mu.Lock()
	n.uploading[ref.ID] = sandboxID
	n.mu.Unlock()
	if ref.SandboxID == "" {
		ref.SandboxID = sandboxID
	}
	reported := !periodic
	if periodic {
		if err := n.reportSnapshot(n.ctx, ref); err != nil {
			n.log.Warn("could not report a periodic snapshot; reporting it before its upload", "snapshot", ref.ID, "err", err)
		} else {
			reported = true
		}
	}
	go func() {
		defer n.uploadEnded(ref.ID)
		ctx := n.ctx
		if !reported {
			if _, err := n.waitConnected(ctx); err == nil {
				_ = n.reportSnapshot(ctx, ref)
			}
		}
		var err error
		for attempt := range 5 {
			// Superseded layers stay: the server deletes the ones nothing
			// needs (DELETE /v1/snapshots/{id}) and keeps saved snapshots,
			// so its snapshot locations stay true for this node.
			if err = n.UploadSnapshot(ctx, sandboxID, ref.ID); err == nil {
				return
			}
			var ge *goneError
			if errors.As(err, &ge) && ge.id == ref.ID {
				// Its sandbox or the snapshot was deleted meanwhile: the
				// server keeps nothing of it, so neither do we.
				n.log.Info("snapshot deleted on the server during its upload; erasing it here", "snapshot", ref.ID, "sandbox", sandboxID)
				_ = n.drv.Store().DeleteSnapshot(ref.ID)
				break
			}
			n.log.Warn("snapshot upload failed", "snapshot", ref.ID, "attempt", attempt+1, "err", err)
			if !sleepCtx(ctx, time.Duration(1<<attempt)*2*time.Second) {
				break
			}
		}
		n.uploadEnded(ref.ID)
		if ctx.Err() != nil {
			return // shutting down: not failed, the server asks for it again after the restart
		}
		n.emit(sandboxID, proto.EvSnapshotFailed, map[string]string{"snapshotId": ref.ID, "error": errString(err)})
	}()
}

// RequestUpload (POST /v1/snapshots/{id}/upload): the server asks for a
// layer it recorded but does not store, e.g. one whose upload a restart cut
// off. It uploads in the background (parents first), and is a no-op while
// the layer uploads. false = not in the local store.
func (n *Node) RequestUpload(id string) bool {
	ref, ok := n.drv.Store().SnapshotRef(id)
	if !ok {
		return false
	}
	n.mu.Lock()
	_, up := n.uploading[id]
	if !up {
		n.uploading[id] = ref.SandboxID
	}
	n.mu.Unlock()
	if !up {
		n.log.Info("server asked for a snapshot it does not store; uploading it", "snapshot", id, "sandbox", ref.SandboxID)
		n.uploadAsync(ref.SandboxID, ref, false)
	}
	return true
}

// uploadEnded drops id from the uploads in flight (before the event that
// tells the server, so it never sees the upload as still running).
func (n *Node) uploadEnded(id string) {
	n.mu.Lock()
	delete(n.uploading, id)
	n.mu.Unlock()
}

// reportSnapshot: POST /node/v1/snapshots (see proto.SnapshotRef). It does
// not wait for a connection.
func (n *Node) reportSnapshot(ctx context.Context, ref proto.SnapshotRef) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := n.serverDo(ctx, "POST", "/node/v1/snapshots", jsonBody(ref), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// serverStored reports whether the server stores snapshot id (false when
// it has no row for it, or a row whose upload has not ended).
func (n *Node) serverStored(ctx context.Context, id string) (bool, error) {
	if _, err := n.waitConnected(ctx); err != nil {
		return false, err
	}
	resp, err := n.serverDo(ctx, "GET", "/node/v1/snapshots/"+id, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var meta struct {
		Uploaded *bool `json:"uploaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return false, err
	}
	return meta.Uploaded == nil || *meta.Uploaded, nil
}

// goneError: the server refused an upload (410) because the snapshot or its
// sandbox was deleted.
type goneError struct{ id string }

func (e *goneError) Error() string { return "snapshot " + e.id + " was deleted on the server" }

// LocalSnapshots lists the snapshots in the local store and those still
// uploading (GET /v1/snapshots).
func (n *Node) LocalSnapshots(uploadingOnly bool) []proto.NodeSnapshot {
	n.mu.Lock()
	up := make(map[string]string, len(n.uploading))
	for id, sb := range n.uploading {
		up[id] = sb
	}
	n.mu.Unlock()
	out := []proto.NodeSnapshot{}
	st := n.drv.Store()
	for id, sb := range up {
		ref, _ := st.SnapshotRef(id)
		ns := nodeSnapshot(ref)
		ns.ID, ns.SandboxID, ns.Uploading = id, sb, true
		out = append(out, ns)
	}
	if uploadingOnly {
		return out
	}
	for _, ref := range st.Snapshots() {
		if _, ok := up[ref.ID]; !ok {
			ns := nodeSnapshot(ref)
			ns.SandboxID = ref.SandboxID
			out = append(out, ns)
		}
	}
	return out
}

func nodeSnapshot(ref proto.SnapshotRef) proto.NodeSnapshot {
	ns := proto.NodeSnapshot{ID: ref.ID, ParentID: ref.ParentID, ImageID: ref.ImageID, SizeBytes: ref.SizeBytes, Supersedes: ref.Supersedes}
	if !ref.CreatedAt.IsZero() {
		t := ref.CreatedAt
		ns.CreatedAt = &t
	}
	return ns
}

// UploadSnapshot uploads snapshot id (after any ancestors the server lacks)
// and emits snapshot.uploaded for each uploaded layer.
func (n *Node) UploadSnapshot(ctx context.Context, sandboxID, id string) error {
	return n.once(ctx, n.uploads, "snapshot/"+id, func(ctx context.Context) error {
		ref, ok := n.drv.Store().SnapshotRef(id)
		if !ok {
			return fmt.Errorf("snapshot %s not in local store", id)
		}
		if _, err := n.waitConnected(ctx); err != nil {
			return err
		}
		// The parent goes first. The server may have a row for it that is
		// not stored yet (a periodic layer still uploading, which this
		// joins, or one whose upload gave up).
		if ref.ParentID != "" {
			stored, err := n.serverStored(ctx, ref.ParentID)
			if err != nil {
				return err
			}
			if !stored {
				if err := n.UploadSnapshot(ctx, sandboxID, ref.ParentID); err != nil {
					return fmt.Errorf("parent %s: %w", ref.ParentID, err)
				}
			}
		}
		path, err := n.drv.Store().SnapshotPath(id)
		if err != nil {
			return err
		}
		// A sparse layer goes up as a copy of its allocated data only (bugs-r18-opus 2).
		if c, ok := n.drv.Store().(vm.Compactor); ok {
			if cp, err := c.CompactSnapshot(id, filepath.Join(n.cfg.DataDir, "tmp")); err != nil {
				n.log.Warn("could not compact a sparse snapshot; uploading it as is", "snapshot", id, "err", err)
			} else if cp != "" {
				defer os.Remove(cp)
				path = cp
			}
		}
		sum, size, err := fileSHA256(path)
		if err != nil {
			return err
		}
		ref.SHA256, ref.SizeBytes = sum, size
		refJSON, _ := json.Marshal(ref)
		hdr := http.Header{HeaderSHA256: {sum}, "X-Crowdvm-Snapshot": {string(refJSON)}}
		if err := n.upload(ctx, sandboxID, "snapshot", id, path, size, hdr); err != nil {
			if isStatus(err, http.StatusGone) {
				return &goneError{id: id}
			}
			return err
		}
		n.uploadEnded(id)
		n.emit(sandboxID, proto.EvSnapshotUploaded, ref)
		return nil
	})
}

// PushImage uploads base image id to the server.
func (n *Node) PushImage(ctx context.Context, id string) error {
	return n.once(ctx, n.uploads, "image/"+id, func(ctx context.Context) error {
		path, err := n.drv.Store().ImagePath(id)
		if err != nil {
			return err
		}
		sum, size, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if _, err := n.waitConnected(ctx); err != nil {
			return err
		}
		return n.upload(ctx, "", "image", id, path, size, http.Header{HeaderSHA256: {sum}})
	})
}

func (n *Node) upload(ctx context.Context, sandboxID, kind, id, path string, size int64, hdr http.Header) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := n.serverClient()
	if err != nil {
		return err
	}
	tp := &proto.TransferProgress{Kind: kind, ID: id, Direction: "upload", Total: size}
	pw := n.progress(sandboxID, tp)
	req, err := http.NewRequestWithContext(ctx, "PUT", serverBase+"/node/v1/blobs/"+kind+"/"+id, io.TeeReader(f, pw))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header = hdr.Clone()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.Do(req)
	if err == nil {
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			err = &httpError{Status: resp.StatusCode, Body: string(b)}
		}
		resp.Body.Close()
	}
	if err != nil {
		err = fmt.Errorf("upload %s %s: %w", kind, id, err)
	}
	pw.finish(err)
	return err
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

type progressWriter struct {
	n         *Node
	sandboxID string
	key       string
	mu        sync.Mutex
	tp        *proto.TransferProgress
	last      time.Time
}

func (n *Node) progress(sandboxID string, tp *proto.TransferProgress) *progressWriter {
	key := tp.Direction + "/" + tp.Kind + "/" + tp.ID
	n.mu.Lock()
	n.transfers[key] = tp
	n.mu.Unlock()
	pw := &progressWriter{n: n, sandboxID: sandboxID, key: key, tp: tp, last: time.Now()}
	n.log.Info("transfer started", "dir", tp.Direction, "kind", tp.Kind, "id", tp.ID, "bytes", tp.Total)
	n.emit(sandboxID, proto.EvTransfer, *tp)
	return pw
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n.mu.Lock()
	p.tp.Bytes += int64(len(b))
	snap := *p.tp
	p.n.mu.Unlock()
	if time.Since(p.last) >= time.Second {
		p.last = time.Now()
		p.n.emit(p.sandboxID, proto.EvTransfer, snap)
	}
	return len(b), nil
}

func (p *progressWriter) finish(err error) {
	p.n.mu.Lock()
	p.tp.Done = true
	p.tp.Error = errString(err)
	snap := *p.tp
	delete(p.n.transfers, p.key)
	p.n.mu.Unlock()
	p.n.log.Info("transfer finished", "dir", snap.Direction, "kind", snap.Kind, "id", snap.ID, "bytes", snap.Bytes, "err", snap.Error)
	p.n.emit(p.sandboxID, proto.EvTransfer, snap)
}

// Transfers lists transfers in progress.
func (n *Node) Transfers() []proto.TransferProgress {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []proto.TransferProgress{}
	for _, t := range n.transfers {
		out = append(out, *t)
	}
	return out
}

// removePartFiles deletes partial downloads left in <data>/tmp by a killed node.
func removePartFiles(dataDir string) {
	parts, _ := filepath.Glob(filepath.Join(dataDir, "tmp", "*.part"))
	for _, p := range parts {
		os.Remove(p)
	}
}
