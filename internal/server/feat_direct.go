package server

// Direct ports (URL-012, ADAPTED from Boat's "raw TCP/UDP via the sandbox
// IP"): a node with a direct host (crowdvm-node --direct-host) publishes
// chosen guest ports on its LAN/Tailscale address. The set is the
// sandbox's public hosted ports (`host --public` also opens the TCP port)
// plus the ports asked for with PUT /sandboxes/{id}/direct-ports. It is
// pushed to the node while the sandbox runs (PUT /v1/vms/{id}/direct-ports)
// and sent again at every boot (VMSpec.DirectPorts). Traffic enters the
// guest's NIC, so the guest firewall applies: the node opens the published
// TCP ports in it (guest PUT /v1/internal/firewall, on every change and
// boot), so `host --public` works like Boat's; UDP needs `ufw allow`.
//
//	GET|PUT /sandboxes/{id}/direct-ports  {ports:[{port,proto}]}
//	  → {sandboxId, directHost, ports:[{port,proto,endpoint,source}]}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

const maxDirectPorts = 20

// directBlocked are guest ports never published raw: the agent API and the
// password-less VNC displays (SSH has its own direct port already).
var directBlocked = []int{22, 5900, 5901, 5902, 6080, 7070}

func init() {
	RegisterFeature("direct-ports", func(s *Server) error {
		s.API("GET /sandboxes/{sandboxId}/direct-ports", "host", s.handleGetDirect)
		s.API("PUT /sandboxes/{sandboxId}/direct-ports", "host", s.handlePutDirect)
		s.Hooks.PrepareGuest = append(s.Hooks.PrepareGuest, func(_ context.Context, sb *Sandbox, g *GuestPrep) error {
			g.Spec.DirectPorts = s.directPortsOf(sb.ID)
			return nil
		})
		s.Hooks.SandboxJSON = append(s.Hooks.SandboxJSON, func(sb *Sandbox, m map[string]any) {
			eps := map[string]string{}
			if sb.Usable() {
				for k, v := range sb.Direct {
					if k != "ssh" {
						eps[k] = v
					}
				}
			}
			m["directEndpoints"] = eps
		})
		return nil
	})
}

// requestedDirect is the explicitly asked-for set (sandbox extra).
func requestedDirect(sb *Sandbox) []proto.DirectPort {
	var out []proto.DirectPort
	if raw, ok := sb.Extra["directPorts"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// directPortsOf is what the node should publish for a sandbox.
func (s *Server) directPortsOf(id string) []proto.DirectPort {
	var out []proto.DirectPort
	if sb, err := s.SandboxByID(id); err == nil {
		out = append(out, requestedDirect(sb)...)
	}
	rows, err := s.DB.Query(`SELECT port FROM hosted_ports WHERE sandbox_id=? AND public=1 AND hidden=0 ORDER BY port`, id)
	if err == nil {
		for rows.Next() {
			var p int
			if rows.Scan(&p) == nil && !slices.Contains(directBlocked, p) {
				out = append(out, proto.DirectPort{Port: p, Proto: "tcp"})
			}
		}
		rows.Close()
	}
	slices.SortFunc(out, func(a, b proto.DirectPort) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return cmpStr(a.Proto, b.Proto)
	})
	return slices.Compact(out)
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// syncDirect pushes the set to the node of a running sandbox, stores the
// endpoints it answers and sets them on sb. errDirectDisabled: the node
// publishes nothing.
func (s *Server) syncDirect(ctx context.Context, sb *Sandbox) error {
	if !sb.Usable() || sb.NodeID == "" {
		return nil
	}
	conn, err := s.NodeForSandbox(sb)
	if err != nil {
		return err
	}
	ports := s.directPortsOf(sb.ID)
	if ports == nil {
		ports = []proto.DirectPort{}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var st proto.VMStatus
	if err := conn.Do(cctx, "PUT", "/v1/vms/"+sb.ID+"/direct-ports", proto.DirectPortsRequest{Ports: ports}, &st); err != nil {
		var ne *NodeError
		if errors.As(err, &ne) && (ne.Status == 409 || ne.Status == 404 || ne.Status == 405) {
			return errDirectDisabled
		}
		return nodeAPIError(err)
	}
	if st.Direct != nil {
		_, _ = s.DB.Exec(`UPDATE sandboxes SET direct=? WHERE id=?`, mustJSON(st.Direct), sb.ID)
		sb.Direct = st.Direct
	}
	if g := s.guests.get(sb.ID); g != nil { // the node may not touch a channel guest's firewall
		s.pushGuestFirewall(ctx, g)
	}
	return nil
}

var errDirectDisabled = ErrD(409, "direct_disabled", "The node hosting this sandbox publishes no direct ports (its owner did not set a direct host). Use the public URL or `crowdvm tunnel`.", map[string]any{"retryable": false})

func directKey(p proto.DirectPort) string {
	return firstNonEmpty(p.Proto, "tcp") + "/" + strconv.Itoa(p.Port)
}

func (s *Server) directJSON(sb *Sandbox) map[string]any {
	req := requestedDirect(sb)
	out := []map[string]any{}
	for _, p := range s.directPortsOf(sb.ID) {
		src := "hosted"
		if slices.Contains(req, p) {
			src = "requested"
		}
		var ep any
		if v := sb.Direct[directKey(p)]; v != "" && sb.Usable() {
			ep = v
		}
		out = append(out, map[string]any{"port": p.Port, "proto": firstNonEmpty(p.Proto, "tcp"), "endpoint": ep, "source": src})
	}
	var host any
	if n, err := s.NodeByID(sb.NodeID); err == nil && n.Info.DirectHost != "" {
		host = n.Info.DirectHost
	}
	return map[string]any{"sandboxId": sb.ID, "directHost": host, "ports": out}
}

func (s *Server) handleGetDirect(c *Ctx) error {
	sb, err := c.Sandbox("host", false)
	if err != nil {
		return err
	}
	return c.OK(200, "direct_ports.list", s.directJSON(sb))
}

func (s *Server) handlePutDirect(c *Ctx) error {
	var req struct {
		Ports []proto.DirectPort `json:"ports"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	sb, err := c.Sandbox("host", false)
	if err != nil {
		return err
	}
	if s.accessOf(c.P, sb) == AccessView {
		return Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	if len(req.Ports) > maxDirectPorts {
		return Err(400, "invalid_ports", fmt.Sprintf("At most %d direct ports.", maxDirectPorts))
	}
	ports := []proto.DirectPort{}
	for _, p := range req.Ports {
		p.Proto = firstNonEmpty(p.Proto, "tcp")
		switch {
		case p.Port < 1 || p.Port > 65535:
			return Err(400, "invalid_port", "port must be 1 to 65535.")
		case p.Proto != "tcp" && p.Proto != "udp":
			return Err(400, "invalid_proto", "proto must be tcp or udp.")
		case slices.Contains(directBlocked, p.Port):
			return ErrD(400, "reserved_port", fmt.Sprintf("Port %d is used by CrowdVM inside the sandbox and is never published.", p.Port), map[string]any{"reservedPorts": directBlocked})
		}
		if !slices.Contains(ports, p) {
			ports = append(ports, p)
		}
	}
	if err := s.SetSandboxExtra(sb.ID, "directPorts", ports); err != nil {
		return err
	}
	if sb, err = s.SandboxByID(sb.ID); err != nil {
		return err
	}
	if err := s.syncDirect(c.R.Context(), sb); err != nil { // sets sb.Direct from the node's answer
		return err
	}
	return c.OK(200, "direct_ports.updated", s.directJSON(sb))
}
