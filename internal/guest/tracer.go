package guest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// tracer records the output of the sandbox's other processes as Boat
// `process` history records (CMD-021/022) from three sources:
//   - the systemd journal (`journalctl -f -o json`): the stdout/stderr of
//     every systemd service (system and user units), syslog and journal
//     writes, and Docker container output when the daemon logs to journald;
//   - Docker's default json-file logs (followDocker);
//   - an eBPF write(2)/writev(2) tracer (tracer_bpf.go) for what ssh
//     sessions (with or without a tty) and processes started by integrated
//     agents write to a tty, a pipe or a socket.
//
// One record per process (or per container), 64 KiB/s per record with
// 1 MiB bursts (excess dropped, outputTruncated), binary output skipped,
// platform services and exec'd commands excluded. Output written only to
// files is not captured. The server pulls it every ~30 s via history/export.

const (
	traceRate      = 64 << 10
	traceBurst     = 1 << 20
	traceMaxActive = 2000
)

type tracer struct {
	a    *Agent
	mu   sync.Mutex
	recs map[string]*traced // key: pid:<n> or ctr:<id>
}

type traced struct {
	m      *meta
	pid    int
	tokens float64
	last   time.Time
	seen   time.Time
}

func newTracer(a *Agent) *tracer { return &tracer{a: a, recs: map[string]*traced{}} }

// Platform services (the platform's own, Boat excludes them) and noise.
var tracePlatformUnits = []string{"crowdvm-", "systemd-", "ssh.", "sshd", "dbus", "cron", "polkit", "rsyslog", "getty@",
	"serial-getty@", "snapd", "unattended-upgrades", "ModemManager", "multipathd", "networkd-dispatcher", "udisks2",
	"ufw", "apparmor", "fwupd", "packagekit", "accounts-daemon", "thermald", "irqbalance", "init.scope", "user@",
	"user-runtime-dir@", "containerd", "docker.service", "docker.socket", "cloud-", "lvm2", "chrony", "qemu-guest-agent",
	"e2scrub", "fstrim", "logrotate", "man-db", "apt-", "motd-news", "plymouth", "keyboard-setup", "console-setup",
	"blk-availability", "finalrd", "grub-", "secureboot", "ua-", "ubuntu-advantage", "upower", "wpa_supplicant",
	"NetworkManager", "avahi", "cups", "colord", "rtkit", "lightdm", "gdm", "xrdp", "cua-driver", "pipewire",
	"wireplumber", "xdg-", "gvfs", "at-spi", "pulseaudio", "gpg-agent", "tracker-", "evolution-", "dirmngr", "ssh-agent",
	"session-", "kmod", "setvtrgb", "ssl-cert", "pollinate", "rc-local", "dmesg"}

var traceSkipIdent = map[string]bool{"sshd": true, "sshd-session": true, "sudo": true, "su": true, "systemd": true,
	"systemd-logind": true, "(sd-pam)": true, "CRON": true, "cron": true, "kernel": true, "crowdvm-guest": true,
	"audit": true, "dbus-daemon": true, "polkitd": true, "ufw": true, "Xvnc": true}

func platformUnit(u string) bool {
	for _, p := range tracePlatformUnits {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func (t *tracer) run(stop <-chan struct{}) {
	if t.a.unpriv || os.Geteuid() != 0 {
		return
	}
	go t.reaper(stop)
	go t.followWrites(stop)
	go t.followDocker(stop, dockerDir)
	if _, err := exec.LookPath("journalctl"); err != nil {
		return
	}
	for {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		t.follow(ctx)
		cancel()
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second): // journald restarted (e.g. new machine-id)
		}
	}
}

func (t *tracer) follow(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "journalctl", "-f", "-o", "json", "-n", "0", "--no-pager")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // dies with the agent
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			t.entry(e)
		}
	}
	_ = cmd.Wait()
}

func str(e map[string]any, k string) string { s, _ := e[k].(string); return s }

// entry handles one journal entry.
func (t *tracer) entry(e map[string]any) {
	if t.a.hist.unmonitor {
		return
	}
	msg, isText := e["MESSAGE"].(string) // binary messages arrive as byte arrays
	if !isText || msg == "" {
		return
	}
	transport := str(e, "_TRANSPORT")
	if transport == "kernel" || transport == "audit" || transport == "driver" {
		return
	}
	pid, _ := strconv.Atoi(str(e, "_PID"))
	unit, userUnit := str(e, "_SYSTEMD_UNIT"), str(e, "_SYSTEMD_USER_UNIT")
	cgroup := str(e, "_SYSTEMD_CGROUP")
	key, command := "", ""
	if ctr := str(e, "CONTAINER_ID_FULL"); ctr != "" || str(e, "CONTAINER_ID") != "" {
		if ctr == "" {
			ctr = str(e, "CONTAINER_ID")
		}
		key = "ctr:" + ctr
		command = "docker container " + strings.TrimPrefix(str(e, "CONTAINER_NAME"), "/")
		if img := str(e, "IMAGE_NAME"); img != "" {
			command += " (" + img + ")"
		}
	} else {
		if pid <= 1 || traceSkipIdent[str(e, "SYSLOG_IDENTIFIER")] || traceSkipIdent[str(e, "_COMM")] {
			return
		}
		if userUnit != "" {
			if platformUnit(userUnit) {
				return
			}
		} else if unit == "" || platformUnit(unit) {
			return
		}
		// CMD-022: exec'd commands (and their children) are already commands.
		if strings.Contains(cgroup, "/"+workloadsUnit+"/cmd_") || t.a.cmdSession(pid) {
			return
		}
		key = "pid:" + strconv.Itoa(pid)
		command = str(e, "_CMDLINE")
		if command == "" {
			command = str(e, "_COMM")
		}
		if command == "" {
			command = unit
		}
	}
	stream := "stdout"
	if p, err := strconv.Atoi(str(e, "PRIORITY")); err == nil && p <= 3 {
		stream = "stderr"
	}
	t.write(key, pid, command, stream, msg+"\n")
}

// cmdSession reports whether pid belongs to a running exec'd command
// (session/process group, for commands started without a cgroup).
func (a *Agent) cmdSession(pid int) bool {
	st, err := readStat(pid)
	if err != nil {
		return false
	}
	for _, m := range a.hist.all() {
		if m.Status == "running" && m.Mode != "process" && m.PID > 0 && (m.PID == st.session || m.PID == st.pgrp) {
			return true
		}
	}
	return false
}

func (t *tracer) write(key string, pid int, command, stream, data string) {
	now := time.Now()
	t.mu.Lock()
	tr := t.recs[key]
	if tr == nil {
		if len(t.recs) >= traceMaxActive {
			t.mu.Unlock()
			return
		}
		if len(command) > maxCommandText {
			command = command[:maxCommandText]
		}
		m := &meta{CommandRecord: CommandRecord{CommandID: newCommandID(), SandboxID: t.a.cfg.SandboxID, Command: command,
			Mode: "process", Status: "running", StartedAt: now.UTC()}, BootID: t.a.bootID}
		if pid > 0 && !strings.HasPrefix(key, "ctr:") {
			m.ProcessID, m.PID, m.PIDStart = ptr(pid), pid, pidStartTime(pid)
		}
		if err := t.a.hist.create(m); err != nil {
			t.mu.Unlock()
			return
		}
		tr = &traced{m: m, pid: pid, tokens: traceBurst, last: now}
		t.recs[key] = tr
	}
	tr.seen = now
	tr.tokens = min(traceBurst, tr.tokens+now.Sub(tr.last).Seconds()*traceRate)
	tr.last = now
	allowed := float64(len(data)) <= tr.tokens
	if allowed {
		tr.tokens -= float64(len(data))
	}
	m := tr.m
	t.mu.Unlock()
	if !allowed {
		if !m.OutputTruncated {
			t.a.hist.update(m, func(m *meta) { m.OutputTruncated = true })
		}
		return
	}
	f, err := os.OpenFile(t.a.hist.path(m.CommandID, stream), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(data)
	f.Close()
}

// reaper finishes records whose process is gone (containers: no output for
// 10 minutes and not running).
func (t *tracer) reaper(stop <-chan struct{}) {
	tk := time.NewTicker(10 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		t.reap(time.Now())
	}
}

func (t *tracer) reap(now time.Time) {
	// Only query Docker when a quiet container needs checking. A daemon
	// failure is not evidence that the container exited.
	needsDocker := false
	t.mu.Lock()
	for k, tr := range t.recs {
		if strings.HasPrefix(k, "ctr:") && now.Sub(tr.seen) > 10*time.Minute {
			needsDocker = true
			break
		}
	}
	t.mu.Unlock()
	running := map[string]bool{}
	known := false
	if needsDocker && t.a.dockerOK() {
		out, err := t.a.docker(context.Background(), 2*time.Second, "ps", "-q", "--no-trunc")
		known = err == nil
		for _, id := range strings.Fields(string(out)) {
			running["ctr:"+id] = true
			running["ctr:"+id[:min(12, len(id))]] = true // journald may only report the short id
		}
	}
	var ended []*traced
	t.mu.Lock()
	for k, tr := range t.recs {
		gone := false
		if tr.pid > 0 && !strings.HasPrefix(k, "ctr:") {
			gone = !pidAlive(tr.pid, tr.m.PIDStart)
		} else {
			gone = now.Sub(tr.seen) > 10*time.Minute
			if strings.HasPrefix(k, "ctr:") {
				gone = gone && known && !running[k]
			}
		}
		if gone {
			ended = append(ended, tr)
			delete(t.recs, k)
		}
	}
	t.mu.Unlock()
	for _, tr := range ended {
		t.finished(tr, nil)
	}
}

// finished ends a record; ws is the exit status when the kernel reported it
// (exit code or signal, like an exec'd command's).
func (t *tracer) finished(tr *traced, ws *syscall.WaitStatus) {
	t.a.hist.update(tr.m, func(m *meta) {
		m.Status = "exited"
		m.FinishedAt = ptr(time.Now().UTC())
		switch {
		case ws == nil:
		case ws.Signaled():
			m.Signal = ptr(unix.SignalName(ws.Signal()))
			m.Status = "killed"
		default:
			m.ExitCode = ptr(ws.ExitStatus())
		}
	})
}

// ---------------------------------------------------------------------------
// Docker json-file logs (the default log driver)
// ---------------------------------------------------------------------------

var (
	dockerDir      = "/var/lib/docker/containers"
	dockerPoll     = time.Second
	dockerMaxChunk = 1 << 20
)

type dockerLog struct {
	key, command string
	off          int64
}

// followDocker tails /var/lib/docker/containers/<id>/<id>-json.log of every
// container (logs present when it starts are followed from their end, new
// containers from the start) as `ctr:<id>` process records, like journald
// containers.
func (t *tracer) followDocker(stop <-chan struct{}, dir string) {
	logs := map[string]*dockerLog{}
	first := true
	tk := time.NewTicker(dockerPoll)
	defer tk.Stop()
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "*", "*-json.log"))
		seen := map[string]bool{}
		for _, f := range files {
			seen[f] = true
			l := logs[f]
			if l == nil {
				id := filepath.Base(filepath.Dir(f))
				l = &dockerLog{key: "ctr:" + id, command: dockerCommand(filepath.Dir(f), id)}
				if first {
					l.off = fileSize(f)
				}
				logs[f] = l
			}
			t.readDockerLog(f, l)
		}
		for f := range logs {
			if !seen[f] {
				delete(logs, f) // container removed
			}
		}
		first = false
		select {
		case <-stop:
			return
		case <-tk.C:
		}
	}
}

// dockerCommand names a container like the journald path does.
func dockerCommand(dir, id string) string {
	var c struct {
		Name   string
		Config struct{ Image string }
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.v2.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	name := strings.TrimPrefix(c.Name, "/")
	if name == "" {
		name = id[:min(12, len(id))]
	}
	cmd := "docker container " + name
	if c.Config.Image != "" {
		cmd += " (" + c.Config.Image + ")"
	}
	return cmd
}

func (t *tracer) readDockerLog(path string, l *dockerLog) {
	if t.a.hist.unmonitor {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() < l.off {
		l.off = 0 // rotated or truncated
	}
	if _, err := f.Seek(l.off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(io.LimitReader(f, int64(dockerMaxChunk)), 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // incomplete line: read it again next time
			return
		}
		l.off += int64(len(line))
		var e struct {
			Log    string `json:"log"`
			Stream string `json:"stream"`
		}
		if json.Unmarshal(line, &e) != nil || e.Log == "" || !utf8.ValidString(e.Log) {
			continue
		}
		stream := "stdout"
		if e.Stream == "stderr" {
			stream = "stderr"
		}
		t.write(l.key, 0, l.command, stream, e.Log)
	}
}
