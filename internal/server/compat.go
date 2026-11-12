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
	"github.com/crowdvm/crowdvm/internal/version"
)

// NodeUpdateCommand is what a node owner runs to update.
const NodeUpdateCommand = "crowdvm-node update"

// nodeUpdateCommand is how a node of generation gen updates: nodes from
// before generations (0) have no update command and are installed again.
func (s *Server) nodeUpdateCommand(gen int) string {
	if gen == 0 {
		return "curl -fsSL " + strings.TrimRight(s.Cfg.PublicURL, "/") + "/install | sh -s -- --node && crowdvm-node install-service"
	}
	return NodeUpdateCommand
}

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
	latest, cmd := s.latestRelease(), s.nodeUpdateCommand(gen)
	if at, ok := refusedLogAt.Load(n.ID); !ok || time.Since(at.(time.Time)) > 10*time.Minute { // it retries every few seconds
		refusedLogAt.Store(n.ID, time.Now())
		s.Log.Warn("node refused: crowdvm-node too old", "node", n.ID, "gen", gen, "minGen", compat.MinNodeGen)
	}
	w.Header().Set(compat.Header, strconv.Itoa(compat.ServerGen))
	api.WriteError(w, ErrD(426, "node_outdated",
		"This crowdvm-node is too old for this server. Update it with `"+cmd+"`"+
			map[bool]string{true: " (latest: " + latest + ")", false: ""}[latest != ""]+", then start it again.",
		map[string]any{"gen": gen, "minGen": compat.MinNodeGen, "latest": latest, "command": cmd}))
	return true
}

var refusedLogAt sync.Map // node id -> last "node refused" log line

// nodeUpdate is node n's update state. latest is the newest release the
// server offers ("" = unknown: then only the generation counts).
func (s *Server) nodeUpdate(n *Node) *proto.NodeUpdate {
	u := &proto.NodeUpdate{State: compat.NodeState(n.Info.Gen), Version: n.Info.Version, Latest: s.latestRelease(), Command: s.nodeUpdateCommand(n.Info.Gen)}
	switch u.State {
	case compat.StateOutdated:
		u.Message = "This crowdvm-node misses a required update: it gets no new sandboxes"
		if !s.currentNodeOnline() {
			u.Message = "This crowdvm-node misses a required update. It still takes sandboxes while no updated node is online"
		} else if ev := s.evacuation(n.ID); ev != nil && ev.Reason == "outdated" {
			d := ev.Deadline
			u.Deadline = &d
			u.Message += ", and sandboxes still on it move to other nodes at " + d.UTC().Format("Mon 15:04 UTC")
		}
		u.Message += ". Run `" + u.Command + "`."
	case compat.StateIncompatible:
		u.Message = "This crowdvm-node is too old for this server. Run `" + u.Command + "`."
	case compat.StateCurrent:
		if newerRelease(u.Latest, u.Version) {
			u.State = compat.StateAvailable
			u.Message = "crowdvm-node " + u.Latest + " is available. Run `" + NodeUpdateCommand + "`."
		}
	}
	return u
}

// syncOutdated starts or ends node id's 'outdated' evacuation to match its
// generation (after it connects or reports new info). An evacuation its
// owner or an admin started is left alone.
func (s *Server) syncOutdated(nodeID string) {
	n, err := s.NodeByID(nodeID)
	if err != nil {
		return
	}
	if !nodeOutdated(n) {
		if ev := s.evacuation(nodeID); ev != nil && ev.Reason == "outdated" {
			// Moves that waited for a layer only this node held boot now
			// (what the end of an evacuation does, evacuateStep).
			for _, sb := range s.failedEvacMoves(nodeID) {
				s.evacuationResume(sb, nodeID)
			}
			_, _ = s.DB.Exec(`DELETE FROM node_evacuations WHERE node_id=? AND reason='outdated'`, nodeID)
			s.Log.Info("node updated: outdated evacuation ended", "node", nodeID, "version", n.Info.Version)
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
	s.Log.Warn("node outdated: no new sandboxes; running ones move at the deadline", "node", nodeID, "gen", n.Info.Gen, "requiredGen", compat.RequiredNodeGen,
		"deadline", store.Time(deadline).UTC().Format(time.RFC3339))
	if !s.currentNodeOnline() {
		s.Log.Warn("no online node is current: outdated nodes keep taking sandboxes, and their grace period starts once one is updated")
	}
}

// currentNodeOnline: some online, enabled node runs a current generation.
// While none does, outdated nodes keep taking sandboxes and their
// evacuations wait: a server upgraded before its nodes must not stop the
// whole pool (the first rollout of generations, or a required update the
// nodes have not fetched yet).
func (s *Server) currentNodeOnline() bool {
	for id := range s.nodes.online() {
		if n, err := s.NodeByID(id); err == nil && !n.Disabled && !nodeOutdated(n) {
			return true
		}
	}
	return false
}

// outdatedStepEvery: how often an outdated node's evacuation is looked at
// after its deadline (an empty one stays until the node is updated).
var outdatedStepEvery = 30 * time.Second

// outdatedDue: the outdated evacuation ev of node id should act now.
func (s *Server) outdatedDue(nodeID string, ev *proto.Evacuation) bool {
	if time.Now().Before(ev.Deadline) || !s.currentNodeOnline() {
		return false
	}
	s.earnMu.Lock()
	defer s.earnMu.Unlock()
	if s.outdatedSteps == nil {
		s.outdatedSteps = map[string]time.Time{}
	}
	if time.Since(s.outdatedSteps[nodeID]) < outdatedStepEvery {
		return false
	}
	s.outdatedSteps[nodeID] = time.Now()
	return true
}

// slideOutdated (reaper): while no current node is online, outdated
// deadlines stay a full grace period away, so the grace starts when a
// node the sandboxes can move to exists.
func (s *Server) slideOutdated() {
	if s.currentNodeOnline() {
		return
	}
	grace := s.Cfg.NodeUpdateGrace
	slack := min(time.Minute, grace/2) // not a write every reaper pass
	now := store.Now()
	_, _ = s.DB.Exec(`UPDATE node_evacuations SET deadline=? WHERE reason='outdated' AND deadline<?`, now+grace.Milliseconds(), now+(grace-slack).Milliseconds())
}

// OutdatedNoticeDelay: an outdated node's owner and the owners of its
// sandboxes are told only if it is still outdated this long after it was
// first seen (a node with auto-update fixes itself within seconds).
var OutdatedNoticeDelay = 10 * time.Minute

// notifyOutdated (reaper) tells the people concerned about outdated nodes
// that did not update themselves in time, once per outdated period.
func (s *Server) notifyOutdated() {
	rows, err := s.DB.Query(`SELECT node_id, deadline FROM node_evacuations WHERE reason='outdated' AND notified=0 AND started_at<=?`,
		store.Now()-OutdatedNoticeDelay.Milliseconds())
	if err != nil {
		return
	}
	type due struct {
		id       string
		deadline int64
	}
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.deadline) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		if res, err := s.DB.Exec(`UPDATE node_evacuations SET notified=1 WHERE node_id=? AND notified=0`, d.id); err != nil {
			continue
		} else if k, _ := res.RowsAffected(); k == 0 {
			continue
		}
		n, err := s.NodeByID(d.id)
		if err != nil {
			continue
		}
		at := store.Time(d.deadline).UTC().Format("Mon Jan 2 15:04 UTC")
		if n.OwnerID != "" {
			s.walletNotify(n.OwnerID, n.OwnerID, "node_outdated", "Update your node "+n.Name,
				fmt.Sprintf("Your node %s runs crowdvm-node %s, which misses a required update. It gets no new sandboxes until it is updated, and sandboxes still on it move to other nodes from %s on. Update it on the machine with: %s",
					n.Name, firstNonEmpty(n.Info.Version, "(unknown)"), at, s.nodeUpdateCommand(n.Info.Gen)))
		}
		for _, sb := range s.sandboxesOn(d.id, "") {
			if sb.SnapshotsEnabled {
				s.notifySandbox(sb, "node_evacuation", "warning", 0, "Sandbox may move to another node",
					"The machine hosting "+sb.Name+" misses a required update. Unless its owner updates it, "+sb.Name+" is stopped at "+at+" and resumed on another node. Stop it before then to choose the moment.")
			} else {
				s.notifySandbox(sb, "node_evacuation", "warning", 0, "Sandbox may be erased",
					"The machine hosting "+sb.Name+" misses a required update. Unless its owner updates it, "+sb.Name+" is erased at "+at+" (it has snapshots off, so it cannot move). Copy out anything you need before then.")
			}
		}
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
	if gen >= compat.RequiredAPIGen && gen >= compat.MinAPIGen {
		return false
	}
	latest := s.latestRelease()
	if gen < compat.MinAPIGen {
		api.WriteError(w, ErrD(426, "client_outdated",
			"This client is too old for this server. Update the CrowdVM CLI (`crowdvm self-update`, or npm i -g crowdvm@latest) or SDK"+
				map[bool]string{true: " to " + latest, false: ""}[latest != ""]+".",
			map[string]any{"gen": gen, "minGen": compat.MinAPIGen, "latest": latest}))
		return true
	}
	v := "outdated"
	if latest != "" {
		v += "; latest=" + latest
	}
	w.Header().Set(compat.ClientStatusHeader, v)
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
	p := filepath.Join(s.DownloadsDir(), manifestName)
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

// newerRelease: latest is a release newer than have (nightlies ordered;
// "dev" and other non-releases never are).
func newerRelease(latest, have string) bool {
	c, ok := version.Compare(latest, have)
	return ok && c > 0
}
