package mux

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/yamux"
)

func TestBidirectionalHTTPAndTunnel(t *testing.T) {
	sessCh := make(chan *yamux.Session, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := Accept(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		sessCh <- s
		// server side also serves HTTP to the node
		_ = Serve(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "from-server") }))
	}))
	defer srv.Close()

	// echo TCP backend for the tunnel
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	node, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeMux := http.NewServeMux()
	nodeMux.HandleFunc("/v1/info", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "from-node") })
	nodeMux.HandleFunc("/v1/tunnel", func(w http.ResponseWriter, r *http.Request) {
		back, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		c, _, err := HijackUpgrade(w)
		if err != nil {
			return
		}
		Join(c, back)
	})
	go Serve(node, nodeMux)
	server := <-sessCh

	get := func(c *http.Client, u string) string {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get(Client(server), "http://node/v1/info"); got != "from-node" {
		t.Fatalf("got %q", got)
	}
	if got := get(Client(node), "http://server/x"); got != "from-server" {
		t.Fatalf("got %q", got)
	}
	st, _ := server.Open()
	tc, err := DialUpgrade(st, "node", "/v1/tunnel", nil)
	if err != nil {
		t.Fatal(err)
	}
	tc.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(tc, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel echo: %q %v", buf, err)
	}
	tc.Close()
}
