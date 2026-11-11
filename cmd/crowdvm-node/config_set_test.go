package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
)

// fakeDaemon answers the local API like a running node daemon would.
func fakeDaemon(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg, _ := node.LoadConfig(dir)
	cfg.UIAddr = strings.TrimPrefix(srv.URL, "http://")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConfigSetShowsDaemonMessage(t *testing.T) {
	dir := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"ok":false,"error":{"code":"invalid_setting","message":"cpus must be 0..16"}}`))
	})
	err := run(t, "--data-dir", dir, "config", "set", "cpus", "999")
	if err == nil || err.Error() != "cpus must be 0..16" {
		t.Fatalf("error %v", err)
	}
}

func TestConfigSetPausedOnOldDaemon(t *testing.T) {
	var mu sync.Mutex
	var paused string
	dir := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pause" { // a daemon from before /api/config
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		buf := make([]byte, 64)
		k, _ := r.Body.Read(buf)
		b.Write(buf[:k])
		mu.Lock()
		paused = b.String()
		mu.Unlock()
		w.Write([]byte(`{"paused":true}`))
	})
	if err := run(t, "--data-dir", dir, "config", "set", "paused", "true"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(paused, "true") {
		t.Fatalf("old daemon not paused live: %q", paused)
	}
	if cfg, _ := node.LoadConfig(dir); !cfg.Paused {
		t.Fatal("not saved")
	}
}
