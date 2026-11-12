// Package e2e runs CrowdVM end to end on this machine: real binaries, a real
// server, two real QEMU/KVM nodes and real guest VMs, driven mostly through
// the crowdvm CLI as a coding agent would use it. See docs/E2E.md.
//
//	CROWDVM_E2E=1 go test -count=1 -timeout 60m -v ./test/e2e/
//	CROWDVM_E2E=1 go test -count=1 -timeout 60m -v -run 'TestE2E/(exec|files)' ./test/e2e/
//
// Environment:
//
//	CROWDVM_E2E=1          required, otherwise every test is skipped
//	CROWDVM_E2E_IMAGE      base image qcow2 (default ~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v2.qcow2)
//	CROWDVM_E2E_DIR        parent of the per-run directory (default ~/.local/share/crowdvm-dev/e2e; must not be tmpfs)
//	CROWDVM_E2E_KEEP=1     keep the run directory (data dirs and logs) after the run
//	CROWDVM_E2E_BOOTLOOP=N boot stress group "bootloop" (N boots; see bootloop_test.go),
//	                       CROWDVM_E2E_BOOTLOOP_MODE=create for creates only
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// cluster is one server, two nodes and a signed-in CLI, all private to one
// lane of this run (e2e_test.go).
type cluster struct {
	lane     int
	root     string // per-cluster run directory; every process we start has it in its argv
	bin      string
	repo     string
	logs     string
	port     int
	url      string // http://127.0.0.1:port
	apiBase  string // url + /api/v1
	cfgDir   string // CLI config dir of the admin user "e2e"
	home     string // HOME for CLI invocations (ssh keys, ssh-config)
	apiKey   string // admin API key for direct API calls
	image    string
	nodes    []*node
	procs    []*proc
	mu       sync.Mutex
	stopOnce sync.Once

	sharedOnce sync.Once
	sharedID   string
	sharedErr  error

	bobOnce sync.Once
	bob     *account // second account (features_test.go)

	hooks *receivers // webhook / OTLP / AI vendor receivers
	link  *nodeLink  // node 1's slowable link to the server (snapshots group)
}

type node struct {
	name   string
	id     string
	dir    string
	uiAddr string
	proc   *proc
}

type proc struct {
	name string
	cmd  *exec.Cmd
	log  string
	done chan struct{}
	err  error
}

const (
	adminUser = "e2e"
	adminPass = "e2e-pass-123456"
)

func repoRoot(t testing.TB) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func baseImage() string {
	if p := os.Getenv("CROWDVM_E2E_IMAGE"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v2.qcow2")
}

func runsRoot() string {
	if p := os.Getenv("CROWDVM_E2E_DIR"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local/share/crowdvm-dev/e2e")
}

// clusters lists every cluster of this run for the watchdog.
var clusters struct {
	sync.Mutex
	all []*cluster
}

// newCluster makes the run directory of lane's cluster and registers it for
// the watchdog; the caller tears it down. setup starts it.
func newCluster(t *testing.T, lane int) *cluster {
	t.Helper()
	img := baseImage()
	if _, err := os.Stat(img); err != nil {
		t.Fatalf("base image %s: %v (set CROWDVM_E2E_IMAGE)", img, err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf("/dev/kvm: %v", err)
	}
	parent := runsRoot()
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, fmt.Sprintf("run-%s-l%d-", time.Now().Format("0102-150405"), lane))
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(root, "owner.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644))
	c := &cluster{lane: lane, root: root, repo: repoRoot(t), bin: filepath.Join(root, "bin"), logs: filepath.Join(root, "logs"),
		cfgDir: filepath.Join(root, "cli"), home: filepath.Join(root, "home"), image: strings.TrimSuffix(filepath.Base(img), filepath.Ext(img))}
	clusters.Lock()
	clusters.all = append(clusters.all, c)
	clusters.Unlock()
	t.Logf("lane %d: run dir %s", lane, root)
	return c
}

// setup builds the binaries and starts server + 2 nodes; the base image is
// copied in turn (see imageTurn).
func (c *cluster) setup(t *testing.T, turn *imageTurn) {
	t.Helper()
	defer turn.end()
	img := baseImage()
	for _, d := range []string{c.bin, c.logs, c.home} {
		must(t, os.MkdirAll(d, 0o755))
	}
	step := func(name string, f func()) {
		t0 := time.Now()
		f()
		t.Logf("setup: %s (%s)", name, time.Since(t0).Round(100*time.Millisecond))
	}
	step("build", func() { c.build(t) })
	step("receivers", func() { c.hooks = startReceivers(t) })
	step("server", func() { c.startServer(t) })
	step("admin user + CLI login", func() { c.login(t) })
	step("nodes join (benchmark)", func() {
		for i := 1; i <= 2; i++ {
			c.joinNode(t, i)
		}
	})
	step("wait for the image turn", turn.wait)
	step("image import + push (node 1)", func() {
		c.nodeCmd(t, c.nodes[0], 2*time.Minute, "image", "import", img)
		c.nodeCmd(t, c.nodes[0], 10*time.Minute, "image", "push", c.image)
	})
	step("image pull (node 2)", func() { c.nodeCmd(t, c.nodes[1], 10*time.Minute, "image", "pull", c.image) })
	turn.end()
	step("nodes run", func() {
		for _, n := range c.nodes {
			n.proc = c.start(t, n.name, c.nodeBin(), "run", "--data-dir", n.dir)
		}
		waitFor(t, 60*time.Second, time.Second, "both nodes online", func() (bool, string) {
			var out struct {
				Nodes []struct {
					ID     string   `json:"id"`
					Name   string   `json:"name"`
					Online bool     `json:"online"`
					Images []string `json:"images"`
				} `json:"nodes"`
			}
			if st := c.apiJSON(t, "GET", "/nodes", nil, &out); st != 200 {
				return false, fmt.Sprint("status ", st)
			}
			on := 0
			for _, n := range out.Nodes {
				for _, mine := range c.nodes {
					if n.Name == mine.name {
						mine.id = n.ID
						if n.Online && contains(n.Images, c.image) {
							on++
						}
					}
				}
			}
			return on == 2, fmt.Sprintf("%d/2 online with image", on)
		})
	})
	step("node labels", func() {
		for i, n := range c.nodes {
			c.cli(t, "nodes", "update", n.id, "--label", "zone="+string(rune('a'+i))).ok(t)
		}
	})
}

func (c *cluster) serverBin() string { return filepath.Join(c.bin, "crowdvm-server") }
func (c *cluster) nodeBin() string   { return filepath.Join(c.bin, "crowdvm-node") }
func (c *cluster) cliBin() string    { return filepath.Join(c.bin, "crowdvm") }

func (c *cluster) build(t *testing.T) {
	run := func(env []string, args ...string) {
		cmd := exec.Command("go", args...)
		cmd.Dir = c.repo
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// CGO off: the node ships crowdvm + crowdvm-guest into the VM's config drive.
	run([]string{"CGO_ENABLED=0"}, "build", "-o", c.bin+"/", "./cmd/crowdvm", "./cmd/crowdvm-server", "./cmd/crowdvm-node")
	run([]string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}, "build", "-o", filepath.Join(c.bin, "crowdvm-guest"), "./cmd/crowdvm-guest")
}

func (c *cluster) startServer(t *testing.T) {
	c.port = freePort(t)
	c.url = fmt.Sprintf("http://127.0.0.1:%d", c.port)
	c.apiBase = c.url + "/api/v1"
	c.startEnv(t, "server", c.hooks.serverEnv(), c.serverBin(), "serve",
		"--listen", fmt.Sprintf("127.0.0.1:%d", c.port),
		"--data-dir", filepath.Join(c.root, "server"),
		"--public-url", c.url,
		"--public-domain", "localhost",
		"--starts-per-minute", "0", "--starts-per-hour", "0", "--starts-per-day", "0",
		"--ip-rate-limit", "0", "--signup-credits", "1000", "--log-level", "debug",
		"--webhook-allow-local") // the webhook / OTLP receivers listen on 127.0.0.1
	waitFor(t, 30*time.Second, 200*time.Millisecond, "server up", func() (bool, string) {
		resp, err := http.Get(c.apiBase + "/server")
		if err != nil {
			return false, err.Error()
		}
		resp.Body.Close()
		return resp.StatusCode < 500, resp.Status
	})
}

func (c *cluster) login(t *testing.T) {
	c.cmd(t, 30*time.Second, nil, c.serverBin(), "user", "create", "--data-dir", filepath.Join(c.root, "server"),
		"--username", adminUser, "--password", adminPass, "--admin")
	r := c.cliIn(t, adminPass+"\n", "login", "--server", c.url, "--username", adminUser, "--password-stdin")
	r.ok(t)
	// A long-lived admin API key for direct API calls (the CLI keeps its own session key).
	r = c.cli(t, "api-key", "create", "e2e-admin", "--preset", "admin", "--ttl", "1d")
	r.ok(t)
	c.apiKey = findKey(r.out)
	if c.apiKey == "" {
		t.Fatalf("no key in api-key create output: %s", r.out)
	}
}

// findKey returns the first cvm_... token in s.
func findKey(s string) string {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(" \t\n\"',:{}[]", r) }) {
		if strings.HasPrefix(f, "cvm_") && len(f) > 20 {
			return f
		}
	}
	return ""
}

func (c *cluster) joinNode(t *testing.T, i int) {
	n := &node{name: fmt.Sprintf("e2e-node-%d", i), dir: filepath.Join(c.root, fmt.Sprintf("node%d", i)),
		uiAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t))}
	r := c.cli(t, "nodes", "join-token", "--name", n.name)
	r.ok(t)
	tok, _ := r.last()["token"].(string)
	if !strings.HasPrefix(tok, "cvj_") {
		t.Fatalf("join-token: %s", r.out)
	}
	// 8 vCPU / 18 GiB / 2 sandboxes per node: at most 4 VMs per cluster.
	c.nodeCmd(t, n, 3*time.Minute, "join", "--server", c.url, "--token", tok, "--name", n.name,
		"--driver", "qemu", "--guest-binary", filepath.Join(c.bin, "crowdvm-guest"),
		"--ui-addr", n.uiAddr, "--cpus", "8", "--memory", "18G", "--disk", "80G", "--max-sandboxes", "2",
		"--snapshot-interval", "60s")
	if i == 2 {
		// Node 2's owner allows this host's LAN address (exact /32) so guests
		// there can clone from a local git daemon (environments group). Node 1
		// keeps the default policy (network group).
		if ip := lanIP(); ip != "" {
			c.nodeCmd(t, n, 30*time.Second, "config", "set", "net.allow", ip+"/32")
		}
	}
	c.nodes = append(c.nodes, n)
}

func (c *cluster) nodeCmd(t *testing.T, n *node, timeout time.Duration, args ...string) string {
	t.Helper()
	return c.cmd(t, timeout, nil, c.nodeBin(), append(args, "--data-dir", n.dir)...)
}

// cmd runs a helper command to completion and fails the test on error.
func (c *cluster) cmd(t *testing.T, timeout time.Duration, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(c.baseEnv(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, tail(string(out), 4000))
	}
	return string(out)
}

// start launches a long-running process with its log in c.logs.
func (c *cluster) start(t *testing.T, name, bin string, args ...string) *proc {
	t.Helper()
	return c.startEnv(t, name, nil, bin, args...)
}

func (c *cluster) startEnv(t *testing.T, name string, env []string, bin string, args ...string) *proc {
	t.Helper()
	logPath := filepath.Join(c.logs, name+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // a restarted process appends
	must(t, err)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(c.baseEnv(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	// Own process group (killed as a group) and SIGKILL if the test binary dies.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	must(t, cmd.Start())
	p := &proc{name: name, cmd: cmd, log: logPath, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); f.Close(); close(p.done) }()
	c.mu.Lock()
	c.procs = append(c.procs, p)
	c.mu.Unlock()
	return p
}

func (c *cluster) baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CROWDVM_") || strings.HasPrefix(kv, "BOAT_") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+c.home, "CROWDVM_CONFIG_DIR="+c.cfgDir, "CROWDVM_NO_UPDATE_CHECK=1")
}

// watchdog tears every cluster down on SIGINT/SIGTERM and 45 s before the go
// test deadline (a timeout panic would skip t.Cleanup and leak VMs).
func watchdog(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var timer <-chan time.Time
	if dl, ok := t.Deadline(); ok {
		timer = time.After(time.Until(dl) - 45*time.Second)
	}
	go func() {
		select {
		case s := <-sig:
			fmt.Fprintf(os.Stderr, "e2e: %v: tearing the clusters down\n", s)
			teardownAll(nil, false)
			os.Exit(2)
		case <-timer:
			fmt.Fprintln(os.Stderr, "e2e: go test deadline close: tearing the clusters down")
			teardownAll(nil, true) // every lane still running is about to fail
		}
	}()
}

// teardownAll tears every cluster of the run down at once; a lane's cluster
// already torn down when its lane ended is skipped (stopOnce).
func teardownAll(t *testing.T, failed bool) {
	clusters.Lock()
	cs := append([]*cluster(nil), clusters.all...)
	clusters.Unlock()
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func() { defer wg.Done(); c.teardown(t, failed) }()
	}
	wg.Wait()
}

// teardown stops the cluster and removes its run directory; failed keeps a
// copy of its logs. t (nil from the watchdog) gets the log lines.
func (c *cluster) teardown(t *testing.T, failed bool) {
	c.stopOnce.Do(func() {
		logf := func(f string, a ...any) {
			f = fmt.Sprintf("lane %d: %s", c.lane, f)
			if t != nil {
				t.Logf(f, a...)
			} else {
				fmt.Fprintf(os.Stderr, f+"\n", a...)
			}
		}
		if c.hooks != nil {
			c.hooks.close()
		}
		c.mu.Lock()
		if c.link != nil {
			c.link.l.Close()
		}
		procs := append([]*proc(nil), c.procs...)
		c.mu.Unlock()
		// Nodes first (they own the VMs), then the server.
		for i := len(procs) - 1; i >= 0; i-- {
			p := procs[i]
			select {
			case <-p.done:
				continue
			default:
			}
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(20 * time.Second):
				logf("teardown: %s did not exit on SIGTERM; killing", p.name)
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
				<-p.done
			}
		}
		// QEMU runs in its own process group: kill whatever still has our run dir in argv.
		if n := killMatching(c.root); n > 0 {
			logf("teardown: killed %d leftover process(es) (qemu)", n)
		}
		if failed {
			dst := filepath.Join(os.TempDir(), "crowdvm-e2e-logs-"+filepath.Base(c.root))
			if err := exec.Command("cp", "-r", c.logs, dst).Run(); err == nil {
				logf("logs kept in %s", dst)
			}
		}
		if os.Getenv("CROWDVM_E2E_KEEP") == "1" {
			logf("kept %s", c.root)
			return
		}
		if err := os.RemoveAll(c.root); err != nil {
			logf("teardown: remove %s: %v", c.root, err)
		}
	})
}

// killMatching SIGKILLs every process (not us) whose argv mentions dir.
func killMatching(dir string) int {
	n := 0
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !bytes.Contains(b, []byte(dir)) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			n++
		}
	}
	return n
}

// killStale cleans up after crashed earlier runs (once, before any cluster of this run starts). A run whose go test process
// still lives (owner.pid) is a concurrent run, e.g. of another checkout or
// agent, and is left alone.
func killStale(t *testing.T, parent string) {
	old, _ := filepath.Glob(filepath.Join(parent, "run-*"))
	n := 0
	for _, d := range old {
		if ownerAlive(d) {
			t.Logf("leaving %s alone: its run is still going", d)
			continue
		}
		n += killMatching(d)
		if os.Getenv("CROWDVM_E2E_KEEP") != "1" {
			defer os.RemoveAll(d)
		}
	}
	if n > 0 {
		t.Logf("killed %d process(es) of an earlier run", n)
		time.Sleep(time.Second)
	}
}

// otherE2E reports whether another e2e test binary runs on this machine.
func otherE2E() bool {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err != nil || pid == os.Getpid() {
			continue
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil && bytes.HasSuffix(bytes.SplitN(b, []byte{0}, 2)[0], []byte("e2e.test")) {
			return true
		}
	}
	return false
}

// ownerAlive reports whether the go test process that made run dir d still runs.
func ownerAlive(d string) bool {
	b, err := os.ReadFile(filepath.Join(d, "owner.pid"))
	if err != nil {
		return otherE2E() // a run of an older harness: alive while any other e2e test binary runs
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid == os.Getpid() {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && bytes.HasSuffix(bytes.SplitN(cmdline, []byte{0}, 2)[0], []byte("e2e.test"))
}

// ---------- CLI ----------

type result struct {
	args []string
	out  string // stdout
	err  string // stderr
	code int
	dur  time.Duration
}

func (r result) String() string {
	return fmt.Sprintf("crowdvm %s → exit %d (%s)\nstdout: %s\nstderr: %s", strings.Join(r.args, " "), r.code,
		r.dur.Round(time.Millisecond), tail(r.out, 3000), tail(r.err, 3000))
}

func (r result) ok(t testing.TB) result {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("%v", r)
	}
	return r
}

// lines decodes every JSON object line of stdout.
func (r result) lines() []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(r.out))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (r result) last() map[string]any {
	l := r.lines()
	if len(l) == 0 {
		var m map[string]any
		if json.Unmarshal([]byte(r.out), &m) == nil {
			return m
		}
		return map[string]any{}
	}
	return l[len(l)-1]
}

// event returns the first JSON line whose "event" (or "type") is name.
func (r result) event(name string) map[string]any {
	for _, m := range r.lines() {
		if m["event"] == name || m["type"] == name {
			return m
		}
	}
	return nil
}

type cliOpt struct {
	cfgDir  string
	stdin   string
	timeout time.Duration
	env     []string
	dir     string
}

func (c *cluster) cli(t testing.TB, args ...string) result {
	t.Helper()
	return c.cliWith(t, cliOpt{}, args...)
}

func (c *cluster) cliIn(t testing.TB, stdin string, args ...string) result {
	t.Helper()
	return c.cliWith(t, cliOpt{stdin: stdin}, args...)
}

func (c *cluster) cliT(t testing.TB, timeout time.Duration, args ...string) result {
	t.Helper()
	return c.cliWith(t, cliOpt{timeout: timeout}, args...)
}

func (c *cluster) cliWith(t testing.TB, o cliOpt, args ...string) result {
	t.Helper()
	if o.timeout == 0 {
		o.timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.cliBin(), args...)
	cmd.Env = c.baseEnv()
	if o.cfgDir != "" {
		cmd.Env = append(cmd.Env, "CROWDVM_CONFIG_DIR="+o.cfgDir)
	}
	cmd.Env = append(cmd.Env, o.env...)
	cmd.Dir = o.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if o.stdin != "" {
		cmd.Stdin = strings.NewReader(o.stdin)
	}
	t0 := time.Now()
	err := cmd.Run()
	r := result{args: args, out: so.String(), err: se.String(), dur: time.Since(t0)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		r.code = -1
		r.err += "\n[harness] " + err.Error()
	}
	if ctx.Err() != nil {
		r.code = -1
		r.err += fmt.Sprintf("\n[harness] timed out after %s", o.timeout)
	}
	return r
}

// ---------- API ----------

type apiResp struct {
	status int
	body   []byte
	header http.Header
}

func (a apiResp) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(a.body, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func (a apiResp) String() string {
	return fmt.Sprintf("HTTP %d %s", a.status, tail(string(a.body), 2000))
}

var httpc = &http.Client{Timeout: 5 * time.Minute}

// apiKeyCall calls /api/v1+path with the given bearer key.
func (c *cluster) apiKeyCall(t testing.TB, key, method, path string, body any) apiResp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.apiBase + path
	}
	req, err := http.NewRequest(method, u, rd)
	must(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return apiResp{status: resp.StatusCode, body: b, header: resp.Header}
}

func (c *cluster) api(t testing.TB, method, path string, body any) apiResp {
	t.Helper()
	return c.apiKeyCall(t, c.apiKey, method, path, body)
}

func (c *cluster) apiJSON(t testing.TB, method, path string, body, out any) int {
	t.Helper()
	r := c.api(t, method, path, body)
	if out != nil {
		_ = json.Unmarshal(r.body, out)
	}
	return r.status
}

// sandbox returns GET /sandboxes/{id} (the sandbox object).
func (c *cluster) sandbox(t testing.TB, id string) map[string]any {
	t.Helper()
	r := c.api(t, "GET", "/sandboxes/"+id, nil)
	m := r.json()
	if sb, ok := m["sandbox"].(map[string]any); ok {
		return sb
	}
	return m
}

func (c *cluster) state(t testing.TB, id string) string {
	t.Helper()
	s, _ := c.sandbox(t, id)["state"].(string)
	return s
}

func (c *cluster) waitState(t testing.TB, id string, timeout time.Duration, states ...string) map[string]any {
	t.Helper()
	var sb map[string]any
	waitFor(t, timeout, time.Second, id+" state in "+strings.Join(states, "|"), func() (bool, string) {
		sb = c.sandbox(t, id)
		st, _ := sb["state"].(string)
		return contains(states, st), "state=" + st
	})
	return sb
}

// ---------- sandboxes ----------

// newSandbox runs `crowdvm new` and returns the id once ready. The sandbox is
// deleted when the test ends.
func (c *cluster) newSandbox(t testing.TB, args ...string) string {
	t.Helper()
	r := c.cliT(t, 8*time.Minute, append([]string{"new", "--json", "--type", "small"}, args...)...)
	if ev := r.event("created"); ev != nil {
		if id, _ := ev["id"].(string); id != "" {
			c.cleanupSandbox(t, id)
		}
	}
	r.ok(t)
	ev := r.event("ready")
	if ev == nil {
		t.Fatalf("no ready event: %v", r)
	}
	return ev["id"].(string)
}

func (c *cluster) cleanupSandbox(t testing.TB, id string) {
	t.Cleanup(func() { c.deleteSandbox(t, id) })
}

// deleteSandbox deletes and waits until the node no longer runs the VM.
func (c *cluster) deleteSandbox(t testing.TB, id string) {
	r := c.cliT(t, 3*time.Minute, "delete", id, "--yes")
	if r.code != 0 && !strings.Contains(r.out+r.err, "not_found") {
		t.Logf("cleanup: %v", r)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && qemuPID(id) != 0 {
		time.Sleep(time.Second)
	}
	if pid := qemuPID(id); pid != 0 {
		t.Errorf("cleanup: qemu of %s (pid %d) still runs 90 s after delete", id, pid)
	}
}

// sh runs a shell command line in the sandbox via `crowdvm exec` and returns
// stdout; it fails the test on a non-zero exit.
func (c *cluster) sh(t testing.TB, id, script string) string {
	t.Helper()
	r := c.execR(t, id, script)
	if r.code != 0 {
		t.Fatalf("exec %q in %s: %v", script, id, r)
	}
	return r.out
}

func (c *cluster) execR(t testing.TB, id, script string, flags ...string) result {
	t.Helper()
	args := append([]string{"exec", id}, flags...)
	return c.cliT(t, 5*time.Minute, append(args, "--", "bash", "-lc", script)...)
}

// shared returns a long-lived small sandbox for read-mostly groups.
func (c *cluster) shared(t testing.TB) string {
	t.Helper()
	c.sharedOnce.Do(func() {
		r := c.cliT(t, 8*time.Minute, "new", "--json", "--type", "small", "--name", "e2e-shared", "--ttl", "7200", "--node", c.nodes[0].name)
		if ev := r.event("ready"); ev != nil {
			c.sharedID = ev["id"].(string)
		} else {
			c.sharedErr = fmt.Errorf("%v", r)
		}
	})
	if c.sharedErr != nil {
		t.Fatalf("shared sandbox: %v", c.sharedErr)
	}
	return c.sharedID
}

// qemuPID finds the QEMU process of a sandbox (argv has -name crowdvm-<id>).
func qemuPID(sandboxID string) int {
	ents, _ := os.ReadDir("/proc")
	needle := []byte("crowdvm-" + sandboxID + "\x00")
	needle2 := []byte("crowdvm-" + sandboxID + ",")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !bytes.HasPrefix(b, []byte("qemu-system")) && !bytes.Contains(b, []byte("/qemu-system")) {
			continue
		}
		if bytes.Contains(b, needle) || bytes.Contains(b, needle2) {
			return pid
		}
	}
	return 0
}

// nodeOf returns the node id a sandbox runs on.
func (c *cluster) nodeOf(t testing.TB, id string) string {
	t.Helper()
	sb := c.sandbox(t, id)
	n, _ := sb["node"].(string)
	if n == "" {
		n, _ = sb["nodeId"].(string)
	}
	return n
}

// ---------- small helpers ----------

func waitFor(t testing.TB, timeout, every time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, info := f()
		if ok {
			return
		}
		last = info
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (last: %s)", timeout, what, last)
		}
		time.Sleep(every)
	}
}

// eventually is waitFor that reports an error instead of stopping the test.
func eventually(t testing.TB, timeout, every time.Duration, what string, f func() (bool, string)) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, info := f()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			t.Errorf("timed out after %s waiting for %s (last: %s)", timeout, what, info)
			return false
		}
		time.Sleep(every)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// get walks a decoded JSON value by dotted path ("sandbox.state", "items.0.id").
func get(v any, path string) any {
	for _, p := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

func gets(v any, path string) string {
	s, _ := get(v, path).(string)
	return s
}

func list(v any, path string) []any {
	l, _ := get(v, path).([]any)
	return l
}

func expect(t testing.TB, cond bool, f string, a ...any) {
	t.Helper()
	if !cond {
		t.Errorf(f, a...)
	}
}

func expectContains(t testing.TB, s, sub, what string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s: %q not in %q", what, sub, tail(s, 1500))
	}
}

// logTail returns the last n bytes of a cluster log ("server", "e2e-node-1"...).
func (c *cluster) logTail(name string, n int) string {
	b, _ := os.ReadFile(filepath.Join(c.logs, name+".log"))
	return tail(string(b), n)
}
