package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A VM qemu paused on ENOSPC ("io-error") is continued once the data dir
// has room; one that is running is left alone.
func TestResumeOnSpace(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "q.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	var paused, conts atomic.Int32
	paused.Store(1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		fmt.Fprintln(c, `{"QMP":{"version":{},"capabilities":[]}}`)
		dec := json.NewDecoder(c)
		for {
			var req struct {
				Execute string
				ID      int
			}
			if dec.Decode(&req) != nil {
				return
			}
			switch req.Execute {
			case "query-status":
				st := "running"
				if paused.Load() == 1 {
					st = "io-error"
				}
				fmt.Fprintf(c, `{"return":{"status":%q},"id":%d}`+"\n", st, req.ID)
			case "cont":
				conts.Add(1)
				paused.Store(0)
				fmt.Fprintf(c, `{"return":{},"id":%d}`+"\n", req.ID)
			default:
				fmt.Fprintf(c, `{"return":{},"id":%d}`+"\n", req.ID)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, err := dialQMP(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()

	old := ioErrorPoll
	ioErrorPoll = 20 * time.Millisecond
	defer func() { ioErrorPoll = old }()
	d := &Driver{opt: Options{DataDir: t.TempDir(), Logf: t.Logf}}
	r := &run{q: q, exited: make(chan struct{})}
	go d.resumeOnSpace(&machine{id: "bx_test"}, r)
	time.Sleep(300 * time.Millisecond)
	close(r.exited)
	if n := conts.Load(); n != 1 {
		t.Fatalf("cont sent %d times, want once", n)
	}
}
