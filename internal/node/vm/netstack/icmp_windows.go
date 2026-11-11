package netstack

import (
	"context"
	"encoding/binary"
	"net/netip"
	"syscall"
	"time"
	"unsafe"
)

var (
	iphlpapi            = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
)

// ping sends one echo request with IcmpSendEcho (no privileges needed) and
// reports whether a reply came back.
func ping(ctx context.Context, dst netip.Addr, seq uint16, data []byte, timeout time.Duration) bool {
	if err := procIcmpCreateFile.Find(); err != nil {
		return false
	}
	h, _, _ := procIcmpCreateFile.Call()
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return false
	}
	defer procIcmpCloseHandle.Call(h)
	a := dst.As4()
	// ICMP_ECHO_REPLY (40 bytes on 64-bit) + data + room for an ICMP error.
	reply := make([]byte, 64+len(data)+8)
	n, _, _ := procIcmpSendEcho.Call(h, uintptr(binary.LittleEndian.Uint32(a[:])),
		uintptr(unsafe.Pointer(unsafe.SliceData(data))), uintptr(len(data)),
		0, uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeout.Milliseconds()))
	// ICMP_ECHO_REPLY.Status (offset 4) is IP_SUCCESS (0) for an echo reply.
	return n > 0 && binary.LittleEndian.Uint32(reply[4:8]) == 0 && ctx.Err() == nil
}
