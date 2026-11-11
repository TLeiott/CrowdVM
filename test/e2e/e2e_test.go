package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type group struct {
	name string
	fn   func(t *testing.T, c *cluster)
}

// groups run in this order; later groups may rely on state of earlier ones
// only where noted (webhooks registered in "webhooks" are checked in "lifecycle").
var groups = []group{
	{"nodes", testNodes},
	{"create", testCreate},
	{"exec", testExec},
	{"files", testFiles},
	{"ssh", testSSH},
	{"host", testHost},
	{"desktop", testDesktop},
	{"network", testNetwork},
	{"webhooks", testWebhooks},
	{"lifecycle", testLifecycle},
	{"snapshots", testSnapshots},
	{"saved-snapshots", testSavedSnapshots},
	{"environments", testEnvironments},
	{"agent", testAgent},
	{"ai-proxy", testAIProxy},
	{"apikeys", testAPIKeys},
	{"orgs", testOrgs},
	{"credits", testCredits},
	{"legacy-box", testLegacyBox},
	{"sdk", testSDKs},
	{"cli-fixes", testCLIFixes},
	{"docker-resume", testDockerResume},
	{"disk-cleanup", testDiskCleanup},
	{"agent-docs", testAgentDocs},
	{"boat-sdk-samples", testBoatSDKSamples},
	{"otel", testOTel},
	{"process-history", testProcessHistory},
	{"bootloop", testBootLoop}, // opt-in: CROWDVM_E2E_BOOTLOOP=N
}

func TestE2E(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1 to run the end-to-end suite (needs KVM, QEMU and the base image)")
	}
	t0 := time.Now()
	c := startCluster(t)
	t.Logf("cluster ready in %s: server %s, nodes %s, %s", time.Since(t0).Round(time.Second), c.url, c.nodes[0].id, c.nodes[1].id)
	type row struct {
		name   string
		status string
		dur    time.Duration
	}
	var rows []row
	for _, g := range groups {
		start := time.Now()
		ran := false
		ok := t.Run(g.name, func(t *testing.T) { ran = true; g.fn(t, c) })
		st := "PASS"
		switch {
		case !ok:
			st = "FAIL"
		case !ran:
			continue // filtered out with -run
		}
		rows = append(rows, row{g.name, st, time.Since(start)})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-18s %-5s %s\n", "group", "", "time")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-18s %-5s %s\n", r.name, r.status, r.dur.Round(time.Second))
	}
	fmt.Fprintf(&b, "total %s", time.Since(t0).Round(time.Second))
	t.Log(b.String())
}
