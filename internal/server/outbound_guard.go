package server

// Outbound guard for user-supplied URLs the coordinator calls (webhooks,
// OTel exports; HOOK-004 ADAPTED). LAN and Tailscale targets are the point
// of a LAN deployment and stay allowed; loopback, link-local (including the
// 169.254.169.254 metadata service), unspecified, multicast and other cloud
// metadata addresses are refused unless the admin starts the server with
// --webhook-allow-local (Config.WebhookAllowLocal). The check runs when the
// URL is saved and again on every connection (DNS can change).

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), net.ParseIP("fd00:ec2::254"), net.ParseIP("100.100.100.200"), // AWS/GCP/Azure, AWS IPv6, Alibaba
}

// blockedTarget names why ip may not be called ("" = allowed).
// parseIP parses an IP literal, also an IPv6 one with a zone ("::1%lo",
// which net.ParseIP rejects) and IPv4-mapped IPv6, as the plain address.
func parseIP(host string) net.IP {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return net.IP(a.WithZone("").Unmap().AsSlice())
}

func blockedTarget(ip net.IP) string {
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return "a cloud metadata address"
		}
	}
	switch {
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "a link-local address"
	case ip.IsUnspecified():
		return "an unspecified address"
	case ip.IsMulticast():
		return "a multicast address"
	}
	return ""
}

type outboundKey struct{}

// withOutboundPolicy marks ctx: allowLocal lifts the guard for its dials.
func withOutboundPolicy(ctx context.Context, allowLocal bool) context.Context {
	return context.WithValue(ctx, outboundKey{}, allowLocal)
}

// checkOutboundURL refuses a URL whose host is (or resolves to) a blocked
// address. Names that do not resolve now are accepted; dials check again.
func (s *Server) checkOutboundURL(ctx context.Context, raw, code string) error {
	if s.Cfg.WebhookAllowLocal {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil // the caller's syntax check reports it
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return Err(400, code, "The URL points at this server's loopback address; ask the admin to start the server with --webhook-allow-local to allow it.")
	}
	var ips []net.IP
	if ip := parseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, _ := net.DefaultResolver.LookupIPAddr(cctx, host)
		cancel()
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if why := blockedTarget(ip); why != "" {
			return ErrD(400, code, fmt.Sprintf("The URL resolves to %s (%s). LAN and Tailscale addresses are allowed; loopback, link-local and metadata addresses are not unless the admin starts the server with --webhook-allow-local.", ip, why),
				map[string]any{"address": ip.String()})
		}
	}
	return nil
}

// guardedDial dials like net.Dialer but refuses blocked addresses unless
// the request context allows them.
func guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	allow, _ := ctx.Value(outboundKey{}).(bool)
	d := net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if allow {
			return nil
		}
		host, _, _ := net.SplitHostPort(address)
		ip := parseIP(host) // the dialer passes resolved addresses
		if ip == nil {
			return fmt.Errorf("refused to connect to %q: not an IP address", host)
		}
		if why := blockedTarget(ip); why != "" {
			return fmt.Errorf("refused to connect to %s (%s); the server admin can allow it with --webhook-allow-local", ip, why)
		}
		return nil
	}}
	return d.DialContext(ctx, network, addr)
}

// whClient delivers webhooks and OTel exports: no redirects, no proxy, no
// pooled connections (each dial is checked against the caller's policy).
var whClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // never follow redirects
	Transport: &http.Transport{
		DialContext:         guardedDial,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}
