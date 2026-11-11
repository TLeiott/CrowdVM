package node

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func init() {
	InspectIdle = 400 * time.Millisecond
	inspectTick = 50 * time.Millisecond
	inspectPoll = 10 * time.Millisecond
}

func TestInspector(t *testing.T) {
	h := start(t, func(c *Config) { c.SnapshotInterval = "100ms"; c.DirectHost = "127.0.0.1" })
	h.srv.putImage("img1", []byte("base"))
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_a", ImageID: "img1"}, []byte("layer a"))
	h.srv.putSnapshot(proto.SnapshotRef{ID: "snap_b", ParentID: "snap_a", ImageID: "img1"}, []byte("layer b"))

	resp, body := call(t, h.sess, "POST", "/v1/inspect", InspectRequest{SnapshotID: "snap_b", Token: "guest-tok"})
	if resp.StatusCode != 200 {
		t.Fatalf("inspect: %d %s", resp.StatusCode, body)
	}
	var st InspectStatus
	json.Unmarshal(body, &st)
	if !strings.HasPrefix(st.ID, inspectPrefix) || !st.Ready || st.Reused || st.SnapshotID != "snap_b" {
		t.Fatalf("status: %s", body)
	}
	// The chain was pulled and the VM booted as an inspector.
	if !h.drv.store.HasSnapshot("snap_a") || !h.drv.store.HasSnapshot("snap_b") {
		t.Fatal("chain not pulled")
	}
	h.drv.mu.Lock()
	starts := append([]proto.VMSpec(nil), h.drv.starts...)
	h.drv.mu.Unlock()
	if len(starts) != 1 || starts[0].Guest.Boot != proto.BootInspect || starts[0].ParentID != "snap_b" || starts[0].SandboxID != st.ID {
		t.Fatalf("starts: %+v", starts)
	}

	// Same snapshot: same inspector.
	_, body = call(t, h.sess, "POST", "/v1/inspect", InspectRequest{SnapshotID: "snap_b"})
	var st2 InspectStatus
	json.Unmarshal(body, &st2)
	if st2.ID != st.ID || !st2.Reused {
		t.Fatalf("not reused: %s", body)
	}

	// Guest calls are proxied with the inspector's token.
	resp, body = call(t, h.sess, "POST", "/v1/inspect/"+st.ID+"/guest/echo/files?path=%2Fhome", map[string]string{"a": "b"})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"uri":"/v1/echo/files?path=%2Fhome"`) || !strings.Contains(string(body), `{\"a\":\"b\"}`) {
		t.Fatalf("guest proxy: %d %s", resp.StatusCode, body)
	}

	// Inspectors are not sandboxes: no events, no heartbeat entries, no periodic snapshots.
	time.Sleep(250 * time.Millisecond)
	h.srv.mu.Lock()
	for _, e := range h.srv.events {
		if strings.HasPrefix(e.SandboxID, inspectPrefix) {
			h.srv.mu.Unlock()
			t.Fatalf("inspector event leaked: %+v", e)
		}
	}
	for _, hb := range h.srv.beats {
		for _, v := range hb.VMs {
			if strings.HasPrefix(v.SandboxID, inspectPrefix) {
				h.srv.mu.Unlock()
				t.Fatal("inspector in heartbeat")
			}
		}
	}
	h.srv.mu.Unlock()
	if len(h.srv.eventsOf(proto.EvSnapshotUploaded)) != 0 {
		t.Fatal("inspector was snapshotted")
	}

	// Idle inspectors are destroyed; guest calls then answer inspector_not_found.
	waitFor(t, "idle inspector destroyed", func() bool {
		_, ok := h.drv.Status(st.ID)
		return !ok
	})
	resp, body = call(t, h.sess, "GET", "/v1/inspect/"+st.ID+"/guest/echo/x", nil)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "inspector_not_found") {
		t.Fatalf("after reap: %d %s", resp.StatusCode, body)
	}

	// Explicit delete.
	_, body = call(t, h.sess, "POST", "/v1/inspect", InspectRequest{SnapshotID: "snap_a"})
	json.Unmarshal(body, &st)
	if resp, _ := call(t, h.sess, "DELETE", "/v1/inspect/"+st.ID, nil); resp.StatusCode != 200 {
		t.Fatal("delete failed")
	}
	waitFor(t, "deleted inspector gone", func() bool { _, ok := h.drv.Status(st.ID); return !ok })
	if resp, _ := call(t, h.sess, "DELETE", "/v1/inspect/"+st.ID, nil); resp.StatusCode != 404 {
		t.Fatal("second delete should be 404")
	}

	// Unknown snapshot: the chain cannot be pulled.
	resp, body = call(t, h.sess, "POST", "/v1/inspect", InspectRequest{SnapshotID: "snap_missing"})
	if resp.StatusCode != 502 || !strings.Contains(string(body), "inspector_failed") {
		t.Fatalf("missing snapshot: %d %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, h.sess, "POST", "/v1/inspect", InspectRequest{}); resp.StatusCode != 400 {
		t.Fatal("empty request should be 400")
	}
}
