package netstack

import (
	"context"
	"errors"
	"net"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// bugs-r13-opus 6: a guest that reboots sends nothing, so a node-side
// connection to it never drew a RST and a call in flight hung forever. TCP
// keepalive on the netstack side of every guest connection finds the dead peer
// (idle + interval*count, ~60 s), and the splice ends.
var (
	keepaliveIdle     = 30 * time.Second
	keepaliveInterval = 10 * time.Second
	keepaliveCount    = 3
)

func setKeepalive(ep tcpip.Endpoint) {
	idle, interval := tcpip.KeepaliveIdleOption(keepaliveIdle), tcpip.KeepaliveIntervalOption(keepaliveInterval)
	ep.SetSockOpt(&idle)
	ep.SetSockOpt(&interval)
	ep.SetSockOptInt(tcpip.KeepaliveCountOption, keepaliveCount)
	ep.SocketOptions().SetKeepAlive(true)
}

// dialGuestTCP is gonet.DialContextTCP with keepalive set before the connect.
func dialGuestTCP(ctx context.Context, s *stack.Stack, addr tcpip.FullAddress) (*gonet.TCPConn, tcpip.Endpoint, error) {
	var wq waiter.Queue
	ep, terr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return nil, nil, errors.New(terr.String())
	}
	setKeepalive(ep)
	waitEntry, notifyCh := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&waitEntry)
	defer wq.EventUnregister(&waitEntry)
	err := ep.Connect(addr)
	if _, ok := err.(*tcpip.ErrConnectStarted); ok {
		select {
		case <-ctx.Done():
			ep.Close()
			return nil, nil, ctx.Err()
		case <-notifyCh:
		}
		err = ep.LastError()
	}
	if err != nil {
		ep.Close()
		return nil, nil, &net.OpError{Op: "connect", Net: "tcp", Err: errors.New(err.String())}
	}
	return gonet.NewTCPConn(&wq, ep), ep, nil
}
