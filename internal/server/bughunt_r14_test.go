package server_test

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/hashicorp/yamux"
)

// rawChannel is a guest channel client written for the tests: it redeems
// ticket for sandbox id the way internal/guest/channel.go does (Upgrade,
// TLS inside, hello line, welcome line, yamux client) and returns the
// session, without running a guest agent.
func rawChannel(t *testing.T, env *servertest.Env, id, ticket string) (*yamux.Session, proto.GuestWelcome) {
	t.Helper()
	sess, w := rawChannelTry(t, env, id, ticket)
	if w.Error != "" {
		t.Fatalf("welcome: %s", w.Error)
	}
	return sess, w
}

// rawDial opens the upgraded connection and the TLS inside it.
func rawDial(t *testing.T, env *servertest.Env) (net.Conn, *tls.Conn, *bufio.Reader) {
	t.Helper()
	u, _ := url.Parse(env.URL)
	raw, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	req, _ := http.NewRequest("GET", env.URL+"/guest/v1/connect", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "crowdvm-guest")
	if err := req.Write(raw); err != nil {
		t.Fatal(err)
	}
	obr := bufio.NewReader(raw)
	resp, err := http.ReadResponse(obr, req)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	tc := tls.Client(&bufConn{Conn: raw, r: obr}, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	return raw, tc, bufio.NewReader(tc)
}

// rawChannelTry is rawChannel that returns a refusal in the welcome.
func rawChannelTry(t *testing.T, env *servertest.Env, id, ticket string) (*yamux.Session, proto.GuestWelcome) {
	t.Helper()
	return rawChannelTryNonce(t, env, id, ticket, "")
}

func rawChannelTryNonce(t *testing.T, env *servertest.Env, id, ticket, nonce string) (*yamux.Session, proto.GuestWelcome) {
	t.Helper()
	raw, tc, br := rawDial(t, env)
	hello, _ := json.Marshal(proto.GuestHello{SandboxID: id, Ticket: ticket, Disk: "luks", Nonce: nonce})
	if _, err := tc.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var w proto.GuestWelcome
	if err := json.Unmarshal(line, &w); err != nil {
		t.Fatal(err)
	}
	if w.Error != "" {
		raw.Close()
		return nil, w
	}
	_ = raw.SetDeadline(time.Time{})
	sess, err := yamux.Client(&bufConn{Conn: tc, r: br}, mux.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, w
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// nodeReboot is the node's POST /node/v1/sandboxes/{id}/reboot (a new boot
// ticket after its VM went down).
func nodeReboot(t *testing.T, env *servertest.Env, node *servertest.FakeNode, id string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", env.URL+"/node/v1/sandboxes/"+id+"/reboot", nil)
	req.Header.Set("Authorization", "Bearer "+node.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// bugs-r14-sonnet 1: handleGuestReboot gives a node a new boot ticket only
// for a reboot the guest announced on its channel (POST /v1/rebooting), "so
// a node that kills its VM to get a ticket of its own gets 403". The
// announcement is kept in guests.expect for 3 minutes and is never cleared
// by anything but takeExpected. A guest announces at EVERY shutdown (it
// cannot tell a reboot from a stop: the agent calls announceReboot whenever
// systemctl says "stopping"), so after a plain stop the stale announcement
// stays for 3 minutes. Resume the sandbox inside that time, and the node
// can kill the new boot's VM and get the ticket the announcement was never
// meant for: the design's protection (the node cannot ask for a ticket on its
// own) is open for 3 minutes after each stop.
func TestBughuntR14StaleRebootAnnouncementOutlivesTheBoot(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	spec, _ := node.Spec(id)

	// Boot 1: the guest connects, and on shutdown announces a reboot.
	s1, _ := rawChannel(t, env, id, spec.Guest.Channel.Ticket)
	req, _ := http.NewRequest("POST", "http://server/v1/rebooting", nil)
	resp, err := (&http.Client{Transport: mux.Transport(s1)}).Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("announce: %v %v", resp, err)
	}
	resp.Body.Close()
	_ = s1.Close()

	// The owner stops it for good and resumes it a minute later (here at once).
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 200 && r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{}); r.Status != 200 && r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	env.WaitState(key, id, "ready")
	spec2, _ := node.Spec(id)
	if spec2.Guest.Channel == nil || spec2.Guest.Channel.Ticket == spec.Guest.Channel.Ticket {
		t.Fatalf("no new ticket for the new boot")
	}
	s2, _ := rawChannel(t, env, id, spec2.Guest.Channel.Ticket)

	// Boot 2 never announced anything. Its node kills the VM (the channel
	// closes) and asks for a ticket of its own.
	_ = s2.Close()
	var code int
	env.Eventually("channel closed", func() bool {
		code = nodeReboot(t, env, node, id)
		return code != http.StatusConflict
	})
	if code != http.StatusForbidden {
		t.Fatalf("a node that killed boot 2's VM without any announcement of boot 2 got status %d (200 = a new ticket for the config and disk key), want 403", code)
	}
}

// bugs-r14-sonnet 1b: the same announcement makes a STOPPED sandbox hand out
// tickets. The guest announces at every shutdown, a stop included, and
// handleGuestReboot never looks at the sandbox's state: for 3 minutes after
// each stop its node may ask for a ticket, boot its own (modified) guest from
// the stopped disk with it and redeem it for the sandbox's full config (every
// secret) and the disk key, which is exactly what the guest channel exists to
// prevent. A reboot ticket only makes sense for a sandbox that is running.
func TestBughuntR14StoppedSandboxHandsItsNodeATicket(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"env": map[string]any{"MY_SECRET": "hunter2-very-secret"}})
	env.WaitState(key, id, "ready")
	spec, _ := node.Spec(id)
	s1, _ := rawChannel(t, env, id, spec.Guest.Channel.Ticket)
	req, _ := http.NewRequest("POST", "http://server/v1/rebooting", nil)
	resp, err := (&http.Client{Transport: mux.Transport(s1)}).Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("announce: %v %v", resp, err)
	}
	resp.Body.Close()
	_ = s1.Close() // the guest powers off
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 200 && r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived") // stopped: no VM, nothing to reboot

	var code int
	env.Eventually("channel closed", func() bool {
		code = nodeReboot(t, env, node, id)
		return code != http.StatusConflict
	})
	if code == http.StatusOK {
		t.Fatalf("the node got a new boot ticket (config + disk key) for a sandbox that is stopped")
	}
}

// nodeCall is a raw call of a node to the server's node API.
func nodeCall(t *testing.T, env *servertest.Env, node *servertest.FakeNode, method, path string, hdr map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, env.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.Token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// bugs-r14-sonnet 4: mayFetch (nodes.go, secure layer) lets a node download
// "the chain of the head of a sandbox placed on it". The node chooses that
// chain: POST /node/v1/snapshots (recordTaken) accepts any parentId, and the
// layer becomes the head of the node's own sandbox once its blob is stored.
// A node that knows the id of someone else's snapshot (a share code, a deploy
// of a saved snapshot hands it ids of its chain, logs) names it as the
// parent of a layer of its own sandbox and may then read the victim's blob:
// 200 for a stranger's disk layer where the grants were meant to give 404.
func TestBughuntR14NodeNamesAForeignSnapshotAsParentAndReadsIt(t *testing.T) {
	env := servertest.New(t)
	nodeV := env.StartNode(servertest.NodeOptions{Name: "victim-node"})
	alice := env.AdminKey
	a := createSandbox(t, env, alice, map[string]any{"noEnv": true, "placement": map[string]any{"node": nodeV.ID}})
	env.WaitState(alice, a, "ready")
	if r := env.Do(alice, "POST", "/api/v1/sandboxes/"+a+"/stop", nil); r.Status != 200 && r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(alice, a, "archived")
	var victimSnap string
	env.Eventually("victim snapshot stored", func() bool {
		return env.S.DB.QueryRow(`SELECT id FROM snapshots WHERE sandbox_id=? AND uploaded_at IS NOT NULL ORDER BY created_at DESC LIMIT 1`, a).Scan(&victimSnap) == nil
	})

	nodeM := env.StartNode(servertest.NodeOptions{Name: "mallory-node"})
	_, mal := env.NewUser("mallory", false)
	x := createSandbox(t, env, mal, map[string]any{"noEnv": true, "placement": map[string]any{"node": nodeM.ID}})
	env.WaitState(mal, x, "ready")

	if code, _ := nodeCall(t, env, nodeM, "GET", "/node/v1/blobs/snapshot/"+victimSnap, nil, ""); code != 404 {
		t.Fatalf("control: a node without a grant reads a stranger's layer: %d", code)
	}
	const forged = "snap_forgedlayer0001"
	ref, _ := json.Marshal(proto.SnapshotRef{ID: forged, SandboxID: x, ParentID: victimSnap, SizeBytes: 12, CreatedAt: time.Now().Add(time.Minute)})
	if code, body := nodeCall(t, env, nodeM, "POST", "/node/v1/snapshots", nil, string(ref)); code != 200 {
		t.Fatalf("report: %d %s", code, body)
	}
	if code, body := nodeCall(t, env, nodeM, "PUT", "/node/v1/blobs/snapshot/"+forged, map[string]string{"X-Crowdvm-Snapshot": string(ref)}, "forged layer"); code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	var head string
	_ = env.S.DB.QueryRow(`SELECT COALESCE(head_snapshot_id,'') FROM sandboxes WHERE id=?`, x).Scan(&head)
	if head != forged {
		t.Skipf("the forged layer did not become the head (%q): the chain trick needs it", head)
	}
	if code, _ := nodeCall(t, env, nodeM, "GET", "/node/v1/blobs/snapshot/"+victimSnap, nil, ""); code != 404 {
		t.Fatalf("mallory's node read alice's snapshot %s through a parentId it chose: %d (want 404)", victimSnap, code)
	}
}

// bugs-r14-sonnet 5: redeeming a boot ticket is not retry-safe. redeemGuest
// marks the ticket redeemed (and stores the new session's hash) BEFORE the
// welcome is written, and handleGuestConnect does nothing to undo that when
// the guest never receives it: the connection dropped between its hello and
// the answer (a flaky node network, a guest that gave up waiting). The
// guest's retry with the same ticket then gets ticket_used, the owner gets
// "Possible tampering on a node", and preinit powers the VM off on
// ticket_used. One lost packet costs the boot (the sandbox stays down until its
// owner recovers it) and raises a security alert about a node that did
// nothing wrong.
func TestBughuntR14LostWelcomeBurnsTheBootTicket(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	spec, _ := node.Spec(id)
	ticket := spec.Guest.Channel.Ticket

	// Attempt 1: the hello gets out, the answer never arrives.
	raw, tc, _ := rawDial(t, env)
	// A guest with the fix sends a per-boot nonce with its ticket.
	hello, _ := json.Marshal(proto.GuestHello{SandboxID: id, Ticket: ticket, Disk: "luks", Nonce: "boot-nonce-1"})
	if _, err := tc.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	env.Eventually("server processed the hello", func() bool {
		var n int
		return env.S.DB.QueryRow(`SELECT COUNT(*) FROM guest_channels WHERE sandbox_id=? AND redeemed_at IS NOT NULL`, id).Scan(&n) == nil && n == 1
	})

	// Attempt 2: the guest retries with the same ticket (and nonce).
	_, w := rawChannelTryNonce(t, env, id, ticket, "boot-nonce-1")
	if w.Error != "" {
		t.Fatalf("the retry of a ticket whose welcome was lost is refused with %q: the VM powers off, and the owner is told of tampering", w.Error)
	}
}
