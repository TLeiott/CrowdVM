package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Direct ports: the node publishes a sandbox's ssh port and the user ports
// the server asks for (URL-012) on its LAN/Tailscale DirectHost. ssh goes
// through the guest agent tunnel; user ports enter the guest's NIC (the
// guest firewall applies, as with Boat's sandbox IP), or the tunnel (TCP)
// when the driver has no reachable NIC.

type directPort struct {
	key  string // "ssh", "tcp/3000", "udp/5353"
	addr string // published host:port
	ln   net.Listener
	pc   net.PacketConn
}

func (d *directPort) close() {
	if d.ln != nil {
		d.ln.Close()
	}
	if d.pc != nil {
		d.pc.Close()
	}
}

// directMap (n.mu held) is VMStatus.Direct.
func (r *vmRec) directMap() map[string]string {
	if r.direct == nil && len(r.ports) == 0 {
		return nil
	}
	m := map[string]string{}
	if r.direct != nil {
		m["ssh"] = r.direct.addr
	}
	for k, p := range r.ports {
		m[k] = p.addr
	}
	return m
}

func directKey(p proto.DirectPort) (string, error) {
	pr := p.Proto
	if pr == "" {
		pr = "tcp"
	}
	if pr != "tcp" && pr != "udp" {
		return "", fmt.Errorf("proto must be tcp or udp, not %q", p.Proto)
	}
	if p.Port < 1 || p.Port > 65535 {
		return "", fmt.Errorf("port must be 1..65535, not %d", p.Port)
	}
	return pr + "/" + strconv.Itoa(p.Port), nil
}

var errDirectDisabled = errors.New("this node publishes no direct ports (crowdvm-node config set direct-host <LAN or Tailscale IP>)")

// openDirect publishes ssh and the spec's DirectPorts (no-op without
// DirectHost; already open ports stay).
func (n *Node) openDirect(r *vmRec) {
	host := n.config().DirectHost
	if host == "" {
		return
	}
	n.mu.Lock()
	hasSSH, want := r.direct != nil, r.spec.DirectPorts
	n.mu.Unlock()
	if !hasSSH {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			n.log.Warn("direct port listen failed", "sandbox", r.id, "host", host, "err", err)
		} else {
			d := &directPort{key: "ssh", addr: ln.Addr().String(), ln: ln}
			n.mu.Lock()
			r.direct = d
			n.mu.Unlock()
			go n.serveDirectTCP(r, ln, func(ctx context.Context) (net.Conn, error) { return n.dialGuestPort(ctx, r, 22) })
		}
	}
	if err := n.syncPorts(r, host, want); err != nil {
		n.log.Warn("direct ports", "sandbox", r.id, "err", err)
	}
	n.setState(r, func(*proto.VMStatus) {})
}

// closeDirect stops publishing every port of r.
func (n *Node) closeDirect(r *vmRec) {
	n.mu.Lock()
	all := []*directPort{r.direct}
	for _, p := range r.ports {
		all = append(all, p)
	}
	had := r.direct != nil || len(r.ports) > 0
	r.direct, r.ports = nil, nil
	n.mu.Unlock()
	for _, d := range all {
		if d != nil {
			d.close()
		}
	}
	if had {
		n.setState(r, func(*proto.VMStatus) {})
	}
}

// SetDirectPorts replaces the user ports a VM publishes (PUT
// /v1/vms/{id}/direct-ports); they are kept in the VM's spec.
func (n *Node) SetDirectPorts(id string, ports []proto.DirectPort) (proto.VMStatus, error) {
	r := n.rec(id)
	if r == nil {
		return proto.VMStatus{}, errNotFound
	}
	norm := []proto.DirectPort{}
	seen := map[string]bool{}
	for _, p := range ports {
		k, err := directKey(p)
		if err != nil {
			return proto.VMStatus{}, &badRequest{err.Error()}
		}
		if !seen[k] {
			seen[k] = true
			norm = append(norm, proto.DirectPort{Port: p.Port, Proto: k[:3]})
		}
	}
	host := n.config().DirectHost
	if host == "" && len(norm) > 0 {
		return proto.VMStatus{}, errDirectDisabled
	}
	n.mu.Lock()
	r.spec.DirectPorts = norm
	n.saveSpecsLocked()
	running := active(r.status.State)
	n.mu.Unlock()
	var err error
	if running && host != "" {
		err = n.syncPorts(r, host, norm)
		go n.syncGuestFirewall(n.ctx, r)
	}
	n.setState(r, func(*proto.VMStatus) {})
	n.mu.Lock()
	st := r.status
	n.mu.Unlock()
	return st, err
}

// syncPorts opens the wanted user ports that are not open and closes the
// others. The guest port number is tried on the host first.
func (n *Node) syncPorts(r *vmRec, host string, want []proto.DirectPort) error {
	keep := map[string]proto.DirectPort{}
	for _, p := range want {
		if k, err := directKey(p); err == nil {
			keep[k] = p
		}
	}
	n.mu.Lock()
	var drop []*directPort
	for k, d := range r.ports {
		if _, ok := keep[k]; !ok {
			drop = append(drop, d)
			delete(r.ports, k)
		}
	}
	var open []proto.DirectPort
	for k, p := range keep {
		if r.ports[k] == nil {
			open = append(open, proto.DirectPort{Port: p.Port, Proto: k[:3]})
		}
	}
	n.mu.Unlock()
	for _, d := range drop {
		d.close()
	}
	var errs []error
	for _, p := range open {
		d, err := n.listenDirect(r, host, p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n.mu.Lock()
		if r.ports == nil {
			r.ports = map[string]*directPort{}
		}
		r.ports[d.key] = d
		n.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (n *Node) listenDirect(r *vmRec, host string, p proto.DirectPort) (*directPort, error) {
	key, _ := directKey(p)
	addrs := []string{net.JoinHostPort(host, strconv.Itoa(p.Port)), net.JoinHostPort(host, "0")}
	var err error
	for _, a := range addrs {
		if p.Proto == "udp" {
			var pc net.PacketConn
			if pc, err = net.ListenPacket("udp", a); err == nil {
				d := &directPort{key: key, addr: pc.LocalAddr().String(), pc: pc}
				go n.serveDirectUDP(r, pc, p.Port)
				return d, nil
			}
			continue
		}
		var ln net.Listener
		if ln, err = net.Listen("tcp", a); err == nil {
			d := &directPort{key: key, addr: ln.Addr().String(), ln: ln}
			port := p.Port
			go n.serveDirectTCP(r, ln, func(ctx context.Context) (net.Conn, error) { return n.dialGuestNIC(ctx, r, "tcp", port) })
			return d, nil
		}
	}
	return nil, fmt.Errorf("publish %s: %w", key, err)
}

// syncGuestFirewall opens the guest firewall for the published TCP ports
// (Boat: "`host <id> <port> --public` also opens that TCP port on the IP
// address. boat opens it again after a resume"): PUT guest
// /v1/internal/firewall with the TCP DirectPorts. UDP ports stay the
// user's own ufw rule, as in Boat's raw-port recipe. Guests without the
// endpoint (404) are left alone.
func (n *Node) syncGuestFirewall(ctx context.Context, r *vmRec) {
	if n.config().DirectHost == "" || r.spec.Guest.Channel != nil {
		return // channel guests: the server sets their firewall (the node may not)
	}
	n.mu.Lock()
	ports := r.spec.DirectPorts
	n.mu.Unlock()
	open := []string{}
	for _, p := range ports {
		if p.Proto == "" || p.Proto == "tcp" {
			open = append(open, strconv.Itoa(p.Port)+"/tcp")
		}
	}
	addr, err := n.drv.GuestAddr(r.id)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	resp, err := doReq(cctx, &http.Client{Transport: n.guestHTTP}, "PUT", "http://"+addr+"/v1/internal/firewall",
		jsonBody(map[string]any{"open": open}), http.Header{"Authorization": {"Bearer " + r.spec.Guest.Token}})
	if err != nil {
		if !isStatus(err, http.StatusNotFound) && !isStatus(err, http.StatusMethodNotAllowed) && ctx.Err() == nil {
			n.log.Warn("guest firewall for direct ports", "sandbox", r.id, "err", err)
		}
		return
	}
	resp.Body.Close()
}

// dialGuestNIC reaches a guest port through its NIC, or through the agent
// tunnel (TCP) when the driver has none.
func (n *Node) dialGuestNIC(ctx context.Context, r *vmRec, network string, port int) (net.Conn, error) {
	if d, ok := n.drv.(vm.NICDialer); ok {
		c, err := d.DialGuestNIC(ctx, r.id, network, port)
		if err == nil || !errors.Is(err, vm.ErrNoNIC) {
			return c, err
		}
	}
	if network != "tcp" {
		return nil, fmt.Errorf("%s direct ports need a VM NIC (qemu netstack networking)", network)
	}
	return n.dialGuestPort(ctx, r, port)
}

func (n *Node) serveDirectTCP(r *vmRec, ln net.Listener, dial func(context.Context) (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
			g, err := dial(ctx)
			cancel()
			if err != nil {
				n.log.Debug("direct dial failed", "sandbox", r.id, "err", err)
				c.Close()
				return
			}
			mux.Join(c, g)
		}()
	}
}

var directUDPIdle = 2 * time.Minute

// serveDirectUDP relays datagrams per client address to the guest port.
func (n *Node) serveDirectUDP(r *vmRec, pc net.PacketConn, port int) {
	var mu sync.Mutex
	flows := map[string]net.Conn{}
	defer func() {
		mu.Lock()
		for _, g := range flows {
			g.Close()
		}
		mu.Unlock()
	}()
	buf := make([]byte, 65536)
	for {
		k, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		mu.Lock()
		g := flows[from.String()]
		mu.Unlock()
		if g == nil {
			ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
			g, err = n.dialGuestNIC(ctx, r, "udp", port)
			cancel()
			if err != nil {
				n.log.Debug("direct udp dial failed", "sandbox", r.id, "err", err)
				continue
			}
			mu.Lock()
			flows[from.String()] = g
			mu.Unlock()
			go func(g net.Conn, from net.Addr) { // guest -> client until idle
				b := make([]byte, 65536)
				for {
					g.SetReadDeadline(time.Now().Add(directUDPIdle))
					k, err := g.Read(b)
					if err != nil {
						break
					}
					pc.WriteTo(b[:k], from)
				}
				g.Close()
				mu.Lock()
				if flows[from.String()] == g {
					delete(flows, from.String())
				}
				mu.Unlock()
			}(g, from)
		}
		g.Write(buf[:k])
	}
}

type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

// writeDirectErr maps SetDirectPorts errors.
func directStatus(err error) *api.Error {
	var br *badRequest
	switch {
	case errors.As(err, &br):
		return api.NewError(400, "invalid_request", br.msg, nil)
	case errors.Is(err, errDirectDisabled):
		return api.NewError(409, "direct_disabled", err.Error(), nil)
	}
	return nil
}
