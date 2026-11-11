package mux

import (
	"io"
	"net"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan *net.TCPConn, 1)
	go func() { c, _ := ln.Accept(); ch <- c.(*net.TCPConn) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c.(*net.TCPConn), <-ch
}

// A client that finishes its request with a TCP half-close (shutdown(SHUT_WR),
// e.g. `printf req | nc host port`) must still receive the backend's reply.
func TestBughuntR6JoinHalfCloseLosesReply(t *testing.T) {
	client, tunnelA := tcpPair(t)
	tunnelB, backend := tcpPair(t)
	defer client.Close()
	go func() { // backend: read the whole request (until EOF), then reply
		b, _ := io.ReadAll(backend)
		backend.Write([]byte("reply to " + string(b)))
		backend.Close()
	}()
	go Join(tunnelA, tunnelB)
	client.Write([]byte("req"))
	client.CloseWrite()
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(client)
	if string(got) != "reply to req" {
		t.Fatalf("reply lost after client half-close: got %q err=%v", got, err)
	}
}
