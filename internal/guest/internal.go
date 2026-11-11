package guest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func (a *Agent) routes() {
	m := a.mux
	// public (sandbox-scoped) endpoints
	m.HandleFunc("POST /v1/commands", a.handleCommand)
	m.HandleFunc("GET /v1/commands/{processId}", a.handleCommandStatus)
	m.HandleFunc("DELETE /v1/commands/{processId}", a.handleCommandKill)
	m.HandleFunc("GET /v1/commands/{processId}/stream", a.handleCommandAttach)
	m.HandleFunc("GET /v1/command-history", a.handleHistoryList)
	m.HandleFunc("GET /v1/command-history/{commandId}", a.handleHistoryGet)
	m.HandleFunc("GET /v1/command-history/{commandId}/logs", a.handleHistoryLogs)
	m.HandleFunc("GET /v1/processes", a.handleProcesses)
	m.HandleFunc("GET /v1/files", a.handleFileRead)
	m.HandleFunc("PUT /v1/files", a.handleFileWrite)
	m.HandleFunc("DELETE /v1/files", a.handleFileDelete)
	m.HandleFunc("GET /v1/files/list", a.handleFileList)
	m.HandleFunc("GET /v1/files/info", a.handleFileInfo)
	m.HandleFunc("GET /v1/files/search", a.handleFileSearch)
	m.HandleFunc("GET /v1/files/find", a.handleFileFind)
	m.HandleFunc("POST /v1/files/folder", a.handleFolder)
	m.HandleFunc("POST /v1/files/move", a.handleMove)
	m.HandleFunc("POST /v1/files/permissions", a.handlePermissions)
	m.HandleFunc("POST /v1/files/replace", a.handleReplace)
	m.HandleFunc("GET /v1/files/raw", a.handleRawGet)
	m.HandleFunc("PUT /v1/files/raw", a.handleRawPut)
	m.HandleFunc("GET /v1/artifacts", a.handleArtifact)
	m.HandleFunc("POST /v1/sshkey", a.handleSSHKey)
	// guest-internal endpoints (node only)
	m.HandleFunc("GET /v1/health", a.handleHealth)
	m.HandleFunc("GET /v1/tunnel", a.handleTunnel)
	m.HandleFunc("POST /v1/internal/prepare-stop", a.handlePrepareStop)
	m.HandleFunc("POST /v1/internal/sync", a.handleSync)
	m.HandleFunc("GET /v1/internal/events/stream", a.handleEventsStream)
	m.HandleFunc("GET /v1/internal/setup-status", a.handleSetupStatus)
	m.HandleFunc("POST /v1/internal/freeze", a.handleFreeze)
	m.HandleFunc("POST /v1/internal/thaw", a.handleThaw)
	m.HandleFunc("GET /v1/internal/scrub", a.handleScrub)
	m.HandleFunc("POST /v1/internal/apply-config", a.handleApplyConfig)
	m.HandleFunc("GET /v1/internal/history/export", a.handleHistoryExport)
	m.HandleFunc("POST /v1/internal/history/ack", a.handleHistoryAck)
	m.HandleFunc("POST /v1/internal/browser/start", a.handleBrowserStart)
	m.HandleFunc("GET /v1/internal/browser/status", a.handleBrowserStatus)
	m.HandleFunc("POST /v1/internal/playbook/finish", a.handlePlaybookFinish)
	m.HandleFunc("PUT /v1/internal/firewall", a.handleFirewall)
	a.desk.routes(m)
}

// GET /v1/health (no auth)
func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := a.setup.get()
	ok(w, "guest.health", map[string]any{
		"version": Version, "sandboxId": a.cfg.SandboxID, "boot": a.cfg.Boot,
		"uptimeSeconds": int(time.Since(a.started).Seconds()), "setupStatus": st.Status,
		"setupBlocking": st.Blocking, "monitoring": !a.hist.unmonitor, "unprivileged": a.unpriv,
		"reposCloning": a.reposCloning(), // LIFE-009: the node reports ready after the clones
	})
}

// GET /v1/tunnel?port=N  (Upgrade: crowdvm-tunnel) -> raw TCP to 127.0.0.1:N
func (a *Agent) handleTunnel(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, 400, "invalid_port", "port must be in 1-65535")
		return
	}
	if port == 22 {
		a.waitSSHReady(r.Context()) // host keys are regenerated in the background at boot
	}
	// 127.0.0.1, then ::1 for apps that bind only IPv6 loopback (URL-007)
	target, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		target, err = net.DialTimeout("tcp", net.JoinHostPort("::1", strconv.Itoa(port)), 2*time.Second)
	}
	if err != nil {
		writeErr(w, 502, "port_unreachable", "nothing is listening on port "+strconv.Itoa(port)+" in the sandbox")
		return
	}
	conn, rw, err := mux.HijackUpgrade(w)
	if err != nil {
		target.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	mux.Join(&hijacked{Conn: conn, r: rw.Reader}, target)
}

// hijacked reads through the bufio.Reader that may hold early client bytes.
type hijacked struct {
	net.Conn
	r *bufio.Reader
}

func (h *hijacked) Read(p []byte) (int, error) { return h.r.Read(p) }

// POST /v1/internal/prepare-stop [proto.PrepareStopRequest]: Boat's stop
// order (SNAP-005/006, RUN-005): the user's pre-stop hook, then `docker
// stop` (30 s per container), then delete what a snapshot does not keep,
// fstrim, sync and (freeze) fsfreeze. Force skips the hook and Docker.
func (a *Agent) handlePrepareStop(w http.ResponseWriter, r *http.Request) {
	var req proto.PrepareStopRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid_body", "invalid JSON body: "+err.Error())
			return
		}
	}
	res := map[string]any{"preStop": nil, "dockerStopped": 0}
	if !req.Force && !a.inspect() {
		if hr := a.runPreStop(r.Context()); hr != nil {
			res["preStop"] = hr
		}
		res["dockerStopped"] = a.stopDocker(r.Context()) // records them for the next boot first
	} else if !a.inspect() {
		a.recordDocker(r.Context(), 2*time.Second) // no docker stop, but they start again after the boot
	}
	a.desk.stopRecording()
	a.markCleanShutdown()
	var removed []string
	var errs []string
	skip := map[string]bool{a.data: true, filepath.Join(a.home, ".cache"): true}
	removed = append(removed, cleanIgnored(a.home, skip)...)
	for _, dir := range []string{filepath.Join(a.home, ".cache"), a.tmp} {
		removed = append(removed, a.emptyDir(dir, &errs)...)
	}
	if !a.unpriv {
		a.runQuiet(&errs, "journalctl", "--rotate")
		a.runQuiet(&errs, "journalctl", "--vacuum-time=1s")
	}
	// Boat keeps only images a container or a Compose project uses (SNAP-011)
	a.pruneDocker(r.Context(), !req.Force, &errs)
	if !a.unpriv {
		a.runQuiet(&errs, "fstrim", "-av")
	}
	syscall.Sync()
	paths := make([]string, 0, len(removed))
	for _, p := range removed {
		paths = append(paths, a.toVirt(p))
	}
	res["removed"], res["removedPaths"] = len(paths), paths
	if len(paths) > 200 {
		res["removedPaths"] = paths[:200]
	}
	res["frozen"] = false
	if req.Freeze && !a.unpriv {
		mounts, err := a.frz.freeze(a, req.FreezeTimeoutSeconds)
		if err != nil {
			errs = append(errs, "fsfreeze: "+err.Error())
		}
		res["frozen"], res["frozenMounts"] = len(mounts) > 0, mounts
	}
	if len(errs) > 0 {
		res["errors"] = errs
	}
	ok(w, "guest.prepared_stop", res)
}

const preStopLimit = 60 * time.Second

// runPreStop runs ~/.crowdvm/pre-stop (else ~/.boat/pre-stop) as the user,
// in the home folder, for at most 60 s. Its failure never stops the stop.
// Output goes to /tmp/routine-logs/pre-stop.log (deleted with /tmp).
func (a *Agent) runPreStop(ctx context.Context) map[string]any {
	var hook string
	for _, p := range []string{".crowdvm/pre-stop", ".boat/pre-stop"} {
		if fi, err := os.Stat(filepath.Join(a.home, p)); err == nil && fi.Mode().IsRegular() {
			hook = filepath.Join(a.home, p)
			break
		}
	}
	if hook == "" {
		return nil
	}
	start := time.Now()
	out := map[string]any{"path": a.toVirt(hook)}
	cmdline := shQuote(hook)
	if fi, _ := os.Stat(hook); fi.Mode()&0o111 == 0 {
		cmdline = "bash " + cmdline // not executable: run it as a bash script
	}
	cmd := a.shellCmd(cmdline, a.home, nil)
	logf, _ := os.CreateTemp("", "crowdvm-pre-stop-*.log")
	if logf != nil {
		defer os.Remove(logf.Name())
		defer logf.Close()
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := a.spawn(cmd, "setup"); err != nil {
		out["error"] = err.Error()
		return out
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(preStopLimit):
		out["timedOut"] = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		out["error"] = err.Error()
	}
	out["exitCode"], out["durationMs"] = code, time.Since(start).Milliseconds()
	if code != 0 && logf != nil {
		tail, _ := readTail(logf.Name(), 1024)
		out["outputTail"] = string(tail)
	}
	return out
}

// emptyDir removes the contents of dir (not dir itself).
func (a *Agent) emptyDir(dir string, errs *[]string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			*errs = append(*errs, a.scrub(err.Error()))
			continue
		}
		out = append(out, p)
	}
	return out
}

func (a *Agent) runQuiet(errs *[]string, name string, args ...string) {
	if _, err := exec.LookPath(name); err != nil {
		return
	}
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		*errs = append(*errs, name+" "+strings.Join(args, " ")+": "+strings.TrimSpace(lastLines(string(out), 3)))
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// POST /v1/internal/sync: flush filesystems before a live snapshot.
func (a *Agent) handleSync(w http.ResponseWriter, r *http.Request) {
	syscall.Sync()
	ok(w, "guest.synced", nil)
}

// POST /v1/internal/freeze [proto.FreezeRequest]: sync + FIFREEZE every
// writable block filesystem (live snapshot); auto-thaw after the timeout.
func (a *Agent) handleFreeze(w http.ResponseWriter, r *http.Request) {
	var req proto.FreezeRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid_body", "invalid JSON body: "+err.Error())
			return
		}
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 600 {
		writeErr(w, 400, "invalid_timeout", "timeoutSeconds must be in 0-600")
		return
	}
	if !a.inspect() {
		a.recordDocker(r.Context(), 2*time.Second) // a fork or deploy from this snapshot starts them again
	}
	syscall.Sync()
	if a.unpriv {
		ok(w, "guest.frozen", map[string]any{"mounts": []string{}})
		return
	}
	mounts, err := a.frz.freeze(a, req.TimeoutSeconds)
	if err != nil {
		writeErr(w, 500, "freeze_failed", err.Error())
		return
	}
	ok(w, "guest.frozen", map[string]any{"mounts": mounts})
}

// POST /v1/internal/thaw (idempotent)
func (a *Agent) handleThaw(w http.ResponseWriter, r *http.Request) {
	mounts := a.frz.thaw()
	if a.unmarkCleanShutdown() { // a failed stop: the machine runs on (Boat: the sandbox stays usable)
		go a.restartDocker(context.WithoutCancel(r.Context())) // the containers prepare-stop stopped
	}
	ok(w, "guest.thawed", map[string]any{"mounts": mounts})
}

// GET /v1/internal/setup-status
func (a *Agent) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, "setup.status", a.setup.get())
}

// POST /v1/sshkey
func (a *Agent) handleSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.Contains(req.Key, "PRIVATE KEY") {
		writeErr(w, 400, "invalid_key", "private keys are rejected; send the public key")
		return
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.Key)); err != nil {
		writeErr(w, 400, "invalid_key", "not an OpenSSH public key")
		return
	}
	if err := a.addAuthorizedKeys([]string{req.Key}); err != nil {
		writeErr(w, 500, "ssh_key_failed", err.Error())
		return
	}
	a.waitSSHReady(r.Context())
	res := map[string]any{"success": true, "machineIp": nil, "sshUser": a.userName, "sshEndpoint": nil}
	if hk := hostKey(); hk != "" && !a.unpriv {
		res["hostKey"] = hk
	}
	ok(w, "ssh_key.configured", res)
}

// addAuthorizedKeys appends keys not yet present to ~/.ssh/authorized_keys.
func (a *Agent) addAuthorizedKeys(keys []string) error {
	dir := filepath.Join(a.home, ".ssh")
	if err := a.mkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	p := filepath.Join(dir, "authorized_keys")
	cur, _ := os.ReadFile(p)
	have := map[string]bool{}
	for _, l := range strings.Split(string(cur), "\n") {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l)); err == nil {
			have[string(pk.Marshal())] = true
		}
	}
	out := string(cur)
	added := false
	for _, k := range keys {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
		if err != nil || have[string(pk.Marshal())] {
			continue
		}
		have[string(pk.Marshal())] = true
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += strings.TrimSpace(k) + "\n"
		added = true
	}
	if !added {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(out)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	a.chown(p)
	return err
}

func hostKey() string {
	for _, t := range []string{"ed25519", "ecdsa", "rsa"} {
		if b, err := os.ReadFile("/etc/ssh/ssh_host_" + t + "_key.pub"); err == nil {
			f := strings.Fields(string(b))
			if len(f) >= 2 {
				return f[0] + " " + f[1]
			}
		}
	}
	return ""
}
