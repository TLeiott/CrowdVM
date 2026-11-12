package cli

// Update checks (CLI-025, ADAPTED). Boat's CLI installs updates by itself after
// a command ends. The CrowdVM CLI only tells you when the server runs a newer
// release than this build (at most once every 6 hours, on a terminal) and
// leaves the install to
// `crowdvm self-update`, which checks the version of the downloaded binary and
// never installs an older one unless --force is given.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/crowdvm/crowdvm/internal/version"
)

const updateCheckEvery = 6 * time.Hour

// serverVersion asks GET /api/v1/server for the server's release.
func (a *App) serverVersion(ctx context.Context) (string, error) {
	cl, err := a.client()
	if err != nil {
		return "", err
	}
	info, err := cl.ServerInfo(ctx)
	if err != nil {
		return "", err
	}
	v, _ := info["version"].(string)
	return v, nil
}

// binaryVersion runs `path --version` ("crowdvm version v1.2.3").
func binaryVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

type updateState struct {
	CheckedAt time.Time `json:"checkedAt"`
	Server    string    `json:"server"`
	Version   string    `json:"version"`
}

// updateNotice prints one line on stderr when the server runs a newer
// release than this CLI. It runs after a successful command, at most every
// 6 hours per server, only on a terminal, never in JSON mode, and never with
// --no-update or CROWDVM_NO_UPDATE=1 (BOAT_NO_UPDATE=1).
func (a *App) updateNotice(cmd string) {
	if a.noUpdate || a.jsonOut || a.Getenv("CROWDVM_NO_UPDATE") == "1" || a.Getenv("BOAT_NO_UPDATE") == "1" || a.Getenv("CI") != "" || !isTerminal(a.Err) {
		return
	}
	switch cmd {
	case "self-update", "version", "completions", "completion", "__complete", "logout", "help":
		return
	}
	if _, ok := version.Parse(client.Version); !ok || a.server() == "" || a.apiKey() == "" {
		return
	}
	path := filepath.Join(a.configDir(), "update-check.json")
	var st updateState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Server == a.server() && time.Since(st.CheckedAt) < updateCheckEvery {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := a.serverVersion(ctx)
	if err != nil {
		return // offline or signed out: try again next time
	}
	st = updateState{CheckedAt: time.Now().UTC(), Server: a.server(), Version: v}
	if b, err := json.Marshal(st); err == nil {
		_ = os.MkdirAll(a.configDir(), 0o700)
		_ = os.WriteFile(path, b, 0o600)
	}
	if c, ok := version.Compare(v, client.Version); ok && c > 0 {
		fmt.Fprintf(a.Err, "\nA newer crowdvm is available: %s (you have %s). Run `%s` to install it.\n", v, client.Version, updateCommand())
	}
}

// npmInstalled: npm's crowdvm package started this binary (its launcher
// sets CROWDVM_NPM_LAUNCHER). npm then owns updates.
func npmInstalled() bool { return os.Getenv("CROWDVM_NPM_LAUNCHER") != "" }

// channel: "nightly" for a nightly build, else "stable".
func channel() string {
	if strings.Contains(client.Version, "-nightly.") {
		return "nightly"
	}
	return "stable"
}

// updateCommand is how this installation is updated.
func updateCommand() string {
	if !npmInstalled() {
		return "crowdvm self-update"
	}
	if channel() == "nightly" {
		return "npm i -g crowdvm@nightly"
	}
	return "npm i -g crowdvm@latest"
}
