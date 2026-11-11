package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/node"
)

// fakeDaemon answers the local API like a running node daemon would.
func fakeDaemon(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir := t.TempDir()
	token, err := node.UIToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	// bugs-r12-opus 2: the CLI only talks to a daemon that proves the token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hello" {
			w.Write([]byte(`{"proof":"` + node.HelloProof(token, r.URL.Query().Get("nonce")) + `"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
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
