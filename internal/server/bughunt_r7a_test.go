package server_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// r7aRoute is one route of the authz table.
type r7aRoute struct{ Method, Path, Action string }

// r7aRoutes lists the server's API/Route registrations from its OpenAPI
// document (generated from the live route table) plus the raw handlers
// registered with Router.HandleFunc that the document leaves out.
func r7aRoutes(t *testing.T, env *servertest.Env) []r7aRoute {
	r := env.Do("", "GET", "/api/v1/openapi.json", nil)
	if r.Status != 200 {
		t.Fatalf("openapi: %s", r)
	}
	var out []r7aRoute
	for p, v := range r.JSON["paths"].(map[string]any) {
		item := v.(map[string]any)
		abs := item["servers"] != nil
		for m, op := range item {
			if m == "servers" {
				continue
			}
			path := p
			if !abs {
				path = "/api/v1" + p
			}
			out = append(out, r7aRoute{strings.ToUpper(m), path, fmt.Sprint(op.(map[string]any)["x-crowdvm-action"])})
		}
	}
	// Raw (non-API()) handlers that act on a victim's objects.
	out = append(out,
		r7aRoute{"GET", "/api/v1/sandboxes/{sandboxId}/desktop/ws", "raw"},
		r7aRoute{"GET", "/api/v1/snapshots/{snapshotId}/download/inventory", "raw"},
		r7aRoute{"GET", "/api/v1/snapshots/{snapshotId}/download/blob/disk/0", "raw"},
		r7aRoute{"GET", "/p/{sandboxId}/8080/", "raw"},
		r7aRoute{"GET", "/ai-proxy/openai/v1/models", "raw"},
		r7aRoute{"GET", "/api/box/v1/sandboxes/{sandboxId}", "raw"},
		r7aRoute{"GET", "/api/box/v1/environments/{environmentId}", "raw"},
		r7aRoute{"GET", "/api/box/v1/webhooks/{webhookId}", "raw"},
	)
	sort.Slice(out, func(i, j int) bool {
		// DELETEs last so a broken delete cannot hide later checks.
		di, dj := out[i].Method == "DELETE", out[j].Method == "DELETE"
		if di != dj {
			return dj
		}
		return out[i].Path+out[i].Method < out[j].Path+out[j].Method
	})
	return out
}

// r7aPublic are the routes that answer without authentication, with why.
var r7aPublic = map[string]string{
	"POST /auth/signup":       "account creation",
	"POST /auth/login":        "password login",
	"POST /auth/email/start":  "email login code",
	"POST /auth/email/verify": "email login code",
	"GET /auth/config":        "login page config (no account data)",
	"POST /auth/device/start": "OAuth device flow start",
	"POST /auth/device/poll":  "OAuth device flow poll (device code is the secret)",
	"POST /agent/identity":    "agent self-signup",
	"GET /api/v1/snapshots/{snapshotId}/download/inventory": "raw: authenticates itself (checked below for 401/404)",
}

// r7aPersonalOnly are routes whose path params name the caller's own
// objects (its personal credential profile "default", its login methods) or
// that ignore the org scope and act on the caller's personal account.
var r7aPersonalOnly = map[string]bool{
	"PATCH /api/v1/account-settings":                                                   true,
	"PATCH /api/v1/orgs/active":                                                        true,
	"POST /api/v1/secrets":                                                             true,
	"PUT /api/v1/credential-profiles/{profile}":                                        true,
	"GET /api/v1/credential-profiles/{profile}/accounts/{credential}":                  true,
	"PUT /api/v1/credential-profiles/{profile}/keys/{credential}":                      true,
	"DELETE /api/v1/credential-profiles/{profile}/keys/{credential}":                   true,
	"POST /api/v1/credential-profiles/{profile}/accounts/{credential}":                 true,
	"PUT /api/v1/credential-profiles/{profile}/accounts/{credential}/{account}/active": true,
	"DELETE /api/v1/credential-profiles/{profile}/accounts/{credential}/{account}":     true,
	"PUT /api/v1/credential-profiles/{profile}/custom-apis/{name}":                     true,
	"DELETE /api/v1/credential-profiles/{profile}/custom-apis/{name}":                  true,
	"PUT /api/v1/credential-profiles/{profile}/sign-ins/{credential}":                  true,
	"DELETE /api/v1/credential-profiles/{profile}":                                     true,
	"DELETE /api/v1/account/connections/{method}":                                      true,
}

// r7aKnownLeak are routes with a dedicated failing test below.
var r7aKnownLeak = map[string]bool{
	"GET /api/v1/nodes/{nodeId}": true, // TestR7aNodeDetailLeaksOtherTenantsAndOwnerIP
}

func TestR7aAuthzEveryRoute(t *testing.T) {
	env := servertest.New(t)
	vic, vk := env.NewUser("r7avictim", false)
	vs := env.Session(vic)
	must := func(r *servertest.Resp, what string) *servertest.Resp {
		t.Helper()
		if r.Status/100 != 2 {
			t.Fatalf("%s: %s", what, r)
		}
		return r
	}
	// Victim fixtures.
	// Join tokens and nodes are the (admin) operator's; the attackers must
	// not reach those either.
	tokenID := must(env.Do(env.AdminKey, "POST", "/api/v1/join-tokens", map[string]any{"name": "r7a"}), "join token").Str("id")
	node := env.StartNode(servertest.NodeOptions{})
	orgID := must(env.Do(vk, "POST", "/api/v1/orgs", map[string]any{"name": "r7avorg"}), "org").Str("org.id")
	inviteID := must(env.Do(vk, "POST", "/api/v1/orgs/r7avorg/invites", map[string]any{"email": "x@example.test"}), "invite").Str("invite.id")
	sb := createSandbox(t, env, vk, nil)
	env.WaitState(vk, sb, "ready")
	envID := must(env.Do(vk, "POST", "/api/v1/environments", map[string]any{"name": "r7aenv"}), "env").Str("environment.id")
	must(env.Do(vk, "PUT", "/api/v1/environments/"+envID+"/vars/R7AKEY", map[string]any{"value": "r7a-secret-value"}), "var")
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer hook.Close()
	whID := must(env.Do(vk, "POST", "/api/v1/webhooks", map[string]any{"url": hook.URL + "/r7a-hook", "events": []string{"sandbox.ready"}}), "webhook").Str("webhook.id")
	keyID := must(env.Do(vs, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "r7a-victim-key", "preset": "read-only"}), "key").Str("apiKey.id")
	must(env.Do(vk, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": sb, "name": "r7avimg"}), "save")
	env.WaitFor(vk, "/api/v1/saved-snapshots/r7avimg", "snapshot.status", 10*time.Second, "ready")
	snapID := must(env.Do(vk, "GET", "/api/v1/sandboxes/"+sb+"/snapshots/latest", nil), "latest").Str("snapshot.id")
	shareID := must(env.Do(vk, "POST", "/api/v1/saved-snapshots/r7avimg/shares", map[string]any{}), "share").Str("share.id")
	env.Do(vk, "POST", "/api/v1/sandboxes/"+sb+"/host", map[string]any{"port": 8080})
	env.Do(vk, "PUT", "/api/v1/credential-profiles/default/keys/openai-api-key", map[string]any{"value": "sk-r7a-victim"})
	procID := "proc_r7a" // the fake guest does not track processes
	// Victim keys lacking a scope.
	noScope := func(actions ...string) string {
		return must(env.Do(vs, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "r7a-lim", "actions": actions}), "lim key").Str("secret")
	}
	limA := noScope("file.read")    // lacks everything but file.read
	limB := noScope("sandbox.read") // for file.read routes

	// Attackers.
	_, atkA := env.NewUser("r7aatka", false)
	atkBUser, atkB := env.NewUser("r7aatkb", false)
	must(env.Do(atkB, "POST", "/api/v1/orgs", map[string]any{"name": "r7aborg"}), "b org")
	must(env.Do(env.Session(atkBUser), "PATCH", "/api/v1/orgs/active", map[string]any{"org": "r7aborg"}), "b active")
	// A plain member of the victim's org.
	_, atkM := env.NewUser("r7aatkm", false)
	code := must(env.Do(vk, "POST", "/api/v1/orgs/r7avorg/invites", map[string]any{"role": "member"}), "invite m").Str("code")
	must(env.Do(atkM, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code}), "accept")

	params := map[string]string{
		"sandboxId": sb, "snapshotId": snapID, "name": "r7avimg", "shareId": shareID, "environmentId": envID,
		"key": "R7AKEY", "webhookId": whID, "apiKeyId": keyID, "tokenId": tokenID, "nodeId": node.ID, "org": orgID,
		"inviteId": inviteID, "userId": vic.ID, "wallet": vic.ID, "processId": procID, "port": "8080",
		"profile": "default", "credential": "openai-api-key", "account": "default", "repositoryId": "repo_r7a",
		"operationId": "dop_r7a", "stopId": "stp_r7a", "commandId": procID, "promptId": "prm_r7a", "jobId": "job_r7a",
		"linkId": "cil_r7a", "method": "password", "codeId": "inv_r7a", "sourceId": "src_r7a", "noticeId": "ntc_r7a",
	}
	markers := []string{sb, snapID, envID, whID, keyID, tokenID, orgID, inviteID, shareID, "r7a-secret-value", "r7a-hook", "sk-r7a-victim", "r7avimg", "r7a-victim-key"}
	for k, v := range params {
		if v == "" {
			t.Fatalf("fixture %s empty", k)
		}
	}
	fill := func(p string) string {
		for k, v := range params {
			p = strings.ReplaceAll(p, "{"+k+"}", v)
		}
		return p
	}
	// Self-only session routes: they act on the caller's own account; the
	// attackers' keys get 403 session_required, so they are not IDOR targets.
	skipAtk := map[string]bool{"POST /auth/logout": true}

	type fail struct{ who, route, got string }
	var fails []fail
	routes := r7aRoutes(t, env)
	for _, rt := range routes {
		pat := rt.Method + " " + rt.Path
		path := fill(rt.Path)
		if strings.Contains(path, "{") {
			t.Fatalf("unfilled param in %s", pat)
		}
		var body any
		if rt.Method != "GET" && rt.Method != "DELETE" {
			body = map[string]any{}
		}
		hdr := []string{"X-Ascii-Confirm-Delete: " + sb}
		// (d) unauthenticated.
		if _, pub := r7aPublic[pat]; !pub {
			if r := env.Do("", rt.Method, path, body, hdr...); r.Status/100 == 2 {
				fails = append(fails, fail{"anon", pat, r.String()})
			}
		}
		// (c) victim's own key lacking the route's scope.
		switch rt.Action {
		case "any", "public", "raw":
		default:
			k := limA
			if rt.Action == "file.read" {
				k = limB
			}
			if r := env.Do(k, rt.Method, path, body, hdr...); r.Status/100 == 2 {
				fails = append(fails, fail{"limited-key", pat, r.String()})
			}
		}
		if skipAtk[pat] {
			continue
		}
		// (m2) a plain member acting in the victim's org scope on writes that
		// need account.admin / environment.write: 2xx only on routes that act
		// on the caller's own personal account whatever the scope.
		if (rt.Action == "account.admin" || rt.Action == "environment.write") && rt.Method != "GET" && !strings.Contains(rt.Path, "{org}") {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			r := env.Do(atkM, rt.Method, path+sep+"org="+orgID, body, append(hdr, "X-CrowdVM-Org: "+orgID)...)
			if r.Status/100 == 2 && !r7aPersonalOnly[pat] {
				fails = append(fails, fail{"org-member-scope", pat, r.String()})
			}
		}
		// (a) unrelated user, (b) unrelated user in another org, (m) plain
		// member of the victim's org (on {org} and admin routes).
		for who, tok := range map[string]string{"stranger": atkA, "other-org": atkB, "org-member": atkM} {
			if who == "org-member" && !strings.Contains(rt.Path, "{org}") && !strings.HasPrefix(rt.Path, "/api/v1/admin") {
				continue
			}
			r := env.Do(tok, rt.Method, path, body, hdr...)
			if r.Status/100 != 2 || r7aKnownLeak[pat] {
				continue
			}
			switch {
			case strings.HasPrefix(rt.Path, "/api/v1/admin"):
				fails = append(fails, fail{who, pat, r.String()})
			case who == "org-member":
				// GETs of the member's own org are fine; changes need a role.
				if rt.Action == "account.admin" {
					fails = append(fails, fail{who, pat, r.String()})
				}
			case strings.Contains(rt.Path, "{") && !r7aPersonalOnly[pat]:
				fails = append(fails, fail{who, pat, r.String()})
			default:
				for _, m := range markers {
					if strings.Contains(string(r.Body), m) {
						fails = append(fails, fail{who, pat, "leaks " + m + ": " + r.String()})
						break
					}
				}
			}
		}
	}
	// The victim's objects are intact.
	if r := env.Do(vk, "GET", "/api/v1/sandboxes/"+sb, nil); r.Status != 200 || r.Get("sandbox.deletedAt") != nil {
		t.Errorf("victim sandbox after attack: %s", r)
	}
	for _, p := range []string{"/api/v1/environments/" + envID, "/api/v1/webhooks/" + whID, "/api/v1/saved-snapshots/r7avimg", "/api/v1/orgs/" + orgID} {
		if r := env.Do(vk, "GET", p, nil); r.Status != 200 {
			t.Errorf("victim object %s after attack: %s", p, r)
		}
	}
	t.Logf("checked %d routes", len(routes))
	for _, f := range fails {
		b := f.got
		if len(b) > 300 {
			b = b[:300]
		}
		t.Errorf("%s reached %s: %s", f.who, f.route, b)
	}
}

// GET /nodes/{nodeId} is open to every account (ActionAny, any key with
// account.read) and renders nodeJSON(n, true): the node owner's remote IP
// address, its earned credits and the ids of every tenant's sandboxes on
// it. Only the owner/admin (canManageNode) should get the detail fields.
func TestR7aNodeDetailLeaksOtherTenantsAndOwnerIP(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	_, vk := env.NewUser("r7avictim", false)
	sb := createSandbox(t, env, vk, nil)
	env.WaitState(vk, sb, "ready")
	_, atk := env.NewUser("r7astranger", false)
	r := env.Do(atk, "GET", "/api/v1/nodes/"+node.ID, nil)
	if r.Status != 200 {
		t.Fatalf("node detail: %s", r)
	}
	if strings.Contains(string(r.Body), sb) {
		t.Errorf("a stranger sees the victim's sandbox id %s on the node (node.sandboxIds): %v", sb, r.Get("node.sandboxIds"))
	}
	if ra := r.Str("node.remoteAddr"); ra != "" {
		t.Errorf("a stranger sees the node owner's remote address %q", ra)
	}
}

// With --trust-proxy, clientIP takes the LEFTMOST X-Forwarded-For entry
// (util.go clientIP). Reverse proxies append the peer they saw (nginx
// $proxy_add_x_forwarded_for, Caddy, Traefik), so the leftmost entry is
// whatever the client sent: a client denied by a hosted URL's firewall walks
// through it by sending its own X-Forwarded-For. The trusted entry is the
// rightmost one (added by the proxy).
func TestR7aTrustProxyXFFSpoofBypassesHostFirewall(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.TrustProxy = true })
	env.StartNode(servertest.NodeOptions{Tunnel: appTunnel(hostedApp(t))})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "public": true,
		"firewall": map[string]any{"deny": []string{"198.51.100.0/24"}}}); r.Status != 200 {
		t.Fatalf("host: %s", r)
	}
	// The denied client 198.51.100.7 as the proxy forwards it.
	if r := hget(t, env, "", "/p/"+id+"/3000/", "X-Forwarded-For: 198.51.100.7"); r.StatusCode != 403 {
		t.Fatalf("setup: denied client not refused: %d %s", r.StatusCode, r.body)
	}
	// The same client with a forged header; the proxy appends its real address.
	if r := hget(t, env, "", "/p/"+id+"/3000/", "X-Forwarded-For: 10.9.9.9, 198.51.100.7"); r.StatusCode != 403 {
		t.Fatalf("denied client 198.51.100.7 passed the firewall with a forged X-Forwarded-For: %d %s", r.StatusCode, r.body)
	}
}

// Same root cause: the per-IP API rate limit keys on the forged leftmost
// X-Forwarded-For entry, so one client gets a fresh bucket per request.
func TestR7aTrustProxyXFFSpoofBypassesIPRateLimit(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.TrustProxy = true; c.IPRateLimit = 3 })
	limited := false
	for i := 0; i < 10; i++ {
		r := env.Do("", "GET", "/auth/config", nil, fmt.Sprintf("X-Forwarded-For: 10.0.0.%d, 198.51.100.7", i))
		limited = limited || r.Status == 429
	}
	if !limited {
		t.Fatal("10 requests from one client (198.51.100.7, behind the proxy) with a limit of 3/min were never limited")
	}
}

// Per-account object caps: webhooks stop at whMaxURLs (10), hosted ports at
// 50 per sandbox, direct ports at 20, but API keys, environments, orgs and
// org invites have no cap at all. GET /api-keys is unpaginated and every
// key creation echoes the whole list ("apiKeys"), so N keys cost O(N) per
// create and per list (O(N^2) to build up) and the rows feed every list.
func TestR7aNoPerAccountObjectCaps(t *testing.T) {
	env := servertest.New(t)
	u, key := env.NewUser("r7aquota", false)
	sess := env.Session(u)
	if r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "r7aq"}); r.Status/100 != 2 {
		t.Fatal(r)
	}
	const n = 250
	kinds := []struct {
		what string
		do   func(i int) *servertest.Resp
	}{
		{"api keys", func(i int) *servertest.Resp {
			return env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": fmt.Sprint("k", i), "preset": "read-only"})
		}},
		{"environments", func(i int) *servertest.Resp {
			return env.Do(key, "POST", "/api/v1/environments", map[string]any{"name": fmt.Sprint("e", i)})
		}},
		{"organizations", func(i int) *servertest.Resp {
			return env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": fmt.Sprint("o", i)})
		}},
		{"org invites", func(i int) *servertest.Resp {
			return env.Do(key, "POST", "/api/v1/orgs/r7aq/invites", map[string]any{"email": fmt.Sprintf("x%d@example.test", i)})
		}},
	}
	for _, k := range kinds {
		capped := false
		for i := 0; i < n && !capped; i++ {
			r := k.do(i)
			switch {
			case r.Status == 409 || r.Status == 429 || r.Status == 403:
				capped = true
			case r.Status/100 != 2:
				t.Fatalf("%s #%d: %s", k.what, i, r)
			}
		}
		if !capped {
			t.Errorf("one account created %d %s without hitting any cap (webhooks stop at 10)", n, k.what)
		}
	}
}

// Free-text fields persisted without a length limit, while their siblings
// are capped (webhook url 2048, display name 80, org name 80, feedback
// 20000, sandbox name 120). Up to the 8 MiB Bind limit lands in the DB and is
// rendered in every list (an API key's name is echoed in each GET /api-keys
// and each key creation; an invite email is mailed and listed).
func TestR7aUnboundedStringFields(t *testing.T) {
	env := servertest.New(t)
	u, key := env.NewUser("r7abig", false)
	sess := env.Session(u)
	if r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "r7ab"}); r.Status/100 != 2 {
		t.Fatal(r)
	}
	big := strings.Repeat("a", 4<<20)
	for _, c := range []struct {
		what, tok, method, path string
		body                    map[string]any
	}{
		{"API key name", sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": big, "preset": "read-only"}},
		{"telemetry endpoint", key, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": "https://example.test/" + big}},
		{"org invite email", key, "POST", "/api/v1/orgs/r7ab/invites", map[string]any{"email": big + "@example.test"}},
	} {
		if r := env.Do(c.tok, c.method, c.path, c.body); r.Status/100 == 2 {
			t.Errorf("%s of 4 MiB accepted and stored: %d (response %d bytes)", c.what, r.Status, len(r.Body))
		}
	}
	if r := env.Do(key, "GET", "/api/v1/api-keys", nil); len(r.Body) > 1<<20 {
		t.Errorf("GET /api-keys answers %d bytes for one user's keys", len(r.Body))
	}
}

// r7aJSONArray streams "[1.5,1.5,...,1.5]" of about left bytes without
// allocating it, counting what the server pulled.
type r7aJSONArray struct {
	left, read int
	pend       []byte
	started    bool
}

func (a *r7aJSONArray) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(a.pend) == 0 {
			switch {
			case !a.started:
				a.pend, a.started = []byte("[1.5"), true
			case a.left > 0:
				a.pend = []byte(",1.5")
				a.left -= 4
			case a.left == 0:
				a.pend, a.left = []byte("]"), -1
			default:
				a.read += n
				if n == 0 {
					return 0, io.EOF
				}
				return n, nil
			}
		}
		c := copy(p[n:], a.pend)
		a.pend = a.pend[c:]
		n += c
	}
	a.read += n
	return n, nil
}

// The legacy /api/box/v1 alias (feat_legacy.go serveBoxAlias) is a raw
// handler: before any authentication it reads up to 16 MiB of a JSON body,
// json.Unmarshals it into `any`, renames keys and re-marshals it, and only
// then hands the request to the authenticated /api/v1 route. One anonymous
// request makes the server allocate hundreds of MB; the same request to
// /api/v1 is refused with 401 before the body is read.
func TestR7aBoxAliasParsesHugeBodyBeforeAuth(t *testing.T) {
	env := servertest.New(t)
	alloc := func(path string) (status int, mb uint64, sent int) {
		body := &r7aJSONArray{left: 15 << 20}
		req, _ := http.NewRequest("POST", env.URL+path, body)
		req.Header.Set("Content-Type", "application/json")
		var a, b runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&a)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		runtime.ReadMemStats(&b)
		return resp.StatusCode, (b.TotalAlloc - a.TotalAlloc) >> 20, body.read
	}
	st, mb, sent := alloc("/api/v1/sandboxes")
	t.Logf("/api/v1/sandboxes: %d, %d MB allocated, %d bytes sent", st, mb, sent)
	if st != 401 {
		t.Fatalf("control: %d", st)
	}
	st, mb, sent = alloc("/api/box/v1/sandboxes")
	t.Logf("/api/box/v1/sandboxes: %d, %d MB allocated, %d bytes sent", st, mb, sent)
	if st != 401 {
		t.Fatalf("box alias: %d", st)
	}
	if mb > 48 {
		t.Errorf("an unauthenticated 15 MiB request to /api/box/v1 made the server allocate %d MB before answering 401", mb)
	}
}

// validName (sandboxes.go) truncates a long name with n[:120], a byte
// slice: a multi-byte character across byte 120 is cut in half and the
// stored name is invalid UTF-8 (JSON renders it as U+FFFD).
func TestR7aSandboxNameTruncationSplitsRune(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	name := "a" + strings.Repeat("€", 60) // byte 120 falls inside a "€"
	r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes", map[string]any{"name": name})
	if r.Status != 202 {
		t.Fatalf("create: %s", r)
	}
	got := r.Str("sandbox.name")
	if strings.ContainsRune(got, utf8.RuneError) || !strings.HasPrefix(name, got) {
		t.Fatalf("stored name is not a prefix of the given one (cut inside a character): %q", got)
	}
}
