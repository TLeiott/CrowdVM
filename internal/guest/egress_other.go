//go:build !linux

package guest

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

func originalDst(*net.TCPConn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("transparent egress needs Linux")
}

func markChannel(_, _ string, _ syscall.RawConn) error { return nil }

func enablePktinfo(*net.UDPConn) {}

func replyOOB([]byte) []byte { return nil }
