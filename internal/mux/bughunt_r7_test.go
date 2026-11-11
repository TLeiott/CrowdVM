package mux

import (
	"net"
	"testing"
	"time"
)

func r7pair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c, <-ch
}

// The client goes away completely (close, not a half-close) while the backend
// is idle and does not react to EOF. Join must end: nobody is left to read a
// reply. With the half-close change the b->a copy waits for the backend for
// ever, and both connections (and two goroutines) leak per abandoned session.
func TestBughuntR7JoinClientGoneIdleBackendLeaks(t *testing.T) {
	defer halfCloseIdle.Store(halfCloseIdle.Load())
	halfCloseIdle.Store(int64(500 * time.Millisecond))
	client, joinA := r7pair(t) // joinA is the tunnel end facing the client
	joinB, backend := r7pair(t)
	defer backend.Close()
	done := make(chan struct{})
	go func() { Join(joinA, joinB); close(done) }()
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Join still running 2s after the client closed; the idle backend keeps both connections open")
	}
}
