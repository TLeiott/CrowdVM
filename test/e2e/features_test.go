package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------- second account ----------

type account struct {
	cfg string // CLI config dir
}

var bobOnce sync.Once
var bobAcct *account

// second creates (once) the account "bob" with its own CLI config.
func (c *cluster) second(t testing.TB) *account {
	t.Helper()
	bobOnce.Do(func() {
		a := &account{cfg: filepath.Join(c.root, "cli-bob")}
		r := c.cliIn(t, "bob-pass-123456\n", "admin", "users", "create", "bob", "--password-stdin", "--credits", "100", "--email", "bob@e2e.test")
		if r.code != 0 {
			t.Fatalf("create bob: %v", r)
		}
		r = c.cliWith(t, cliOpt{cfgDir: a.cfg, stdin: "bob-pass-123456\n"}, "login", "--server", c.url, "--username", "bob", "--password-stdin")
		if r.code != 0 {
			t.Fatalf("bob login: %v", r)
		}
		bobAcct = a
	})
	if bobAcct == nil {
		t.Fatal("second account unavailable")
	}
	return bobAcct
}

// ---------- environments ----------

func testEnvironments(t *testing.T, c *cluster) {
	ip := lanIP()
	if ip == "" {
		t.Skip("no LAN address for the git daemon")
	}
	// A bare repo served by git daemon on the LAN address (node 2 allows it).
	gd := t.TempDir()
	work := filepath.Join(gd, "work")
	must(t, os.MkdirAll(work, 0o755))
	must(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("e2e repo\n"), 0o644))
	gitRun(t, work, "init", "-q", "-b", "main")
	gitRun(t, work, "add", ".")
	gitRun(t, work, "-c", "user.email=e2e@x", "-c", "user.name=e2e", "commit", "-qm", "init")
	gitRun(t, gd, "clone", "-q", "--bare", work, filepath.Join(gd, "proj.git"))
	port := c.gitPort
	daemon := exec.Command("git", "daemon", "--reuseaddr", "--export-all", "--base-path="+gd, "--listen="+ip, fmt.Sprintf("--port=%d", port), gd)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	must(t, daemon.Start())
	t.Cleanup(func() { _ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL); _ = daemon.Wait() })
	waitFor(t, 10*time.Second, 200*time.Millisecond, "git daemon", func() (bool, string) {
		conn, err := net.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		if err == nil {
			conn.Close()
		}
		return err == nil, fmt.Sprint(err)
	})
	repoURL := fmt.Sprintf("git://%s:%d/proj.git", ip, port)

	env := "e2e-env"
	c.cli(t, "env", "new", env).ok(t)
	t.Cleanup(func() { c.cli(t, "env", "rm", env) })
	c.cli(t, "env", "set-var", env, "GREETING=hello-env").ok(t)
	c.cliIn(t, "TOKEN=from-secret-file\n", "env", "set-file", env, ".config/e2e/token.env").ok(t)
	// A secret file inside the repository's folder (Boat: backend/.env).
	c.cliIn(t, "DB=secret\n", "env", "set-file", env, "proj/.env").ok(t)
	c.cli(t, "env", "add-repo", env, repoURL, "--branch", "main").ok(t)
	// Repo setup script (API only): replaces the repo list.
	ei := c.cli(t, "env", "info", env).ok(t).last()
	envID := gets(ei, "environment.id")
	if envID == "" {
		envID = gets(ei, "id")
	}
	up := c.api(t, "PUT", "/environments/"+envID, map[string]any{"repositories": []map[string]any{{
		"repositoryId": repoURL, "baseBranch": "main", "setupScript": "echo setup-in-$(basename $PWD) > ~/repo-setup.txt",
	}}})
	expect(t, up.status == 200, "set repo setup script: %v", up)
	expectContains(t, c.cli(t, "env", "list").ok(t).out, env, "env list")
	// A local path is not a repository the guest could reach.
	bad := c.cli(t, "env", "add-repo", env, filepath.Join(gd, "proj.git"))
	expect(t, bad.code != 0, "env add-repo with a local path succeeded")

	e := c.newSandbox(t, "--environment", env, "--node", c.nodes[1].name, "--name", "env-sb")
	expect(t, strings.TrimSpace(c.sh(t, e, "echo $GREETING")) == "hello-env", "env var")
	out := c.sh(t, e, "cat ~/.config/e2e/token.env; stat -c %a ~/.config/e2e/token.env")
	expect(t, strings.Contains(out, "TOKEN=from-secret-file") && strings.Contains(out, "600"), "secret file: %q", out)
	expect(t, strings.TrimSpace(c.sh(t, e, "cat ~/proj/README.md")) == "e2e repo", "repo cloned before ready")
	out = c.sh(t, e, "cat ~/proj/.env; git -C ~/proj rev-parse --abbrev-ref HEAD")
	expect(t, strings.Contains(out, "DB=secret") && strings.Contains(out, "main"), "secret file in the repo folder + clone: %q", out)
	waitFor(t, 3*time.Minute, 2*time.Second, "repo setup script", func() (bool, string) {
		r := c.execR(t, e, "cat ~/repo-setup.txt 2>/dev/null")
		return strings.TrimSpace(r.out) == "setup-in-proj", r.out
	})

	// Change the environment and upgrade the running sandbox live.
	c.cli(t, "env", "set-var", env, "GREETING=hello-v2").ok(t)
	c.cli(t, "env", "set-var", env, "ADDED=yes").ok(t)
	c.cli(t, "env", "rm-file", env, ".config/e2e/token.env").ok(t)
	ug := c.cli(t, "env", "upgrade", env).ok(t).last()
	expect(t, fmt.Sprint(get(ug, "upgradedLive")) != "0" && get(ug, "upgradedLive") != nil, "env upgrade: %v", ug)
	waitFor(t, 2*time.Minute, 2*time.Second, "live upgrade applied", func() (bool, string) {
		r := c.execR(t, e, "echo $GREETING-$ADDED; test -e ~/.config/e2e/token.env && echo file-present || echo file-gone")
		return strings.Contains(r.out, "hello-v2-yes") && strings.Contains(r.out, "file-gone"), r.out
	})
	sb := c.sandbox(t, e)
	expect(t, get(sb, "environmentVersion") != 1.0, "environmentVersion after upgrade: %v", sb["environmentVersion"])

	// --no-env: none of the owner's secrets.
	ne := c.newSandbox(t, "--environment", env, "--no-env", "--no-snapshots")
	expect(t, strings.TrimSpace(c.sh(t, ne, "echo ${GREETING:-unset}")) == "unset", "--no-env still got env vars")
}

// ---------- integrated agent (OpenCode, free model, no credentials) ----------

const freeModel = "opencode/beam-free"

// testAgent runs one row per harness: Codex and Claude Code on the caller's
// own login (crowdvm prompt --local-login), then OpenCode's free model
// (optional: it skips when the model does not answer).
func testAgent(t *testing.T, c *cluster) {
	t.Run("codex", func(t *testing.T) { testLocalLogin(t, c, "codex", "gpt-6-luna") })
	t.Run("claude", func(t *testing.T) { testLocalLogin(t, c, "claude", "claude-haiku-5-5") })
	t.Run("opencode", func(t *testing.T) { testAgentOpencode(t, c) })
}

// testLocalLogin: one turn on this machine's own login, then a check that
// the credential reached no disk, event or history (guest, node, server).
func testLocalLogin(t *testing.T, c *cluster, provider, model string) {
	home, _ := os.UserHomeDir()
	// The CLI runs with an isolated HOME: point it at the real logins.
	env := map[string][]string{"codex": {"CODEX_HOME=" + home + "/.codex"}, "claude": {"CLAUDE_CONFIG_DIR=" + home + "/.claude"}}[provider]
	a := c.newSandbox(t, "--name", "agent-"+provider)
	r := c.cliWith(t, cliOpt{timeout: 6 * time.Minute, env: env}, "prompt", a, "--provider", provider, "--model", model, "--new", "--local-login",
		"Reply with exactly the word PONG and nothing else. Do not use any tools.")
	for _, code := range []string{"local_login_not_found", "local_login_expired"} {
		if strings.Contains(r.out+r.err, code) {
			t.Skipf("no usable local %s login on this machine (%s): %s", provider, code, tail(r.out+r.err, 300))
		}
	}
	turnEnd := time.Now()
	needles := localSecrets(home, provider)
	if len(needles) == 0 {
		t.Fatalf("the prompt ran but no local %s secret was found to check for leaks", provider)
	}
	done := r.event("done")
	// The provider account's own quota ran out: an account limit, not a
	// CrowdVM failure (the login and the agent turn worked up to the model).
	if done != nil && gets(done, "status") == "failed" && strings.Contains(strings.ToLower(r.out), "usage limit") {
		t.Skipf("the local %s account hit its usage limit: %s", provider, tail(scrub(r.out, needles), 300))
	}
	if done == nil || gets(done, "status") != "finished" {
		t.Fatalf("prompt did not finish: %s\nnode log tail:\n%s", scrub(r.out+r.err, needles), scrub(c.logTail("e2e-node-1", 1500), needles))
	}
	q := r.event("queued")
	t.Logf("local login: %s %s from %s (expires in %vs)", provider, gets(q, "localLogin.kind"), gets(q, "localLogin.source"), get(q, "localLogin.expiresInSeconds"))
	var reply string
	for _, ev := range r.lines() {
		if gets(ev, "type") == "response" {
			reply = gets(ev, "data.content")
		}
	}
	expect(t, strings.Contains(strings.ToUpper(reply), "PONG"), "%s reply %q", provider, reply)

	// Leak check. Never print a secret: only counts and file names.
	for _, out := range []string{r.out, r.err, c.cli(t, "events", a).out, c.cli(t, "history", a, "--limit", "100").out} {
		expect(t, !containsAny(out, needles), "a %s secret is in the CLI output, events or history", provider)
	}
	g := c.cliWith(t, cliOpt{timeout: 3 * time.Minute, stdin: strings.Join(needles, "\n") + "\n"}, "ssh", a, "--",
		`umask 077; f=$(mktemp -p /dev/shm); cat > "$f"; sudo -n true && echo sudo-ok; sudo grep -rlF -f "$f" /home/user /var/lib/crowdvm 2>/dev/null; echo "leak-files=$(sudo grep -rlF -f "$f" /home/user /var/lib/crowdvm 2>/dev/null | wc -l)"; rm -f "$f"`)
	expect(t, strings.Contains(g.out, "sudo-ok") && strings.Contains(g.out, "leak-files=0"), "%s secret found in the sandbox: %s", provider, scrub(g.out+g.err, needles))
	time.Sleep(time.Until(turnEnd.Add(40 * time.Second))) // the server pulls guest history every ~30 s
	dirs := []string{filepath.Join(c.root, "server")}
	for _, n := range c.nodes {
		dirs = append(dirs, n.dir)
	}
	dirs = append(dirs, c.logs)
	hits := filesContaining(t, dirs, needles)
	expect(t, len(hits) == 0, "%s secret found on the host in %v", provider, hits)
}

// localSecrets: every value `crowdvm prompt --local-login` may send for provider.
func localSecrets(home, provider string) []string {
	var out []string
	add := func(vs ...string) {
		for _, v := range vs {
			if v = strings.TrimSpace(v); len(v) >= 16 {
				out = append(out, v)
			}
		}
	}
	switch provider {
	case "claude":
		var f struct {
			OAuth struct {
				AccessToken string `json:"accessToken"`
			} `json:"claudeAiOauth"`
		}
		b, _ := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		_ = json.Unmarshal(b, &f)
		add(f.OAuth.AccessToken, os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), os.Getenv("ANTHROPIC_API_KEY"))
	case "codex":
		var f struct {
			Key    string `json:"OPENAI_API_KEY"`
			Tokens struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		b, _ := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
		_ = json.Unmarshal(b, &f)
		add(f.Tokens.AccessToken, f.Key, os.Getenv("OPENAI_API_KEY"))
	}
	return out
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// scrub replaces the secrets in s before it is printed.
func scrub(s string, needles []string) string {
	for _, n := range needles {
		s = strings.ReplaceAll(s, n, "[secret]")
	}
	return s
}

// filesContaining lists the files under dirs that contain a needle. Base
// images are skipped (they hold no sandbox data); VM disks are scanned.
func filesContaining(t *testing.T, dirs, needles []string) []string {
	var hits []string
	buf := make([]byte, 4<<20)
	longest := 0
	for _, n := range needles {
		longest = max(longest, len(n))
	}
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !e.Type().IsRegular() || strings.HasPrefix(e.Name(), "image-") || strings.Contains(p, "/vm/images/") {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			carry := 0
			for {
				n, err := f.Read(buf[carry:])
				chunk := string(buf[:carry+n])
				if containsAny(chunk, needles) {
					hits = append(hits, p)
					return nil
				}
				if err != nil {
					return nil
				}
				carry = min(longest, len(chunk))
				copy(buf, chunk[len(chunk)-carry:])
			}
		})
	}
	return hits
}

func testAgentOpencode(t *testing.T, c *cluster) {
	a := c.newSandbox(t, "--name", "agent")
	r := c.cliT(t, 6*time.Minute, "prompt", a, "--provider", "opencode", "--model", freeModel, "--new",
		"Reply with exactly the word PONG and nothing else. Do not use any tools.")
	done := r.event("done")
	if done == nil && !strings.Contains(r.out, `"type":"response"`) && !strings.Contains(r.out, `"status":"failed"`) {
		// The free model gave nothing back at all (its quota or service is
		// down; OpenCode keeps retrying): an outside dependency, like the
		// registry pulls other groups skip on.
		t.Skipf("the free OpenCode model did not answer within the timeout (quota or outage): %s", tail(r.out, 300))
	}
	if done == nil || gets(done, "status") != "finished" {
		t.Fatalf("prompt did not finish: %v\nnode log tail:\n%s", r, c.logTail("e2e-node-1", 1500))
	}
	conv := gets(done, "conversationId")
	var reply string
	for _, ev := range r.lines() {
		if gets(ev, "type") == "response" {
			reply = gets(ev, "data.content")
		}
	}
	expect(t, strings.Contains(strings.ToUpper(reply), "PONG"), "agent reply %q", reply)

	evs := c.cli(t, "events", a).ok(t)
	expect(t, strings.Contains(evs.out, `"prompt"`) && strings.Contains(evs.out, `"response"`), "events: %s", tail(evs.out, 500))
	cv := convs(c.cli(t, "conversations", a).ok(t))
	expect(t, len(cv) >= 1, "conversations: %v", cv)

	// A long turn: steer it, then interrupt it.
	q := c.cliT(t, 2*time.Minute, "prompt", a, "--provider", "opencode", "--model", freeModel, "--resume", conv, "--no-wait",
		"Use the shell tool to run exactly: for i in $(seq 1 120); do echo $i; sleep 1; done . Then tell me the last number.").ok(t)
	pid := gets(q.event("queued"), "promptId")
	waitFor(t, 2*time.Minute, 2*time.Second, "turn running", func() (bool, string) {
		p := c.api(t, "GET", "/sandboxes/"+a+"/prompts/"+pid, nil).json()
		return gets(p, "promptRun.status") == "running", gets(p, "promptRun.status")
	})
	sr := c.cli(t, "steer", a, "--convo", conv, "Also print the word STEERED at the end.")
	expect(t, sr.code == 0 || strings.Contains(sr.out+sr.err, "no_running_turn"), "steer: %v", sr)
	ir := c.cli(t, "interrupt", a).ok(t)
	_ = ir
	waitFor(t, 2*time.Minute, 2*time.Second, "interrupted", func() (bool, string) {
		p := c.api(t, "GET", "/sandboxes/"+a+"/prompts/"+pid, nil).json()
		return get(p, "promptRun.done") == true, gets(p, "promptRun.status")
	})
	// The last prompt (the steer may continue it) ends interrupted or finished, never failed.
	p := c.api(t, "GET", "/sandboxes/"+a+"/prompts/"+pid, nil).json()
	expect(t, gets(p, "promptRun.status") == "interrupted", "prompt after interrupt: %v", gets(p, "promptRun.status"))

	// Two conversations in parallel.
	for i := 0; i < 2; i++ {
		c.cliT(t, time.Minute, "prompt", a, "--provider", "opencode", "--model", freeModel, "--new", "--no-wait", "Say hi.").ok(t)
	}
	waitFor(t, 5*time.Minute, 3*time.Second, "parallel conversations finish", func() (bool, string) {
		cv := convs(c.cli(t, "conversations", a))
		running := 0
		for _, x := range cv {
			if get(x, "running") == true {
				running++
			}
		}
		return len(cv) >= 3 && running == 0, fmt.Sprintf("%d conversations, %d running", len(cv), running)
	})
	// Events stay readable while stopped.
	c.cliT(t, 6*time.Minute, "stop", a, "--wait").ok(t)
	evs = c.cli(t, "events", a).ok(t)
	expect(t, strings.Contains(evs.out, `"response"`), "events while stopped: %s", tail(evs.out, 300))
	expect(t, len(convs(c.cli(t, "conversations", a).ok(t))) >= 3, "conversations while stopped")
}

// convs returns the conversations of `crowdvm conversations` (one JSON object per line).
func convs(r result) []any {
	if l := list(r.last(), "conversations"); l != nil {
		return l
	}
	var out []any
	for _, m := range r.lines() {
		if gets(m, "id") != "" {
			out = append(out, m)
		}
	}
	return out
}

// ---------- AI credentials proxy ----------

func testAIProxy(t *testing.T, c *cluster) {
	c.cli(t, "ai", "profile", "set-key", "default", "anthropic-api-key", fakeVendorKey).ok(t)
	t.Cleanup(func() { c.cli(t, "ai", "profile", "rm-key", "default", "anthropic-api-key") })
	c.cli(t, "ai", "proxy", "on").ok(t)
	t.Cleanup(func() { c.cli(t, "ai", "proxy", "off") })

	p := c.newSandbox(t, "--name", "ai-proxy", "--no-snapshots")
	envOut := c.sh(t, p, `echo "$ANTHROPIC_BASE_URL"; echo "$ANTHROPIC_API_KEY"`)
	f := strings.Fields(envOut)
	if len(f) != 2 {
		t.Fatalf("proxy env: %q", envOut)
	}
	expect(t, f[0] == fmt.Sprintf("http://10.0.2.2:%d/ai-proxy/anthropic", c.port), "ANTHROPIC_BASE_URL %q", f[0])
	expect(t, strings.HasPrefix(f[1], "cvmai_"), "ANTHROPIC_API_KEY is not a placeholder: %q", f[1])
	// The key is passed base64-encoded so the command history does not hold it.
	k64 := base64.StdEncoding.EncodeToString([]byte(fakeVendorKey))
	leak := c.execR(t, p, `k=$(echo `+k64+` | base64 -d); sudo grep -rIl "$k" /home/user /etc /run/crowdvm /var/lib/crowdvm 2>/dev/null | head; env | grep -c "$k"`)
	expect(t, !strings.Contains(leak.out, "/") && strings.TrimSpace(leak.out) == "0", "real AI key reachable in the guest: %q", leak.out)

	call := `curl -s -m 20 "$ANTHROPIC_BASE_URL/v1/messages" -H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' -d '{"model":"claude-e2e","max_tokens":5,"messages":[{"role":"user","content":"ping"}]}'`
	n0 := len(c.hooks.vendorReqs())
	out := c.sh(t, p, call)
	expectContains(t, out, "pong from fake vendor", "proxied call")
	vr := c.hooks.vendorReqs()
	if len(vr) <= n0 {
		t.Fatalf("fake vendor saw no request")
	}
	last := vr[len(vr)-1]
	expect(t, last.path == "/v1/messages", "upstream path %q", last.path)
	expect(t, last.header.Get("X-Api-Key") == fakeVendorKey, "upstream did not get the real key (X-Api-Key=%q)", last.header.Get("X-Api-Key"))

	bad := c.sh(t, p, `curl -s -m 10 -o /dev/null -w '%{http_code}' "$ANTHROPIC_BASE_URL/v1/messages" -H "x-api-key: cvmai_0000000000000000000000000000000000000000" -d '{}'`)
	expect(t, strings.TrimSpace(bad) == "401", "bad placeholder: %q", bad)

	waitFor(t, 30*time.Second, time.Second, "AI usage recorded", func() (bool, string) {
		u := c.cli(t, "ai", "usage", p).last()
		var req, in, outT float64
		for _, it := range list(u, "usage") {
			r, _ := get(it, "requests").(float64)
			i, _ := get(it, "inputTokens").(float64)
			o, _ := get(it, "outputTokens").(float64)
			req, in, outT = req+r, in+i, outT+o
		}
		return req >= 1 && in >= 11 && outT >= 7, fmt.Sprint(u)
	})

	c.cli(t, "ai", "block", p).ok(t)
	code := c.sh(t, p, strings.Replace(call, "curl -s", "curl -s -o /dev/null -w '%{http_code}'", 1))
	expect(t, strings.TrimSpace(code) == "403", "blocked sandbox: %q", code)
	c.cli(t, "ai", "unblock", p).ok(t)
	expectContains(t, c.sh(t, p, call), "pong from fake vendor", "unblocked")
}

// ---------- API keys + scopes ----------

func testAPIKeys(t *testing.T, c *cluster) {
	a := c.shared(t)
	b := c.newSandbox(t, "--name", "keys-b", "--no-snapshots")
	r := c.cli(t, "api-key", "create", "e2e-scoped", "--sandbox", a, "--actions", "sandbox.read,exec", "--ttl", "1d").ok(t)
	key := gets(r.last(), "secret")
	if key == "" {
		key = findKey(r.out)
	}
	keyID := gets(r.last(), "apiKey.id")
	if key == "" || keyID == "" {
		t.Fatalf("scoped key: %v", r)
	}
	cmd := map[string]any{"command": "echo scoped-ok"}
	ok := c.apiKeyCall(t, key, "POST", "/sandboxes/"+a+"/commands", cmd)
	expect(t, ok.status == 200 && strings.Contains(gets(ok.json(), "stdout"), "scoped-ok"), "scoped key on its sandbox: %v", ok)
	no := c.apiKeyCall(t, key, "POST", "/sandboxes/"+b+"/commands", cmd)
	expect(t, no.status == 403 && gets(no.json(), "code") == "api_key_sandbox_forbidden", "scoped key on another sandbox: %v", no)
	no = c.apiKeyCall(t, key, "POST", "/sandboxes/"+a+"/stop", nil)
	expect(t, no.status == 403, "scoped key without sandbox.stop: %v", no)
	no = c.apiKeyCall(t, key, "GET", "/webhooks", nil)
	expect(t, no.status == 403, "scoped key on account settings: %v", no)

	// The CLI with the scoped key (environment, as an agent would get it).
	kcfg := filepath.Join(c.root, "cli-scoped")
	kenv := []string{"CROWDVM_API_KEY=" + key, "CROWDVM_API_BASE=" + c.apiBase}
	er := c.cliWith(t, cliOpt{cfgDir: kcfg, env: kenv}, "exec", a, "echo cli-scoped")
	expect(t, er.code == 0 && strings.Contains(er.out, "cli-scoped"), "CLI with scoped key: %v", er)
	er = c.cliWith(t, cliOpt{cfgDir: kcfg, env: kenv}, "exec", b, "echo nope")
	expect(t, er.code != 0 && strings.Contains(er.out+er.err, "api_key_sandbox_forbidden"), "CLI with scoped key on another sandbox: %v", er)
	// docs/E2E.md B4: `crowdvm login` refuses keys without account.read (the api-key create example key).
	lr0 := c.cliWith(t, cliOpt{cfgDir: kcfg, stdin: key + "\n"}, "login", "--server", c.url, "--key-stdin")
	expect(t, lr0.code == 0, "REGRESSION B4: crowdvm login --key-stdin with a sandbox-scoped key: %v", lr0)

	// read-only preset
	ro := findKey(c.cli(t, "api-key", "create", "e2e-ro", "--preset", "read-only").ok(t).out)
	lr := c.apiKeyCall(t, ro, "GET", "/sandboxes", nil)
	expect(t, lr.status == 200, "read-only list: %v", lr)
	wr := c.apiKeyCall(t, ro, "POST", "/sandboxes/"+a+"/commands", cmd)
	expect(t, wr.status == 403 && gets(wr.json(), "code") == "api_key_action_forbidden", "read-only exec: %v", wr)

	// list, usage, rotate, revoke
	expectContains(t, c.cli(t, "api-key", "list").ok(t).out, keyID, "api-key list")
	c.cli(t, "api-key", "usage", keyID).ok(t)
	rot := c.cli(t, "api-key", "rotate", keyID).ok(t)
	nk := gets(rot.last(), "secret")
	if nk == "" {
		nk = findKey(rot.out)
	}
	expect(t, nk != "" && nk != key, "rotate: %v", rot)
	expect(t, c.apiKeyCall(t, key, "GET", "/sandboxes/"+a, nil).status == 401, "old secret still works after rotate")
	expect(t, c.apiKeyCall(t, nk, "GET", "/sandboxes/"+a, nil).status == 200, "rotated secret does not work")
	c.cli(t, "api-key", "revoke", keyID).ok(t)
	expect(t, c.apiKeyCall(t, nk, "GET", "/sandboxes/"+a, nil).status == 401, "revoked key still works")
	expect(t, c.apiKeyCall(t, "cvm_"+strings.Repeat("x", 40), "GET", "/sandboxes", nil).status == 401, "bogus key accepted")

	// The sandbox's own token: the in-sandbox CLI manages only itself.
	in := c.execR(t, a, "crowdvm info --json 2>&1 | head -c 400")
	expect(t, strings.Contains(in.out, a), "in-sandbox CLI info: %v", in)
	in = c.execR(t, a, "crowdvm exec "+b+" -- echo cross 2>&1; echo rc=$?")
	expect(t, !strings.Contains(in.out, "cross\n") || strings.Contains(in.out, "rc=1"), "sandbox token reached another sandbox: %q", in.out)
}

// ---------- organizations ----------

func testOrgs(t *testing.T, c *cluster) {
	bob := c.second(t)
	bcli := func(args ...string) result {
		return c.cliWith(t, cliOpt{cfgDir: bob.cfg, timeout: 3 * time.Minute}, args...)
	}
	c.cli(t, "org", "create", "e2e-org").ok(t)
	inv := c.cli(t, "--org", "e2e-org", "org", "invite", "bob@e2e.test", "--role", "member").ok(t).last()
	code := gets(inv, "code")
	if code == "" {
		code = gets(inv, "invite.code")
	}
	if code == "" {
		t.Fatalf("org invite: %v", inv)
	}
	bcli("org", "join", code).ok(t)
	mem := c.cli(t, "--org", "e2e-org", "org", "members").ok(t)
	expectContains(t, mem.out, "bob", "org members")
	expectContains(t, bcli("org", "list").ok(t).out, "e2e-org", "bob's org list")
	c.cli(t, "admin", "credits", "grant", "50", "--org", "e2e-org", "--reason", "e2e").ok(t)

	// An org sandbox, private to its creator.
	r := c.cliWith(t, cliOpt{timeout: 8 * time.Minute}, "--org", "e2e-org", "new", "--json", "--type", "small", "--visibility", "private", "--name", "org-sb")
	o := gets(r.event("ready"), "id")
	if ev := r.event("created"); ev != nil {
		c.cleanupSandbox(t, gets(ev, "id"))
	}
	if o == "" {
		t.Fatalf("org sandbox: %v", r)
	}
	ex := bcli("--org", "e2e-org", "exec", o, "echo member-access")
	expect(t, ex.code != 0 && strings.Contains(ex.err+ex.out, "sandbox_private"), "member reached a private org sandbox: %v", ex)
	expect(t, bcli("--org", "e2e-org", "info", o).code == 0, "member cannot even read the private org sandbox")
	shr := c.cli(t, "--org", "e2e-org", "share", o).ok(t)
	// The API says whether the share needs a restart (the sandbox holds the creator's logins).
	sh := c.api(t, "POST", "/sandboxes/"+o+"/share", nil).json()
	if get(sh, "restartRequired") == true {
		expect(t, strings.Contains(strings.ToLower(shr.out+shr.err), "restart"),
			"REGRESSION B6: `crowdvm share` does not tell that teammates can only view until stop+resume: %v", shr)
		// It holds the creator's logins: view-only until stopped and resumed (without them).
		ex = bcli("--org", "e2e-org", "exec", o, "echo member-access")
		expect(t, ex.code != 0, "member used a shared sandbox that still holds the creator's logins: %v", ex)
		c.cliT(t, 6*time.Minute, "--org", "e2e-org", "stop", o, "--wait").ok(t)
		c.cliT(t, 8*time.Minute, "--org", "e2e-org", "resume", o).ok(t)
	}
	ex = bcli("--org", "e2e-org", "exec", o, "echo member-access; echo key=${CROWDVM_API_KEY:0:8}")
	expect(t, ex.code == 0 && strings.Contains(ex.out, "member-access"), "member after share: %v", ex)
	c.cli(t, "--org", "e2e-org", "unshare", o).ok(t)
	ex = bcli("--org", "e2e-org", "exec", o, "echo member-access")
	expect(t, ex.code != 0, "member after unshare: %v", ex)
	// Outsider (no org) cannot see it at all.
	ex = bcli("exec", o, "echo outsider")
	expect(t, ex.code != 0, "personal scope reached an org sandbox: %v", ex)
	// Org wallet is charged, not the creator's.
	cr := c.cli(t, "--org", "e2e-org", "credits").ok(t).last()
	expect(t, fmt.Sprint(cr) != "map[]", "org credits: %v", cr)

	c.deleteSandbox(t, o)
	// Boat: the owner deletes an organization only as its last member.
	del := c.cli(t, "--org", "e2e-org", "org", "delete", "--yes")
	expect(t, del.code != 0 && strings.Contains(del.out, "org_has_members"), "org delete with a member: %v", del)
	for _, m := range list(c.api(t, "GET", "/orgs/e2e-org/members", nil).json(), "members") {
		if gets(m, "role") != "owner" {
			c.api(t, "DELETE", "/orgs/e2e-org/members/"+gets(m, "id"), nil)
		}
	}
	// Credits left in the organization are moved out first (no silent loss).
	if bal, _ := get(c.api(t, "GET", "/orgs/e2e-org", nil).json(), "org.wallet.balanceCredits").(float64); bal > 0 {
		del = c.cli(t, "--org", "e2e-org", "org", "delete", "--yes")
		expect(t, del.code != 0 && strings.Contains(del.out, "org_has_balance"), "org delete with credits: %v", del)
		tr := c.api(t, "POST", "/credits/transfer", map[string]any{"from": "e2e-org", "to": "personal", "amount": bal})
		expect(t, tr.status == 200, "move the org's credits out: %v", tr)
	}
	c.cli(t, "--org", "e2e-org", "org", "delete", "--yes").ok(t)
	expect(t, !strings.Contains(c.cli(t, "org", "list").out, "e2e-org"), "org delete")
}

// ---------- credits / usage ----------

func credits(r result) float64 {
	m := r.last()
	for _, k := range []string{"wallet.balanceCredits", "balanceCredits", "balance", "wallet.balance"} {
		if v, ok := get(m, k).(float64); ok {
			return v
		}
	}
	return -1e9
}

func testCredits(t *testing.T, c *cluster) {
	bal0 := credits(c.cli(t, "credits").ok(t))
	if bal0 == -1e9 {
		t.Fatalf("credits: no balance in %v", c.cli(t, "credits").out)
	}
	c.cli(t, "admin", "credits", "grant", "25", "--user", adminUser, "--reason", "e2e grant").ok(t)
	bal1 := credits(c.cli(t, "credits").ok(t))
	expect(t, bal1 >= bal0+24.9, "grant: %v → %v", bal0, bal1)

	id := c.shared(t)
	u := c.cli(t, "usage", id).ok(t)
	expect(t, strings.Contains(u.out, "credits") || strings.Contains(u.out, "seconds"), "usage: %s", u.out)
	c.cli(t, "billing", "--all").ok(t)
	c.cli(t, "limits").ok(t)

	// Transfer personal → org wallet.
	c.cli(t, "org", "create", "e2e-credits-org").ok(t)
	t.Cleanup(func() { c.cli(t, "--org", "e2e-credits-org", "org", "delete", "--yes") })
	ob0 := credits(c.cli(t, "--org", "e2e-credits-org", "credits").ok(t))
	c.cli(t, "credits", "transfer", "--to", "e2e-credits-org", "--amount", "2").ok(t)
	ob1 := credits(c.cli(t, "--org", "e2e-credits-org", "credits").ok(t))
	expect(t, ob1 >= ob0+1.99, "transfer to org: %v → %v", ob0, ob1)
	// docs/E2E.md B5: --to is documented as "user or org", the server only takes orgs.
	bob := c.second(t)
	bb0 := credits(c.cliWith(t, cliOpt{cfgDir: bob.cfg}, "credits"))
	tr := c.cli(t, "credits", "transfer", "--to", "bob", "--amount", "2")
	bb1 := credits(c.cliWith(t, cliOpt{cfgDir: bob.cfg}, "credits"))
	expect(t, tr.code == 0 && bb1 >= bb0+1.99, "REGRESSION B5: credits transfer --to <user>: %v (bob %v → %v)", tr, bb0, bb1)

	// Node owners earn credits for hosted VM time.
	ni := c.cli(t, "nodes", "info", c.nodes[0].id).ok(t)
	expect(t, strings.Contains(ni.out, "earn"), "nodes info has no earned credits: %s", tail(ni.out, 400))
}

// ---------- legacy Box API ----------

func testLegacyBox(t *testing.T, c *cluster) {
	base := c.url + "/api/box/v1"
	r := c.api(t, "POST", base+"/boxes", map[string]any{"type": "small", "ttlSeconds": 900, "name": "legacy"})
	if r.status != 202 && r.status != 200 && r.status != 201 {
		t.Fatalf("box create: %v", r)
	}
	id := gets(r.json(), "box.id")
	if id == "" {
		t.Fatalf("box create: no box.id: %v", r)
	}
	t.Cleanup(func() { c.deleteSandbox(t, id) })
	expect(t, strings.HasPrefix(gets(r.json(), "type"), "box."), "box create type %v", r.json()["type"])
	expect(t, r.header.Get("Deprecation") != "", "no Deprecation header")
	waitFor(t, 5*time.Minute, time.Second, "box ready", func() (bool, string) {
		g := c.api(t, "GET", base+"/boxes/"+id, nil).json()
		return gets(g, "box.state") == "ready", gets(g, "box.state")
	})
	cr := c.api(t, "POST", base+"/boxes/"+id+"/commands", map[string]any{"command": "echo legacy-ok"})
	expect(t, cr.status == 200 && strings.Contains(gets(cr.json(), "stdout"), "legacy-ok"), "box command: %v", cr)
	dr := c.api(t, "DELETE", base+"/boxes/"+id, nil)
	expect(t, dr.status == 409, "box delete without confirmation: %v", dr)
	req := mustReq("DELETE", base+"/boxes/"+id)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-CrowdVM-Confirm-Delete", id)
	resp, err := httpc.Do(req)
	must(t, err)
	resp.Body.Close()
	expect(t, resp.StatusCode == 202, "box delete: %d", resp.StatusCode)
	waitFor(t, 2*time.Minute, time.Second, "box gone", func() (bool, string) {
		st := c.api(t, "GET", base+"/boxes/"+id, nil).status
		return st == 404, strconv.Itoa(st)
	})
}

// ---------- SDKs ----------

func testSDKs(t *testing.T, c *cluster) {
	env := append(c.baseEnv(), "CROWDVM_API_BASE="+c.apiBase, "CROWDVM_API_KEY="+c.apiKey)
	t.Run("python", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "smoke.py")
		must(t, os.WriteFile(script, []byte(pySmoke), 0o644))
		cmd := exec.Command("python3", script)
		cmd.Env = append(env, "PYTHONPATH="+filepath.Join(c.repo, "sdk/python/src"))
		cmd.Dir = dir
		out, err := runTimeout(cmd, 8*time.Minute)
		if id := sdkID(out); id != "" {
			t.Cleanup(func() { c.deleteSandbox(t, id) })
		}
		if err != nil || !strings.Contains(out, "PY-SDK-OK") {
			t.Errorf("python SDK smoke: %v\n%s", err, tail(out, 3000))
		}
	})
	t.Run("typescript", func(t *testing.T) {
		sdk := filepath.Join(c.repo, "sdk/typescript")
		tsc := filepath.Join(sdk, "node_modules/.bin/tsc")
		if _, err := os.Stat(tsc); err != nil {
			t.Skip("sdk/typescript/node_modules missing (npm install)")
		}
		dist := t.TempDir()
		if out, err := exec.Command(tsc, "-p", filepath.Join(sdk, "tsconfig.json"), "--outDir", dist).CombinedOutput(); err != nil {
			t.Fatalf("tsc: %v\n%s", err, out)
		}
		must(t, os.WriteFile(filepath.Join(dist, "package.json"), []byte(`{"type":"module"}`), 0o644))
		script := filepath.Join(dist, "smoke.mjs")
		must(t, os.WriteFile(script, []byte(tsSmoke), 0o644))
		cmd := exec.Command("node", script)
		cmd.Env = env
		cmd.Dir = dist
		out, err := runTimeout(cmd, 8*time.Minute)
		if id := sdkID(out); id != "" {
			t.Cleanup(func() { c.deleteSandbox(t, id) })
		}
		if err != nil || !strings.Contains(out, "TS-SDK-OK") {
			t.Errorf("TypeScript SDK smoke: %v\n%s", err, tail(out, 3000))
		}
	})
}

func runTimeout(cmd *exec.Cmd, d time.Duration) (string, error) {
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(d):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return buf.String(), fmt.Errorf("timed out after %s", d)
	}
}

func sdkID(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "SANDBOX ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "SANDBOX "))
		}
	}
	return ""
}

const pySmoke = `
from crowdvm import CrowdVM, wait_until_ready, read_text, write_text, stop_and_remove
cvm = CrowdVM()
created = cvm.create(ttl_seconds=900, type="small", name="py-sdk")
sid = created.sandbox.id
print("SANDBOX", sid, flush=True)
wait_until_ready(cvm, sid)
out = cvm.command(sid, "echo py-sdk-$((40+2))")
assert out.exit_code == 0 and "py-sdk-42" in out.stdout, out
write_text(cvm, sid, "/home/user/py.txt", "from python")
assert read_text(cvm, sid, "/home/user/py.txt") == "from python"
assert cvm.get(sid).sandbox.state in ("ready", "idle", "running"), cvm.get(sid)
stop_and_remove(cvm, sid)
print("PY-SDK-OK", flush=True)
`

const tsSmoke = `
import { CrowdVM, waitUntilReady, readText, writeText, stopAndRemove } from "./index.js";
const cvm = new CrowdVM();
const created = await cvm.create({ ttlSeconds: 900, type: "small", name: "ts-sdk" });
const sandboxId = created.sandbox.id;
console.log("SANDBOX " + sandboxId);
await waitUntilReady(cvm, sandboxId);
const r = await cvm.command({ sandboxId, command: "echo ts-sdk-$((40+2))" });
if (r.exitCode !== 0 || !r.stdout.includes("ts-sdk-42")) throw new Error("command: " + JSON.stringify(r));
await writeText(cvm, sandboxId, "/home/user/ts.txt", "from typescript");
const back = await readText(cvm, sandboxId, "/home/user/ts.txt");
if (back !== "from typescript") throw new Error("readText: " + back);
await stopAndRemove(cvm, sandboxId);
console.log("TS-SDK-OK");
`

// ---------- OpenTelemetry (checked last) ----------

func testOTel(t *testing.T, c *cluster) {
	waitFor(t, 3*time.Minute, 2*time.Second, "OTLP logs with lifecycle events", func() (bool, string) {
		for _, r := range c.hooks.otlpReqs("/v1/logs") {
			if strings.Contains(string(r.body), "sandbox.ready") {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d log requests", len(c.hooks.otlpReqs("/v1/logs")))
	})
	var archived, degraded bool
	for _, r := range c.hooks.otlpReqs("/v1/logs") {
		archived = archived || strings.Contains(string(r.body), "sandbox.archived")
		degraded = degraded || strings.Contains(string(r.body), "sandbox.degraded")
	}
	expect(t, archived, "no sandbox.archived OTLP log record")
	expect(t, degraded, "no sandbox.degraded OTLP log record")
	waitFor(t, 3*time.Minute, 5*time.Second, "OTLP metrics with sandbox data", func() (bool, string) {
		for _, r := range c.hooks.otlpReqs("/v1/metrics") {
			if strings.Contains(string(r.body), "sandbox.cpu.utilization") || strings.Contains(string(r.body), "sandboxes.active") {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d metric requests", len(c.hooks.otlpReqs("/v1/metrics")))
	})
	tl := c.cli(t, "telemetry").ok(t)
	expectContains(t, tl.out, c.hooks.otlpURL, "telemetry show")
	c.cli(t, "telemetry", "unset").ok(t)
}
