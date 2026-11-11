package netstack

import (
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const pingTimeout = 5 * time.Second

// handleEcho forwards a guest echo request with a host ping (unprivileged
// ICMP socket or IcmpSendEcho) and answers it when the host gets a reply.
// Blocked destinations get "communication administratively prohibited".
func (n *Network) handleEcho(guestMAC tcpip.LinkAddress, ip header.IPv4) {
	dst := netAddr(ip.DestinationAddress())
	if Subnet.Contains(dst) || !n.check("icmp", netip.AddrPortFrom(dst, 0)) {
		n.writeFrame(icmpUnreachable(guestMAC, ip, 13))
		return
	}
	select {
	case n.pings <- struct{}{}:
	default:
		return // too many in flight
	}
	req := header.ICMPv4(ip.Payload())
	id, seq := req.Ident(), req.Sequence()
	data := append([]byte(nil), req.Payload()...)
	src := ip.SourceAddress()
	go func() {
		defer func() { <-n.pings }()
		if !ping(n.ctx, dst, seq, data, pingTimeout) {
			return
		}
		b := make([]byte, header.ICMPv4MinimumSize+len(data))
		r := header.ICMPv4(b)
		r.SetType(header.ICMPv4EchoReply)
		r.SetIdent(id)
		r.SetSequence(seq)
		copy(b[header.ICMPv4MinimumSize:], data)
		r.SetChecksum(^checksum.Checksum(b, 0))
		n.writeFrame(ipv4Frame(guestMAC, tcpAddr(dst), src, header.ICMPv4ProtocolNumber, b))
	}()
}

// icmpUnreachable answers the packet in ip with ICMP destination
// unreachable (code 13: administratively prohibited) from the gateway.
func icmpUnreachable(guestMAC tcpip.LinkAddress, ip header.IPv4, code byte) []byte {
	orig := ip[:min(len(ip), int(ip.HeaderLength())+8)]
	b := make([]byte, header.ICMPv4MinimumSize+len(orig))
	r := header.ICMPv4(b)
	r.SetType(header.ICMPv4DstUnreachable)
	r.SetCode(header.ICMPv4Code(code))
	copy(b[header.ICMPv4MinimumSize:], orig)
	r.SetChecksum(^checksum.Checksum(b, 0))
	return ipv4Frame(guestMAC, tcpAddr(Gateway), ip.SourceAddress(), header.ICMPv4ProtocolNumber, b)
}
