package mux

import (
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// bugs-r14-opus 3: a guest flooding its channel with streams grew the
// server by gigabytes. ServeLimit serves at most max streams at once.
func TestServeLimitCapsConcurrentStreams(t *testing.T) {
	a, b := net.Pipe()
	srv, _ := yamux.Server(a, Config())
	cli, _ := yamux.Client(b, Config())
	defer srv.Close()
	defer cli.Close()
	var busy, peak atomic.Int32
	release := make(chan struct{})
	go ServeLimit(srv, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := busy.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release
		busy.Add(-1)
	}), 2)
	c := &http.Client{Transport: Transport(cli)}
	for i := 0; i < 5; i++ {
		go func() {
			if resp, err := c.Get("http://peer/"); err == nil {
				resp.Body.Close()
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("%d streams served at once, want 2", p)
	}
	close(release)
}
