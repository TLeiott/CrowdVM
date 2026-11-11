package guest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Credential files derived from the sandbox env, rewritten whenever the env
// changes (boot, apply-config):
//
//   - KEY-017: ~/.config/crowdvm/config.json, profile "default" = the
//     in-sandbox CLI's server (CROWDVM_API_URL) and per-sandbox key
//     (CROWDVM_API_KEY); removed again when the key is withheld. Legacy
//     path ~/.config/ascii/boat -> ../crowdvm.
//   - ENV-016: ~/.claude/.credentials.json with the profile's Claude
//     sign-in (CLAUDE_CODE_OAUTH_TOKEN, no refresh token: the server renews).
//   - AIC-011: a Claude sign-in the user made in the sandbox (/login: the
//     file has a refresh token) is never overwritten or removed, and new
//     shells stop exporting the profile's Claude variables (claudeLoginGuard).
//
// A value the guest wrote is only replaced or removed while the file still
// holds it (sha256 kept under the data dir), so the user's own `crowdvm
// login` or Claude sign-in wins.

type managedCreds struct {
	CLIKey      string `json:"cliKey,omitempty"`      // sha256 of the apiKey written
	ClaudeToken string `json:"claudeToken,omitempty"` // sha256 of the accessToken written
}

func (a *Agent) credsStatePath() string { return filepath.Join(a.data, "managed-creds.json") }

func hashSecret(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (a *Agent) writeCredentialFiles(env map[string]string) {
	var st managedCreds
	if b, err := os.ReadFile(a.credsStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	old := st
	st.CLIKey = a.writeCLIConfig(env["CROWDVM_API_KEY"], env["CROWDVM_API_URL"], st.CLIKey)
	st.ClaudeToken = a.writeClaudeCredentials(env["CLAUDE_CODE_OAUTH_TOKEN"], st.ClaudeToken)
	a.writeGitCredentials() // ENV-016: GH_TOKEN -> github.com credential helper
	if st != old {
		b, _ := json.Marshal(st)
		_ = os.WriteFile(a.credsStatePath(), b, 0o600)
	}
}

// readJSONObject reads a JSON object file ({} when missing; nil when it
// exists but is not a JSON object: never touch such a file).
func readJSONObject(p string) map[string]any {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return map[string]any{}
	}
	var m map[string]any
	if err != nil || json.Unmarshal(b, &m) != nil || m == nil {
		return nil
	}
	return m
}

// writeJSONFile writes m (0600, owned by the user) when it changed; an
// empty object removes the file.
func (a *Agent) writeJSONFile(p string, m map[string]any) {
	if len(m) == 0 {
		_ = os.Remove(p)
		return
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	b = append(b, '\n')
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, b) {
		return
	}
	if err := a.mkdirAll(filepath.Dir(p), 0o700); err != nil {
		a.log.Printf("%s: %v", p, err)
		return
	}
	if err := a.writeFileAtomic(p, b, 0o600, true); err != nil {
		a.log.Printf("%s: %v", p, err)
	}
}

// writeCLIConfig (KEY-017) returns the hash of the key now managed.
func (a *Agent) writeCLIConfig(key, apiURL, prevHash string) string {
	// Boat's recipes write into ~/.config/boat/ (wallpaper.png): have it ready.
	_ = a.mkdirAll(filepath.Join(a.home, ".config", "boat"), 0o755)
	dir := filepath.Join(a.home, ".config", "crowdvm")
	p := filepath.Join(dir, "config.json")
	cfg := readJSONObject(p)
	if cfg == nil {
		return prevHash
	}
	profiles, _ := cfg["profiles"].(map[string]any)
	if profiles == nil {
		profiles = map[string]any{}
	}
	prof, _ := profiles["default"].(map[string]any)
	if prof == nil {
		prof = map[string]any{}
	}
	cur, _ := prof["apiKey"].(string)
	ours := cur == "" || hashSecret(cur) == prevHash
	if !ours {
		return prevHash // the user signed in with their own key
	}
	if key == "" {
		if cur == "" {
			return ""
		}
		delete(prof, "apiKey") // withheld (no-env, safe for third parties, credentials off)
	} else {
		prof["apiKey"] = key
		if apiURL != "" {
			prof["server"] = strings.TrimSuffix(strings.TrimSuffix(apiURL, "/"), "/api/v1")
		}
	}
	if len(prof) == 0 {
		delete(profiles, "default")
	} else {
		profiles["default"] = prof
	}
	cfg["profiles"] = profiles
	if len(profiles) == 0 && len(cfg) == 1 {
		cfg = nil // nothing left: remove the file
	}
	a.writeJSONFile(p, cfg)
	if key != "" {
		a.legacyCLILink(dir)
	}
	return hashSecret(key)
}

// legacyCLILink: Boat's path ~/.config/ascii/boat (only when free).
func (a *Agent) legacyCLILink(dir string) {
	l := filepath.Join(a.home, ".config", "ascii", "boat")
	if _, err := os.Lstat(l); err == nil {
		return
	}
	if a.mkdirAll(filepath.Dir(l), 0o755) == nil && os.Symlink(filepath.Join("..", "crowdvm"), l) == nil {
		a.chown(l)
	}
}

// claudeUserLogin reports whether ~/.claude/.credentials.json holds a
// sign-in made inside the sandbox (/login stores a refresh token; the
// profile's sign-in never has one).
func claudeUserLogin(m map[string]any) bool {
	o, _ := m["claudeAiOauth"].(map[string]any)
	rt, _ := o["refreshToken"].(string)
	return rt != ""
}

// writeClaudeCredentials (ENV-016) returns the hash of the token now managed.
func (a *Agent) writeClaudeCredentials(token, prevHash string) string {
	p := filepath.Join(a.home, ".claude", ".credentials.json")
	m := readJSONObject(p)
	if m == nil || claudeUserLogin(m) {
		return prevHash // AIC-011: the user's own sign-in stays
	}
	o, _ := m["claudeAiOauth"].(map[string]any)
	cur, _ := o["accessToken"].(string)
	if cur != "" && hashSecret(cur) != prevHash {
		return prevHash // not ours
	}
	if token == "" {
		if o == nil {
			return ""
		}
		delete(m, "claudeAiOauth")
	} else {
		// Claude Code's own shape for an env token: no refresh, no expiry.
		m["claudeAiOauth"] = map[string]any{"accessToken": token, "refreshToken": nil, "expiresAt": nil,
			"scopes": []string{"user:inference"}, "subscriptionType": nil}
	}
	a.writeJSONFile(p, m)
	return hashSecret(token)
}

// claudeLoginGuard (AIC-011) is a shell line for the env files: after a
// Claude sign-in inside the sandbox (/login), new shells drop the
// profile's Claude variables (and the proxy's placeholder key) so claude
// uses that sign-in. Integrated-agent turns keep the profile.
func (a *Agent) claudeLoginGuard(env map[string]string) string {
	var unset []string
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if _, ok := env[k]; ok {
			unset = append(unset, k)
		}
	}
	if p := a.config().AIProxy; p != nil && p.Token != "" && env["ANTHROPIC_API_KEY"] == p.Token {
		unset = append(unset, "ANTHROPIC_API_KEY")
	}
	if len(unset) == 0 {
		return ""
	}
	return `if grep -qs '"refreshToken" *: *"[^"]' "$HOME/.claude/.credentials.json"; then unset ` + strings.Join(unset, " ") + "; fi\n"
}
