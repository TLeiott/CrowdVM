package server

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

// DefaultNodeCacheTTL: how long nodes keep their copy of a stopped
// sandbox's disk once the server stores all of it (Config.NodeCacheTTL 0).
// A resume on that node within it needs no download; after it, the node
// downloads the layers again.
const DefaultNodeCacheTTL = 24 * time.Hour

func (s *Server) nodeCacheTTL() time.Duration {
	if s.Cfg.NodeCacheTTL == 0 {
		return DefaultNodeCacheTTL
	}
	return s.Cfg.NodeCacheTTL
}

var evicting atomic.Bool

// kickEviction runs evictStoppedCopies in the background (one pass at a
// time): it calls nodes and holds sandbox locks, so not on the maintenance
// loop.
func (s *Server) kickEviction() {
	if evicting.CompareAndSwap(false, true) {
		s.Go(func(ctx context.Context) {
			defer evicting.Store(false)
			s.evictStoppedCopies(ctx)
		})
	}
}

// evictStoppedCopies erases nodes' copies of the disks of sandboxes stopped
// longer than the node cache TTL whose every layer the server stores and
// verified. Layers something else may build on (a running or starting
// machine, a queued boot, a sandbox stopped more recently) stay; the node
// also keeps any file a local disk still builds on.
func (s *Server) evictStoppedCopies(ctx context.Context) {
	ttl := s.nodeCacheTTL()
	if ttl < 0 {
		return
	}
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND state='archived' AND head_snapshot_id IS NOT NULL
		AND COALESCE(stopped_at,0)>0 AND stopped_at<? AND COALESCE(json_extract(extra,'$.pendingBoot.parent'),'')=''`, store.T(time.Now().Add(-ttl)))
	if err != nil {
		return
	}
	var due []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			due = append(due, id)
		}
	}
	rows.Close()
	for _, id := range due {
		if ctx.Err() != nil {
			return
		}
		s.evictSandboxCopies(ctx, id, ttl)
	}
}

// inUseExcept: the layers in the chains of every sandbox except sbID (head,
// boot parent, queued boot). A running machine's newer layers build on its
// boot parent's chain.
func (s *Server) inUseExcept(sbID string) map[string]bool {
	keep := map[string]bool{}
	rows, err := s.DB.Query(`SELECT COALESCE(head_snapshot_id,''), COALESCE(json_extract(extra,'$.bootParent'),''), COALESCE(json_extract(extra,'$.pendingBoot.parent'),'')
		FROM sandboxes WHERE deleted_at IS NULL AND id<>?`, sbID)
	if err != nil {
		return nil
	}
	var roots []string
	for rows.Next() {
		var a, b, c string
		if rows.Scan(&a, &b, &c) == nil {
			roots = append(roots, a, b, c)
		}
	}
	rows.Close()
	for _, root := range roots {
		if root != "" && !keep[root] {
			for _, id := range s.snapshotChain(root) {
				keep[id] = true
			}
		}
	}
	return keep
}

func (s *Server) evictSandboxCopies(ctx context.Context, sbID string, ttl time.Duration) {
	unlock := s.sbLocks.Lock(sbID) // no resume or delete of it meanwhile (forks: bootingOn)
	defer unlock()
	sb, err := s.SandboxByID(sbID)
	if err != nil || sb.DeletedAt != nil || sb.State != "archived" || sb.HeadSnapshotID == "" || sb.StoppedAt == nil || time.Since(*sb.StoppedAt) < ttl {
		return
	}
	chain := s.snapshotChain(sb.HeadSnapshotID)
	for _, id := range chain {
		var safe bool
		if s.DB.QueryRow(`SELECT uploaded_at IS NOT NULL AND verified_at IS NOT NULL FROM snapshots WHERE id=? AND purged_at IS NULL`, id).Scan(&safe) != nil || !safe {
			return // the server lacks part of it: the nodes' copies are the disk
		}
	}
	keep := s.inUseExcept(sbID)
	if keep == nil {
		return
	}
	for _, id := range chain {
		if keep[id] {
			continue
		}
		for _, c := range s.holders(id) {
			if bootingOn(c.ID, id) { // a fork or deploy there skipped downloading it
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := c.Do(cctx, "DELETE", "/v1/snapshots/"+id, nil, nil)
			cancel()
			var ne *NodeError
			if err != nil && !(errors.As(err, &ne) && ne.Status == 404) {
				s.Log.Warn("node cache: could not erase a stopped sandbox's layer; retrying later", "node", c.ID, "sandbox", sbID, "snapshot", id, "err", err)
				return
			}
			_, _ = s.DB.Exec(`DELETE FROM snapshot_locations WHERE snapshot_id=? AND node_id=?`, id, c.ID)
			s.Log.Info("node cache: erased a stopped sandbox's layer the server stores", "node", c.ID, "sandbox", sbID, "snapshot", id)
		}
	}
}
