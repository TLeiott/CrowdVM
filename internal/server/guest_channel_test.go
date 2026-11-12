package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/guest"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// channelGuest runs a real (unprivileged) guest agent for a stub the fake
// node received, connected over the guest channel.
func channelGuest(t *testing.T, env *servertest.Env, stub proto.GuestConfig) (*guest.Agent, string) {
	a, addr, _ := channelGuestCtx(t, context.Background(), env, stub)
	return a, addr
}

func channelGuestCtx(t *testing.T, parent context.Context, env *servertest.Env, stub proto.GuestConfig) (*guest.Agent, string, *guest.Channel) {
	t.Helper()
	stub.Channel.URL = env.URL // the agent runs on this host, not behind the node's 10.0.2.2
	ctx, cancel := context.WithCancel(parent)
	t.Cleanup(cancel)
	home := t.TempDir()
	quiet := log.New(io.Discard, "", 0)
	cfg, ch, err := guest.ConnectChannel(ctx, stub, filepath.Join(home, ".state"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg.ListenAddr = addr
	a, err := guest.New(guest.Options{Config: cfg, Home: home, DataDir: filepath.Join(home, ".data"), Unprivileged: true, Logger: quiet, Channel: ch})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return a, addr, ch
}

// Secure layer: the node gets a boot stub without secrets; the guest fetches
// its config over the pinned channel, and the server reaches the guest API
// only through the channel.
func TestGuestChannelKeepsSecretsFromTheNode(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"env": map[string]any{"MY_SECRET": "hunter2-very-secret"}})
	env.WaitState(key, id, "ready")
	spec, ok := node.Spec(id)
	if !ok {
		t.Fatal("no spec on the node")
	}
	stub := spec.Guest
	raw, _ := json.Marshal(spec)
	if stub.Channel == nil || stub.Channel.Ticket == "" || stub.Channel.Pin == "" || len(stub.Env) != 0 || strings.Contains(string(raw), "hunter2") {
		t.Fatalf("the node got more than a boot stub: %s", raw)
	}
	ctx0, stopGuest := context.WithCancel(context.Background())
	_, local, ch0 := channelGuestCtx(t, ctx0, env, stub)
	// The disk key comes with the ticket redemption (and only then).
	diskKey := ch0.TakeDataKey()
	if len(diskKey) != 64 || strings.Contains(string(raw), base64.StdEncoding.EncodeToString(diskKey)) {
		t.Fatalf("disk key: %d bytes", len(diskKey))
	}

	// The API reaches the guest over the channel.
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "printf %s \"$MY_SECRET\""})
	if r.Status != 200 || !strings.Contains(string(r.Body), "hunter2-very-secret") {
		t.Fatalf("command over the channel: %s", r)
	}
	for _, c := range node.Calls() {
		if strings.Contains(c, "/guest/") {
			t.Fatalf("a guest call went through the node: %s", c)
		}
	}
	// The node's local access (the ops token from the stub) runs nothing.
	req, _ := http.NewRequest("POST", "http://"+local+"/v1/commands", strings.NewReader(`{"command":"id"}`))
	req.Header.Set("Authorization", "Bearer "+stub.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("node-side command: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", "http://"+local+"/v1/health", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("node-side health: %v %v", resp, err)
	}

	// The ticket from the config drive is spent: a second redemption is
	// refused and the owner is told.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	replay := stub
	replay.Channel = &proto.GuestChannel{URL: env.URL, Pin: stub.Channel.Pin, Ticket: stub.Channel.Ticket}
	if _, _, err := guest.ConnectChannel(ctx, replay, t.TempDir(), log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("the ticket was accepted twice")
	}
	env.Eventually("ticket reuse notice", func() bool {
		return strings.Contains(string(env.Do(key, "GET", "/api/v1/notices", nil).Body), "security_ticket_reused")
	})
	// A wrong pin: the guest refuses the server.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	bad := stub
	bad.Channel = &proto.GuestChannel{URL: env.URL, Pin: "AAAA", Ticket: "x"}
	if _, _, err := guest.ConnectChannel(ctx2, bad, t.TempDir(), log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("connected with a wrong pin")
	}

	// A reboot: the guest's session was in RAM, its node asks for a new
	// ticket. Refused while the guest is connected, and for other nodes.
	reboot := func(token string) (int, proto.GuestChannel) {
		req, _ := http.NewRequest("POST", env.URL+"/node/v1/sandboxes/"+id+"/reboot", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ch proto.GuestChannel
		_ = json.NewDecoder(resp.Body).Decode(&ch)
		return resp.StatusCode, ch
	}
	if code, _ := reboot(node.Token); code != http.StatusConflict {
		t.Fatalf("re-ticket while the guest is connected: %d", code)
	}
	other := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	stopGuest()
	env.Eventually("channel closed", func() bool {
		code, _ := reboot(other.Token)
		return code == http.StatusNotFound
	})
	// That machine went down without announcing it (a crash, or a node
	// that killed it for a ticket): no ticket, and the owner is told.
	env.Eventually("unannounced reboot refused", func() bool {
		code, _ := reboot(node.Token)
		return code == http.StatusForbidden
	})
	env.Eventually("unannounced reboot notice", func() bool {
		return strings.Contains(string(env.Do(key, "GET", "/api/v1/notices", nil).Body), "security_unannounced_reboot")
	})
	// The node reports the machine down; the owner recovers it, and the
	// server expects the node's request.
	node.SetVMState(id, proto.VMError, false, "new boot ticket: unannounced_reboot")
	env.WaitState(key, id, "error")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/recover", nil); r.Status != 200 {
		t.Fatalf("recover: %s", r)
	}
	var ch proto.GuestChannel
	env.Eventually("new ticket", func() bool {
		var code int
		code, ch = reboot(node.Token)
		return code == 200
	})
	if ch.Ticket == "" || ch.Ticket == stub.Channel.Ticket || ch.Pin != stub.Channel.Pin {
		t.Fatalf("new ticket: %+v", ch)
	}
	next := stub
	next.Channel = &proto.GuestChannel{URL: env.URL, Pin: ch.Pin, Ticket: ch.Ticket}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	cfg, ch3, err := guest.ConnectChannel(ctx3, next, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil || cfg.Boot != proto.BootRecover || cfg.FirstBoot || cfg.Env["MY_SECRET"] != "hunter2-very-secret" {
		t.Fatalf("redeem after reboot: %v boot=%s first=%v", err, cfg.Boot, cfg.FirstBoot)
	}
	if k := ch3.TakeDataKey(); string(k) != string(diskKey) {
		t.Fatal("the disk key changed across a reboot")
	}

}
