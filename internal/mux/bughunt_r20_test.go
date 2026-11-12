package mux

import (
	"io"
	"net"
	"testing"
	"time"
)

// wrapConn is what the tunnel code puts around a hijacked connection that
// already buffered bytes (node/api.go readerConn, guest/internal.go hijacked,
// server/passthrough.go readerConn): a struct that embeds the net.Conn
// interface. SetLinger is not part of that interface, so it is not promoted.
type wrapConn struct{ net.Conn }

// Unwrap: the product's wrappers (readerConn, hijacked, bufConn, limitConn,
// stallConn) give Join the conn below this way since the fix.
func (c wrapConn) Unwrap() net.Conn { return c.Conn }

// Round 19 makes a read failure on the sender abort the pair by resetting a
// TCP receiver, but only when the receiver value itself has SetLinger. Every
// tunnel in the product joins wrapped connections (and yamux streams), so the
// receiver sees a clean end of input: the truncated upload still looks complete.
func TestBughuntR20JoinAbortIsLostOnWrappedConnections(t *testing.T) {
	sender, proxyIn := tcpPair(t)
	proxyOut, backend := tcpPair(t)
	defer backend.Close()
	go Join(wrapConn{proxyIn}, wrapConn{proxyOut})
	sender.Write([]byte("partial upload"))
	time.Sleep(100 * time.Millisecond)
	_ = sender.SetLinger(0)
	sender.Close() // a reset, not an end of input
	_ = backend.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1024)
	for {
		_, err := backend.Read(buf)
		if err == nil {
			continue
		}
		if err == io.EOF {
			t.Fatal("the backend saw a clean end of input after the sender was reset: a truncated upload looks complete")
		}
		return // a reset (or any error): the abort was passed on
	}
}
