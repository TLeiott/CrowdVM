package server

import (
	"context"
	"net"
	"testing"
)

// The dial guard parses the address with net.ParseIP, which rejects a zone
// ("::1%lo"), so a zoned loopback / link-local literal is never checked. A
// redirect (ciHTTP follows them; only dials are guarded) can therefore reach
// the server's own loopback although --webhook-allow-local is off.
func TestBughuntR5GuardedDialZonedLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// Sanity: the plain literal is refused.
	if c, err := guardedDial(context.Background(), "tcp", net.JoinHostPort("::1", port)); err == nil {
		c.Close()
		t.Fatal("setup: plain ::1 must be refused")
	}
	c, err := guardedDial(context.Background(), "tcp", net.JoinHostPort("::1%lo", port))
	if err == nil {
		c.Close()
		t.Fatalf("guardedDial connected to loopback through the zoned literal [::1%%lo]:%s", port)
	}
}
