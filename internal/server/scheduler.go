package server

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Placement is the CrowdVM extension field "placement" of create, resume and
// fork: hard requirements on the node that hosts the sandbox.
type Placement struct {
	GPU     bool              `json:"gpu,omitempty"`     // node must have a GPU
	Nested  bool              `json:"nested,omitempty"`  // node must support nested virtualization
	Labels  map[string]string `json:"labels,omitempty"`  // node labels that must match exactly
	MinTier string            `json:"minTier,omitempty"` // bronze < silver < gold < platinum
	// Categories: minimum benchmark level per dimension, e.g. {"cpu":"high","disk":"medium"}.
	Categories map[string]string `json:"categories,omitempty"`
	Node       string            `json:"node,omitempty"` // pin to a node id or name
	// Trust (SEC-014): a node's owner can read the disks of the VMs it
	// hosts. "own" = only nodes of the sandbox's owner (for an organization
	// sandbox: of its members); "trusted" = those plus nodes an
	// administrator marked trusted; "" = any node.
	Trust string `json:"trust,omitempty"`
}

func (p Placement) empty() bool {
	return !p.GPU && !p.Nested && len(p.Labels) == 0 && p.MinTier == "" && len(p.Categories) == 0 && p.Node == "" && p.Trust == ""
}

var tierRank = map[string]int{"bronze": 1, "silver": 2, "gold": 3, "platinum": 4}
var levelRank = map[string]int{"none": 0, "low": 1, "medium": 2, "high": 3, "extreme": 4}

func (p Placement) validate() error {
	if p.Trust != "" && p.Trust != "own" && p.Trust != "trusted" && p.Trust != "any" {
		return Err(400, "invalid_placement", "placement.trust must be own, trusted or any.")
	}
	if p.MinTier != "" {
		if _, ok := tierRank[p.MinTier]; !ok {
			return Err(400, "invalid_placement", "placement.minTier must be bronze, silver, gold or platinum.")
		}
	}
	for k, v := range p.Categories {
		if _, ok := levelRank[v]; !ok {
			return Err(400, "invalid_placement", "placement.categories."+k+" must be low, medium, high or extreme.")
		}
	}
	return nil
}

// schedReq describes what a sandbox needs from a node.
type schedReq struct {
	Size      api.SizeSpec
	Placement Placement
	Chain     []string // snapshot chain, newest first (locality)
	ImageID   string
	Only      string // must run on this node (fork of a running sandbox)
	Exclude   string
	Owners    []string // users whose nodes are "own" (placement.trust)
}

// reservedOn sums the sizes of the sandboxes the server placed on a node.
func (s *Server) reservedOn(nodeID string) proto.Usage {
	var u proto.Usage
	rows, err := s.DB.Query(`SELECT size FROM sandboxes WHERE node_id=? AND deleted_at IS NULL AND state IN ('provisioning','provisioned','cloning','ready','idle','running','archiving')`, nodeID)
	if err != nil {
		return u
	}
	defer rows.Close()
	for rows.Next() {
		var sz string
		if rows.Scan(&sz) != nil {
			continue
		}
		spec, _ := api.SizeByName(sz)
		u.VCPU += spec.VCPU
		u.MemoryMiB += spec.MemoryMiB
		u.DiskGiB += spec.DiskGiB
		u.VMs++
	}
	return u
}

type candidate struct {
	node   *Node
	score  float64
	reason string
}

// matchReason returns "" when n satisfies the placement, else why not.
// owners: the users whose nodes count as the sandbox owner's own.
func matchReason(n *Node, p Placement, owners []string) string {
	if p.Node != "" && p.Node != n.ID && !strings.EqualFold(p.Node, n.Name) {
		return "not the pinned node"
	}
	own := n.OwnerID != "" && slices.Contains(owners, n.OwnerID)
	switch {
	case p.Trust == "own" && !own:
		return "not one of your nodes"
	case p.Trust == "trusted" && !own && !n.Trusted:
		return "not a trusted node"
	}
	// A detected GPU is not enough: the node must hand one to the VM
	// (VFIO passthrough configured and validated by the node owner).
	if p.GPU && len(n.Info.GPUs) == 0 {
		if len(n.Info.Bench.GPUs) > 0 {
			return "GPU present but not configured for passthrough"
		}
		return "no GPU"
	}
	if p.Nested && !n.Info.Bench.Virtualization.Nested {
		return "no nested virtualization"
	}
	labels := n.AllLabels()
	for k, v := range p.Labels {
		if labels[k] != v {
			return "label " + k + " does not match"
		}
	}
	if p.MinTier != "" && tierRank[n.Info.Bench.Tier] < tierRank[p.MinTier] {
		return "tier below " + p.MinTier
	}
	for k, v := range p.Categories {
		if levelRank[n.Info.Bench.Categories[k]] < levelRank[v] {
			return k + " below " + v
		}
	}
	return ""
}

// fits reports whether size fits into the node's shared capacity.
func fits(n *Node, used proto.Usage, sz api.SizeSpec) (bool, float64) {
	c := n.Info.Capacity
	if c.VCPU <= 0 || c.MemoryMiB <= 0 {
		return false, 0
	}
	if used.VCPU+sz.VCPU > c.VCPU || used.MemoryMiB+sz.MemoryMiB > c.MemoryMiB {
		return false, 0
	}
	if c.DiskGiB > 0 && used.DiskGiB+sz.DiskGiB > c.DiskGiB {
		return false, 0
	}
	if c.MaxSandboxes > 0 && used.VMs+1 > c.MaxSandboxes {
		return false, 0
	}
	freeMem := float64(c.MemoryMiB-used.MemoryMiB-sz.MemoryMiB) / float64(c.MemoryMiB)
	freeCPU := float64(c.VCPU-used.VCPU-sz.VCPU) / float64(c.VCPU)
	return true, freeMem*100 + freeCPU*50
}

// pickNode chooses the best online node for req (caller holds schedMu).
func (s *Server) pickNode(req schedReq) (*Node, error) {
	online := s.nodes.online()
	if len(online) == 0 {
		return nil, ErrD(503, "out_of_capacity", "No node is online right now.", map[string]any{"retryAfterSeconds": s.Cfg.RetryAfter, "reason": "no_nodes"})
	}
	located := map[string]int{} // node -> best chain depth
	if len(req.Chain) > 0 {
		for depth, id := range req.Chain {
			rows, err := s.DB.Query(`SELECT node_id FROM snapshot_locations WHERE snapshot_id=?`, id)
			if err != nil {
				continue
			}
			for rows.Next() {
				var nid string
				if rows.Scan(&nid) == nil {
					if _, ok := located[nid]; !ok {
						located[nid] = depth
					}
				}
			}
			rows.Close()
		}
	}
	var cands []candidate
	var reasons []string
	matching := 0
	for id := range online {
		n, err := s.NodeByID(id)
		if err != nil || n.Disabled {
			continue
		}
		if req.Only != "" && id != req.Only {
			continue
		}
		if id == req.Exclude {
			continue
		}
		if n.Info.Paused { // a new sandbox only: the owner paused the node
			reasons = append(reasons, n.Name+": paused by its owner")
			continue
		}
		if why := matchReason(n, req.Placement, req.Owners); why != "" {
			reasons = append(reasons, n.Name+": "+why)
			continue
		}
		matching++
		if req.Placement.GPU && s.gpusInUse(id) >= len(n.Info.GPUs) {
			reasons = append(reasons, n.Name+": all GPUs in use")
			continue
		}
		ok, spread := fits(n, s.reservedOn(id), req.Size)
		if !ok {
			reasons = append(reasons, n.Name+": full")
			continue
		}
		score := spread + float64(n.Info.Bench.Score)/20
		if d, ok := located[id]; ok {
			if d == 0 {
				score += 1000
			} else {
				score += 500 - float64(d)
			}
		}
		if req.ImageID != "" && slices.Contains(n.Info.Images, req.ImageID) {
			score += 50
		}
		cands = append(cands, candidate{node: n, score: score})
	}
	if len(cands) == 0 {
		sort.Strings(reasons)
		reason := "full"
		msg := fmt.Sprintf("No node has room for a %s sandbox right now.", req.Size.Name)
		if matching == 0 {
			reason = "no_matching_node"
			msg = "No online node matches the placement requirements."
			if req.Only != "" {
				msg = "The node that holds this sandbox is offline or full."
			}
		}
		return nil, ErrD(503, "out_of_capacity", msg, map[string]any{"retryAfterSeconds": s.Cfg.RetryAfter, "reason": reason, "nodes": reasons})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].node.ID < cands[j].node.ID
	})
	return cands[0].node, nil
}

// ownersFor lists the users whose nodes are "own" for a sandbox of
// owner (and org): the owner, plus every member of the organization.
func (s *Server) ownersFor(owner, org string) []string {
	out := []string{owner}
	if org == "" {
		return out
	}
	rows, err := s.DB.Query(`SELECT user_id FROM org_members WHERE org_id=?`, org)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil && u != owner {
			out = append(out, u)
		}
	}
	return out
}

// placeSandbox picks a node and atomically assigns it with the new state.
func (s *Server) placeSandbox(sbID, state string, req schedReq) (*Node, error) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	n, err := s.pickNode(req)
	if err != nil {
		return nil, err
	}
	spec := req.Size
	_, err = s.DB.Exec(`UPDATE sandboxes SET node_id=?, state=?, size=?, updated_at=? WHERE id=?`, n.ID, state, spec.Name, store.Now(), sbID)
	return n, err
}

// canEverFit reports per size whether some online node could host it.
func (s *Server) sizeAvailability() map[string]string {
	out := map[string]string{}
	online := s.nodes.online()
	for _, sz := range api.Sizes {
		reason := "no node is online"
		for id := range online {
			n, err := s.NodeByID(id)
			if err != nil || n.Disabled {
				continue
			}
			c := n.Info.Capacity
			fits := c.VCPU >= sz.VCPU && c.MemoryMiB >= sz.MemoryMiB && (c.DiskGiB == 0 || c.DiskGiB >= sz.DiskGiB)
			if n.Info.Paused {
				if fits {
					reason = "only a node its owner paused can host " + sz.Name + " (a request waits until it is resumed)"
				} else if reason == "no node is online" {
					reason = "every online node is paused by its owner"
				}
				continue
			}
			if fits {
				reason = ""
				break
			}
			if !strings.HasPrefix(reason, "only a node its owner paused") {
				reason = "no node in the pool is big enough for " + sz.Name
			}
		}
		out[sz.Name] = reason
	}
	return out
}

// defaultImage returns the base image for new sandboxes.
func (s *Server) defaultImage() string {
	if s.Cfg.DefaultImage != "" {
		return s.Cfg.DefaultImage
	}
	var id string
	if s.DB.QueryRow(`SELECT id FROM images ORDER BY created_at DESC LIMIT 1`).Scan(&id) == nil {
		return id
	}
	count := map[string]int{}
	for nid := range s.nodes.online() {
		if n, err := s.NodeByID(nid); err == nil {
			for _, im := range n.Info.Images {
				count[im]++
			}
		}
	}
	best := ""
	for im, c := range count {
		if c > count[best] || (c == count[best] && im > best) {
			best = im
		}
	}
	return best
}

// gpusInUse counts live sandboxes on nodeID that hold a passthrough GPU.
func (s *Server) gpusInUse(nodeID string) int {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE node_id=? AND deleted_at IS NULL
		AND state IN ('provisioning','provisioned','cloning','ready','idle','running','archiving')
		AND json_extract(placement,'$.gpu')=1`, nodeID).Scan(&n)
	return n
}
