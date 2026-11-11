package server_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// r8RunInstall runs scripts/install.sh under sh with stub uname/curl (no
// network, no root) and returns the URLs it fetched and BIN_DIR.
func r8RunInstall(t *testing.T, unameS, unameM string, args ...string) ([]string, string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub")
	home := filepath.Join(dir, "home")
	log := filepath.Join(dir, "curl.log")
	_ = os.MkdirAll(stub, 0o755)
	_ = os.MkdirAll(home, 0o755)
	w := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stub, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w("uname", `case "$1" in -s) echo `+unameS+` ;; -m) echo `+unameM+` ;; *) echo `+unameS+` ;; esac`+"\n")
	// curl -fsSL URL -o FILE: record URL, write a fake binary that accepts anything.
	w("curl", `url=""; out=""
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done
echo "$url" >> "`+log+`"
printf '#!/bin/sh\nexit 0\n' > "$out"
`)
	script, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "install.sh"))
	cmd := exec.Command(sh, append([]string{script, "--no-shell", "--no-login", "--server=http://srv:8420"}, args...)...)
	cmd.Env = []string{"PATH=" + stub + ":/usr/bin:/bin", "HOME=" + home, "SHELL=/bin/sh"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(log)
	return strings.Fields(string(b)), filepath.Join(home, ".local", "bin")
}

// A node needs the linux/amd64 in-sandbox CLI next to crowdvm-node
// (internal/node/config.go: crowdvm-linux-amd64, or crowdvm only on
// linux/amd64). install.ps1 fetches crowdvm-linux-amd64; install.sh does not,
// so sandboxes on a Linux arm64 node get no crowdvm/host/boat commands.
func TestR8InstallShArm64NodeGetsInSandboxCLI(t *testing.T) {
	urls, bin := r8RunInstall(t, "Linux", "aarch64", "--node")
	want := []string{"crowdvm", "crowdvm-node", "crowdvm-guest"}
	for _, n := range want {
		if _, err := os.Stat(filepath.Join(bin, n)); err != nil {
			t.Errorf("%s not installed (fetched %v)", n, urls)
		}
	}
	if _, err := os.Stat(filepath.Join(bin, "crowdvm-linux-amd64")); err != nil {
		t.Errorf("linux/arm64 --node: crowdvm-linux-amd64 not installed, so the node has no in-sandbox CLI for its config drive; fetched %v", urls)
	}
}

// Control: on linux/amd64 the node uses the downloaded crowdvm itself.
func TestR8InstallShAmd64Node(t *testing.T) {
	urls, bin := r8RunInstall(t, "Linux", "x86_64", "--node")
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://srv:8420/downloads/linux/amd64/") {
			t.Errorf("unexpected URL %s", u)
		}
	}
	for _, n := range []string{"crowdvm", "crowdvm-node", "crowdvm-guest"} {
		if _, err := os.Stat(filepath.Join(bin, n)); err != nil {
			t.Errorf("%s not installed", n)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(bin, "*.tmp")); len(m) > 0 {
		t.Errorf("temp files left: %v", m)
	}
}

// install.ps1 writes the profile line as `& '<path>'` without doubling single
// quotes. A Windows user named O'Brien gets a profile that fails to parse on
// every PowerShell start.
func TestR8InstallPs1ProfileLineQuoting(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*\$Line = "(.*)"\s*$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no $Line in install.ps1")
	}
	escapes := strings.Contains(string(b), `-replace "'", "''"`) || strings.Contains(string(b), `.Replace("'", "''")`)
	cli := `C:\Users\O'Brien\AppData\Local\Programs\crowdvm\crowdvm.exe`
	if !escapes {
		cli = strings.ReplaceAll(m[1], "$Cli", cli)
	} else {
		cli = strings.ReplaceAll(m[1], "$Cli", strings.ReplaceAll(cli, "'", "''"))
	}
	// 'C:\Users\O'Brien\...' ends the literal at O' and leaves Brien\... as bare tokens.
	if !escapes {
		t.Errorf("profile line for a path with a quote is not valid PowerShell: %s", cli)
	}
}
