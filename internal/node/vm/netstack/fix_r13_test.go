package netstack

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// bugs-r13-opus 6: a guest that goes away without a RST ends a forwarded
// connection (keepalive), and CloseGuestConns ends it at once.
func forwardedConn(t *testing.T) (client net.Conn, n *Network, killGuest func()) {
	t.Helper()
	keepaliveIdle, keepaliveInterval, keepaliveCount = 150*time.Millisecond, 100*time.Millisecond, 2
	t.Cleanup(func() { keepaliveIdle, keepaliveInterval, keepaliveCount = 30*time.Second, 10*time.Second, 3 })
	rules, err := NewRules(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	n, err = New(Config{Rules: rules, MTU: 9000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	q, a := streamPair(t)
	b, nodeSide := streamPair(t)
	var dead atomic.Bool // a black hole between guest and node: no FIN, no RST, nothing
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 64<<10)
		for {
			k, err := src.Read(buf)
			if err != nil {
				return
			}
			if !dead.Load() {
				dst.Write(buf[:k])
			}
		}
	}
	go pipe(b, a)
	go pipe(a, b)
	n.Attach(nodeSide)
	g := fakeGuest(t, q, 9000)
	gl, err := gonet.ListenTCP(g, tcpip.FullAddress{NIC: 1, Port: 7070}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := gl.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	n.Forward(l, 7070)
	client, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	client.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2)
	if _, err := client.Write([]byte("up")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "up" {
		t.Fatalf("forward: %q %v", buf, err)
	}
	client.SetDeadline(time.Time{})
	return client, n, func() { dead.Store(true) }
}

func waitEOF(t *testing.T, c net.Conn, what string) {
	t.Helper()
	start := time.Now()
	c.SetReadDeadline(start.Add(5 * time.Second))
	var ne net.Error
	_, err := c.Read(make([]byte, 8))
	if err == nil || errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("%s: the forwarded connection stays open (%v)", what, err)
	}
	t.Logf("%s: ended after %v (%v)", what, time.Since(start), err)
}

func TestForwardEndsWhenGuestVanishes(t *testing.T) {
	client, _, kill := forwardedConn(t)
	kill()
	waitEOF(t, client, "guest vanished")
}

func TestCloseGuestConns(t *testing.T) {
	client, n, _ := forwardedConn(t)
	n.CloseGuestConns()
	waitEOF(t, client, "CloseGuestConns")
}
