package e2e

// Groups for the findings of the parity reviews (docs/reviews/*.md) that were
// fixed afterwards: CLI bugs, Docker containers across stop/resume, the docs
// for agents (llms.txt, /docs, MCP), the in-sandbox skill, and Boat's SDK doc
// samples run unchanged against this cluster.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testCLIFixes: opus-2 #2, #3, #7, #8 and the `crowdvm skill` command.
func testCLIFixes(t *testing.T, c *cluster) {
	id := c.newSandbox(t, "--name", "cli-fixes")

	t.Run("push-outside-home", func(t *testing.T) {
		// Boat: `boat push bx_... ./api --to /srv/api` creates /srv/api. A link counts as a file.
		src := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
		must(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("push-a\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(src, "sub/b.txt"), []byte("push-b\n"), 0o644))
		must(t, os.Symlink("a.txt", filepath.Join(src, "link")))
		ev := c.cliT(t, 3*time.Minute, "push", id, src, "--to", "/srv/api").ok(t).event("pushed")
		expect(t, gets(ev, "to") == "/srv/api" && get(ev, "files") == float64(3), "push event: %v", ev)
		out := c.sh(t, id, "cat /srv/api/a.txt /srv/api/sub/b.txt; readlink /srv/api/link; stat -c %U /srv/api /srv/api/a.txt")
		expect(t, out == "push-a\npush-b\na.txt\nuser\nuser\n", "pushed tree: %q", out)
	})

	t.Run("env-new-json", func(t *testing.T) {
		ev := c.cli(t, "env", "new", "e2e-review-env").ok(t).last()
		expect(t, gets(ev, "id") != "" && gets(ev, "name") == "e2e-review-env", "env new: %v", ev)
		c.cli(t, "env", "rm", "e2e-review-env")
	})

	t.Run("run-summary-cost", func(t *testing.T) {
		// the summary line goes to stderr: "exit 0 · 4s · small · 0.0001 credits · bx_... deleted"
		rr := c.cliT(t, 8*time.Minute, "run", "--type", "small", "--", "true")
		expect(t, rr.code == 0 && regexp.MustCompile(`exit 0 · \S+ · small · [0-9.]+ credits · bx_\w+ deleted`).MatchString(rr.err), "run summary: %v", rr)
	})

	t.Run("org-default-visibility", func(t *testing.T) {
		c.cli(t, "org", "create", "e2e-vis").ok(t)
		t.Cleanup(func() { c.cli(t, "--org", "e2e-vis", "org", "delete", "--yes") })
		show := c.cli(t, "--org", "e2e-vis", "org", "default-visibility").ok(t).last()
		expect(t, gets(show, "defaultVisibility") == "shared", "default visibility of a new org: %v", show)
		c.cli(t, "--org", "e2e-vis", "org", "default-visibility", "private").ok(t)
		show = c.cli(t, "--org", "e2e-vis", "org", "default-visibility").ok(t).last()
		expect(t, gets(show, "defaultVisibility") == "private", "after setting private: %v", show)
	})

	t.Run("data-retention-enable", func(t *testing.T) {
		// A fresh account: zero data retention deletes stopped sandboxes for good.
		cfg := filepath.Join(c.root, "cli-zdr")
		c.cliIn(t, "zdr-pass-123456\n", "admin", "users", "create", "zdr", "--password-stdin", "--credits", "10", "--email", "zdr@e2e.test").ok(t)
		c.cliWith(t, cliOpt{cfgDir: cfg, stdin: "zdr-pass-123456\n"}, "login", "--server", c.url, "--username", "zdr", "--password-stdin").ok(t)
		r := c.cliWith(t, cliOpt{cfgDir: cfg}, "data-retention", "enable", "--yes").ok(t).last()
		expect(t, get(r, "enabled") == true, "data-retention enable: %v", r)
		st := c.cliWith(t, cliOpt{cfgDir: cfg}, "data-retention").ok(t).last()
		expect(t, get(st, "enabled") == true, "data-retention status: %v", st)
	})

	t.Run("skill-install", func(t *testing.T) {
		home := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
		c.cliWith(t, cliOpt{env: []string{"HOME=" + home}}, "skill", "install").ok(t)
		b, err := os.ReadFile(filepath.Join(home, ".claude/skills/crowdvm/SKILL.md"))
		expect(t, err == nil && bytes.Contains(b, []byte("name: crowdvm")), "installed skill: %v %q", err, tail(string(b), 200))
		p := c.cliWith(t, cliOpt{env: []string{"HOME=" + home}}, "skill", "path", "--agent", "claude").ok(t).last()
		expect(t, get(p, "installed") == true, "skill path: %v", p)
	})
}

// testDockerResume: Boat snapshots__whats-saved: "Docker containers that ran start again by
// themselves" after a stop and resume (sonnet-2 M1), whatever their restart policy.
func testDockerResume(t *testing.T, c *cluster) {
	id := c.newSandbox(t, "--name", "docker-resume")
	pull := c.execR(t, id, "for i in 1 2 3; do docker pull -q alpine:3 >/dev/null && exit 0; sleep 5; done; exit 1")
	if pull.code != 0 {
		t.Skipf("cannot pull alpine:3 (internet access from the guest?): %v", pull)
	}
	c.sh(t, id, "docker run -d --name plain alpine:3 sleep infinity >/dev/null && "+
		"docker run -d --name policy --restart unless-stopped alpine:3 sleep infinity >/dev/null && "+
		"docker run -d --name stopped alpine:3 sleep infinity >/dev/null && docker stop -t 1 stopped >/dev/null")
	// A named volume (snapshot pull writes it to docker/<vol>/) and a Compose
	// project with a profile-gated service whose image a stop keeps (sonnet-11).
	c.sh(t, id, "docker volume create cvol >/dev/null && docker run --rm -v cvol:/d alpine:3 sh -c 'echo volfile > /d/v.txt' && "+
		"mkdir -p ~/cp && printf 'services:\n  web:\n    image: alpine:3\n    command: sleep infinity\n  tools:\n    image: busybox:1.36\n    profiles: [tools]\n    command: \"true\"\n' > ~/cp/compose.yaml && "+
		"cd ~/cp && docker compose -p cpx up -d web >/dev/null 2>&1 && docker pull -q busybox:1.36 >/dev/null")
	// Boat's "Keep a hostname" recipe (snapshots__whats-saved).
	c.sh(t, id, "printf '[Unit]\nDescription=Set a fixed hostname\n\n[Service]\nType=oneshot\nExecStart=/usr/bin/hostnamectl set-hostname my-sandbox\n\n[Install]\nWantedBy=multi-user.target\n' | "+
		"sudo tee /etc/systemd/system/set-hostname.service >/dev/null && sudo systemctl enable --now set-hostname.service >/dev/null 2>&1")
	c.cliT(t, 6*time.Minute, "stop", id, "--wait").ok(t)
	out := t.TempDir()
	if enc, _ := get(c.sandbox(t, id), "storage.encrypted").(bool); enc {
		// Secure layer: the server cannot read a stopped encrypted disk.
		r := c.cliT(t, time.Minute, "snapshot", "pull", id, "-o", out)
		expect(t, strings.Contains(r.out, "snapshot_encrypted"), "snapshot pull of an encrypted sandbox: %s", r.out)
	} else {
		c.cliT(t, 5*time.Minute, "snapshot", "pull", id, "-o", out).ok(t)
		vb, err := os.ReadFile(filepath.Join(out, "docker", "cvol", "v.txt"))
		expect(t, err == nil && strings.TrimSpace(string(vb)) == "volfile", "snapshot pull docker/cvol/v.txt: %v %q", err, vb)
	}
	c.cliT(t, 6*time.Minute, "resume", id).ok(t)
	eventually(t, 2*time.Minute, 3*time.Second, "containers running again after resume", func() (bool, string) {
		r := c.execR(t, id, "docker ps --format '{{.Names}}' | sort | tr '\\n' ' '")
		return r.code == 0 && strings.Contains(r.out, "plain") && strings.Contains(r.out, "policy"), r.out + r.err
	})
	ps := c.sh(t, id, "docker ps --format '{{.Names}}'")
	expect(t, !strings.Contains(ps, "stopped"), "a container stopped before the stop came back: %q", ps)
	// A published port answers on the sandbox itself (docker-proxy dials the
	// container: VM-internal traffic must not go out through the server).
	pf := c.execR(t, id, "docker run -d --name web -p 18080:8080 busybox:1.36 httpd -f -p 8080 >/dev/null && sleep 2 && curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/")
	expect(t, pf.code == 0 && strings.Contains(pf.out, "404"), "docker -p port forward: %q %q", pf.out, pf.err)
	h := strings.TrimSpace(c.sh(t, id, "hostname"))
	expect(t, h == "my-sandbox", "hostname after resume with Boat's set-hostname.service: %q", h)
	imgs := c.sh(t, id, "docker image ls --format '{{.Repository}}:{{.Tag}}'")
	expect(t, strings.Contains(imgs, "busybox:1.36"), "the profile-gated Compose service's image was pruned on stop: %q", imgs)
}

// testAgentDocs: Boat ai-tools §2 (llms.txt, page .md, docs MCP server) and
// integrated-agents__customize (every harness has the skill) — opus-1 #6, opus-2 #4/#5, sonnet-1 #1/#2.
func testAgentDocs(t *testing.T, c *cluster) {
	fetch := func(path string) (int, string) {
		resp, err := http.Get(c.url + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	st, idx := fetch("/llms.txt")
	expect(t, st == 200 && strings.Contains(strings.ToLower(idx), "crowdvm"), "/llms.txt: %d %q", st, tail(idx, 300))
	st, full := fetch("/llms-full.txt")
	expect(t, st == 200 && len(full) > len(idx), "/llms-full.txt: %d, %d bytes (index %d)", st, len(full), len(idx))
	if m := regexp.MustCompile(`\((?:https?://[^)\s]+)?(/docs/[\w./-]+\.md)\)`).FindStringSubmatch(idx); m == nil {
		t.Errorf("/llms.txt links no /docs/<page>.md: %q", tail(idx, 500))
	} else {
		st, page := fetch(m[1])
		expect(t, st == 200 && strings.HasPrefix(strings.TrimSpace(page), "#"), "%s: %d %q", m[1], st, tail(page, 200))
	}

	// MCP over streamable HTTP: initialize, then tools/list and one search.
	sid := ""
	rpc := func(id int, method string, params any) map[string]any {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequest("POST", c.url+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("mcp %s: %v", method, err)
		}
		defer resp.Body.Close()
		if v := resp.Header.Get("Mcp-Session-Id"); v != "" {
			sid = v
		}
		raw, _ := io.ReadAll(resp.Body)
		if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
			sc := bufio.NewScanner(bytes.NewReader(raw))
			for sc.Scan() {
				if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
					raw = []byte(d)
				}
			}
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("mcp %s: %d %q", method, resp.StatusCode, tail(string(raw), 300))
		}
		return out
	}
	ini := rpc(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "1"}})
	expect(t, get(ini, "result.serverInfo") != nil, "mcp initialize: %v", ini)
	tools := list(rpc(2, "tools/list", map[string]any{}), "result.tools")
	var names []string
	for _, tl := range tools {
		names = append(names, gets(tl, "name"))
	}
	expect(t, contains(names, "search_docs"), "mcp tools: %v", names)
	res := rpc(3, "tools/call", map[string]any{"name": "search_docs", "arguments": map[string]any{"query": "snapshot"}})
	expect(t, get(res, "result.isError") != true && len(list(res, "result.content")) > 0, "mcp search_docs: %v", tail(fmt.Sprint(res), 400))

	// The crowdvm skill in every harness's skills folder inside a sandbox.
	id := c.shared(t)
	out := c.sh(t, id, `for d in .claude/skills .codex/skills .config/opencode/skills .pi/agent/skills .agents/skills; do `+
		`grep -l '^name: crowdvm' ~/$d/crowdvm/SKILL.md >/dev/null 2>&1 && echo ok $d || echo missing $d; done`)
	expect(t, !strings.Contains(out, "missing"), "crowdvm skill in the sandbox:\n%s", out)
}

// testBoatSDKSamples: every Python and TypeScript sample of Boat's SDK, evals, platform-guide and
// recipes pages, verbatim apart from the base URL, against this cluster (opus-1 #5).
func testBoatSDKSamples(t *testing.T, c *cluster) {
	if _, err := os.Stat(filepath.Join(c.repo, ".ref/pages/sdks__python.md")); err != nil {
		t.Skip(".ref/pages (Boat's docs) is not checked out")
	}
	// A `large` sample needs a whole node (8 vCPU): stop the shared sandbox meanwhile.
	if c.sharedID != "" {
		c.cliT(t, 6*time.Minute, "stop", c.sharedID, "--wait").ok(t)
		t.Cleanup(func() { c.cliT(t, 6*time.Minute, "resume", c.sharedID) })
	}
	cmd := exec.Command("python3", "-u", filepath.Join(c.repo, "sdk/tools/doc_samples.py"), "--base", c.apiBase, "--key", c.apiKey)
	cmd.Env = c.baseEnv()
	out, err := runTimeout(cmd, 25*time.Minute)
	_ = os.WriteFile(filepath.Join(c.logs, "doc-samples.log"), []byte(out), 0o644)
	// keep the FAIL blocks (each followed by its indented detail) for the message
	var failed []string
	var detail strings.Builder
	in := false
	for _, l := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(l, "FAIL "); ok {
			failed = append(failed, name)
			in = true
		} else if !strings.HasPrefix(l, "    ") {
			in = false
		}
		if in {
			detail.WriteString(l + "\n")
		}
	}
	t.Logf("doc samples: %s", lastLine(out))
	if err != nil || len(failed) > 0 {
		t.Errorf("Boat SDK doc samples failed (%v): %v\n%s", err, failed, tail(detail.String(), 12000))
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndex(s, "\n")+1:]
}

// testDiskCleanup: Boat troubleshooting: at 95% disk boat deletes "pulled Docker images
// that no container uses ... boat never deletes your own files" (opus-3 N6, opus-4 B1).
func testDiskCleanup(t *testing.T, c *cluster) {
	id := c.newSandbox(t, "--name", "disk-cleanup")
	pull := c.execR(t, id, "for i in 1 2 3; do docker pull -q alpine:3 >/dev/null && docker pull -q busybox:1.36 >/dev/null && exit 0; sleep 5; done; exit 1")
	if pull.code != 0 {
		t.Skipf("cannot pull images (internet access from the guest?): %v", pull)
	}
	c.sh(t, id, "mkdir -p /tmp/b && printf 'FROM alpine:3\\nRUN echo hi > /x\\n' > /tmp/b/Dockerfile && docker build -q -t mybuilt:1 /tmp/b >/dev/null && "+
		"docker run -d --name keepme busybox:1.36 sleep infinity >/dev/null")
	// Fill / to ~97% so the guest's sampler runs the 95% cleanup.
	c.sh(t, id, "set -e; read size avail <<<\"$(df -B1 --output=size,avail / | tail -1)\"; "+
		"sudo fallocate -l $((avail - size * 3 / 100)) /var/tmp/fill")
	defer c.execR(t, id, "sudo rm -f /var/tmp/fill")
	images := func() string {
		return c.execR(t, id, "docker image ls --format '{{.Repository}}:{{.Tag}}' | sort | tr '\\n' ' '").out
	}
	eventually(t, 3*time.Minute, 5*time.Second, "unused pulled image deleted at 95% disk", func() (bool, string) {
		out := images()
		return !strings.Contains(out, "alpine:3"), out
	})
	out := images()
	expect(t, strings.Contains(out, "mybuilt:1"), "an image built in the sandbox was deleted: %q", out)
	expect(t, strings.Contains(out, "busybox:1.36"), "an image a container uses was deleted: %q", out)
}
