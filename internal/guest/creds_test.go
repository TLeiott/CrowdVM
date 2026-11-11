package guest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	m := readJSONObject(p)
	if m == nil {
		t.Fatalf("%s: not a JSON object", p)
	}
	return m
}

// KEY-017, ENV-016, AIC-011.
func TestCredentialFiles(t *testing.T) {
	cfg := proto.GuestConfig{Boot: proto.BootCreate, FirstBoot: true, Env: map[string]string{
		"CROWDVM_API_KEY": "cvm_key1", "CROWDVM_API_URL": "http://10.0.2.2:8420/api/v1",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-a", "ANTHROPIC_BASE_URL": "https://x"}}
	e := newEnv(t, cfg)
	cliCfg := filepath.Join(e.home, ".config/crowdvm/config.json")
	prof := readJSON(t, cliCfg)["profiles"].(map[string]any)["default"].(map[string]any)
	if prof["apiKey"] != "cvm_key1" || prof["server"] != "http://10.0.2.2:8420" {
		t.Fatalf("cli config: %v", prof)
	}
	if fi, err := os.Stat(cliCfg); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cli config mode: %v %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(e.home, ".config/ascii/boat/config.json")); err != nil || !strings.Contains(string(b), "cvm_key1") {
		t.Fatalf("legacy path: %v %s", err, b)
	}
	cred := filepath.Join(e.home, ".claude/.credentials.json")
	if o := readJSON(t, cred)["claudeAiOauth"].(map[string]any); o["accessToken"] != "sk-ant-oat01-a" || o["refreshToken"] != nil {
		t.Fatalf("claude credentials: %v", o)
	}

	// Renewed token and rotated key replace the managed values.
	e.mustOK("POST", "/v1/internal/apply-config", map[string]any{"envSet": map[string]string{"CROWDVM_API_KEY": "cvm_key2", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-b"}})
	if o := readJSON(t, cred)["claudeAiOauth"].(map[string]any); o["accessToken"] != "sk-ant-oat01-b" {
		t.Fatalf("renewed claude token: %v", o)
	}
	// Shells keep the profile's variables until the user signs in.
	shell := func() string {
		out, err := exec.Command("bash", "-c", `HOME=`+e.home+`; source "$HOME/.bashrc"; echo "[$CLAUDE_CODE_OAUTH_TOKEN|$ANTHROPIC_BASE_URL]"`).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := shell(); got != "[sk-ant-oat01-b|https://x]" {
		t.Fatalf("shell before /login: %s", got)
	}
	// AIC-011: /login inside the sandbox (refresh token) wins and stays.
	login, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "user-at", "refreshToken": "user-rt", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}})
	os.WriteFile(cred, login, 0o600)
	if got := shell(); got != "[|]" {
		t.Fatalf("shell after /login: %s", got)
	}
	// The user's own CLI login stays too.
	os.WriteFile(cliCfg, []byte(`{"profiles":{"default":{"server":"http://other","apiKey":"cvm_mine"}}}`), 0o600)
	e.mustOK("POST", "/v1/internal/apply-config", map[string]any{"envSet": map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-c", "CROWDVM_API_KEY": "cvm_key3"}})
	if o := readJSON(t, cred)["claudeAiOauth"].(map[string]any); o["accessToken"] != "user-at" {
		t.Fatalf("user sign-in overwritten: %v", o)
	}
	if p := readJSON(t, cliCfg)["profiles"].(map[string]any)["default"].(map[string]any); p["apiKey"] != "cvm_mine" {
		t.Fatalf("user CLI login overwritten: %v", p)
	}
}

// Withholding the key and the sign-in removes what the guest wrote.
func TestCredentialFilesWithheld(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{Boot: proto.BootCreate, FirstBoot: true, Env: map[string]string{
		"CROWDVM_API_KEY": "cvm_key1", "CLAUDE_CODE_OAUTH_TOKEN": "tok"}})
	e.mustOK("POST", "/v1/internal/apply-config", map[string]any{"envUnset": []string{"CROWDVM_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}})
	if _, err := os.Stat(filepath.Join(e.home, ".claude/.credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("claude credentials kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config/crowdvm/config.json")); !os.IsNotExist(err) {
		t.Fatalf("cli config kept: %v", err)
	}
}

// LIFE-009: health says reposCloning until the boot clones ran.
func TestHealthReposCloning(t *testing.T) {
	src := t.TempDir()
	for _, c := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", src}, c...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", c, err, out)
		}
	}
	e := newEnv(t, proto.GuestConfig{Boot: proto.BootCreate, FirstBoot: true,
		Repos: []proto.Repo{{URL: src, Path: "app", SetupScript: "sleep 1; touch done"}}})
	if m := e.mustOK("GET", "/v1/health", nil); m["reposCloning"] != true {
		t.Fatalf("before clones: %v", m)
	}
	go e.a.BootBackground(context.Background())
	deadline := time.Now().Add(20 * time.Second)
	for e.mustOK("GET", "/v1/health", nil)["reposCloning"] != false {
		if time.Now().After(deadline) {
			t.Fatal("still cloning")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(e.home, "app/.git")); err != nil {
		t.Fatalf("not cloned when reposCloning turned false: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, "app/done")); err == nil {
		t.Fatal("ready waited for the setup script too")
	}
	// No repos: never cloning.
	e2 := newEnv(t, proto.GuestConfig{Boot: proto.BootResume})
	if m := e2.mustOK("GET", "/v1/health", nil); m["reposCloning"] != false {
		t.Fatalf("no repos: %v", m)
	}
}

// ENV-016: a GitHub source's GH_TOKEN works for git outside cloned repos.
func TestGitHubCredentialHelper(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{Boot: proto.BootCreate, FirstBoot: true, Env: map[string]string{"GH_TOKEN": "ghp_x"}})
	b, _ := os.ReadFile(filepath.Join(e.home, ".config/crowdvm/git-credentials"))
	if string(b) != "https://x-access-token:ghp_x@github.com\n" {
		t.Fatalf("git credentials: %q", b)
	}
	out, err := exec.Command("git", "config", "--file", filepath.Join(e.home, ".gitconfig"), "--get", "credential.https://github.com.helper").Output()
	if err != nil || !strings.Contains(string(out), "store --file="+filepath.Join(e.home, ".config/crowdvm/git-credentials")) {
		t.Fatalf("helper: %q %v", out, err)
	}
	e.mustOK("POST", "/v1/internal/apply-config", map[string]any{"envUnset": []string{"GH_TOKEN"}})
	if _, err := os.Stat(filepath.Join(e.home, ".config/crowdvm/git-credentials")); !os.IsNotExist(err) {
		t.Fatalf("withdrawn token kept: %v", err)
	}
}
