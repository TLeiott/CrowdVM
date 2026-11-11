package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// TestBinaryUnprivileged builds crowdvm-guest, runs it against a temp home
// and checks health, auth, a command and graceful shutdown.
func TestBinaryUnprivileged(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crowdvm-guest")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cfg, _ := json.Marshal(proto.GuestConfig{SandboxID: "bx_bin", Token: "t0k", ListenAddr: addr, Boot: proto.BootCreate, FirstBoot: true})
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, cfg, 0o600)
	home := filepath.Join(dir, "home")
	cmd := exec.Command(bin, "--config", cfgPath, "--home", home, "--unprivileged")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not become healthy")
		}
		time.Sleep(50 * time.Millisecond)
	}
	req, _ := http.NewRequest("POST", base+"/v1/commands", strings.NewReader(`{"command":"echo $HOME"}`))
	req.Header.Set("Authorization", "Bearer t0k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if m["stdout"] != home+"\n" {
		t.Fatalf("command: %v", m)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}
