//go:build !windows

package netstack

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/checksum"
)

// ping sends one echo request with an unprivileged ICMP socket ("ping
// socket"; Linux: net.ipv4.ping_group_range) and reports whether a reply
// came back. The kernel picks the echo identifier.
func ping(ctx context.Context, dst netip.Addr, seq uint16, data []byte, timeout time.Duration) bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), "ping")
	c, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		return false
	}
	defer c.Close()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer stop()

	msg := make([]byte, 8+len(data))
	msg[0] = 8 // echo request
	binary.BigEndian.PutUint16(msg[6:], seq)
	copy(msg[8:], data)
	binary.BigEndian.PutUint16(msg[2:], ^checksum.Checksum(msg, 0))
	if _, err := c.WriteTo(msg, &net.UDPAddr{IP: dst.AsSlice()}); err != nil {
		return false
	}
	buf := make([]byte, 65536)
	for {
		k, _, err := c.ReadFrom(buf)
		if err != nil {
			return false
		}
		b := buf[:k]
		if len(b) >= 20 && b[0]>>4 == 4 { // some systems include the IP header
			b = b[int(b[0]&0xf)*4:]
		}
		if len(b) >= 8 && b[0] == 0 && binary.BigEndian.Uint16(b[6:]) == seq {
			return true
		}
	}
}
