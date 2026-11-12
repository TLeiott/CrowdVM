package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Policy is a node's egress policy for guest traffic. The zero value is the
// secure default: the internet is reachable; this machine, its LANs and its
// tailnets are not.
type Policy struct {
	// AllowLAN opens private networks: RFC 1918, CGNAT/Tailscale 100.64.0.0/10
	// and IPv6 ULA. Loopback, this machine's own addresses, link-local
	// (incl. 169.254.169.254) and multicast stay blocked.
	AllowLAN bool
	// Allow and Deny are owner overrides. The longest matching prefix of
	// both lists decides (a tie denies). An Allow entry opens loopback or one
	// of this machine's own addresses only if it is a single address (/32, /128).
	Allow, Deny []netip.Prefix
	// Pinholes are host:port destinations that are always reachable, even if
	// a rule would block them (the CrowdVM server). Hostnames are resolved
	// and re-resolved every minute.
	Pinholes []string
}

// ParsePrefixes parses CIDRs or bare addresses ("10.1.2.3" = /32).
func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR or address %q", s)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

func mustPrefixes(s ...string) []netip.Prefix {
	p, err := ParsePrefixes(s)
	if err != nil {
		panic(err)
	}
	return p
}

var (
	// hostNets reach this machine itself (0.0.0.0 connects to loopback on Linux).
	hostNets = mustPrefixes("127.0.0.0/8", "0.0.0.0/8", "::1/128", "::/128")
	// lanNets are opened by AllowLAN.
	lanNets = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7", "fec0::/10")
	// otherNets are never reachable by default: link-local (cloud metadata),
	// multicast, reserved and broadcast. 168.63.129.16 is Azure's WireServer
	// (a public address, so not link-local).
	otherNets = mustPrefixes("169.254.0.0/16", "168.63.129.16/32", "fe80::/10", "224.0.0.0/4", "240.0.0.0/4", "ff00::/8") // bugs-r11-opus 11
)

func contains(list []netip.Prefix, a netip.Addr) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// longest returns the length of the longest prefix in list containing a, or -1.
func longest(list []netip.Prefix, a netip.Addr) int {
	best := -1
	for _, p := range list {
		if p.Bits() > best && p.Contains(a) {
			best = p.Bits()
		}
	}
	return best
}

// Rules is a compiled Policy, shared by all VMs of a node.
type Rules struct {
	p Policy
	// LocalAddrs lists this machine's addresses (default: all interface
	// addresses, cached for 10s). Tests replace it.
	LocalAddrs func() []netip.Addr

	pins  []pinhole
	local cache[[]netip.Addr]
}

type pinhole struct {
	host string
	port uint16
	ips  cache[[]netip.Addr]
}

// cache is a value refreshed in the background once older than ttl
// (stale-while-revalidate; the first load is synchronous).
type cache[T any] struct {
	mu      sync.Mutex
	val     T
	at      time.Time
	loading atomic.Bool
}

func (c *cache[T]) get(ttl time.Duration, load func() T) T {
	c.mu.Lock()
	v, at := c.val, c.at
	c.mu.Unlock()
	if at.IsZero() {
		v = load()
		c.mu.Lock()
		c.val, c.at = v, time.Now()
		c.mu.Unlock()
		return v
	}
	if time.Since(at) > ttl && c.loading.CompareAndSwap(false, true) {
		go func() {
			nv := load()
			c.mu.Lock()
			c.val, c.at = nv, time.Now()
			c.mu.Unlock()
			c.loading.Store(false)
		}()
	}
	return v
}

// NewRules validates and compiles p.
func NewRules(p Policy) (*Rules, error) {
	r := &Rules{p: p}
	for _, hp := range p.Pinholes {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			return nil, fmt.Errorf("pinhole %q: %w", hp, err)
		}
		n, err := netip.ParseAddrPort("0.0.0.0:" + port)
		if err != nil || n.Port() == 0 {
			return nil, fmt.Errorf("pinhole %q: bad port", hp)
		}
		r.pins = append(r.pins, pinhole{host: host, port: n.Port()})
	}
	for i := range r.pins { // resolve now so the first check does not block on DNS
		r.pinIPs(&r.pins[i])
	}
	return r, nil
}

// Policy returns the policy r was compiled from.
func (r *Rules) Policy() Policy { return r.p }

func (r *Rules) pinIPs(pin *pinhole) []netip.Addr {
	return pin.ips.get(time.Minute, func() []netip.Addr {
		if a, err := netip.ParseAddr(pin.host); err == nil {
			return []netip.Addr{a.Unmap()}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ips, _ := net.DefaultResolver.LookupNetIP(ctx, "ip", pin.host)
		for i := range ips {
			ips[i] = ips[i].Unmap()
		}
		return ips
	})
}

func (r *Rules) localAddrs() []netip.Addr {
	if r.LocalAddrs != nil {
		return r.LocalAddrs()
	}
	return r.local.get(10*time.Second, func() []netip.Addr {
		addrs, _ := net.InterfaceAddrs()
		var out []netip.Addr
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					out = append(out, ip.Unmap())
				}
			}
		}
		return out
	})
}

// Sandbox narrows the node policy for one VM (proto.NetPolicy from the
// server). It can only take away: the node policy and the pinholes still apply.
type Sandbox struct {
	Allow []netip.Prefix // if non-empty, only these destinations are reachable
	Deny  []netip.Prefix
	NoDNS bool // the 10.0.2.3 resolver does not answer
}

// Check reports whether a guest may connect to dst, and why not.
func (r *Rules) Check(dst netip.AddrPort, sb *Sandbox) (ok bool, reason string) {
	a := dst.Addr().Unmap()
	for i := range r.pins {
		if pin := &r.pins[i]; pin.port == dst.Port() {
			for _, ip := range r.pinIPs(pin) {
				if ip == a {
					return true, "server"
				}
			}
		}
	}
	if !a.IsValid() {
		return false, "invalid address"
	}
	if sb != nil {
		if contains(sb.Deny, a) {
			return false, "sandbox policy"
		}
		if len(sb.Allow) > 0 && !contains(sb.Allow, a) {
			return false, "sandbox policy"
		}
	}
	isHost := contains(hostNets, a)
	if !isHost {
		for _, l := range r.localAddrs() {
			if l == a {
				isHost = true
				break
			}
		}
	}
	allow, deny := longest(r.p.Allow, a), longest(r.p.Deny, a)
	if deny >= 0 && deny >= allow {
		return false, "node policy (deny)"
	}
	if allow >= 0 && (!isHost || allow == a.BitLen()) {
		return true, "node policy (allow)"
	}
	switch {
	case isHost:
		return false, "this machine"
	case contains(otherNets, a):
		return false, "link-local/multicast/reserved"
	case contains(lanNets, a) && !r.p.AllowLAN:
		return false, "private network (net.allow-lan is off)"
	}
	return true, "internet"
}
