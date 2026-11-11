package bench

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestRunQuick(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/node/v1/bench/download":
			n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
			buf := make([]byte, 64<<10)
			for n > 0 {
				m := min(n, int64(len(buf)))
				if _, err := w.Write(buf[:m]); err != nil {
					return
				}
				n -= m
			}
		case "/node/v1/bench/upload":
			_, _ = io.Copy(io.Discard, r.Body)
		}
	}))
	defer srv.Close()
	start := time.Now()
	b := Run(context.Background(), Options{Quick: true, Dir: t.TempDir(), ServerURL: srv.URL, Client: srv.Client(), Log: t.Logf})
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("quick bench took %v", d)
	}
	out, _ := json.MarshalIndent(b, "", "  ")
	t.Logf("%s", out)
	if b.CPU.SingleScore <= 0 || b.CPU.MultiScore < b.CPU.SingleScore || b.Memory.TotalMiB <= 0 {
		t.Fatalf("bad cpu/mem: %+v %+v", b.CPU, b.Memory)
	}
	if b.Disk.WriteMBps <= 0 || b.Disk.ReadMBps <= 0 {
		t.Fatalf("bad disk: %+v", b.Disk)
	}
	if b.Network.Via != "loopback" || b.Network.DownMbps <= 0 || b.Network.UpMbps <= 0 {
		t.Fatalf("bad net: %+v", b.Network)
	}
	if b.Tier == "" || len(b.Categories) != 5 {
		t.Fatalf("bad scoring: %v %v", b.Tier, b.Categories)
	}
}

func TestClassify(t *testing.T) {
	for host, want := range map[string]string{
		"127.0.0.1:8420":           "loopback",
		"100.101.1.2:8420":         "tailscale",
		"192.168.1.5":              "lan",
		"10.0.0.1:1":               "lan",
		"[fd7a:115c:a1e0::1]:8420": "tailscale",
		"foo.tail1234.ts.net":      "tailscale",
	} {
		if got := Classify(host); got != want {
			t.Errorf("Classify(%s) = %s, want %s", host, got, want)
		}
	}
}

func TestInternetCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
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
	if st, ms := Internet(context.Background(), []string{ln.Addr().String(), "127.0.0.1:1"}, time.Second); st != "reachable" || ms <= 0 {
		t.Fatalf("reachable: %s %v", st, ms)
	}
	if st, ms := Internet(context.Background(), []string{"127.0.0.1:1"}, time.Second); st != "unreachable" || ms != 0 {
		t.Fatalf("unreachable: %s %v", st, ms)
	}
}
