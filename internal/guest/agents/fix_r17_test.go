package agents

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r17-opus 7: with the AI proxy on, every vendor's base URL pointed at
// the proxy, which refuses vendors the profile holds no key for (OpenCode's
// free models among them). Only the vendors the server names are proxied.
func TestR17ProxyEnvOnlyConfiguredVendors(t *testing.T) {
	m := &Manager{}
	m.proxy = &proto.AIProxy{BaseURL: "http://10.0.2.2:8420/ai-proxy", Token: "cvmai_x", Vendors: []string{"anthropic"}, VendorsSet: true}
	env := m.ProxyEnv()
	if env["ANTHROPIC_BASE_URL"] == "" || env["ANTHROPIC_API_KEY"] != "cvmai_x" {
		t.Fatalf("anthropic not proxied: %v", env)
	}
	if _, ok := env["OPENCODE_BASE_URL"]; ok {
		t.Fatalf("opencode proxied without a key: %v", env)
	}
	m.proxy.VendorsSet = false // an older server: every vendor, as before
	if env := m.ProxyEnv(); env["OPENCODE_BASE_URL"] == "" {
		t.Fatalf("older server: %v", env)
	}
}
