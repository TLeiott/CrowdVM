package guest

import (
	"encoding/binary"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// fakeProcs builds a procClass over a fake process table.
type fakeProc struct {
	ppid         int
	comm, cgroup string
	cmdline      string
	stdout       string
}

func fakeClass(self int, procs map[int]fakeProc) *procClass {
	return &procClass{self: self, cache: map[int]procVerdict{}, args: map[int]string{},
		cgroup:     func(_ uint64, pid int) string { return procs[pid].cgroup },
		service:    func(s string) string { return map[string]string{"3": "sshd", "c1": "login"}[s] },
		ppid:       func(pid int) int { return procs[pid].ppid },
		comm:       func(pid int) string { return procs[pid].comm },
		cmdline:    func(pid int) string { return procs[pid].cmdline },
		stdout:     func(pid int) string { return procs[pid].stdout },
		guestPipes: func() map[string]bool { return map[string]bool{"pipe:[777]": true} },
	}
}

func TestProcClass(t *testing.T) {
	const self = 100
	wl := "/user.slice/crowdvm-workloads.service/"
	sess := "/user.slice/user-1000.slice/session-3.scope"
	procs := map[int]fakeProc{
		1:   {comm: "systemd", cgroup: "/init.scope"},
		100: {ppid: 1, comm: "crowdvm-guest", cgroup: "/system.slice/crowdvm-guest.service"},
		101: {ppid: 100, comm: "journalctl", cgroup: "/system.slice/crowdvm-guest.service"},
		// ssh: listener, session monitor, the user's shell and a setsid'd writer
		200: {ppid: 1, comm: "sshd", cgroup: "/system.slice/ssh.service"},
		201: {ppid: 200, comm: "sshd", cgroup: sess},
		202: {ppid: 201, comm: "bash", cgroup: sess, cmdline: "bash -c echo hi"},
		203: {ppid: 1, comm: "sh", cgroup: sess, cmdline: "sh -c while :; do date; done"},
		// no logind: still an sshd descendant
		210: {ppid: 200, comm: "sshd", cgroup: "/system.slice/ssh.service"},
		211: {ppid: 210, comm: "bash", cgroup: "/system.slice/ssh.service", cmdline: "bash"},
		// exec'd command and its background child
		300: {ppid: 100, comm: "bash", cgroup: wl + "cmd_abc", cmdline: "bash -lc make"},
		301: {ppid: 1, comm: "node", cgroup: wl + "cmd_abc", cmdline: "node server.js"},
		// harness (its stdout is the agent protocol) and what it started
		400: {ppid: 100, comm: "claude", cgroup: wl + "agents", cmdline: "claude -p"},
		401: {ppid: 400, comm: "bash", cgroup: wl + "agents", cmdline: "bash -c npm run dev"},
		402: {ppid: 1, comm: "node", cgroup: wl + "agents", cmdline: "node vite"},
		403: {ppid: 400, comm: "crowdvm-guest", cgroup: wl + "agents", cmdline: "crowdvm-guest mcp"},
		// the platform's computer-use MCP server, and codex's native binary
		// behind its node launcher (stdout = the guest's protocol pipe)
		404: {ppid: 400, comm: "cua-driver", cgroup: wl + "agents", cmdline: "/opt/crowdvm/cua-driver/cua-driver mcp --socket /run/x"},
		410: {ppid: 100, comm: "node", cgroup: wl + "agents", cmdline: "node /usr/local/bin/codex app-server"},
		411: {ppid: 410, comm: "codex", cgroup: wl + "agents", cmdline: "codex app-server", stdout: "pipe:[777]"},
		412: {ppid: 411, comm: "bash", cgroup: wl + "agents", cmdline: "bash -c ls", stdout: "pipe:[888]"},
		// setup script / desktop workloads
		500: {ppid: 100, comm: "bash", cgroup: wl + "setup_1"},
		// services, containers, user units
		600: {ppid: 1, comm: "nginx", cgroup: "/system.slice/nginx.service"},
		601: {ppid: 602, comm: "node", cgroup: "/system.slice/docker-0123abcd.scope"},
		602: {ppid: 1, comm: "containerd-shim", cgroup: "/system.slice/containerd.service"},
		603: {ppid: 1, comm: "pipewire", cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/pipewire.service"},
		// commands without workload cgroups (fallback): under the guest
		700: {ppid: 100, comm: "bash", cgroup: "/system.slice/crowdvm-guest.service"},
		701: {ppid: 700, comm: "python3", cgroup: "/system.slice/crowdvm-guest.service"},
		// sudo in a session relays its child's output
		800: {ppid: 202, comm: "sudo", cgroup: sess},
		// the desktop's login session: terminal output only
		900: {ppid: 1, comm: "xfce4-panel", cgroup: "/user.slice/user-1000.slice/session-c1.scope", cmdline: "xfce4-panel"},
	}
	want := map[int]int{202: modeAll, 203: modeAll, 211: modeAll, 401: modeAll, 402: modeAll, 412: modeAll, 900: modeTTY}
	c := fakeClass(self, procs)
	ev := func(pid int) procEvent { p := procs[pid]; return procEvent{pid: pid, ppid: p.ppid, comm: p.comm} }
	for pid := range procs {
		w, ok := want[pid]
		if !ok {
			w = modeSkip
		}
		v := c.classify(ev(pid))
		if v.mode != w {
			t.Errorf("pid %d (%s %s): mode %d, want %d", pid, procs[pid].comm, procs[pid].cgroup, v.mode, w)
		}
		if v.mode != modeSkip && v.command != procs[pid].cmdline {
			t.Errorf("pid %d command %q", pid, v.command)
		}
	}
	// gone before userspace looked: the kernel's comm and cgroup still decide
	procs[950] = fakeProc{cgroup: sess}
	c.args[950] = "bash -c echo bye"
	if v := c.classify(procEvent{pid: 950, ppid: 201, comm: "bash"}); v.mode != modeAll || v.command != "bash -c echo bye" {
		t.Errorf("exited process: %+v", v)
	}
	if v := c.classify(procEvent{pid: 999, comm: "x"}); v.mode != modeSkip {
		t.Errorf("unknown cgroup recorded: %+v", v)
	}
}

func TestWriteEvents(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	tr := e.a.tracer
	pid := os.Getpid() // a live pid, so the reaper keeps the record
	c := fakeClass(1, map[int]fakeProc{pid: {ppid: 1, comm: "bash", cgroup: "/user.slice/user-1000.slice/session-3.scope", cmdline: "bash -c tick"},
		pid + 1: {ppid: 1, comm: "nginx", cgroup: "/system.slice/nginx.service"}})
	modes := map[uint32]uint32{}
	var forgotten []uint32
	skip := func(p, m uint32) { modes[p] = m }
	forget := func(p uint32) { forgotten = append(forgotten, p) }
	evf := func(pid, fd, kind, flags int, data string) []byte {
		b := make([]byte, evHeader+len(data))
		le := binary.LittleEndian
		le.PutUint32(b, uint32(pid))
		le.PutUint32(b[8:], uint32(fd))
		le.PutUint32(b[12:], uint32(len(data)))
		le.PutUint32(b[16:], uint32(kind))
		le.PutUint32(b[20:], uint32(flags))
		copy(b[32:], "bash")
		copy(b[evHeader:], data)
		return b
	}
	ev := func(pid, fd, kind int, data string) []byte { return evf(pid, fd, kind, 2<<8, data) }
	tr.writeEvent(c, ev(pid, 1, evWrite, "tick 1\n"), skip, forget)
	tr.writeEvent(c, ev(pid, 2, evWrite, "oops\n"), skip, forget)
	tr.writeEvent(c, ev(pid, 1, evWrite, "bin\x00ary"), skip, forget)
	tr.writeEvent(c, ev(pid, 1, evWrite, "\xff\xfe"), skip, forget)
	tr.writeEvent(c, ev(pid, 1, evWrite, "cut \xe2\x82"), skip, forget) // rune cut at the 4 KiB limit
	tr.writeEvent(c, ev(pid+1, 1, evWrite, "service output\n"), skip, forget)
	if len(modes) != 1 || modes[uint32(pid+1)] != modeSkip {
		t.Fatalf("kernel modes %v", modes)
	}
	recs, _, _ := e.a.hist.list(map[string]bool{"process": true}, "", "", 100)
	if len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	r := recs[0]
	if r.Command != "bash -c tick" || r.ProcessID == nil || *r.ProcessID != pid || r.Status != "running" {
		t.Fatalf("record %+v", r)
	}
	out, _ := os.ReadFile(e.a.hist.path(r.CommandID, "stdout"))
	errOut, _ := os.ReadFile(e.a.hist.path(r.CommandID, "stderr"))
	if string(out) != "tick 1\ncut " || string(errOut) != "oops\n" {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
	// exec: classify again; exit: the record is finished at once
	tr.writeEvent(c, ev(pid, 0, evExec, ""), skip, forget)
	if _, ok := c.cache[pid]; ok || len(forgotten) != 1 {
		t.Fatalf("exec kept the verdict (forgotten %v)", forgotten)
	}
	tr.writeEvent(c, ev(pid, 3<<8, evExit, ""), skip, forget) // exit status in the fd field
	if r := e.a.hist.record(e.a.hist.get(r.CommandID)); r.Status != "exited" || r.ExitCode == nil || *r.ExitCode != 3 || r.Signal != nil {
		t.Fatalf("record not finished with exit code 3: %+v", r)
	}
	if r := e.a.hist.record(e.a.hist.get(r.CommandID)); r.OutputTruncated {
		t.Fatalf("truncated without kernel drops: %+v", r)
	}
	// pid reused: a new record; the kernel's rate limit dropped writes
	tr.writeEvent(c, evf(pid, 1, evWrite, 2<<8|fDropped, "x"), skip, forget)
	recs, _, _ = e.a.hist.list(map[string]bool{"process": true}, "", "", 100)
	if len(recs) != 2 {
		t.Fatalf("pid reuse after exit should start a new record: %+v", recs)
	}
	for _, r := range recs {
		if r.Status == "running" && !r.OutputTruncated {
			t.Fatalf("kernel drop not flagged: %+v", r)
		}
	}
}

func TestWriteEventsTTYOnly(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	tr := e.a.tracer
	pid := os.Getpid()
	c := fakeClass(1, map[int]fakeProc{pid: {ppid: 1, comm: "xfce4-terminal", cgroup: "/user.slice/user-1000.slice/session-c1.scope"}})
	modes := map[uint32]uint32{}
	ev := func(kind, fdType int, data string) []byte {
		b := make([]byte, evHeader+len(data))
		le := binary.LittleEndian
		le.PutUint32(b, uint32(pid))
		le.PutUint32(b[8:], 1)
		le.PutUint32(b[12:], uint32(len(data)))
		le.PutUint32(b[16:], uint32(kind))
		le.PutUint32(b[20:], uint32(fdType<<8))
		copy(b[evHeader:], data)
		return b
	}
	tr.writeEvent(c, ev(evExec, 0, "bash\x00-i\x00"), func(p, m uint32) { modes[p] = m }, func(uint32) {})
	tr.writeEvent(c, ev(evWrite, 3, "journal line\n"), func(p, m uint32) { modes[p] = m }, func(uint32) {})
	tr.writeEvent(c, ev(evWrite, fdTTY, "$ ls\n"), func(p, m uint32) { modes[p] = m }, func(uint32) {})
	if modes[uint32(pid)] != modeTTY {
		t.Fatalf("kernel mode %v", modes)
	}
	recs, _, _ := e.a.hist.list(map[string]bool{"process": true}, "", "", 100)
	if len(recs) != 1 || recs[0].Command != "bash -i" {
		t.Fatalf("records %+v", recs)
	}
	if out, _ := os.ReadFile(e.a.hist.path(recs[0].CommandID, "stdout")); string(out) != "$ ls\n" {
		t.Fatalf("stdout %q", out)
	}
}

func TestSendEvents(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	tr := e.a.tracer
	pid := os.Getpid()
	c := fakeClass(1, map[int]fakeProc{pid: {ppid: 1, comm: "logger", cgroup: "/user.slice/user-1000.slice/session-3.scope", cmdline: "logger hi"}})
	ev := func(kind, fd, flags int, data string) []byte {
		b := make([]byte, evHeader+len(data))
		le := binary.LittleEndian
		le.PutUint32(b, uint32(pid))
		le.PutUint32(b[8:], uint32(fd))
		le.PutUint32(b[12:], uint32(len(data)))
		le.PutUint32(b[16:], uint32(kind))
		le.PutUint32(b[20:], uint32(flags))
		copy(b[evHeader:], data)
		return b
	}
	sock := 3<<8 | fSend
	for _, d := range []string{"<13>Nov 10 12:00:00 tl: hello", "<191>x\n", "plain udp", "<abc>kept", "<5>"} {
		tr.writeEvent(c, ev(evWrite, 3, sock, d), func(uint32, uint32) {}, func(uint32) {})
	}
	tr.writeEvent(c, ev(evWrite, 3, 3<<8, "<13>stream write, untouched"), func(uint32, uint32) {}, func(uint32) {})
	recs, _, _ := e.a.hist.list(map[string]bool{"process": true}, "", "", 100)
	if len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	want := "Nov 10 12:00:00 tl: hello\nx\nplain udp\n<abc>kept\n<13>stream write, untouched"
	if out, _ := os.ReadFile(e.a.hist.path(recs[0].CommandID, "stdout")); string(out) != want {
		t.Fatalf("stdout %q, want %q", out, want)
	}
	// killed by SIGKILL: a signal, no exit code
	tr.writeEvent(c, ev(evExit, 9, 0, ""), func(uint32, uint32) {}, func(uint32) {})
	if r := e.a.hist.record(e.a.hist.get(recs[0].CommandID)); r.Status != "killed" || r.Signal == nil || *r.Signal != "SIGKILL" || r.ExitCode != nil {
		t.Fatalf("killed record %+v", r)
	}
}

func TestTracerFinished(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	tr := e.a.tracer
	ws := func(v int) *syscall.WaitStatus { w := syscall.WaitStatus(v); return &w }
	for _, c := range []struct {
		ws     *syscall.WaitStatus
		status string
		code   int // -1: none
		signal string
	}{
		{ws(0), "exited", 0, ""},
		{ws(3 << 8), "exited", 3, ""},
		{ws(255 << 8), "exited", 255, ""},
		{ws(15), "killed", -1, "SIGTERM"},
		{ws(0x80 | 6), "killed", -1, "SIGABRT"}, // core dumped
		{nil, "exited", -1, ""},                 // reaper: status unknown
	} {
		tr.write("pid:1", 0, "x", "stdout", "x\n")
		k := tr.recs["pid:1"]
		delete(tr.recs, "pid:1")
		tr.finished(k, c.ws)
		r := e.a.hist.record(k.m)
		code, sig := -1, ""
		if r.ExitCode != nil {
			code = *r.ExitCode
		}
		if r.Signal != nil {
			sig = *r.Signal
		}
		if r.Status != c.status || code != c.code || sig != c.signal || r.FinishedAt == nil {
			t.Errorf("status %v: got %s/%d/%q", c.ws, r.Status, code, sig)
		}
	}
}

func TestRedactCreds(t *testing.T) {
	in := "POST /v1.56/images/create HTTP/1.1\r\nHost: docker\r\nX-Registry-Auth: eyJzZWNyZXQiOiJ4In0=\r\nauthorization: Bearer tok\r\n\r\n"
	got := textChunk([]byte(in))
	if strings.Contains(got, "eyJzZWNyZXQ") || strings.Contains(got, "Bearer tok") || !strings.Contains(got, "X-Registry-Auth: [redacted]") || !strings.Contains(got, "Host: docker") {
		t.Fatalf("redacted: %q", got)
	}
	if got := textChunk([]byte("hello: world\n")); got != "hello: world\n" {
		t.Fatalf("plain: %q", got)
	}
}
