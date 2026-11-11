package guest

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	desktopDisplay = ":0"
	desktopVNCPort = 5900
	maxWallpaper   = 16 << 20
)

// desktop manages the TigerVNC + Xfce session on DISPLAY :0 (VNC port 5900)
// and its screenshot / input / recording / wallpaper helpers.
type desktop struct {
	a       *Agent
	mu      sync.Mutex
	startMu sync.Mutex // serializes start

	busAddr string // session D-Bus address the Xfce session uses

	rec      *exec.Cmd
	recStdin io.WriteCloser
	recPath  string // virtual
	recStart time.Time
	recDone  chan struct{}

	wallState string
}

func newDesktop(a *Agent) *desktop { return &desktop{a: a} }

func (d *desktop) routes(m *http.ServeMux) {
	m.HandleFunc("POST /v1/internal/desktop/start", d.handleStart)
	m.HandleFunc("GET /v1/internal/desktop/status", d.handleStatus)
	m.HandleFunc("GET /v1/internal/desktop/screenshot", d.handleScreenshot)
	m.HandleFunc("POST /v1/internal/desktop/input", d.handleInput)
	m.HandleFunc("POST /v1/internal/desktop/recording/start", d.handleRecStart)
	m.HandleFunc("POST /v1/internal/desktop/recording/stop", d.handleRecStop)
	m.HandleFunc("GET /v1/internal/desktop/recording", d.handleRecStatus)
	m.HandleFunc("POST /v1/internal/desktop/wallpaper", d.handleWallpaper)
}

func (d *desktop) size() (int, int) {
	w, h := d.a.cfg.Desktop.Width, d.a.cfg.Desktop.Height
	if w <= 0 {
		w = 1920
	}
	if h <= 0 {
		h = 1080
	}
	return w, h
}

func (d *desktop) running() bool { return listening(desktopVNCPort) }

// listening: something listens on TCP port (IPv4 or IPv6), read from
// /proc/net, never by connecting: Xvnc counts a connection closed before
// its handshake as a security failure and blacklists 127.0.0.1 after five,
// which locked every desktop user out (bugs-r4-opus 1). Without /proc it
// falls back to a connect.
func listening(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	found := false
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		found = true
		for _, l := range strings.Split(string(b), "\n")[1:] {
			fs := strings.Fields(l)
			if len(fs) > 3 && strings.HasSuffix(fs[1], want) && fs[3] == "0A" { // 0A: LISTEN
				return true
			}
		}
	}
	if found {
		return false
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// env is the environment for programs on the desktop.
func (d *desktop) env() []string {
	extra := map[string]string{"DISPLAY": desktopDisplay}
	d.mu.Lock()
	if d.busAddr != "" {
		extra["DBUS_SESSION_BUS_ADDRESS"] = d.busAddr
	}
	d.mu.Unlock()
	return d.a.commandEnv(extra)
}

func (d *desktop) userCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = d.env()
	cmd.Dir = d.a.home
	cmd.SysProcAttr = d.a.sysProcAttr()
	return cmd
}

// patchOldImage: images built before the blacklist fix start Xvnc (from
// crowdvm-desktop.service, enabled at boot) with TigerVNC's blacklist on,
// so connections closed before their handshake lock 127.0.0.1 (every
// desktop relay) out (bugs-r5-opus 3). -BlacklistTimeout 0 turns it off;
// -BlacklistThreshold 0 (an earlier attempt) blocks after a single failure
// (bugs-r6-opus 1). Run once at guest start: the session script gets the
// flag through a runtime systemd drop-in, and the desktop starts again with it.
func (d *desktop) patchOldImage() {
	const script = "/usr/local/lib/crowdvm/desktop-session"
	if d.a.unpriv {
		return
	}
	b, err := os.ReadFile(script)
	if err != nil {
		return
	}
	patched, ok := patchSessionScript(b)
	if !ok {
		return
	}
	const dropin = "/run/systemd/system/crowdvm-desktop.service.d"
	if os.MkdirAll("/run/crowdvm", 0o755) != nil || os.WriteFile("/run/crowdvm/desktop-session", patched, 0o755) != nil ||
		os.MkdirAll(dropin, 0o755) != nil ||
		os.WriteFile(dropin+"/blacklist.conf", []byte("[Service]\nExecStart=\nExecStart=/bin/sh /run/crowdvm/desktop-session\n"), 0o644) != nil {
		return
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	// A plain restart leaves the old Xvnc running: PAMName=login moves the
	// session into its own logind scope, which stopping the service does not
	// kill. The new one then cannot take display :0 and systemd restarts it
	// every 2 s for ever (bugs-r7-opus 1). Stop, kill what is left on :0,
	// clear its locks, start.
	_ = exec.Command("systemctl", "stop", "crowdvm-desktop.service").Run()
	_ = exec.Command("pkill", "-f", "Xvnc :0( |$)").Run()
	for i := 0; i < 50 && exec.Command("pgrep", "-f", "Xvnc :0( |$)").Run() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = exec.Command("pkill", "-KILL", "-f", "Xvnc :0( |$)").Run()
	_ = os.Remove("/tmp/.X0-lock")
	_ = os.Remove("/tmp/.X11-unix/X0")
	_ = exec.Command("systemctl", "start", "crowdvm-desktop.service").Run()
}

// patchSessionScript adds -BlacklistTimeout 0 to an image's desktop-session
// script (dropping the wrong -BlacklistThreshold 0); false: nothing to do.
func patchSessionScript(b []byte) ([]byte, bool) {
	if bytes.Contains(b, []byte("BlacklistTimeout 0")) || !bytes.Contains(b, []byte("-SecurityTypes None")) {
		return nil, false
	}
	p := bytes.ReplaceAll(b, []byte(" -BlacklistThreshold 0"), nil)
	return bytes.Replace(p, []byte("-SecurityTypes None"), []byte("-SecurityTypes None -BlacklistTimeout 0"), 1), true
}

var errDesktopMissing = errors.New("the desktop is not installed in this image (TigerVNC Xvnc not found)")

// start ensures Xvnc + the Xfce session run. Idempotent.
func (d *desktop) start() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	if d.running() {
		return nil
	}
	if _, err := exec.LookPath("Xvnc"); err != nil {
		return errDesktopMissing
	}
	if !d.a.unpriv { // stale X locks from a snapshot taken while it ran
		_ = os.Remove("/tmp/.X0-lock")
		_ = os.Remove("/tmp/.X11-unix/X0")
	}
	w, h := d.size()
	// The full image ships crowdvm-desktop.service (Xvnc :0 + Xfce as user);
	// prefer it so the session is supervised by systemd.
	if !d.a.unpriv {
		if _, err := os.Stat("/etc/systemd/system/crowdvm-desktop.service"); err == nil {
			_ = os.MkdirAll("/run/crowdvm", 0o755)
			_ = os.WriteFile("/run/crowdvm/desktop.env", []byte(fmt.Sprintf("CROWDVM_DESKTOP_GEOMETRY=%dx%d\n", w, h)), 0o644)
			if out, err := exec.Command("systemctl", "start", "crowdvm-desktop.service").CombinedOutput(); err != nil {
				return fmt.Errorf("start crowdvm-desktop.service: %v: %s", err, out)
			}
			deadline := time.Now().Add(15 * time.Second)
			for !d.running() {
				if time.Now().After(deadline) {
					return errors.New("crowdvm-desktop.service did not start listening on port 5900")
				}
				time.Sleep(100 * time.Millisecond)
			}
			d.ensureBus()
			d.mu.Lock()
			d.wallState = ""
			d.mu.Unlock()
			return nil
		}
	}
	xvnc := d.userCmd("Xvnc", desktopDisplay, "-geometry", fmt.Sprintf("%dx%d", w, h), "-depth", "24",
		"-rfbport", strconv.Itoa(desktopVNCPort), "-localhost", "-SecurityTypes", "None",
		"-AlwaysShared", "-AcceptSetDesktopSize", "-BlacklistTimeout", "0")
	if err := xvnc.Start(); err != nil {
		return err
	}
	go func() { _ = xvnc.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for !d.running() {
		if time.Now().After(deadline) {
			return errors.New("Xvnc did not start listening on port 5900")
		}
		time.Sleep(100 * time.Millisecond)
	}
	d.ensureBus()
	if _, err := exec.LookPath("startxfce4"); err == nil {
		s := d.userCmd("startxfce4")
		if err := s.Start(); err != nil {
			return err
		}
		go func() { _ = s.Wait() }()
	}
	d.mu.Lock()
	d.wallState = "" // re-apply the wallpaper to the new session
	d.mu.Unlock()
	return nil
}

// ensureBus picks a session bus: the user's systemd bus when it exists,
// otherwise a private one from dbus-launch.
func (d *desktop) ensureBus() {
	if !d.a.unpriv {
		sock := "/run/user/" + strconv.Itoa(d.a.uid) + "/bus"
		if _, err := os.Stat(sock); err == nil {
			d.mu.Lock()
			d.busAddr = "unix:path=" + sock
			d.mu.Unlock()
			return
		}
	}
	if _, err := exec.LookPath("dbus-launch"); err != nil {
		return
	}
	out, err := d.userCmd("dbus-launch").Output()
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "DBUS_SESSION_BUS_ADDRESS="); ok {
			d.mu.Lock()
			d.busAddr = strings.Trim(v, "'\";")
			d.mu.Unlock()
		}
	}
}

func (d *desktop) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := d.start(); err != nil {
		if errors.Is(err, errDesktopMissing) {
			writeErr(w, 409, "desktop_not_installed", err.Error())
			return
		}
		writeErr(w, 500, "desktop_failed", err.Error())
		return
	}
	wd, ht := d.size()
	d.a.Emit(EvDesktopStarted, map[string]any{"port": desktopVNCPort, "display": desktopDisplay})
	go d.applyWallpaper(false)
	ok(w, "desktop.started", map[string]any{"port": desktopVNCPort, "display": desktopDisplay, "width": wd, "height": ht})
}

func (d *desktop) handleStatus(w http.ResponseWriter, r *http.Request) {
	wd, ht := d.size()
	_, err := exec.LookPath("Xvnc")
	ok(w, "desktop.status", map[string]any{"installed": err == nil, "running": d.running(),
		"port": desktopVNCPort, "display": desktopDisplay, "width": wd, "height": ht, "recording": d.recInfo()})
}

func (d *desktop) needRunning(w http.ResponseWriter) bool {
	if d.running() {
		return true
	}
	writeErr(w, 409, "desktop_not_running", "the desktop is not running; POST /v1/internal/desktop/start first")
	return false
}

// GET /v1/internal/desktop/screenshot -> image/png of the whole screen.
func (d *desktop) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if !d.needRunning(w) {
		return
	}
	var out []byte
	var err error
	if _, e := exec.LookPath("ffmpeg"); e == nil {
		out, err = d.userCmd("ffmpeg", "-loglevel", "error", "-f", "x11grab", "-i", desktopDisplay,
			"-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "-").Output()
	} else if _, e := exec.LookPath("import"); e == nil {
		out, err = d.userCmd("import", "-display", desktopDisplay, "-window", "root", "png:-").Output()
	} else {
		writeErr(w, 409, "screenshot_unavailable", "neither ffmpeg nor ImageMagick import is installed")
		return
	}
	if err != nil || len(out) == 0 {
		writeErr(w, 500, "screenshot_failed", fmt.Sprintf("screenshot failed: %v", err))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(out)
}

// InputAction is one step of POST /v1/internal/desktop/input.
type InputAction struct {
	Type      string `json:"type"` // move | click | double_click | mouse_down | mouse_up | drag | scroll | type | key | wait
	X         *int   `json:"x,omitempty"`
	Y         *int   `json:"y,omitempty"`
	ToX       *int   `json:"toX,omitempty"`
	ToY       *int   `json:"toY,omitempty"`
	Button    string `json:"button,omitempty"`    // left | middle | right
	Count     int    `json:"count,omitempty"`     // click repeat
	Direction string `json:"direction,omitempty"` // up | down | left | right
	Amount    int    `json:"amount,omitempty"`    // scroll clicks, default 3
	Text      string `json:"text,omitempty"`
	Keys      string `json:"keys,omitempty"` // xdotool key syntax, e.g. "ctrl+c Return"
	DelayMs   int    `json:"delayMs,omitempty"`
	Ms        int    `json:"ms,omitempty"`
}

func buttonNum(b string) (string, error) {
	switch b {
	case "", "left":
		return "1", nil
	case "middle":
		return "2", nil
	case "right":
		return "3", nil
	}
	return "", fmt.Errorf("unknown button %q", b)
}

// xdotoolArgs converts one action to xdotool arguments (nil for wait).
func xdotoolArgs(act InputAction) ([]string, error) {
	var args []string
	moveTo := func(x, y *int, required bool) error {
		if x == nil || y == nil {
			if required {
				return errors.New(act.Type + " needs x and y")
			}
			return nil
		}
		args = append(args, "mousemove", "--sync", strconv.Itoa(*x), strconv.Itoa(*y))
		return nil
	}
	switch act.Type {
	case "move":
		if err := moveTo(act.X, act.Y, true); err != nil {
			return nil, err
		}
	case "click", "double_click":
		btn, err := buttonNum(act.Button)
		if err != nil {
			return nil, err
		}
		if err := moveTo(act.X, act.Y, false); err != nil {
			return nil, err
		}
		n := act.Count
		if act.Type == "double_click" {
			n = 2
		}
		if n < 1 {
			n = 1
		}
		args = append(args, "click", "--repeat", strconv.Itoa(n), btn)
	case "mouse_down", "mouse_up":
		btn, err := buttonNum(act.Button)
		if err != nil {
			return nil, err
		}
		if err := moveTo(act.X, act.Y, false); err != nil {
			return nil, err
		}
		args = append(args, strings.ReplaceAll(act.Type, "_", ""), btn)
	case "drag":
		btn, err := buttonNum(act.Button)
		if err != nil {
			return nil, err
		}
		if act.ToX == nil || act.ToY == nil {
			return nil, errors.New("drag needs toX and toY")
		}
		if err := moveTo(act.X, act.Y, true); err != nil {
			return nil, err
		}
		args = append(args, "mousedown", btn, "mousemove", "--sync", strconv.Itoa(*act.ToX), strconv.Itoa(*act.ToY), "mouseup", btn)
	case "scroll":
		btn := map[string]string{"up": "4", "down": "5", "left": "6", "right": "7"}[act.Direction]
		if btn == "" {
			return nil, errors.New("scroll direction must be up, down, left or right")
		}
		if err := moveTo(act.X, act.Y, false); err != nil {
			return nil, err
		}
		n := act.Amount
		if n < 1 {
			n = 3
		}
		args = append(args, "click", "--repeat", strconv.Itoa(n), btn)
	case "type":
		if act.Text == "" {
			return nil, errors.New("type needs text")
		}
		delay := act.DelayMs
		if delay <= 0 {
			delay = 12
		}
		args = append(args, "type", "--delay", strconv.Itoa(delay), "--", act.Text)
	case "key":
		keys := strings.Fields(act.Keys)
		if len(keys) == 0 {
			return nil, errors.New("key needs keys")
		}
		args = append(append(args, "key", "--"), keys...)
	case "wait":
		if act.Ms < 0 || act.Ms > 60000 {
			return nil, errors.New("wait ms must be in 0-60000")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown action type %q", act.Type)
	}
	return args, nil
}

// POST /v1/internal/desktop/input {"actions":[...]}
func (d *desktop) handleInput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Actions []InputAction `json:"actions"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Actions) == 0 || len(req.Actions) > 500 {
		writeErr(w, 400, "invalid_actions", "actions must hold 1-500 entries")
		return
	}
	plan := make([][]string, len(req.Actions))
	for i, act := range req.Actions {
		args, err := xdotoolArgs(act)
		if err != nil {
			writeErrDetails(w, 400, "invalid_action", err.Error(), map[string]any{"index": i})
			return
		}
		plan[i] = args
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		writeErr(w, 409, "xdotool_not_installed", "xdotool is not installed in this image")
		return
	}
	if !d.needRunning(w) {
		return
	}
	for i, args := range plan {
		if args == nil {
			time.Sleep(time.Duration(req.Actions[i].Ms) * time.Millisecond)
			continue
		}
		if out, err := d.userCmd("xdotool", args...).CombinedOutput(); err != nil {
			writeErrDetails(w, 500, "input_failed", strings.TrimSpace(string(out)), map[string]any{"index": i, "performed": i})
			return
		}
	}
	ok(w, "desktop.input", map[string]any{"performed": len(plan)})
}

// ---------------------------------------------------------------------------
// Recording (ffmpeg x11grab -> MP4)
// ---------------------------------------------------------------------------

func (d *desktop) recInfo() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rec == nil {
		return map[string]any{"recording": false}
	}
	return map[string]any{"recording": true, "path": d.recPath, "startedAt": d.recStart,
		"durationSeconds": int(time.Since(d.recStart).Seconds())}
}

// POST /v1/internal/desktop/recording/start {"path":"demo.mp4","fps":30}
func (d *desktop) handleRecStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		FPS  int    `json:"fps"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	if req.Path == "" {
		req.Path = "recording-" + time.Now().UTC().Format("20060102-150405") + ".mp4"
	}
	if req.FPS <= 0 || req.FPS > 60 {
		req.FPS = 30
	}
	real, virt, err := d.a.resolve(req.Path, true)
	if err != nil {
		writeErr(w, 400, "invalid_path", err.Error())
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		writeErr(w, 409, "ffmpeg_not_installed", "ffmpeg is not installed in this image")
		return
	}
	if !d.needRunning(w) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rec != nil {
		writeErrDetails(w, 409, "recording_active", "a recording already runs", map[string]any{"path": d.recPath})
		return
	}
	if err := d.a.mkdirAll(filepath.Dir(real), 0o755); err != nil {
		d.a.fsErr(w, err)
		return
	}
	wd, ht := d.size()
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "x11grab", "-framerate", strconv.Itoa(req.FPS),
		"-video_size", fmt.Sprintf("%dx%d", wd, ht), "-i", desktopDisplay,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", real)
	cmd.Env = d.a.commandEnv(map[string]string{"DISPLAY": desktopDisplay})
	cmd.Dir = d.a.home
	cmd.SysProcAttr = d.a.sysProcAttr()
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = d.a.spawn(cmd, "desktop")
	}
	if err != nil {
		writeErr(w, 500, "recording_failed", err.Error())
		return
	}
	done := make(chan struct{})
	d.rec, d.recStdin, d.recPath, d.recStart, d.recDone = cmd, stdin, virt, time.Now().UTC(), done
	go func() {
		_ = cmd.Wait()
		close(done)
		d.mu.Lock()
		if d.rec == cmd {
			d.rec = nil
		}
		d.mu.Unlock()
	}()
	d.a.Emit(EvRecordingStatus, map[string]any{"recording": true, "path": virt})
	ok(w, "desktop.recording_started", map[string]any{"path": virt, "fps": req.FPS, "startedAt": d.recStart})
}

// stopRecording finalizes the MP4 (ffmpeg "q"), then SIGINT/SIGKILL.
func (d *desktop) stopRecording() (string, time.Duration, bool) {
	d.mu.Lock()
	cmd, stdin, path, start, done := d.rec, d.recStdin, d.recPath, d.recStart, d.recDone
	d.rec = nil
	d.mu.Unlock()
	if cmd == nil {
		return "", 0, false
	}
	_, _ = stdin.Write([]byte("q"))
	_ = stdin.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	d.a.Emit(EvRecordingStatus, map[string]any{"recording": false, "path": path})
	return path, time.Since(start), true
}

func (d *desktop) handleRecStop(w http.ResponseWriter, r *http.Request) {
	path, dur, was := d.stopRecording()
	if !was {
		writeErr(w, 409, "not_recording", "no recording runs")
		return
	}
	ok(w, "desktop.recording_stopped", map[string]any{"path": path, "durationSeconds": int(dur.Seconds()),
		"sizeBytes": fileSize(d.a.real(path))})
}

func (d *desktop) handleRecStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, "desktop.recording", d.recInfo())
}

func (d *desktop) shutdown() { d.stopRecording() }

// ---------------------------------------------------------------------------
// Wallpaper: ~/.config/crowdvm/wallpaper.png (or Boat's ~/.config/boat/wallpaper.png)
// ---------------------------------------------------------------------------

func (d *desktop) wallpaperPaths() []string {
	return []string{filepath.Join(d.a.home, ".config/crowdvm/wallpaper.png"), filepath.Join(d.a.home, ".config/boat/wallpaper.png")}
}

// validPNG enforces Boat's limits: PNG, <= 16 MiB, <= 8192 px per side,
// <= 16,777,216 px total.
func validPNG(b []byte) error {
	if len(b) > maxWallpaper {
		return errors.New("wallpaper is larger than 16 MiB")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return errors.New("wallpaper is not a valid PNG")
	}
	if cfg.Width > 8192 || cfg.Height > 8192 || cfg.Width*cfg.Height > 16777216 {
		return errors.New("wallpaper is larger than 8192 px per side or 16,777,216 px in total")
	}
	return nil
}

// currentWallpaper returns the first valid wallpaper file ("" = default).
func (d *desktop) currentWallpaper() (string, string) {
	for _, p := range d.wallpaperPaths() {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > maxWallpaper {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil || validPNG(b) != nil {
			continue
		}
		return p, fmt.Sprintf("%s:%d:%d", p, fi.Size(), fi.ModTime().UnixNano())
	}
	return "", "default"
}

// applyWallpaper sets the Xfce backdrop when the file changed (or force).
func (d *desktop) applyWallpaper(force bool) {
	if !d.running() {
		return
	}
	if _, err := exec.LookPath("xfconf-query"); err != nil {
		return
	}
	path, state := d.currentWallpaper()
	d.mu.Lock()
	same := d.wallState == state
	d.wallState = state
	d.mu.Unlock()
	if same && !force {
		return
	}
	d.adoptSessionBus()
	out, _ := d.userCmd("xfconf-query", "-c", "xfce4-desktop", "-l").Output()
	var props []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), "/last-image") {
			props = append(props, strings.TrimSpace(l))
		}
	}
	if len(props) == 0 && path != "" {
		props = []string{"/backdrop/screen0/monitorVNC-0/workspace0/last-image"}
	}
	for _, p := range props {
		style := strings.TrimSuffix(p, "last-image") + "image-style"
		if path == "" {
			_ = d.userCmd("xfconf-query", "-c", "xfce4-desktop", "-p", p, "-r").Run()
			continue
		}
		_ = d.userCmd("xfconf-query", "-c", "xfce4-desktop", "-p", p, "-n", "-t", "string", "-s", path).Run()
		_ = d.userCmd("xfconf-query", "-c", "xfce4-desktop", "-p", style, "-n", "-t", "int", "-s", "5").Run() // zoomed
	}
}

// watchWallpaper polls the wallpaper files while the desktop runs, so a file
// copied in shows at once.
func (d *desktop) watchWallpaper(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.applyWallpaper(false)
		}
	}
}

// POST /v1/internal/desktop/wallpaper {"content":"<base64 PNG>"} or {"reset":true}
func (d *desktop) handleWallpaper(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
		Reset   bool   `json:"reset"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p := d.wallpaperPaths()[0]
	if req.Reset {
		for _, f := range d.wallpaperPaths() {
			_ = os.Remove(f)
		}
	} else {
		b, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			writeErr(w, 400, "invalid_content", "content must be base64")
			return
		}
		if err := validPNG(b); err != nil {
			writeErr(w, 400, "invalid_wallpaper", err.Error())
			return
		}
		if err := d.a.writeUserFile(p, bytes.NewReader(b)); err != nil {
			d.a.fsErr(w, err)
			return
		}
	}
	d.applyWallpaper(true)
	cur, _ := d.currentWallpaper()
	res := map[string]any{"path": nil, "applied": d.running()}
	if cur != "" {
		res["path"] = d.a.toVirt(cur)
	}
	ok(w, "desktop.wallpaper", res)
}

// adoptSessionBus uses the D-Bus session the running Xfce session is on.
// An image's desktop script may start Xfce under its own dbus-launch, and
// xfconf settings (the wallpaper) only reach the desktop on that bus.
func (d *desktop) adoptSessionBus() {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		if c := strings.TrimSpace(string(comm)); c != "xfdesktop" && c != "xfce4-session" {
			continue
		}
		if fi, err := os.Stat("/proc/" + e.Name()); err != nil || !d.a.unpriv && int(fi.Sys().(*syscall.Stat_t).Uid) != d.a.uid {
			continue
		}
		env, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		for _, kv := range strings.Split(string(env), "\x00") {
			if v, ok := strings.CutPrefix(kv, "DBUS_SESSION_BUS_ADDRESS="); ok && v != "" {
				d.mu.Lock()
				d.busAddr = v
				d.mu.Unlock()
				return
			}
		}
	}
}
