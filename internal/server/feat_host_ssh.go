package server

// SSH endpoint (SSH-004): POST /sandboxes/{id}/sshkey is answered by the
// guest (key authorized, hostKey); the server fills machineIp/sshEndpoint
// from the node's direct ssh port on its LAN/Tailscale address, or leaves
// them null and names the tunnel (`crowdvm tunnel <id> 22` as ProxyCommand).

import (
	"context"
	"net"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
)

func init() {
	RegisterFeature("ssh-endpoint", func(s *Server) error {
		s.API("POST /sandboxes/{sandboxId}/sshkey", "ssh", s.handleSSHKey)
		return nil
	})
}

// nodeDriver is the VM driver of a node ("qemu", "process", ...; "" = unknown).
func (s *Server) nodeDriver(id string) string {
	if n, err := s.NodeByID(id); err == nil {
		return n.Info.Driver
	}
	return ""
}

func (s *Server) handleSSHKey(c *Ctx) error {
	var req map[string]any
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := c.Sandbox("ssh", false)
	if err != nil {
		return err
	}
	if s.accessOf(c.P, sb) == AccessView {
		return Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	if sb, err = s.guestReady(c.R.Context(), sb); err != nil {
		return err
	}
	node, err := s.NodeForSandbox(sb)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	out := map[string]any{}
	if err := node.Do(ctx, "POST", "/v1/vms/"+sb.ID+"/guest/sshkey", req, &out); err != nil {
		return nodeAPIError(err)
	}
	delete(out, "ok")
	delete(out, "type")
	// The guest's user and host key end up in the user's ssh config and
	// known_hosts: only a plain name and one key line pass (bugs-r11-opus 1).
	if u, _ := out["sshUser"].(string); !api.ValidSSHUser(u) {
		out["sshUser"] = nil
	}
	if hk, _ := out["hostKey"].(string); hk != "" && !api.ValidHostKey(hk) {
		out["hostKey"] = ""
	}
	// The guest regenerates SSH host keys in the background after create,
	// fork and resume (API-127): a real VM without one yet cannot be pinned.
	if hk, _ := out["hostKey"].(string); hk == "" && s.nodeDriver(sb.NodeID) == "qemu" {
		return ErrD(503, "ssh_host_key_unavailable", "The sandbox's SSH host key is not generated yet. Retry in a few seconds.", map[string]any{"retryable": true, "retryAfterSeconds": 3})
	}
	var ip, ep any
	if d := sb.Direct["ssh"]; d != "" {
		if host, _, err := net.SplitHostPort(d); err == nil {
			ip, ep = host, d
		}
	}
	out["success"], out["machineIp"], out["sshEndpoint"], out["sandboxId"] = true, ip, ep, sb.ID
	if out["sshUser"] == nil {
		out["sshUser"] = "user"
	}
	out["proxyCommand"] = "crowdvm tunnel " + sb.ID + " 22"
	return c.OK(200, "ssh_key.configured", out)
}
