package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// envRecGuest is the fake guest plus a log of calls; hold pauses commands.
type envRecGuest struct {
	*servertest.FakeGuest
	mu    sync.Mutex
	calls []string
	hold  chan struct{}
}

func newEnvRecGuest() *envRecGuest { return &envRecGuest{FakeGuest: servertest.NewFakeGuest()} }

func (g *envRecGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	g.mu.Lock()
	g.calls = append(g.calls, r.Header.Get("X-Sandbox-Id")+" "+r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+"\n"+string(body))
	hold := g.hold
	g.mu.Unlock()
	if r.URL.Path == "/v1/commands" && hold != nil {
		<-hold
	}
	if r.URL.Path == "/v1/internal/apply-config" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"type":"guest.config_applied"}`))
		return
	}
	g.FakeGuest.ServeHTTP(w, r)
}

// applied decodes the newest POST /v1/internal/apply-config body.
func (g *envRecGuest) applied(t *testing.T) proto.ApplyConfig {
	t.Helper()
	call := g.last("POST /v1/internal/apply-config")
	_, body, _ := strings.Cut(call, "\n")
	var ac proto.ApplyConfig
	if err := json.Unmarshal([]byte(body), &ac); err != nil {
		t.Fatalf("apply-config body %q: %v", call, err)
	}
	return ac
}

// last returns the newest call containing all parts ("" = none).
func (g *envRecGuest) last(parts ...string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.calls) - 1; i >= 0; i-- {
		ok := true
		for _, p := range parts {
			if !strings.Contains(g.calls[i], p) {
				ok = false
				break
			}
		}
		if ok {
			return g.calls[i]
		}
	}
	return ""
}

func envID(t *testing.T, env *servertest.Env, key, name string) string {
	t.Helper()
	r := env.Do(key, "GET", "/api/v1/environments/"+name, nil)
	if r.Status != 200 {
		t.Fatalf("env %s: %s", name, r)
	}
	return r.Str("environment.id")
}

func guestSpec(t *testing.T, node *servertest.FakeNode, id string) proto.GuestConfig {
	t.Helper()
	sp, ok := node.Spec(id)
	if !ok {
		t.Fatalf("no spec for %s", id)
	}
	return sp.Guest
}

func TestEnvironmentsCRUD(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	r := env.Do(key, "GET", "/api/v1/environments", nil)
	if r.Status != 200 || r.Str("type") != "environments.list" || r.Str("environments.0.name") != "base" || r.Str("environments.0.isDefault") != "true" ||
		r.Str("environments.0.passGithub") != "true" || r.Str("environments.0.safeForThirdParties") != "false" || r.Str("environments.0.latestVersionNumber") != "1" {
		t.Fatalf("list: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/environments", map[string]any{"name": "staging"}); r.Status != 200 || len(r.Get("environments").([]any)) != 2 {
		t.Fatalf("create: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/environments", map[string]any{"name": "staging"}); r.Status != 409 || r.Code() != "environment_exists" {
		t.Fatalf("dup: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/environments", map[string]any{"name": "bad name"}); r.Status != 400 {
		t.Fatalf("bad name: %s", r)
	}
	st := envID(t, env, key, "staging")
	r = env.Do(key, "PUT", "/api/v1/environments/"+st, map[string]any{"envContents": "# c\nA=1\nexport B=\"two words\"\n", "isDefault": true,
		"secretFiles": []map[string]any{{"path": "app/.env", "contents": "X=1\n"}}})
	if r.Status != 200 || r.Str("environment.latestVersionNumber") != "2" || r.Str("environment.isDefault") != "true" || r.Str("environment.secretFiles.0.path") != "app/.env" {
		t.Fatalf("update: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/environments/base", nil); r.Str("environment.isDefault") != "false" {
		t.Fatalf("old default: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/"+st, map[string]any{"envContents": "BOAT_ID=x"}); r.Status != 400 || r.Code() != "invalid_env" {
		t.Fatalf("reserved: %s", r)
	}
	r = env.Do(key, "PUT", "/api/v1/environments/"+st+"/vars/C", map[string]any{"value": "it's \"q\"\nx"})
	if r.Status != 200 || r.Str("versionNumber") != "3" || r.Str("versionId") == "" || r.Str("success") != "true" {
		t.Fatalf("set var: %s", r)
	}
	r = env.Do(key, "PUT", "/api/v1/environments/"+st+"/vars/A", map[string]any{"value": "2"})
	r = env.Do(key, "GET", "/api/v1/environments/"+st, nil)
	if c := r.Str("environment.envContents"); !strings.Contains(c, "# c\nA=2\n") || !strings.Contains(c, `C="it's \"q\"\nx"`) {
		t.Fatalf("contents: %q", c)
	}
	if r := env.Do(key, "DELETE", "/api/v1/environments/"+st+"/vars/A", nil); r.Status != 200 || r.Str("versionNumber") != "5" {
		t.Fatalf("rm var: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/environments/"+st+"/vars/NOPE", nil); r.Status != 404 {
		t.Fatalf("rm missing var: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/"+st+"/secret-files", map[string]any{"path": "b.json", "contents": "{}"}); r.Status != 200 {
		t.Fatalf("set file: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/environments/"+st+"/secret-files?path=app%2F.env", nil); r.Status != 200 {
		t.Fatalf("rm file: %s", r)
	}
	r = env.Do(key, "GET", "/api/v1/environments/staging", nil)
	if files := r.Get("environment.secretFiles").([]any); len(files) != 1 || r.Str("environment.secretFiles.0.path") != "b.json" || len(r.Get("environment.versions").([]any)) != 7 {
		t.Fatalf("after files: %s", r)
	}
	// Safe for third parties: toggles stay, effective says no, warning.
	r = env.Do(key, "PUT", "/api/v1/environments/staging", map[string]any{"safeForThirdParties": true})
	if r.Str("environment.passSecrets") != "true" || r.Str("environment.effective.passSecrets") != "false" || r.Str("environment.warning") == "" || r.Str("environment.safetyAnsweredAt") == "" {
		t.Fatalf("safe: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/staging", map[string]any{"access": map[string]any{"restricted": true}}); r.Code() != "invalid_access" {
		t.Fatalf("personal access: %s", r)
	}
	// Delete: not the default, not the last.
	if r := env.Do(key, "DELETE", "/api/v1/environments/staging", nil); r.Status != 409 || r.Code() != "is_default" {
		t.Fatalf("rm default: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/environments/base", nil); r.Status != 200 || r.Str("success") != "true" {
		t.Fatalf("rm base: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/environments", nil); len(r.Get("environments").([]any)) != 1 {
		t.Fatalf("after rm: %s", r)
	}
	// Legacy /secrets edits the default environment.
	r = env.Do(key, "POST", "/api/v1/secrets", map[string]any{"envContents": "K=v\n"})
	if r.Status != 200 || r.Str("type") != "secrets.updated" || r.Str("envContents") != "K=v\n" || r.Str("subenvironmentId") != st || r.Get("pushed") == nil {
		t.Fatalf("post secrets: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/secrets", nil); r.Str("envContents") != "K=v\n" || len(r.Get("secretFiles").([]any)) != 0 {
		t.Fatalf("get secrets: %s", r)
	}
	// Scopes: a read-only key cannot write.
	ro := env.Key(env.Admin, "read-only")
	if r := env.Do(ro, "POST", "/api/v1/environments", map[string]any{"name": "x"}); r.Status != 403 {
		t.Fatalf("read-only write: %s", r)
	}
}

func TestEnvironmentBootUpgradeAndScrub(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{
		"envContents":    "FOO=1\nBAR=env\n",
		"secretFiles":    []map[string]any{{"path": ".env", "contents": "S=1"}},
		"repositories":   []map[string]any{{"repositoryId": "http://git.lan/team/app.git", "baseBranch": "dev", "setupScript": "make\n", "setupBlocking": true}},
		"registryLogins": []map[string]any{{"registry": "reg.lan:5000", "username": "u", "password": "p"}},
	})
	if r.Status != 200 || r.Str("environment.selectedRepositories.0.name") != "app" || r.Str("environment.selectedRepositories.0.setupBlocking") != "true" {
		t.Fatalf("configure: %s", r)
	}
	// Paths are relative to /home/user: others are refused, not skipped.
	for _, bad := range []string{"/etc/abs", "../esc", "~root/x"} {
		if r := env.Do(key, "PUT", "/api/v1/environments/base/secret-files", map[string]any{"path": bad, "contents": "x"}); r.Status != 400 {
			t.Fatalf("secret file %s: %s", bad, r)
		}
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"environment": "nope"}); r.Status != 400 || r.Code() != "unknown_environment" {
		t.Fatalf("unknown env: %s", r)
	}
	id := createSandbox(t, env, key, map[string]any{"env": map[string]string{"BAR": "mine"}})
	r = env.WaitState(key, id, "ready")
	if r.Str("sandbox.environment") != "base" || r.Str("sandbox.environmentVersion") != "2" {
		t.Fatalf("pinned: %s", r)
	}
	gc := guestSpec(t, node, id)
	if gc.Env["FOO"] != "1" || gc.Env["BAR"] != "mine" || gc.Env["BOAT_ID"] != id || gc.Env["CROWDVM_API_KEY"] == "" || gc.Env["ASCII_TOKEN"] == "" || gc.Env["ASCII_TOKEN"] == gc.Env["CROWDVM_API_KEY"] || gc.Env["CROWDVM_TOKEN"] != gc.Env["ASCII_TOKEN"] {
		t.Fatalf("env: %v", gc.Env)
	}
	if !strings.HasPrefix(gc.Env["CROWDVM_API_URL"], "http://10.0.2.2:") {
		t.Fatalf("api url: %s", gc.Env["CROWDVM_API_URL"])
	}
	if len(gc.SecretFiles) != 1 || gc.SecretFiles[0].Path != "/home/user/.env" || gc.SecretFiles[0].Mode != 0o600 {
		t.Fatalf("files: %+v", gc.SecretFiles)
	}
	if len(gc.Repos) != 1 || gc.Repos[0].URL != "http://git.lan/team/app.git" || gc.Repos[0].Branch != "dev" || !gc.Repos[0].SetupBlocking || gc.Repos[0].SetupScript != "make\n" {
		t.Fatalf("repos: %+v", gc.Repos)
	}
	if len(gc.RegistryLogins) != 1 || gc.RegistryLogins[0].Registry != "reg.lan:5000" {
		t.Fatalf("registries: %+v", gc.RegistryLogins)
	}
	if r := env.Do(key, "GET", "/api/v1/environments/base", nil); r.Str("environment.versions.1.sandboxCount") != "1" {
		t.Fatalf("sandbox count: %s", r)
	}

	// A new version does not touch the running sandbox until upgrade.
	env.Do(key, "PUT", "/api/v1/environments/base/vars/FOO", map[string]any{"value": "2"})
	env.Do(key, "DELETE", "/api/v1/environments/base/secret-files?path=.env", nil)
	env.Do(key, "PUT", "/api/v1/environments/base/secret-files", map[string]any{"path": "cfg/new.json", "contents": "{}"})
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.environmentVersion") != "2" {
		t.Fatalf("auto-upgraded: %s", r)
	}
	// Renaming the environment renames it on pinned sandboxes (sonnet-8 B1).
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"name": "base2"}); r.Status != 200 {
		t.Fatalf("rename: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.environment") != "base2" {
		t.Fatalf("pinned sandbox after rename: %s", r)
	}
	env.Do(key, "PUT", "/api/v1/environments/base2", map[string]any{"name": "base"})
	// A fork gets exactly its source's version, not the newer one (Boat; sonnet-7 B1).
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", nil); r.Status != 202 ||
		env.Do(key, "GET", "/api/v1/sandboxes/"+r.Str("id"), nil).Str("sandbox.environmentVersion") != "2" {
		t.Fatalf("fork version: %s", r)
	} else {
		fid := r.Str("id")
		env.WaitState(key, fid, "ready")
		env.Do(key, "DELETE", "/api/v1/sandboxes/"+fid, nil, "X-Ascii-Confirm-Delete: "+fid)
	}
	// The upgrade goes through the guest's apply-config: an env patch (the
	// core key is never re-sent), full secret file and registry lists, and a
	// scrub of withheld files.
	r = env.Do(key, "POST", "/api/v1/environments/base/upgrade", nil)
	if r.Status != 200 || r.Str("upgraded") != "1" || r.Str("upgradedLive") != "1" || r.Str("failed") != "0" || r.Str("latestVersionId") == "" {
		t.Fatalf("upgrade: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.environmentVersion") != "5" {
		t.Fatalf("upgraded version: %s", r)
	}
	ac := g.applied(t)
	if ac.EnvSet["FOO"] != "2" || ac.EnvSet["CROWDVM_API_KEY"] != "" || ac.Env != nil || len(ac.SecretFiles) != 1 || ac.SecretFiles[0].Path != "cfg/new.json" ||
		len(ac.RegistryLogins) != 1 || ac.Scrub == nil || !slices.Contains(ac.Scrub.Files, ".env") {
		t.Fatalf("apply-config: %+v", ac)
	}
	if g.last("POST /v1/commands", "crowdvm-env.sh") != "" {
		t.Fatal("ad-hoc env command run")
	}
	// Safe for third parties withholds everything at the next upgrade.
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"safeForThirdParties": true})
	if r := env.Do(key, "POST", "/api/v1/environments/base/upgrade", map[string]any{"agentIds": []string{id}}); r.Str("upgradedLive") != "1" {
		t.Fatalf("upgrade 2: %s", r)
	}
	ac = g.applied(t)
	if len(ac.EnvSet) != 0 || !slices.Contains(ac.EnvUnset, "FOO") || !slices.Contains(ac.EnvUnset, "CROWDVM_API_KEY") || len(ac.SecretFiles) != 0 ||
		ac.Scrub == nil || !slices.Contains(ac.Scrub.Files, "cfg/new.json") {
		t.Fatalf("withheld apply-config: %+v", ac)
	}
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"safeForThirdParties": false})

	// Stopped sandboxes take the upgrade at resume.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status/100 != 2 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	env.Do(key, "PUT", "/api/v1/environments/base/vars/FOO", map[string]any{"value": "3"})
	if r := env.Do(key, "POST", "/api/v1/environments/base/upgrade", nil); r.Str("upgradedStopped") != "1" {
		t.Fatalf("upgrade stopped: %s", r)
	}
	// Resume with --no-env: nothing of the environment, and the owner's
	// secrets are scrubbed BEFORE the sandbox is ready (ENV-018): it stays
	// provisioned and every call into it answers sandbox_securing.
	g.mu.Lock()
	g.hold = make(chan struct{})
	g.mu.Unlock()
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"noEnv": true}); r.Status != 202 {
		t.Fatalf("resume no-env: %s", r)
	}
	env.Eventually("scrub started", func() bool { return g.last("POST /v1/commands", ".ssh/id_*") != "" })
	gc = guestSpec(t, node, id)
	if gc.Env["FOO"] != "" || gc.Env["CROWDVM_API_KEY"] != "" || gc.Env["BAR"] != "mine" || len(gc.SecretFiles) != 0 || len(gc.Repos) != 0 || gc.Env["BOAT_ID"] != id || gc.Env["ASCII_TOKEN"] == "" {
		t.Fatalf("no-env spec: %+v", gc)
	}
	if gc.Scrub == nil || gc.Scrub.ID == "" || !gc.Scrub.SSHKeys || !slices.Contains(gc.Scrub.Files, ".env") || !slices.Contains(gc.Scrub.Files, ".codex/auth.json") || gc.Scrub.AuthorizedKeys {
		t.Fatalf("guest scrub: %+v", gc.Scrub)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "provisioned" {
		t.Fatalf("usable while securing: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/tunnel?port=22", nil); r.Status != 409 || r.Code() != "sandbox_securing" {
		t.Fatalf("tunnel while securing: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "cat ~/.env"}); r.Status != 409 || r.Code() != "sandbox_securing" {
		t.Fatalf("exec while securing: %s", r)
	}
	g.mu.Lock()
	close(g.hold)
	g.hold = nil
	g.mu.Unlock()
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/tunnel?port=22", nil); r.Code() == "sandbox_securing" {
		t.Fatalf("still securing: %s", r)
	}
	scrub := g.last("POST /v1/commands", ".ssh/id_*")
	for _, p := range []string{".codex/auth.json", ".claude/.credentials.json", ".config/gh/hosts.yml", ".git-credentials", "cfg/new.json", ".env", "gh auth logout"} {
		if !strings.Contains(scrub, p) {
			t.Fatalf("scrub lacks %s: %s", p, scrub)
		}
	}
}

func TestOrgEnvironments(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	aliceKey := env.AdminKey
	bob, bobKey := env.NewUser("bob", false)
	_, carolKey := env.NewUser("carol", false)
	r := env.Do(aliceKey, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	orgID := r.Str("org.id")
	for _, who := range []struct{ email, key string }{{"bob@example.test", bobKey}, {"carol@example.test", carolKey}} {
		inv := env.Do(aliceKey, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": who.email})
		if r := env.Do(who.key, "POST", "/api/v1/org-invites/accept", map[string]any{"code": inv.Str("code")}); r.Status != 200 {
			t.Fatalf("accept: %s", r)
		}
	}
	if r := env.Do(aliceKey, "POST", "/api/v1/environments?org=acme", map[string]any{"name": "team"}); r.Status != 200 {
		t.Fatalf("org create: %s", r)
	}
	if r := env.Do(bobKey, "POST", "/api/v1/environments?org=acme", map[string]any{"name": "mine"}); r.Status != 403 {
		t.Fatalf("member create: %s", r)
	}
	env.Do(aliceKey, "PUT", "/api/v1/environments/team?org=acme", map[string]any{"envContents": "TEAM=1\n", "access": map[string]any{"restricted": true, "members": []string{"bob@example.test"}}})
	if r := env.Do(aliceKey, "PUT", "/api/v1/environments/team?org=acme", map[string]any{"access": map[string]any{"restricted": true, "members": []string{"nobody@x"}}}); r.Code() != "invalid_access" {
		t.Fatalf("bad access: %s", r)
	}
	r = env.Do(bobKey, "GET", "/api/v1/environments?org="+orgID, nil)
	if len(r.Get("environments").([]any)) != 2 || r.Str("environments.1.access.members.0") != bob.ID {
		t.Fatalf("bob list: %s", r)
	}
	if r := env.Do(carolKey, "GET", "/api/v1/environments?org=acme", nil); len(r.Get("environments").([]any)) != 1 {
		t.Fatalf("carol list: %s", r)
	}
	// Personal list is unaffected by the X-Boat-Org header.
	if r := env.Do(bobKey, "GET", "/api/v1/environments", nil, "X-Boat-Org: acme"); len(r.Get("environments").([]any)) != 1 || r.Str("environments.0.owner.kind") != "personal" {
		t.Fatalf("header must not select org envs: %s", r)
	}
	if r := env.Do(carolKey, "POST", "/api/v1/sandboxes", map[string]any{"org": "acme", "environment": "team"}); r.Status != 403 || r.Code() != "environment_forbidden" {
		t.Fatalf("carol use: %s", r)
	}
	// Bob also has a personal "team": his own wins (Boat environments__manage).
	env.Do(bobKey, "POST", "/api/v1/environments", map[string]any{"name": "team"})
	env.Do(bobKey, "PUT", "/api/v1/environments/team", map[string]any{"envContents": "TEAM=personal\n"})
	id := createSandbox(t, env, bobKey, map[string]any{"org": "acme", "environment": "team"})
	if r := env.WaitState(bobKey, id, "ready"); r.Str("sandbox.environment") != "team" || r.Str("sandbox.visibility") != "private" || r.Str("sandbox.holdsCreatorLogins") != "true" {
		t.Fatalf("bob sandbox: %s", r)
	}
	if sp, _ := node.Spec(id); sp.Guest.Env["TEAM"] != "personal" {
		t.Fatalf("org sandbox must get the user's own environment of that name: %v", sp.Guest.Env)
	}
	own := createSandbox(t, env, bobKey, map[string]any{"environment": "team"})
	env.WaitState(bobKey, own, "ready")
	if sp, _ := node.Spec(own); sp.Guest.Env["TEAM"] != "personal" {
		t.Fatalf("personal sandbox: %v", sp.Guest.Env)
	}
	// A sandbox that passes no logins is shared and never gets them later.
	env.Do(aliceKey, "POST", "/api/v1/environments?org=acme", map[string]any{"name": "public"})
	env.Do(aliceKey, "PUT", "/api/v1/environments/public?org=acme", map[string]any{"safeForThirdParties": true})
	shared := createSandbox(t, env, bobKey, map[string]any{"org": "acme", "environment": "public"})
	if r := env.WaitState(bobKey, shared, "ready"); r.Str("sandbox.visibility") != "shared" || r.Str("sandbox.holdsCreatorLogins") != "false" {
		t.Fatalf("safe org sandbox: %s", r)
	}
	env.Do(bobKey, "POST", "/api/v1/sandboxes/"+shared+"/stop", nil)
	env.WaitState(bobKey, shared, "archived")
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+shared+"/resume", map[string]any{"environment": "team"}); r.Status != 409 || r.Code() != "environment_passes_logins" {
		t.Fatalf("resume onto login env: %s", r)
	}
}

func TestEnvironmentRepos(t *testing.T) {
	var auth string
	var mu sync.Mutex
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 42, "name": "web", "full_name": "acme/web", "private": true, "html_url": "https://github.com/acme/web", "clone_url": "https://github.com/acme/web.git",
				"pushed_at": "2026-10-01T00:00:00Z", "permissions": map[string]bool{"admin": true}, "owner": map[string]any{"login": "acme", "type": "Organization"}},
			{"id": 7, "name": "cli", "full_name": "me/cli", "html_url": "https://github.com/me/cli", "clone_url": "https://github.com/me/cli.git",
				"pushed_at": "2026-09-01T00:00:00Z", "owner": map[string]any{"login": "me", "type": "User"}},
		})
	}))
	defer gh.Close()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/repos/sources", map[string]any{"kind": "github", "apiUrl": gh.URL, "token": "ghp_secret1234"})
	if r.Status != 201 || r.Str("source.token") != "…1234" || strings.Contains(string(r.Body), "ghp_secret") {
		t.Fatalf("add source: %s", r)
	}
	r = env.Do(key, "GET", "/api/v1/repos?sync=true", nil)
	if r.Status != 200 || r.Str("type") != "repos.list" || len(r.Get("installations").([]any)) != 2 || r.Str("installations.0.accountLogin") != "acme" ||
		r.Str("installations.0.repositories.0.fullName") != "acme/web" || r.Str("environmentId") == "" {
		t.Fatalf("repos: %s", r)
	}
	mu.Lock()
	if auth != "Bearer ghp_secret1234" {
		t.Fatalf("auth to git host: %q", auth)
	}
	mu.Unlock()
	webID := r.Str("installations.0.repositories.0.databaseId")
	if r := env.Do(key, "GET", "/api/v1/repos?q=cli&limit=1", nil); len(r.Get("installations").([]any)) != 1 || r.Str("installations.0.repositories.0.name") != "cli" {
		t.Fatalf("filter: %s", r)
	}
	r = env.Do(key, "POST", "/api/v1/environments/base/repos", map[string]any{"repositoryId": webID, "baseBranch": "dev"})
	if r.Status != 200 || r.Str("versionNumber") != "2" {
		t.Fatalf("add repo: %s", r)
	}
	r = env.Do(key, "POST", "/api/v1/repos", map[string]any{"repositoryId": "me/cli"})
	if r.Status != 200 || r.Str("type") != "repos.updated" || len(r.Get("selectedRepositories").([]any)) != 2 {
		t.Fatalf("select: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/repos?selected=true", nil); len(r.Get("selectedRepositories").([]any)) != 2 {
		t.Fatalf("selected: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/environments/base/repos", map[string]any{"repositoryId": "nope/nope"}); r.Status != 400 {
		t.Fatalf("unknown repo: %s", r)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	gc := guestSpec(t, node, id)
	if len(gc.Repos) != 2 || gc.Repos[0].Token != "ghp_secret1234" || gc.Repos[0].Branch != "dev" || gc.Env["GH_TOKEN"] != "ghp_secret1234" {
		t.Fatalf("boot repos: %+v %v", gc.Repos, gc.Env)
	}
	found := false
	for _, f := range gc.SecretFiles {
		found = found || (f.Path == "/home/user/.config/gh/hosts.yml" && strings.Contains(string(f.Content), "oauth_token: ghp_secret1234"))
	}
	if !found {
		t.Fatalf("gh hosts: %+v", gc.SecretFiles)
	}
	if r := env.Do(key, "DELETE", "/api/v1/environments/base/repos/"+webID, nil); r.Status != 200 {
		t.Fatalf("rm repo: %s", r)
	}
	// GitHub off: no repos, no token.
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"passGithub": false})
	id2 := createSandbox(t, env, key, nil)
	env.WaitState(key, id2, "ready")
	if gc := guestSpec(t, node, id2); len(gc.Repos) != 0 || gc.Env["GH_TOKEN"] != "" {
		t.Fatalf("github off: %+v", gc)
	}
}

// bugs-r2-opus 3: /home/user/x and ~/x are stored as x (written at boot).
func TestEnvSecretFileHomePath(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	for _, p := range []string{"/home/user/.home-abs", "~/.tilde"} {
		if r := env.Do(key, "PUT", "/api/v1/environments/base/secret-files", map[string]any{"path": p, "contents": "h"}); r.Status != 200 {
			t.Fatalf("secret file %s: %s", p, r)
		}
	}
	r := env.Do(key, "GET", "/api/v1/environments/base", nil)
	if b := r.String(); !strings.Contains(b, `".home-abs"`) || !strings.Contains(b, `".tilde"`) || strings.Contains(b, "/home/user/.home-abs") {
		t.Fatalf("stored paths: %s", r)
	}
}
