package guest

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Browser-only view (DESK-015/016/017): Xvnc :2 on 127.0.0.1:5902 (one
// viewer at a time, no clipboard) with a loop that keeps Chrome open on the
// chosen profile. Chrome runs as the `crowdvm-browser` user inside a private
// mount namespace where /home/user, /tmp, the agent's state and the env file
// are empty tmpfs / /dev/null, so the browser cannot read the sandbox user's
// files. Images without that user fall back to running as `user`.

const (
	browserDisplay = ":2"
	browserPort    = 5902
	browserUser    = "crowdvm-browser"
	// DefaultBrowserProfile matches the server's default (outside /home/user).
	DefaultBrowserProfile = "/var/lib/crowdvm/browser-profile"
)

type browserView struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	profile  string
	isolated bool
}

var browserSystemDirs = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/proc", "/root",
	"/run", "/sbin", "/snap", "/sys", "/usr", "/var", "/tmp", "/home/user"}

func validBrowserProfile(p string) (string, error) {
	if p == "" {
		return DefaultBrowserProfile, nil
	}
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\n'\"\\$`") {
		return "", errors.New("profile must be an absolute path without quotes or control characters")
	}
	p = filepath.Clean(p)
	if p == DefaultBrowserProfile {
		return p, nil
	}
	for _, d := range browserSystemDirs {
		if p == "/" || p == d || strings.HasPrefix(p, d+"/") {
			return "", errors.New("profile must be outside /home/user and system directories, e.g. /data/chrome-profile")
		}
	}
	return p, nil
}

func browserBinary() string {
	for _, b := range []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	return ""
}

func (b *browserView) running() bool {
	if b.cmd == nil {
		return false
	}
	select {
	case <-b.done:
		return false
	default:
	}
	return listening(browserPort)
}

func (b *browserView) stop() {
	if b.cmd == nil {
		return
	}
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-b.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		<-b.done
	}
	b.cmd = nil
}

// POST /v1/internal/browser/start {profile?, width?, height?}
func (a *Agent) handleBrowserStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Profile string `json:"profile"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	profile, err := validBrowserProfile(req.Profile)
	if err != nil {
		writeErr(w, 400, "invalid_profile", err.Error())
		return
	}
	if req.Width <= 0 || req.Height <= 0 {
		req.Width, req.Height = 1920, 1080
	}
	if req.Width > 8192 || req.Height > 8192 {
		writeErr(w, 400, "invalid_size", "width and height must be at most 8192")
		return
	}
	if _, err := exec.LookPath("Xvnc"); err != nil || browserBinary() == "" {
		writeErr(w, 409, "browser_not_installed", "this image has no Xvnc or Chrome")
		return
	}
	b := &a.browser
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running() && b.profile == profile {
		ok(w, "browser.started", a.browserInfo(b, true))
		return
	}
	b.stop()
	if err := a.startBrowser(b, profile, req.Width, req.Height); err != nil {
		writeErr(w, 500, "browser_failed", err.Error())
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for !b.running() {
		select {
		case <-b.done:
			writeErr(w, 500, "browser_failed", "the browser display exited at start")
			return
		default:
		}
		if time.Now().After(deadline) {
			writeErr(w, 500, "browser_failed", "the browser display did not start listening on port 5902")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	ok(w, "browser.started", a.browserInfo(b, false))
}

// GET /v1/internal/browser/status
func (a *Agent) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	b := &a.browser
	b.mu.Lock()
	defer b.mu.Unlock()
	ok(w, "browser.status", a.browserInfo(b, false))
}

func (a *Agent) browserInfo(b *browserView, reused bool) map[string]any {
	return map[string]any{"running": b.running(), "port": browserPort, "display": browserDisplay,
		"profile": b.profile, "isolated": b.isolated, "reused": reused}
}

func (a *Agent) startBrowser(b *browserView, profile string, width, height int) error {
	chrome := browserBinary()
	xvnc := fmt.Sprintf("Xvnc %s -geometry %dx%d -depth 24 -rfbport %d -localhost -SecurityTypes None -BlacklistTimeout 0 -NeverShared -DisconnectClients=1 -AcceptCutText=0 -SendCutText=0",
		browserDisplay, width, height, browserPort)
	loop := fmt.Sprintf(`while :; do %s --crowdvm-browser --user-data-dir=%s --no-first-run --no-default-browser-check --start-maximized --window-position=0,0 --disable-session-crashed-bubble; sleep 1; done`,
		shQuote(chrome), shQuote(profile))
	inner := fmt.Sprintf(`export DISPLAY=%s; %s >/dev/null 2>&1 </dev/null & X=$!; trap 'kill $X 2>/dev/null' EXIT INT TERM; %s`, browserDisplay, xvnc, loop)

	u, uerr := user.Lookup(browserUser)
	isolated := !a.unpriv && os.Geteuid() == 0 && uerr == nil && haveTools("unshare", "setpriv", "mount")
	var cmd *exec.Cmd
	if isolated {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if err := os.MkdirAll(profile, 0o700); err != nil {
			return fmt.Errorf("cannot create the profile folder %s: %v", profile, err)
		}
		_ = os.Chown(profile, uid, gid)
		_ = os.Chmod(profile, 0o700)
		stage := "/run/crowdvm-browser-profile"
		_ = os.MkdirAll(stage, 0o700)
		_ = os.RemoveAll("/tmp/.X2-lock")
		var sb strings.Builder
		sb.WriteString("set -e\n")
		// keep the profile reachable, then hide everything of the sandbox user
		fmt.Fprintf(&sb, "mount --bind %s %s\n", shQuote(profile), stage)
		for _, d := range []string{a.home, "/tmp", a.data, "/run/crowdvm", "/root"} {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				mode := "0755"
				if d == "/tmp" {
					mode = "1777"
				}
				fmt.Fprintf(&sb, "mount -t tmpfs -o mode=%s,size=256m tmpfs %s\n", mode, shQuote(d))
			}
		}
		for _, f := range []string{"/etc/profile.d/crowdvm-env.sh", "/etc/crowdvm/sandbox-id"} {
			if _, err := os.Stat(f); err == nil {
				fmt.Fprintf(&sb, "mount --bind /dev/null %s\n", shQuote(f))
			}
		}
		fmt.Fprintf(&sb, "mkdir -p %s && mount --bind %s %s && umount %s\n", shQuote(profile), stage, shQuote(profile), stage)
		fmt.Fprintf(&sb, "exec setpriv --reuid=%d --regid=%d --init-groups --reset-env -- /bin/sh -c %s\n", uid, gid, shQuote(inner))
		cmd = exec.Command("unshare", "--mount", "--propagation", "private", "--", "/bin/sh", "-c", sb.String())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=" + sysLocale()}
		cmd.Dir = "/"
	} else {
		if err := os.MkdirAll(profile, 0o700); err != nil && !a.unpriv {
			return fmt.Errorf("cannot create the profile folder %s: %v", profile, err)
		}
		if !a.unpriv {
			_ = os.Chown(profile, a.uid, a.gid)
		}
		cmd = a.shellCmd(inner, a.home, nil)
	}
	logPath := filepath.Join(a.runDir(), "browser.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	if f, err := os.Create(logPath); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close()
	}
	if err := a.spawn(cmd, "browser"); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	b.cmd, b.done, b.profile, b.isolated = cmd, done, profile, isolated
	return nil
}

func haveTools(names ...string) bool {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return false
		}
	}
	return true
}
