package guest

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// originalDst is where a connection redirected by iptables was going.
func originalDst(c *net.TCPConn) (netip.AddrPort, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var out netip.AddrPort
	var serr error
	if err := rc.Control(func(fd uintptr) {
		m, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if err != nil {
			serr = err
			return
		}
		// struct sockaddr_in: family, port (big endian), address
		port := binary.BigEndian.Uint16(m.Multiaddr[2:4])
		out = netip.AddrPortFrom(netip.AddrFrom4([4]byte(m.Multiaddr[4:8])), port)
	}); err != nil {
		return out, err
	}
	if serr == nil && !out.IsValid() {
		serr = errors.New("no original destination")
	}
	return out, serr
}

// markChannel marks the channel socket so the egress rules leave it alone
// (needs CAP_NET_ADMIN: root in the VM; ignored elsewhere).
func markChannel(_, _ string, c syscall.RawConn) error {
	_ = c.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, channelMark) })
	return nil
}

// enablePktinfo makes ReadMsgUDP report each packet's destination address,
// so a reply can come from it: after an iptables REDIRECT the query went to
// another address than the socket's, and a reply from the wrong one is not
// mapped back to the address the program asked (it would never see it).
func enablePktinfo(c *net.UDPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_PKTINFO, 1) })
	}
}

// replyOOB turns a received packet's control data into the control data
// that sends the reply from the packet's destination address.
func replyOOB(oob []byte) []byte {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= unix.SizeofInet4Pktinfo {
			var info unix.Inet4Pktinfo
			copy((*[unix.SizeofInet4Pktinfo]byte)(unsafe.Pointer(&info))[:], m.Data)
			return unix.PktInfo4(&unix.Inet4Pktinfo{Spec_dst: info.Addr})
		}
	}
	return nil
}
