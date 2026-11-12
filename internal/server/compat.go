package server

// Version compatibility with nodes and clients (internal/compat).
//
// Nodes: GET /node/v1/connect carries the node's generation. Below
// compat.MinNodeGen the connection is refused with 426 node_outdated. Below
// compat.RequiredNodeGen the node is outdated: the scheduler skips it, its
// owner is told, and an evacuation with reason 'outdated' moves what still
// runs there at first-seen + Config.NodeUpdateGrace. The evacuation ends by
// itself when the node comes back updated. Every heartbeat reply tells the
// node its NodeUpdate (also in the node JSON for the dashboard).
//
// Clients: the CLI and SDKs send their generation on every request. Below
// compat.MinAPIGen the server answers 426 client_outdated; below
// compat.RequiredAPIGen it adds the X-Crowdvm-Client-Status header.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// NodeUpdateCommand is what a node owner runs to update.
const NodeUpdateCommand = "crowdvm-node update"

// DefaultNodeUpdateGrace: how long an outdated node keeps its running
// sandboxes (Config.NodeUpdateGrace).
const DefaultNodeUpdateGrace = 72 * time.Hour

// headerGen reads a generation header; ok is false when it is absent or
// not a number.
func headerGen(r *http.Request) (int, bool) {
	g, err := strconv.Atoi(strings.TrimSpace(r.Header.Get(compat.Header)))
	return g, err == nil
}

// refuseIncompatibleNode answers 426 when the connecting node is below
// compat.MinNodeGen (no header: generation 0). It reports whether it did.
func (s *Server) refuseIncompatibleNode(w http.ResponseWriter, r *http.Request, n *Node) bool {
	gen, _ := headerGen(r)
	if gen >= compat.MinNodeGen {
		return false
	}
	latest := s.latestRelease()
	s.Log.Warn("node refused: crowdvm-node too old", "node", n.ID, "gen", gen, "minGen", compat.MinNodeGen)
	w.Header().Set(compat.Header, strconv.Itoa(compat.ServerGen))
	api.WriteError(w, ErrD(426, "node_outdated",
		"This crowdvm-node is too old for this server. Update it with `"+NodeUpdateCommand+"`"+
			map[bool]string{true: " (latest: " + latest + ")", false: ""}[latest != ""]+", then start it again.",
		map[string]any{"gen": gen, "minGen": compat.MinNodeGen, "latest": latest, "command": NodeUpdateCommand}))
	return true
}

// nodeUpdate is node n's update state. latest is the newest release the
// server offers ("" = unknown: then only the generation counts).
func (s *Server) nodeUpdate(n *Node) *proto.NodeUpdate {
	u := &proto.NodeUpdate{State: compat.NodeState(n.Info.Gen), Version: n.Info.Version, Latest: s.latestRelease(), Command: NodeUpdateCommand}
	switch u.State {
	case compat.StateOutdated:
		u.Message = "This crowdvm-node misses a required update: it gets no new sandboxes"
		if ev := s.evacuation(n.ID); ev != nil && s.evacuationReason(n.ID) == "outdated" {
			d := ev.Deadline
			u.Deadline = &d
			u.Message += ", and sandboxes still on it move to other nodes at " + d.UTC().Format("Mon 15:04 UTC")
		}
		u.Message += ". Run `" + NodeUpdateCommand + "`."
	case compat.StateIncompatible:
		u.Message = "This crowdvm-node is too old for this server. Run `" + NodeUpdateCommand + "`."
	case compat.StateCurrent:
		if newerRelease(u.Latest, u.Version) {
			u.State = compat.StateAvailable
			u.Message = "crowdvm-node " + u.Latest + " is available. Run `" + NodeUpdateCommand + "`."
		}
	}
	return u
}

// evacuationReason: 'owner', 'outdated' or "" (none).
func (s *Server) evacuationReason(nodeID string) string {
	var r string
	_ = s.DB.QueryRow(`SELECT reason FROM node_evacuations WHERE node_id=?`, nodeID).Scan(&r)
	return r
}

// syncOutdated starts or ends node id's 'outdated' evacuation to match its
// generation (after it connects or reports new info). An evacuation its
// owner or an admin started is left alone.
func (s *Server) syncOutdated(nodeID string) {
	n, err := s.NodeByID(nodeID)
	if err != nil {
		return
	}
	outdated := compat.NodeState(n.Info.Gen) != compat.StateCurrent
	if !outdated {
		if res, err := s.DB.Exec(`DELETE FROM node_evacuations WHERE node_id=? AND reason='outdated'`, nodeID); err == nil {
			if k, _ := res.RowsAffected(); k > 0 {
				s.Log.Info("node updated: outdated evacuation ended", "node", nodeID, "version", n.Info.Version)
			}
		}
		return
	}
	now := store.Now()
	deadline := now + s.Cfg.NodeUpdateGrace.Milliseconds()
	res, err := s.DB.Exec(`INSERT INTO node_evacuations(node_id,started_at,deadline,reason) VALUES(?,?,?,'outdated') ON CONFLICT(node_id) DO NOTHING`, nodeID, now, deadline)
	if err != nil {
		return
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return // already evacuating (outdated, or by its owner)
	}
	at := store.Time(deadline).UTC().Format("Mon Jan 2 15:04 UTC")
	s.Log.Warn("node outdated: no new sandboxes; running ones move at the deadline", "node", nodeID, "gen", n.Info.Gen, "requiredGen", compat.RequiredNodeGen, "deadline", at)
	if n.OwnerID != "" {
		s.walletNotify(n.OwnerID, n.OwnerID, "node_outdated", "Update your node "+n.Name,
			fmt.Sprintf("Your node %s runs crowdvm-node %s, which misses a required update. It gets no new sandboxes until it is updated, and sandboxes still on it move to other nodes at %s. Update it on the machine with: %s",
				n.Name, firstNonEmpty(n.Info.Version, "(unknown)"), at, NodeUpdateCommand))
	}
}

// nodeOutdated: the scheduler must not place new sandboxes on n.
func nodeOutdated(n *Node) bool { return compat.NodeState(n.Info.Gen) != compat.StateCurrent }

// clientGate (Router, /api/ only) refuses clients below compat.MinAPIGen
// and marks those below compat.RequiredAPIGen. A request without the
// header is from our own clients when their User-Agent says so (older
// builds: generation 0); anything else (curl, browsers) is not judged.
func (s *Server) clientGate(w http.ResponseWriter, r *http.Request) bool {
	gen, ok := headerGen(r)
	if !ok {
		if !ownClientUA(r.UserAgent()) {
			return false
		}
		gen = 0
	}
	w.Header().Set(compat.Header, strconv.Itoa(compat.ServerGen))
	latest := s.latestRelease()
	if gen < compat.MinAPIGen {
		api.WriteError(w, ErrD(426, "client_outdated",
			"This client is too old for this server. Update the CrowdVM CLI (`crowdvm self-update`, or npm i -g crowdvm@latest) or SDK"+
				map[bool]string{true: " to " + latest, false: ""}[latest != ""]+".",
			map[string]any{"gen": gen, "minGen": compat.MinAPIGen, "latest": latest}))
		return true
	}
	if gen < compat.RequiredAPIGen {
		v := "outdated"
		if latest != "" {
			v += "; latest=" + latest
		}
		w.Header().Set(compat.ClientStatusHeader, v)
	}
	return false
}

func ownClientUA(ua string) bool {
	for _, p := range []string{"crowdvm-cli/", "crowdvm-js/", "crowdvm-python/", "crowdvm-py/"} {
		if strings.HasPrefix(strings.ToLower(ua), p) {
			return true
		}
	}
	return false
}

// latestRelease is the version of the release in the downloads dir
// (manifest.json written by scripts/dist.sh), "" when there is none. Read
// again only when the file changes.
func (s *Server) latestRelease() string {
	p := filepath.Join(s.DownloadsDir(), "manifest.json")
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	latestMu.Lock()
	defer latestMu.Unlock()
	if c, ok := latestCache[p]; ok && c.mod.Equal(st.ModTime()) && c.size == st.Size() {
		return c.version
	}
	var m struct {
		Version string `json:"version"`
	}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	latestCache[p] = latestEntry{st.ModTime(), st.Size(), m.Version}
	return m.Version
}

type latestEntry struct {
	mod     time.Time
	size    int64
	version string
}

var (
	latestMu    sync.Mutex
	latestCache = map[string]latestEntry{}
)

// newerRelease: latest is a release newer than have.
func newerRelease(latest, have string) bool {
	l, ok1 := parseSemver(latest)
	h, ok2 := parseSemver(have)
	return ok1 && ok2 && semverLess(h, l)
}
