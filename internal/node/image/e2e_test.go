package image

// End-to-end tests (need KVM/QEMU and internet). Gated by CROWDVM_E2E=1:
//
//	CROWDVM_E2E=1 CROWDVM_E2E_BUILD=minimal [CROWDVM_E2E_NO_XORRISO=1] go test -run TestE2EBuild -timeout 3h -v ./internal/node/image
//	CROWDVM_E2E=1 CROWDVM_E2E_IMAGE=<path>.qcow2 go test -run TestE2EBoot -timeout 1h -v ./internal/node/image
//
// Images go to CROWDVM_E2E_OUT (default ~/.local/share/crowdvm-dev/images).

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func e2e(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestE2EBuild(t *testing.T) {
	e2e(t)
	profile := os.Getenv("CROWDVM_E2E_BUILD")
	if profile == "" {
		t.Skip("set CROWDVM_E2E_BUILD=minimal|full")
	}
	home, _ := os.UserHomeDir()
	if os.Getenv("CROWDVM_E2E_NO_XORRISO") == "1" { // exercise the pure-Go seed ISO
		bin := t.TempDir()
		for _, name := range []string{"qemu-system-x86_64", "qemu-img"} {
			p, err := exec.LookPath(name)
			if err != nil {
				t.Fatal(err)
			}
			os.Symlink(p, filepath.Join(bin, name))
		}
		t.Setenv("PATH", bin)
	}
	res, err := Build(context.Background(), Options{
		Profile:    profile,
		CloudImage: envOr("CROWDVM_CLOUD_IMAGE", filepath.Join(home, ".cache/crowdvm/images/noble-server-cloudimg-amd64.img")),
		OutDir:     envOr("CROWDVM_E2E_OUT", filepath.Join(home, ".local/share/crowdvm-dev/images")),
		CPUs:       8,
		MemoryMiB:  8192,
		Force:      true,
		Log:        os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d MiB, build %.0fs, tools %v", res.Path, res.Meta.SizeBytes>>20, res.Meta.BuildSeconds, res.Meta.Tools)
}

// TestE2EBoot boots an overlay on CROWDVM_E2E_IMAGE with a CROWDVM config
// drive (written by WriteISO) holding config.json and a crowdvm-guest binary,
// measures boot-to-health, then runs CROWDVM_E2E_CHECKS (or the defaults for
// the image's profile) through the stand-in agent's /debug/exec.
// CROWDVM_E2E_GUEST=<binary> uses a real guest agent instead (health only).
func TestE2EBoot(t *testing.T) {
	e2e(t)
	img := os.Getenv("CROWDVM_E2E_IMAGE")
	if img == "" {
		t.Skip("set CROWDVM_E2E_IMAGE=<image>.qcow2")
	}
	// The overlay grows by the checks' writes: keep it off /tmp (tmpfs).
	home, _ := os.UserHomeDir()
	os.MkdirAll(filepath.Join(home, ".local/share/crowdvm-dev"), 0o755)
	dir, err := os.MkdirTemp(filepath.Join(home, ".local/share/crowdvm-dev"), "image-boot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	guest, standIn := os.Getenv("CROWDVM_E2E_GUEST"), false
	if guest == "" {
		guest, standIn = buildStandIn(t, dir), true
	}
	bin, err := os.ReadFile(guest)
	if err != nil {
		t.Fatal(err)
	}
	const token = "e2e-token"
	cfgJSON := fmt.Sprintf(`{"sandboxId":"sbx_e2e","token":%q,"listenAddr":":7070","hostname":"e2e","boot":"create","firstBoot":true,"desktop":{}}`, token)
	cfgISO := filepath.Join(dir, "cfg.iso")
	f, _ := os.Create(cfgISO)
	files := []ISOFile{{"config.json", []byte(cfgJSON)}, {"crowdvm-guest", bin}}
	if cli := os.Getenv("CROWDVM_E2E_CLI"); cli != "" { // in-sandbox CLI, installed by the real guest
		b, err := os.ReadFile(cli)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, ISOFile{"crowdvm", b})
	}
	if err := WriteISO(f, "CROWDVM", files); err != nil {
		t.Fatal(err)
	}
	f.Close()

	abs, _ := filepath.Abs(img)
	overlay := filepath.Join(dir, "overlay.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", abs, overlay, "32G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	port := freePort(t)
	console, _ := os.Create(filepath.Join(dir, "console.log"))
	defer console.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", qemuArgs(Accel(), 4, 8192,
		fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:7070", port),
		"-drive", "file="+overlay+",if=virtio,format=qcow2,discard=unmap",
		"-drive", "file="+cfgISO+",if=virtio,format=raw,readonly=on",
		"-serial", "file:"+console.Name())...)
	cmd.Stderr = os.Stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	defer func() {
		cancel()
		<-exited
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var health string
	for health == "" {
		select {
		case <-exited:
			b, _ := os.ReadFile(console.Name())
			t.Fatalf("qemu exited: %v; console:\n%s", waitErr, tailStr(string(b), 4000))
		default:
		}
		if time.Since(start) > 3*time.Minute {
			b, _ := os.ReadFile(console.Name())
			t.Fatalf("no health after 3m; console:\n%s", tailStr(string(b), 4000))
		}
		req, _ := http.NewRequest("GET", base+"/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if resp, err := client.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				health = string(b)
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	bootToHealth := time.Since(start)
	t.Logf("BOOT-TO-HEALTH %.2fs (accel %s): %s", bootToHealth.Seconds(), Accel(), strings.TrimSpace(health))
	if !standIn {
		return
	}
	if !strings.Contains(health, "sbx_e2e") {
		t.Errorf("stand-in could not read config.json from the config drive: %s", health)
	}

	exec := func(c string, timeout time.Duration) string {
		cl := &http.Client{Timeout: timeout}
		resp, err := cl.Get(base + "/debug/exec?c=" + url.QueryEscape(c))
		if err != nil {
			return "ERROR: " + err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	t.Logf("systemd-analyze:\n%s", exec("systemd-analyze; systemctl list-jobs --no-pager; systemd-analyze blame | head -12; systemd-analyze critical-chain crowdvm-guest.service; systemctl --failed --no-legend", 30*time.Second))
	checks := os.Getenv("CROWDVM_E2E_CHECKS")
	if checks == "" {
		checks = minimalChecks
		if strings.Contains(exec("command -v docker || true", 10*time.Second), "docker") {
			checks += fullChecks
			if os.Getenv("CROWDVM_E2E_WINE") == "1" || os.Getenv("CROWDVM_E2E_WINE_APPS") == "1" {
				checks += wineChecks
			}
			if os.Getenv("CROWDVM_E2E_WINE_APPS") == "1" {
				checks += wineAppChecks
			}
		}
	}
	failed := false
	for _, c := range strings.Split(strings.TrimSpace(checks), "\n") {
		if strings.TrimSpace(c) == "" {
			continue
		}
		out := exec(c+"; echo \"rc=$?\"", 20*time.Minute)
		ok := strings.HasSuffix(strings.TrimSpace(out), "rc=0")
		failed = failed || !ok
		t.Logf("[%s] %s\n%s", map[bool]string{true: "ok", false: "FAIL"}[ok], c, tailStr(strings.TrimSpace(out), 2500))
	}
	exec("systemctl poweroff", 5*time.Second)
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		t.Error("VM did not power off within 60s")
	}
	if failed {
		t.Error("some checks failed")
	}
}

const minimalChecks = `
id user && sudo -u user sudo -n true
systemctl is-active ssh.socket && ss -ltn | grep -q ':22 '
timeout 5 ssh-keyscan -t ed25519 127.0.0.1 | grep -q ssh-ed25519
test -f /etc/cloud/cloud-init.disabled
ip -4 addr show | grep -q 10.0.2.15
mountpoint -q /run/crowdvm/cfg && test -x /usr/local/bin/crowdvm-guest
locale | grep -q en_US.UTF-8
! command -v snap
which growpart resize2fs
ufw status | grep -q 'Status: active' && ufw status | grep -q '7070/tcp' && ufw status | grep -q '22/tcp'
ufw allow 9999/tcp >/dev/null && /usr/local/lib/crowdvm/firewall-reset && ! ufw status | grep -q 9999 && ufw status | grep -q 'Status: active'
systemctl is-enabled crowdvm-firewall-reset.service && grep -q OOMScoreAdjust=-1000 /etc/systemd/system/crowdvm-guest.service
test "$(stat -c %a /home/user)" = 750
`

const fullChecks = `
until docker info >/dev/null 2>&1; do sleep 1; done; docker run --rm hello-world | grep -q 'Hello from Docker' && docker buildx version && docker compose version
id -nG user | grep -qw docker
node --version && npm --version && pnpm --version && bun --version
sudo -iu user rustc --version && sudo -iu user cargo --version
python3 --version && pip --version && uv --version && python3 -m venv /tmp/v
gh --version | head -1
claude --version && codex --version && opencode --version
google-chrome --version && sudo -iu user code --version | head -1
command -v Xvnc && command -v xdotool && ffmpeg -version | head -1 && command -v scrot convert
! command -v wine
pi --version && kimi --version && vibe --version && hermes --version | head -1 && openclaw --version && sudo -iu user prime-agent --version && cursor-agent --version && agent --version && cua-driver --version
sudo -iu user bash -lc 'go version && deno --version | head -1 && java -version && mvn -v -B | head -1 && gradle --version -q | grep Gradle && kotlinc -version && scalac -version && sbt --script-version && ruby --version && gem --version && bundle --version && php --version | head -1 && composer --version && elixir --version | tail -1 && dotnet --version && R --version | head -1 && clang --version | head -1 && cmake --version | head -1 && ninja --version && nvm ls 22 | grep -q v22'
sudo -iu user bash -lc 'cd $(mktemp -d) && printf "package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"go-ok\")}\n" >m.go && go run m.go && echo "fun main() = println(\"kt-ok\")" >k.kt && kotlinc k.kt -include-runtime -d k.jar 2>/dev/null && java -jar k.jar && echo "puts :ruby_ok" | ruby && gem install --no-document rake >/dev/null && php -r "echo \"php-ok\n\";" && Rscript -e "cat(\"r-ok\n\")" && elixir -e "IO.puts :ex_ok" && deno eval "console.log(\"deno-ok\")"'
id crowdvm-browser && ! id -nG crowdvm-browser | grep -qw -e sudo -e docker && ! sudo -u crowdvm-browser ls /home/user
sudo -iu user google-chrome --headless=new --no-first-run --disable-gpu --dump-dom 'data:text/html,<p>chrome-ok</p>' 2>/dev/null | grep -q chrome-ok
command -v ghostty || command -v xfce4-terminal
git --version && gcc --version | head -1 && jq --version && rg --version | head -1
systemctl is-enabled crowdvm-desktop && systemctl start crowdvm-desktop && sleep 4 && sudo -u user DISPLAY=:0 xdotool getdisplaygeometry && sudo -u user DISPLAY=:0 scrot /tmp/shot.png && ls -l /tmp/shot.png && ss -ltn | grep -q '127.0.0.1:5900' && pgrep -u user xfce4-session
for i in $(seq 30); do sudo -u user XDG_RUNTIME_DIR=/run/user/1000 systemctl --user -q is-active cua-driver.service && break; sleep 1; done; sudo -u user /opt/ascii/cua-driver/cua-driver call --socket /run/ascii-cua/driver.sock get_screen_size | grep -q 1920
sudo -iu user bash -c 'ascii-record-desktop start /tmp/rec.mp4 && sleep 3 && crowdvm-record-desktop status | grep -q recording && ascii-record-desktop stop' && ffprobe -v error -show_entries stream=width -of csv=p=0 /tmp/rec.mp4 | grep -q 1920
grep -q '"require_eula": false' /opt/google/chrome/initial_preferences && sudo -u user DISPLAY=:0 bash -c 'setsid google-chrome-stable --user-data-dir=/tmp/fresh-profile "data:text/html,<title>first-run-ok</title>" >/dev/null 2>&1 </dev/null & for i in $(seq 30); do sleep 1; w=$(wmctrl -l); echo "$w" | grep -q "Terms of Service" && { echo "$w"; exit 1; }; echo "$w" | grep -q first-run-ok && exit 0; done; exit 1'; rc=$?; pkill -x chrome; test $rc = 0
lux 2>&1 | grep -q ascii-record-desktop; test "${PIPESTATUS[0]}" = 127
`

// wineChecks (CROWDVM_E2E_WINE=1, full image) run Boat's install-wine.sh
// unchanged (docs: windows-apps/install-wine), then a 64-bit and a 32-bit
// Windows program. Each check is one line, so the script goes in base64.
var wineChecks = "\nset -o pipefail; echo " + base64.StdEncoding.EncodeToString([]byte(installWine)) +
	" | base64 -d >/tmp/install-wine.sh && chmod 644 /tmp/install-wine.sh && sudo -iu user bash /tmp/install-wine.sh 2>&1 | tail -3" + `
sudo -iu user env DISPLAY=:0 WINEDEBUG=-all wine cmd /c echo wine64-ok 2>&1 | grep -a wine64-ok
sudo -iu user env DISPLAY=:0 WINEDEBUG=-all wine 'C:\windows\syswow64\cmd.exe' /c echo wine32-ok 2>&1 | grep -a wine32-ok
`

// wineAppChecks (CROWDVM_E2E_WINE_APPS=1, after wineChecks) are WIN-005's
// acceptance test: the apps of Boat's tested-app list that can be
// downloaded without a sign-up or an installer UI start under Wine and open
// their main window (xdotool), or, for console tools, print their banner.
// Not automated: WinRAR, HeidiSQL, Winamp, foobar2000, MPC-HC, IrfanView,
// FastStone, XnView, LTspice, FORScan, MicroDicom, RadiAnt, Steam, Paint.NET,
// Spelunky, Cave Story, ImgBurn, HxD (installers with UI or no stable
// download URL); the "do not run" list (CPU-Z, Paint.NET <= 5.1, CCleaner)
// is not asserted.
var wineAppChecks = "\necho " + base64.StdEncoding.EncodeToString([]byte(winApp)) + " | base64 -d >/tmp/winapp.sh && chmod 755 /tmp/winapp.sh" + `
sudo -iu user bash -c 'mkdir -p ~/winapps/7z && cd ~/winapps/7z && curl -fsSLo 7zr.exe https://www.7-zip.org/a/7zr.exe && WINEDEBUG=-all wine 7zr.exe i 2>/dev/null | grep -a "7-Zip"'
sudo -iu user /tmp/winapp.sh notepadpp 'Notepad\+\+' 'gh:notepad-plus-plus/notepad-plus-plus@portable\.x64\.zip$' '*/notepad++.exe'
sudo -iu user /tmp/winapp.sh winmerge 'WinMerge' 'gh:WinMerge/winmerge@x64-exe\.zip$' '*/WinMergeU.exe'
sudo -iu user /tmp/winapp.sh x64dbg 'x64dbg' 'gh:x64dbg/x64dbg@snapshot_.*\.zip$' '*/release/x64/x64dbg.exe'
sudo -iu user /tmp/winapp.sh procexp 'Process Explorer' https://download.sysinternals.com/files/ProcessExplorer.zip '*/procexp64.exe' -accepteula
sudo -iu user /tmp/winapp.sh autoruns 'Autoruns' https://download.sysinternals.com/files/Autoruns.zip '*/Autoruns64.exe' -accepteula
sudo -iu user /tmp/winapp.sh everything 'Everything' https://www.voidtools.com/Everything-1.4.1.1026.x64.zip '*/Everything.exe'
sudo -iu user /tmp/winapp.sh skifree 'SkiFree' https://ski.ihoc.net/ski32.exe '*/ski32.exe'
sudo -iu user wineserver -k; pkill -KILL -u user -f 'wine|\.exe' ; sleep 1; ! pgrep -u user -f 'wineserver|\.exe'
`

// winApp NAME WINDOW-REGEX URL|gh:OWNER/REPO@ASSET-REGEX EXE-PATH-GLOB [ARGS]:
// download (zip or exe), start it under Wine on the desktop and wait up to
// 90 s for a visible window whose title matches.
const winApp = `#!/bin/bash
set -eo pipefail
name=$1 win=$2 url=$3 exe=$4; shift 4
d=$HOME/winapps/$name; mkdir -p "$d"; cd "$d"
case $url in gh:*) r=${url#gh:}; url=$(curl -fsSL "https://api.github.com/repos/${r%@*}/releases/latest" | jq -r '.assets[].browser_download_url' | grep -E "${r#*@}" | head -1);; esac
[ -n "$url" ] && curl -fsSL -o "$d/pkg" "$url"
case $url in *.zip) python3 -m zipfile -e pkg .;; *) cp pkg "$(basename "$url")";; esac
p=$(find "$d" -ipath "$exe" | head -1); [ -n "$p" ]
export DISPLAY=:0 WINEDEBUG=-all
(cd "$(dirname "$p")" && setsid timeout 150 wine "$p" "$@" >/dev/null 2>&1 &)
for i in $(seq 90); do
  if xdotool search --onlyvisible --name "$win" >/dev/null 2>&1; then echo "$name: window up after ${i}s"; wineserver -k || true; exit 0; fi
  sleep 1
done
echo "$name: no '$win' window"; xdotool search --onlyvisible --name . getwindowname %@ 2>/dev/null | head; wineserver -k || true; exit 1
`

const installWine = `set -e
sudo dpkg --add-architecture i386
sudo mkdir -pm755 /etc/apt/keyrings
sudo wget -qO /etc/apt/keyrings/winehq-archive.key https://dl.winehq.org/wine-builds/winehq.key
sudo wget -qNP /etc/apt/sources.list.d/ https://dl.winehq.org/wine-builds/ubuntu/dists/noble/winehq-noble.sources
printf 'Package: libgd3\nPin: release o=Ubuntu\nPin-Priority: 1001\n' | sudo tee /etc/apt/preferences.d/wine-libgd3
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --allow-downgrades --no-install-recommends \
  winehq-staging mesa-vulkan-drivers mesa-vulkan-drivers:i386 cabextract p7zip-full
sudo wget -qO /usr/local/bin/winetricks https://raw.githubusercontent.com/Winetricks/winetricks/master/src/winetricks
sudo chmod +x /usr/local/bin/winetricks
export DISPLAY=:0
WINEDLLOVERRIDES="mscoree,mshtml=" wineboot -i
winetricks -q corefonts vcrun2022 dxvk d3dcompiler_47
wine --version
`

func tailStr(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// buildStandIn builds a tiny static guest agent: GET /v1/health (echoes the
// sandbox id from --config) and GET /debug/exec?c=<bash> (test only).
func buildStandIn(t *testing.T, dir string) string {
	src := filepath.Join(dir, "standin")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module standin\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte(`package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"os/exec"
)

func main() {
	config := flag.String("config", "", "")
	flag.Parse()
	var cfg struct{ SandboxID string }
	b, _ := os.ReadFile(*config)
	json.Unmarshal(b, &cfg)
	http.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "sandboxId": cfg.SandboxID})
	})
	http.HandleFunc("/debug/exec", func(w http.ResponseWriter, r *http.Request) {
		out, _ := exec.Command("bash", "-lc", r.URL.Query().Get("c")).CombinedOutput()
		w.Write(out)
	})
	panic(http.ListenAndServe(":7070", nil))
}
`), 0o644)
	out := filepath.Join(dir, "crowdvm-guest")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stand-in: %v\n%s", err, b)
	}
	return out
}
