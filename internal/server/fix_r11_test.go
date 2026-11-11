package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r11-opus 1: the guest's sshUser and hostKey reach the user's ssh
// config and known_hosts; a newline in them ran commands on the user's
// computer (ProxyCommand, Match exec).
func TestR11SSHKeyAnswerIsSanitized(t *testing.T) {
	env := servertest.New(t)
	g := http.NewServeMux()
	g.HandleFunc("POST /v1/sshkey", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "success": true,
			"sshUser": "user\n  ProxyCommand sh -c id", "hostKey": "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 BBBB"})
	})
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/sshkey", map[string]any{"key": "ssh-ed25519 AAAA me"})
	if r.Str("sshUser") != "user" || r.Str("hostKey") != "" {
		t.Fatalf("guest-controlled ssh values passed through: user %q, hostKey %q", r.Str("sshUser"), r.Str("hostKey"))
	}
}

// bugs-r11-opus 2: a private port's cookie also reaches it from another
// hosted app's page (same site); that page must not open a websocket to it.
// bugs-r11-opus 8: "Domain =x" (space before =) slipped past the Domain filter.
func TestR11HostedPortCrossOriginAndSpacedDomain(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "spaced=1; Domain =localhost; Path=/")
		w.Header().Add("Set-Cookie", "tabbed=1; Domain\t=localhost; Path=/")
		w.Header().Add("Set-Cookie", "own=ok; Path=/")
		_, _ = w.Write([]byte("hi"))
	}))
	defer app.Close()
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Tunnel: appTunnel(app)})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000})
	if r.Status != 200 {
		t.Fatalf("host: %s", r)
	}
	tok := tokenOf(t, r.Str("url"))
	host := strings.ReplaceAll(id, "_", "-") + "-3000.localhost"
	ws := []string{"Upgrade: websocket", "Connection: Upgrade", "Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==", "Cookie: _port_auth=" + tok}
	if res := hget(t, env, host, "/", append(ws, "Origin: http://bx-other-3001.localhost")...); res.StatusCode != 403 {
		t.Fatalf("a sibling app's page opened a websocket to a private port with its cookie: %d", res.StatusCode)
	}
	if res := hget(t, env, host, "/", append(ws, "Origin: http://"+host)...); res.StatusCode == 403 {
		t.Fatalf("the port's own page was refused: %d %s", res.StatusCode, res.body)
	}
	res := hget(t, env, host, "/", "Cookie: _port_auth="+tok)
	if res.StatusCode != 200 {
		t.Fatalf("GET: %d %s", res.StatusCode, res.body)
	}
	for _, c := range res.Header.Values("Set-Cookie") {
		if strings.Contains(strings.ToLower(c), "domain") {
			t.Errorf("a Domain cookie passes: %q", c)
		}
	}
}

// bugs-r11-opus 3: the size and auto-stop policies read "type" and
// "ttlSeconds" case-sensitively, the handlers do not: "Type" bypassed them.
func TestR11PolicyFieldCaseBypass(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("r11carol", false)
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/"+u.Username, map[string]any{"allowedSizes": []string{"default"}, "maxTtlSeconds": 600}); r.Status != 200 {
		t.Fatalf("policy: %s", r)
	}
	for _, body := range []map[string]any{{"Type": "small"}, {"TYPE": "large"}, {"TtlSeconds": nil}} {
		if r := env.Do(key, "POST", "/api/v1/sandboxes", body); r.Status/100 == 2 {
			t.Fatalf("%v passed the policy: %s", body, r)
		}
	}
}

// bugs-r11-opus 3: {"ttlSeconds":null,"ttlSeconds":600} passed the auto-stop
// policy (it reads the last value) but the handler kept the null.
func TestR11RepeatedOptKeyReplacesNull(t *testing.T) {
	var v struct {
		T server.OptInt   `json:"t"`
		F server.OptFloat `json:"f"`
	}
	if err := json.Unmarshal([]byte(`{"t":null,"t":600,"f":null,"f":1.5}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.T.Null || v.T.Value != 600 || v.F.Null || v.F.Value != 1.5 {
		t.Fatalf("a repeated key kept the earlier null: %+v %+v", v.T, v.F)
	}
}
