package server_test

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestSchedulerPlacement(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	if r := env.Do(key, "POST", "/api/v1/sandboxes", nil); r.Status != 503 || r.Code() != "out_of_capacity" || r.Get("error.details.retryAfterSeconds") == nil || r.Header.Get("Retry-After") == "" {
		t.Fatalf("no nodes: %s", r)
	}
	gpuBench := &proto.Benchmark{Tier: "gold", Score: 800, Categories: map[string]string{"cpu": "high", "gpu": "high"},
		GPUs: []proto.GPUInfo{{Vendor: "nvidia", Model: "RTX"}}}
	small := env.StartNode(servertest.NodeOptions{Name: "small", Capacity: proto.Capacity{VCPU: 4, MemoryMiB: 8192, DiskGiB: 200, MaxSandboxes: 4}, Labels: map[string]string{"zone": "a"}})
	gpu := env.StartNode(servertest.NodeOptions{Name: "gpu", Capacity: proto.Capacity{VCPU: 32, MemoryMiB: 131072, DiskGiB: 2000}, Bench: gpuBench, Labels: map[string]string{"zone": "b"},
		PassthroughGPUs: []proto.PassthroughGPU{{Address: "0000:01:00.0", Vendor: "nvidia", Model: "RTX"}}})

	place := func(body map[string]any) (string, string) {
		t.Helper()
		r := env.Do(key, "POST", "/api/v1/sandboxes", body)
		if r.Status != 202 {
			t.Fatalf("create %v: %s", body, r)
		}
		return r.Str("sandbox.id"), r.Str("sandbox.nodeId")
	}
	if _, n := place(map[string]any{"placement": map[string]any{"gpu": true}}); n != gpu.ID {
		t.Fatalf("gpu placement on %s", n)
	}
	if _, n := place(map[string]any{"placement": map[string]any{"labels": map[string]string{"zone": "a"}}, "type": "small"}); n != small.ID {
		t.Fatalf("label placement on %s", n)
	}
	if _, n := place(map[string]any{"placement": map[string]any{"minTier": "gold"}}); n != gpu.ID {
		t.Fatalf("tier placement on %s", n)
	}
	if _, n := place(map[string]any{"placement": map[string]any{"node": "small"}, "type": "small"}); n != small.ID {
		t.Fatalf("pinned placement on %s", n)
	}
	// xlarge (16 vCPU) only fits the big node; spread prefers emptier nodes anyway.
	if _, n := place(map[string]any{"type": "xlarge"}); n != gpu.ID {
		t.Fatalf("xlarge on %s", n)
	}
	// Impossible requirement.
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{"labels": map[string]string{"zone": "z"}}}); r.Status != 503 || r.Str("error.details.reason") != "no_matching_node" {
		t.Fatalf("no match: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{"minTier": "diamond"}}); r.Code() != "invalid_placement" {
		t.Fatalf("bad tier: %s", r)
	}
	// The small node is full for default (4 vCPU) after 2 small sandboxes -> goes to gpu, until pinned.
	// Without failFast the sandbox waits for capacity (SIZE-011).
	waiting := ""
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{"node": "small"}}); r.Status != 202 || r.Str("sandbox.state") != "provisioning" ||
		r.Str("sandbox.nodeId") != "" || r.Get("sandbox.waitingForCapacity") != true {
		t.Fatalf("full node: %s", r)
	} else {
		waiting = r.Str("sandbox.id")
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{"node": "small"}, "failFast": true}); r.Status != 503 || r.Code() != "no_ready_machine" {
		t.Fatalf("failFast: %s", r)
	}
	// The pool view.
	r := env.Do(key, "GET", "/api/v1/nodes", nil)
	if r.Status != 200 || r.Str("summary.online") != "2" || len(r.Get("nodes").([]any)) != 2 {
		t.Fatalf("nodes: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/nodes/"+small.ID, nil); r.Str("node.reserved.vcpu") != "4" || r.Str("node.free.vcpu") != "0" {
		t.Fatalf("node detail: %s", r)
	}
	// Disabled nodes get nothing.
	if r := env.Do(key, "PATCH", "/api/v1/nodes/"+gpu.ID, map[string]any{"disabled": true}); r.Status != 200 {
		t.Fatalf("disable: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "small"}); r.Status != 202 || r.Str("sandbox.nodeId") != "" {
		t.Fatalf("disabled node used: %s", r)
	} else {
		other := r.Str("sandbox.id")
		env.Do(key, "DELETE", "/api/v1/sandboxes/"+other, nil, "X-CrowdVM-Confirm-Delete: "+other)
	}
	// Room on the small node: the waiting sandbox is placed and boots.
	// Free the small node: delete everything placed there (any state; the
	// fake node may still be booting them).
	r = env.Do(key, "GET", "/api/v1/sandboxes", nil)
	for _, sb := range r.Get("sandboxes").([]any) {
		m := sb.(map[string]any)
		if m["nodeId"] == small.ID && m["id"] != waiting {
			env.Do(key, "DELETE", "/api/v1/sandboxes/"+m["id"].(string), nil, "X-CrowdVM-Confirm-Delete: "+m["id"].(string))
		}
	}
	if r := env.WaitState(key, waiting, "ready"); r.Str("sandbox.nodeId") != small.ID {
		t.Fatalf("waiting sandbox placed on %s", r.Str("sandbox.nodeId"))
	}
}

func TestSchedulerLocality(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	// A second, emptier node joins; the resume still prefers a (has the snapshot).
	b := env.StartNode(servertest.NodeOptions{Name: "b", Capacity: proto.Capacity{VCPU: 64, MemoryMiB: 262144, DiskGiB: 4000}})
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 202 || r.Str("sandbox.nodeId") != a.ID {
		t.Fatalf("resume locality: %s", r)
	}
	env.WaitState(key, id, "ready")
	if b.Called("POST /v1/vms") {
		t.Fatal("b should not be used")
	}
	// Without capacity on a, the resume moves to b and pulls the chain.
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": true}); r.Status != 200 {
		t.Fatal(r)
	}
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 202 || r.Str("sandbox.nodeId") != b.ID {
		t.Fatalf("resume elsewhere: %s", r)
	}
	env.WaitState(key, id, "ready")
	if !b.Called("POST /v1/snapshots/") {
		t.Fatal("b did not fetch the snapshot chain")
	}
	spec, _ := b.Spec(id)
	if !b.HasSnapshot(spec.ParentID) {
		t.Fatal("parent not on b")
	}
}

func TestSchedulerLiveLoad(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	cpcty := proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 20}
	busy := env.StartNode(servertest.NodeOptions{Name: "busy", Capacity: cpcty})
	calm := env.StartNode(servertest.NodeOptions{Name: "calm", Capacity: cpcty})
	idle := func(cpu float64, availMiB int) *proto.HostLoad {
		return &proto.HostLoad{CPUBusy: cpu, MemTotalMiB: 65536, MemAvailMiB: availMiB}
	}
	// The owner of "busy" runs a build: new sandboxes go to "calm".
	busy.SetLoad(idle(0.97, 60000))
	calm.SetLoad(idle(0.10, 60000))
	for i := 0; i < 2; i++ {
		r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "small"})
		if r.Status != 202 || r.Str("sandbox.nodeId") != calm.ID {
			t.Fatalf("placed on a hot host: %s", r)
		}
	}
	if r := env.Do(key, "GET", "/api/v1/nodes/"+busy.ID, nil); r.Get("node.load.hot") != true || r.Get("node.load.cpuBusy") == nil {
		t.Fatalf("node load: %s", r)
	}
	// "calm" is now short of memory: a hot host beats one that cannot take it.
	calm.SetLoad(idle(0.10, 600))
	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "small"})
	if r.Status != 202 || r.Str("sandbox.nodeId") != busy.ID {
		t.Fatalf("hot host as the last resort: %s", r)
	}
	// Both short of memory: the request waits for room.
	busy.SetLoad(idle(0.97, 600))
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "small", "failFast": true}); r.Status != 503 {
		t.Fatalf("no host with memory: %s", r)
	}
}

// A busy node that holds the sandbox's newest snapshot still gets its
// resume: moving the disk through the server costs more.
func TestSchedulerHotNodeKeepsLocality(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	a.SetLoad(&proto.HostLoad{CPUBusy: 0.97, MemTotalMiB: 65536, MemAvailMiB: 60000})
	b.SetLoad(&proto.HostLoad{CPUBusy: 0.05, MemTotalMiB: 65536, MemAvailMiB: 60000})
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 202 || r.Str("sandbox.nodeId") != a.ID {
		t.Fatalf("resume left its snapshot: %s", r)
	}
	// A new sandbox has no snapshot anywhere: the cool node gets it.
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "small"}); r.Status != 202 || r.Str("sandbox.nodeId") != b.ID {
		t.Fatalf("new sandbox on the hot node: %s", r)
	}
}
