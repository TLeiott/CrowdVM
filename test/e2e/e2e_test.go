package e2e

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type group struct {
	name string
	fn   func(t *testing.T, c *cluster)
}

// lanes run at the same time, each on a private cluster (server + 2 nodes) of
// its own. A lane runs its groups in order, each one alone on that cluster, as
// the whole suite once did on a single cluster. Groups that depend on each
// other share a lane: lifecycle and otel check the receivers that webhooks
// registers. The lanes are balanced so that none takes much longer than
// snapshots, the longest group; a later lane gets its cluster later (see
// imageTurn) and less work. agent and process-history wait up to 6 min each
// when the free OpenCode model does not answer, so they are in different lanes.
var lanes = [][]group{
	{
		{"snapshots", testSnapshots},
	},
	{
		{"boat-sdk-samples", testBoatSDKSamples},
		{"sdk", testSDKs},
		{"agent-docs", testAgentDocs},
		{"credits", testCredits},
		{"process-history", testProcessHistory},
	},
	{
		{"agent", testAgent},
		{"saved-snapshots", testSavedSnapshots},
		{"docker-resume", testDockerResume},
	},
	{
		{"nodes", testNodes},
		{"create", testCreate},
		{"exec", testExec},
		{"files", testFiles},
		{"host", testHost},
		{"desktop", testDesktop},
		{"network", testNetwork},
		{"webhooks", testWebhooks},
		{"lifecycle", testLifecycle},
		{"environments", testEnvironments},
		{"ai-proxy", testAIProxy},
		{"apikeys", testAPIKeys},
		{"orgs", testOrgs},
		{"legacy-box", testLegacyBox},
		{"cli-fixes", testCLIFixes},
		{"disk-cleanup", testDiskCleanup},
		{"ssh", testSSH},
		{"otel", testOTel},
		{"bootloop", testBootLoop}, // opt-in: CROWDVM_E2E_BOOTLOOP=N
	},
}

// plan merges the lanes onto n clusters (CROWDVM_E2E_CLUSTERS, default one
// per lane); lanes that share a cluster run one after the other, the later
// lane first: lane 4 keeps the original suite's order, so create (placement
// on a full node 1 must fail fast) runs before any group starts the shared
// sandbox there.
func plan() [][]group {
	n, _ := strconv.Atoi(os.Getenv("CROWDVM_E2E_CLUSTERS"))
	if n <= 0 || n > len(lanes) {
		n = len(lanes)
	}
	out := make([][]group, n)
	for i, l := range lanes {
		out[i%n] = append(slices.Clone(l), out[i%n]...)
	}
	return out
}

func TestE2E(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1 to run the end-to-end suite (needs KVM, QEMU and the base image)")
	}
	t0 := time.Now()
	killStale(t, runsRoot())
	watchdog(t)
	t.Cleanup(func() { teardownAll(t, t.Failed()) }) // a panic skips the lanes' own teardown
	type row struct {
		name, status string
		dur          time.Duration
	}
	p := plan()
	rows := make([][]row, len(p))
	setups := make([]time.Duration, len(p))
	turns := make([]imageTurn, len(p))
	for i := range turns {
		turns[i].done = make(chan struct{})
		if i > 0 {
			turns[i].prev = turns[i-1].done
		}
	}
	var wg sync.WaitGroup
	for i, lane := range p {
		wg.Add(1)
		// t.Run may be called from several goroutines; each lane calls it
		// from its own, so the subtests stay TestE2E/<group> for -run.
		go func() {
			defer wg.Done()
			defer turns[i].end() // also when -run selects none of its groups
			var c *cluster       // started by the first group that runs (-run may select none)
			tried, ready, failed := false, false, false
			for _, g := range lane {
				var dur time.Duration
				ran := false
				ok := t.Run(g.name, func(st *testing.T) {
					ran = true
					if !tried {
						tried = true
						s0 := time.Now()
						c = newCluster(st, i+1)
						c.setup(st, &turns[i])
						ready = true
						setups[i] = time.Since(s0)
						st.Logf("lane %d: cluster ready in %s: server %s, nodes %s, %s", i+1, setups[i].Round(time.Second), c.url, c.nodes[0].id, c.nodes[1].id)
					}
					if !ready {
						st.Fatalf("lane %d: the cluster did not start (see the lane's first group)", i+1)
					}
					g0 := time.Now()
					defer func() { dur = time.Since(g0) }()
					g.fn(st, c)
				})
				if !ran {
					continue // filtered out with -run
				}
				st := "PASS"
				if !ok {
					st, failed = "FAIL", true
				}
				rows[i] = append(rows[i], row{g.name, st, dur})
			}
			if c != nil {
				c.teardown(t, failed) // free its VMs and disk while the other lanes still run
			}
		}()
	}
	wg.Wait()
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-4s %-18s %-5s %s\n", "lane", "group", "", "time")
	for i, rs := range rows {
		var sum time.Duration
		for _, r := range rs {
			fmt.Fprintf(&b, "%-4d %-18s %-5s %s\n", i+1, r.name, r.status, r.dur.Round(time.Second))
			sum += r.dur
		}
		if len(rs) > 0 {
			fmt.Fprintf(&b, "lane %d: setup %s + groups %s\n", i+1, setups[i].Round(time.Second), sum.Round(time.Second))
		}
	}
	fmt.Fprintf(&b, "total %s", time.Since(t0).Round(time.Second))
	t.Log(b.String())
}

// imageTurn: the clusters copy the base image (import + push + pull, ~13 GB
// of disk traffic each) one after the other, lane 1 first. Four at a time
// slowed each other down to 2.5 min for all; one at a time takes ~20 s each,
// and the lane with the longest work starts first.
type imageTurn struct {
	prev <-chan struct{} // closed when the previous lane's turn ended; nil for lane 1
	done chan struct{}
	once sync.Once
}

func (it *imageTurn) wait() {
	if it.prev != nil {
		<-it.prev
	}
}

func (it *imageTurn) end() { it.once.Do(func() { close(it.done) }) }
