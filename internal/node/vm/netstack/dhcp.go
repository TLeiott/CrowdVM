package netstack

import (
	"encoding/binary"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// A minimal DHCPv4 server: there is exactly one client and one address.

const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6
	dhcpMagic    = 0x63825363
	leaseSeconds = 86400
)

func dhcpOptions(b []byte) map[byte][]byte {
	opts := map[byte][]byte{}
	for len(b) > 0 {
		code := b[0]
		if code == 255 {
			break
		}
		if code == 0 || len(b) < 2 {
			b = b[1:]
			continue
		}
		l := int(b[1])
		if len(b) < 2+l {
			break
		}
		opts[code] = b[2 : 2+l]
		b = b[2+l:]
	}
	return opts
}

// dhcpReply builds the reply to a client message (nil: no reply).
func dhcpReply(msg []byte, mtu int) []byte {
	if len(msg) < 240 || msg[0] != 1 || binary.BigEndian.Uint32(msg[236:240]) != dhcpMagic {
		return nil
	}
	opts := dhcpOptions(msg[240:])
	var typ byte
	switch t := opts[53]; {
	case len(t) != 1:
		return nil
	case t[0] == dhcpDiscover:
		typ = dhcpOffer
	case t[0] == dhcpRequest:
		typ = dhcpAck
		req := opts[50]
		if len(req) != 4 {
			req = msg[12:16] // ciaddr (renewal)
		}
		if a := [4]byte(req); a != [4]byte{} && a != GuestIP.As4() {
			typ = dhcpNak
		}
	default:
		return nil // release, decline, inform
	}
	r := make([]byte, 240, 300)
	r[0], r[1], r[2] = 2, msg[1], msg[2]
	copy(r[4:8], msg[4:8])     // xid
	copy(r[10:12], msg[10:12]) // flags
	copy(r[24:44], msg[24:44]) // giaddr, chaddr
	binary.BigEndian.PutUint32(r[236:240], dhcpMagic)
	gw, dns, ip := Gateway.As4(), DNS.As4(), GuestIP.As4()
	opt := func(code byte, v ...byte) { r = append(append(r, code, byte(len(v))), v...) }
	opt(53, typ)
	opt(54, gw[:]...)
	if typ != dhcpNak {
		copy(r[16:20], ip[:]) // yiaddr
		var lease [4]byte
		binary.BigEndian.PutUint32(lease[:], leaseSeconds)
		opt(51, lease[:]...)
		opt(1, 255, 255, 255, 0)
		opt(3, gw[:]...)
		opt(6, dns[:]...)
		opt(26, byte(mtu>>8), byte(mtu))
	}
	r = append(r, 255)
	for len(r) < 300 {
		r = append(r, 0)
	}
	return r
}

func (n *Network) handleDHCP(guestMAC tcpip.LinkAddress, ip header.IPv4) {
	reply := dhcpReply(header.UDP(ip.Payload()).Payload(), n.cfg.MTU)
	if reply == nil {
		return
	}
	// Broadcast unless the client renews from a configured address.
	dstMAC, dst := header.EthernetBroadcastAddress, tcpip.AddrFrom4([4]byte{255, 255, 255, 255})
	if src := ip.SourceAddress(); netAddr(src) == GuestIP && reply[242] != dhcpNak {
		dstMAC, dst = guestMAC, src
	}
	n.writeFrame(ipv4Frame(dstMAC, tcpAddr(Gateway), dst, header.UDPProtocolNumber, udpPacket(tcpAddr(Gateway), dst, 67, 68, reply)))
}

func udpPacket(src, dst tcpip.Address, sport, dport uint16, payload []byte) []byte {
	b := make([]byte, header.UDPMinimumSize+len(payload))
	u := header.UDP(b)
	u.Encode(&header.UDPFields{SrcPort: sport, DstPort: dport, Length: uint16(len(b))})
	copy(b[header.UDPMinimumSize:], payload)
	xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, src, dst, uint16(len(b)))
	xsum = checksum.Checksum(payload, xsum)
	u.SetChecksum(^u.CalculateChecksum(xsum))
	return b
}

// ipv4Frame wraps payload in IPv4 and Ethernet headers from the gateway MAC.
func ipv4Frame(dstMAC tcpip.LinkAddress, src, dst tcpip.Address, proto tcpip.TransportProtocolNumber, payload []byte) []byte {
	const off = header.EthernetMinimumSize
	b := make([]byte, off+header.IPv4MinimumSize+len(payload))
	header.Ethernet(b).Encode(&header.EthernetFields{SrcAddr: gatewayMAC, DstAddr: dstMAC, Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(b[off:])
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(len(b) - off), TTL: 64, Protocol: uint8(proto), SrcAddr: src, DstAddr: dst})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(b[off+header.IPv4MinimumSize:], payload)
	return b
}
