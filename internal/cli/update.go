package cli

// Update checks (CLI-025, ADAPTED). Boat's CLI installs updates by itself after
// a command ends. CrowdVM's /downloads has no version manifest yet, so the CLI
// only tells you when the server runs a newer release than this build (at
// most once every 6 hours, on a terminal) and leaves the install to
// `crowdvm self-update`, which checks the version of the downloaded binary and
// never installs an older one unless --force is given.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
)

const updateCheckEvery = 6 * time.Hour

// version is a release version as `git describe` prints it: v1.2.3,
// v1.2.3-rc.1 or v1.2.3-4-gabc1234[-dirty] (4 commits after v1.2.3).
type version struct {
	core  [3]int
	after int    // commits after the tag (describe suffix)
	pre   bool   // pre-release (-rc.1, -beta, ...)
	preID string // the pre-release suffix, compared by its dot-separated identifiers
}

var (
	versionRe  = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?(?:-(.+))?$`)
	describeRe = regexp.MustCompile(`^(\d+)-g[0-9a-f]+(?:-dirty)?$`)
)

// parseVersion reads a release version; ok is false for "dev", bare commit
// hashes and other strings that cannot be ordered.
func parseVersion(s string) (v version, ok bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return v, false
	}
	for i := 0; i < 3; i++ {
		v.core[i], _ = strconv.Atoi(m[i+1])
	}
	switch suf := strings.TrimSuffix(m[4], "-dirty"); {
	case suf == "":
	case describeRe.MatchString(m[4]):
		v.after, _ = strconv.Atoi(describeRe.FindStringSubmatch(m[4])[1])
	default:
		v.pre, v.preID = true, suf
	}
	return v, true
}

// compareVersions returns -1, 0 or 1 for a < b, a == b, a > b; ok is false
// when either side is not a release version.
func compareVersions(a, b string) (int, bool) {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if va.core[i] != vb.core[i] {
			return sign(va.core[i] - vb.core[i]), true
		}
	}
	if va.pre != vb.pre { // 1.2.3-rc.1 < 1.2.3
		if va.pre {
			return -1, true
		}
		return 1, true
	}
	if va.pre { // bugs-r16-sonnet 1: -nightly.20261112.3 > -nightly.20261111.9
		return comparePre(va.preID, vb.preID), true
	}
	return sign(va.after - vb.after), true
}

// comparePre orders two pre-release suffixes identifier by identifier (split
// at dots; numeric identifiers as numbers, below alphanumeric ones).
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		na, ea := strconv.Atoi(as[i])
		nb, eb := strconv.Atoi(bs[i])
		var c int
		switch {
		case ea == nil && eb == nil:
			c = sign(na - nb)
		case ea == nil:
			c = -1
		case eb == nil:
			c = 1
		default:
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}
	return sign(len(as) - len(bs))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

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
	if _, ok := parseVersion(client.Version); !ok || a.server() == "" || a.apiKey() == "" {
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
	if c, ok := compareVersions(v, client.Version); ok && c > 0 {
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
