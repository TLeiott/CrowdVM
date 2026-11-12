package server_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/hashicorp/yamux"
)

// bugs-r14-opus 1: a copy of an encrypted sandbox (a deploy of a saved or
// shared snapshot, or a fork) made with newDiskKey gets a key of its own.
// Until its guest confirms the re-encryption, every ticket redemption carries
// the new key and the source's (old) key; afterwards only the new one. A copy
// without the flag keeps its source's key, as before.
func TestNewDiskKeyForCopies(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey

	// boot redeems the current boot's ticket of id (an encrypted guest).
	boot := func(id string) (*yamux.Session, proto.GuestWelcome) {
		t.Helper()
		spec, _ := node.Spec(id)
		return rawChannel(t, env, id, spec.Guest.Channel.Ticket)
	}
	// reboot: the guest announces a reboot and goes down; its node gets a
	// new ticket, and the next boot redeems it.
	reboot := func(id string, sess *yamux.Session) (*yamux.Session, proto.GuestWelcome) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://server/v1/rebooting", nil)
		resp, err := (&http.Client{Transport: mux.Transport(sess)}).Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("announce: %v %v", resp, err)
		}
		resp.Body.Close()
		_ = sess.Close()
		var code int
		var body string
		env.Eventually("new ticket", func() bool {
			code, body = nodeCall(t, env, node, "POST", "/node/v1/sandboxes/"+id+"/reboot", nil, "")
			return code != http.StatusConflict
		})
		var ch proto.GuestChannel
		if code != 200 || json.Unmarshal([]byte(body), &ch) != nil {
			t.Fatalf("reboot ticket: %d %s", code, body)
		}
		return rawChannel(t, env, id, ch.Ticket)
	}
	confirm := func(sess *yamux.Session, dataKey string) int {
		t.Helper()
		raw, _ := base64.StdEncoding.DecodeString(dataKey)
		h := sha256.Sum256(raw)
		req, _ := http.NewRequest("POST", "http://server/v1/rekeyed", strings.NewReader(`{"keyHash":"`+hex.EncodeToString(h[:])+`"}`))
		resp, err := (&http.Client{Transport: mux.Transport(sess)}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	stopResume := func(id string) {
		t.Helper()
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 200 && r.Status != 202 {
			t.Fatalf("stop: %s", r)
		}
		env.WaitState(key, id, "archived")
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{}); r.Status != 200 && r.Status != 202 {
			t.Fatalf("resume: %s", r)
		}
		env.WaitState(key, id, "ready")
	}

	src := createSandbox(t, env, key, map[string]any{"type": "small"}) // five sandboxes fit the node
	env.WaitState(key, src, "ready")
	_, w := boot(src)
	srcKey := w.DataKey
	if srcKey == "" || w.OldDataKey != "" {
		t.Fatalf("source boot: key %q old %q", srcKey, w.OldDataKey)
	}
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "tpl"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/tpl", "snapshot.status", 5*time.Second, "ready")

	// Without the flag: the source's key, nothing pending.
	same := createSandbox(t, env, key, map[string]any{"from": "tpl"})
	env.WaitState(key, same, "ready")
	if _, w := boot(same); w.DataKey != srcKey || w.OldDataKey != "" {
		t.Fatalf("unflagged copy: key changed %v, old %q", w.DataKey != srcKey, w.OldDataKey)
	}

	// With the flag: a new key, and the source's as the old one.
	cp := createSandbox(t, env, key, map[string]any{"from": "tpl", "newDiskKey": true})
	env.WaitState(key, cp, "ready")
	s1, w := boot(cp)
	newKey := w.DataKey
	if newKey == "" || newKey == srcKey || w.OldDataKey != srcKey {
		t.Fatalf("flagged copy: new key %v, old is the source's %v", newKey != "" && newKey != srcKey, w.OldDataKey == srcKey)
	}
	// Not confirmed: a reboot gets both again.
	s2, w := reboot(cp, s1)
	if w.DataKey != newKey || w.OldDataKey != srcKey {
		t.Fatal("a reboot before the confirmation lost a key")
	}
	// A stop and resume (from the copy's own stop snapshot) too.
	_ = s2.Close()
	stopResume(cp)
	s3, w := boot(cp)
	if w.DataKey != newKey || w.OldDataKey != srcKey {
		t.Fatalf("a resume before the confirmation: same key %v, old %v", w.DataKey == newKey, w.OldDataKey == srcKey)
	}
	// A confirmation for another key is refused; the right one is taken (twice: a lost answer).
	if code := confirm(s3, srcKey); code != http.StatusConflict {
		t.Fatalf("confirmation of the wrong key: %d", code)
	}
	if code := confirm(s3, newKey); code != http.StatusNoContent {
		t.Fatalf("confirmation: %d", code)
	}
	if code := confirm(s3, newKey); code != http.StatusNoContent {
		t.Fatalf("repeated confirmation: %d", code)
	}
	s4, w := reboot(cp, s3)
	if w.DataKey != newKey || w.OldDataKey != "" {
		t.Fatalf("after the confirmation: same key %v, old %q", w.DataKey == newKey, w.OldDataKey)
	}
	_ = s4.Close()
	stopResume(cp)
	if _, w := boot(cp); w.DataKey != newKey || w.OldDataKey != "" {
		t.Fatal("a resume after the confirmation sent the old key or another key")
	}

	// Forks of the source: with the flag a key of their own, without it the source's.
	for _, flag := range []bool{false, true} {
		r := env.Do(key, "POST", "/api/v1/sandboxes/"+src+"/fork", map[string]any{"newDiskKey": flag})
		if r.Status != 202 {
			t.Fatalf("fork: %s", r)
		}
		fk := r.Str("sandbox.id")
		if fk == "" {
			fk = r.Str("id")
		}
		env.WaitState(key, fk, "ready")
		_, w := boot(fk)
		if flag && (w.DataKey == srcKey || w.OldDataKey != srcKey) || !flag && (w.DataKey != srcKey || w.OldDataKey != "") {
			t.Fatalf("fork newDiskKey=%v: key is the source's %v, old is the source's %v", flag, w.DataKey == srcKey, w.OldDataKey == srcKey)
		}
	}
}

// newDiskKey on a copy of an unencrypted disk is accepted and ignored.
func TestNewDiskKeyIgnoredForPlainDisks(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	src := createSandbox(t, env, key, nil)
	env.WaitState(key, src, "ready")
	spec, _ := node.Spec(src)
	raw, tc, br := rawDial(t, env)
	defer raw.Close()
	hello, _ := json.Marshal(proto.GuestHello{SandboxID: src, Ticket: spec.Guest.Channel.Ticket, Disk: "plain"})
	if _, err := tc.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "plain"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/plain", "snapshot.status", 5*time.Second, "ready")
	cp := createSandbox(t, env, key, map[string]any{"from": "plain", "newDiskKey": true})
	env.WaitState(key, cp, "ready")
	spec, _ = node.Spec(cp)
	if _, w := rawChannelTry(t, env, cp, spec.Guest.Channel.Ticket); w.Error != "" || w.OldDataKey != "" {
		t.Fatalf("plain copy: error %q, old key sent %v", w.Error, w.OldDataKey != "")
	}
}
