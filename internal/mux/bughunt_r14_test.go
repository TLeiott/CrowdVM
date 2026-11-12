package mux

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// bugs-r14-sonnet 2: the guest's egress proxy (internal/guest/egress.go
// proxyEgress) is Join(program's TCP conn, DialUpgrade(stream)), and the
// server's egress handler is Join(HijackUpgrade conn, outgoing TCP conn). The
// upgraded ends (bufConn here, readerConn in the server) embed net.Conn and
// have no CloseWrite, so Join closes both ends at once as soon as the
// program half-closes (shutdown(SHUT_WR), e.g. `printf req | nc -N host
// port`). On a yamux stream Close is a half-close, so a CloseWrite is easy to
// offer; without it the reply never reaches the program, which is the case
// TestBughuntR6JoinHalfCloseLosesReply fixed for plain TCP conns only.
func TestBughuntR14UpgradedConnHalfCloseLosesReply(t *testing.T) {
	a, b := net.Pipe()
	cfg := Config()
	srvSess, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cliSess, err := yamux.Client(b, Config())
	if err != nil {
		t.Fatal(err)
	}
	defer srvSess.Close()
	defer cliSess.Close()
	go Serve(srvSess, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := HijackUpgrade(w)
		if err != nil {
			return
		}
		defer conn.Close()
		req, _ := io.ReadAll(rw) // the request ends with the client's half-close
		conn.Write([]byte("reply to " + string(req)))
	}))

	st, err := cliSess.Open()
	if err != nil {
		t.Fatal(err)
	}
	up, err := DialUpgrade(st, "server", "/v1/egress?addr=192.0.2.1:80", nil)
	if err != nil {
		t.Fatal(err)
	}
	client, program := tcpPair(t) // the sandbox program and the proxy's end
	defer client.Close()
	go Join(program, up)
	client.Write([]byte("req"))
	client.CloseWrite()
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(client)
	if string(got) != "reply to req" {
		t.Fatalf("reply lost after the program half-closed: got %q err=%v", got, err)
	}
}
