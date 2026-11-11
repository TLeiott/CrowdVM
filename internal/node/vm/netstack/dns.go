package netstack

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

var nsCache cache[[]string]

// upstreams returns the DNS servers to relay to (tests replace it).
var upstreams = nameservers

// nameservers returns the host's DNS servers (host:53), as Go's own resolver
// reads them: /etc/resolv.conf on Unix, the adapters' DNS servers on
// Windows. Refreshed every 30s.
func nameservers() []string {
	return nsCache.get(30*time.Second, func() []string {
		var mu sync.Mutex
		var out []string
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			if !slices.Contains(out, addr) {
				out = append(out, addr)
			}
			mu.Unlock()
			return nil, errors.New("probe only")
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r.LookupTXT(ctx, "crowdvm-ns-probe.invalid.")
		mu.Lock()
		defer mu.Unlock()
		return out
	})
}

// relayDNS answers the guest's queries on one UDP flow to 10.0.2.3:53 by
// asking the host's resolvers in turn.
func (n *Network) relayDNS(gc *gonet.UDPConn) {
	defer gc.Close()
	buf := make([]byte, 65536)
	for {
		gc.SetReadDeadline(time.Now().Add(5 * time.Second)) // resolvers use a new port per lookup
		k, err := gc.Read(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:k]...)
		go func() {
			if resp := queryUpstream(n.ctx, q); resp != nil {
				gc.Write(resp)
			}
		}()
	}
}

func queryUpstream(ctx context.Context, q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	buf := make([]byte, 65536)
	for _, ns := range upstreams() {
		c, err := net.Dial("udp", ns)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
		_, err = c.Write(q)
		for err == nil {
			var k int
			if k, err = c.Read(buf); err == nil && k >= 12 && buf[0] == q[0] && buf[1] == q[1] {
				stop()
				c.Close()
				return buf[:k]
			}
		}
		stop()
		c.Close()
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}
