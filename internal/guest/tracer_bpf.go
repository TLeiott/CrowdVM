package guest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"

	"github.com/crowdvm/crowdvm/internal/guest/bpf"
)

// eBPF writer tracer (CMD-021): what ssh sessions and processes started by
// integrated agents write to a tty, a pipe or a socket, which neither the
// journal nor Docker sees (the journald follower skips session scopes and
// agent processes, so their syslog/journal messages come through here).
// bpf/writer.c hooks write(2)/writev(2) and send(2)/sendto(2)/sendmsg(2),
// checks the fd's inode in the kernel (tty, pipe or socket; not files or
// /dev/null), applies a per-process token bucket and copies up to 4 KiB per
// call into a ring buffer, with the writer's comm, parent and cgroup id
// (the process may be gone before userspace looks) and its argv at exec.
// Here each process is classified once (procClass); the mode goes back to
// the kernel map, so a skipped process costs one hash lookup per call.
// Recorded output goes through tracer.write (pid:<n> records, rate limit,
// binary skipped); a sent message loses its syslog <PRI> header and ends
// with a newline. The kernel's exit event (the process's last thread)
// finishes the record with the exit status. Without BPF (old kernel, no
// BTF, no privilege) the tracer logs once and keeps journald/Docker only.

const (
	evWrite = 0
	evExec  = 1
	evExit  = 2

	fdTTY     = 1
	fDropped  = 1
	fSend     = 2
	evHeader  = 48 // struct event up to data
	cacheSize = 1 << 15
)

// Process modes in the kernel's procs map.
const (
	modeAll  = 0 // record writes to a tty, a pipe or a socket
	modeSkip = 1
	modeTTY  = 2 // record terminal writes only
)

func (t *tracer) followWrites(stop <-chan struct{}) {
	if err := t.traceWrites(stop); err != nil {
		t.a.log.Printf("tracer: eBPF output capture unavailable (journald/Docker only): %v", err)
	}
}

func (t *tracer) traceWrites(stop <-chan struct{}) error {
	_ = rlimit.RemoveMemlock() // kernels before 5.11 charge BPF maps to RLIMIT_MEMLOCK
	var objs bpf.WriterObjects
	if err := bpf.LoadWriterObjects(&objs, nil); err != nil {
		return err
	}
	defer objs.Close()
	for _, tp := range []struct {
		group, name string
		prog        *ebpf.Program
	}{
		{"syscalls", "sys_enter_write", objs.TraceWrite},
		{"syscalls", "sys_enter_writev", objs.TraceWritev},
		{"syscalls", "sys_enter_sendto", objs.TraceSendto},
		{"syscalls", "sys_enter_sendmsg", objs.TraceSendmsg},
		{"sched", "sched_process_exec", objs.TraceExec},
		{"sched", "sched_process_exit", objs.TraceExit},
	} {
		l, err := link.Tracepoint(tp.group, tp.name, tp.prog, nil)
		if err != nil {
			return err
		}
		defer l.Close()
	}
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		return err
	}
	go func() { <-stop; rd.Close() }()
	t.a.log.Printf("tracer: eBPF output capture on")
	pc := newProcClass(os.Getpid())
	setMode := func(pid uint32, mode uint32) {
		_ = objs.Procs.Update(pid, bpf.WriterState{Mode: mode}, ebpf.UpdateExist)
	}
	forget := func(pid uint32) { _ = objs.Procs.Delete(pid) }
	var rec ringbuf.Record
	for {
		if err := rd.ReadInto(&rec); err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			continue
		}
		t.writeEvent(pc, rec.RawSample, setMode, forget)
	}
}

// writeEvent handles one ring buffer sample (struct event in writer.c).
func (t *tracer) writeEvent(pc *procClass, b []byte, setMode func(pid, mode uint32), forget func(uint32)) {
	if len(b) < evHeader {
		return
	}
	le := binary.LittleEndian
	ev := procEvent{pid: int(le.Uint32(b)), ppid: int(le.Uint32(b[4:])), cgroup: le.Uint64(b[24:]),
		comm: string(bytes.TrimRight(b[32:48], "\x00"))}
	fd, n, kind, flags := le.Uint32(b[8:]), int(le.Uint32(b[12:])), le.Uint32(b[16:]), le.Uint32(b[20:])
	if n > len(b)-evHeader {
		return
	}
	data := b[evHeader : evHeader+n]
	key := "pid:" + strconv.Itoa(ev.pid)
	switch kind {
	case evExec:
		if len(pc.args) >= cacheSize {
			clear(pc.args)
		}
		pc.args[ev.pid] = argv(data)
		delete(pc.cache, ev.pid)
		forget(uint32(ev.pid)) // a verdict stored for the old program after the kernel dropped it
		return
	case evExit:
		delete(pc.cache, ev.pid)
		delete(pc.args, ev.pid)
		if flags&fDropped != 0 {
			t.truncated(key)
		}
		ws := syscall.WaitStatus(fd) // the kernel's exit_code
		t.ended(ev.pid, &ws)
		return
	case evWrite:
	default:
		return
	}
	if t.a.hist.unmonitor {
		return
	}
	v, ok := pc.cache[ev.pid]
	if !ok {
		if len(pc.cache) >= cacheSize {
			clear(pc.cache)
		}
		v = pc.classify(ev)
		pc.cache[ev.pid] = v
		if v.mode != modeAll {
			setMode(uint32(ev.pid), uint32(v.mode))
		}
	}
	if v.mode == modeSkip || v.mode == modeTTY && flags>>8 != fdTTY {
		return
	}
	if flags&fSend != 0 {
		data = message(data)
	}
	if text := textChunk(data); text != "" {
		stream := "stdout"
		if fd == 2 {
			stream = "stderr"
		}
		t.write(key, ev.pid, v.command, stream, text)
	}
	if flags&fDropped != 0 {
		t.truncated(key)
	}
}

// message returns a sent message without a syslog "<PRI>" header (syslog(3),
// logger) and ending with a newline.
func message(b []byte) []byte {
	if i := bytes.IndexByte(b[:min(len(b), 5)], '>'); len(b) > 0 && b[0] == '<' && i > 1 {
		if _, err := strconv.Atoi(string(b[1:i])); err == nil {
			b = b[i+1:]
		}
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b[:len(b):len(b)], '\n')
	}
	return b
}

func argv(b []byte) string {
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
}

// textChunk returns data as text, or "" for binary data (NUL bytes or
// invalid UTF-8; a rune cut by the 4 KiB per-write limit is dropped).
func textChunk(b []byte) string {
	if bytes.IndexByte(b, 0) >= 0 {
		return ""
	}
	if !utf8.Valid(b) {
		for i := 1; i <= 3 && i <= len(b); i++ {
			if utf8.RuneStart(b[len(b)-i]) {
				b = b[:len(b)-i]
				break
			}
		}
		if !utf8.Valid(b) {
			return ""
		}
	}
	return redactCreds(string(b))
}

// credHeaderRe: HTTP headers that carry credentials in IPC a process sends
// over a socket (the docker CLI's X-Registry-Auth after `docker login`,
// Authorization on local APIs). Their values never reach the history.
var credHeaderRe = regexp.MustCompile(`(?im)^((?:x-registry-auth|x-registry-config|authorization|proxy-authorization|cookie|x-api-key)[ \t]*:[ \t]*)[^\r\n]*`)

func redactCreds(s string) string {
	if !strings.Contains(s, ":") {
		return s
	}
	return credHeaderRe.ReplaceAllString(s, "${1}[redacted]")
}

// truncated flags a record whose output the kernel's rate limit dropped.
func (t *tracer) truncated(key string) {
	t.mu.Lock()
	tr := t.recs[key]
	t.mu.Unlock()
	if tr != nil && !tr.m.OutputTruncated {
		t.a.hist.update(tr.m, func(m *meta) { m.OutputTruncated = true })
	}
}

// ended finishes the record of a process the kernel saw exit.
func (t *tracer) ended(pid int, ws *syscall.WaitStatus) {
	key := "pid:" + strconv.Itoa(pid)
	t.mu.Lock()
	tr := t.recs[key]
	delete(t.recs, key)
	t.mu.Unlock()
	if tr != nil {
		t.finished(tr, ws)
	}
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

// procEvent is what the kernel reports about the writing process (it may
// be gone by the time userspace sees the event).
type procEvent struct {
	pid, ppid int
	comm      string
	cgroup    uint64
}

type procVerdict struct {
	mode    int
	command string
}

// procClass decides whose output is recorded:
//   - all tty/pipe/socket writes: processes of an ssh session (cgroup
//     .../session-N.scope of logind's "sshd" service, or without logind a
//     descendant of sshd), and the descendants of an integrated-agent
//     harness (the "agents" workload cgroup; not the harness itself, its
//     stdout is the agent protocol);
//   - terminal writes only: other login sessions (the desktop session, a
//     console login), whose pipes and sockets carry IPC and journal output;
//   - nothing: the guest agent and its other children, exec'd commands and
//     setup scripts (their own records, CMD-022), systemd services and
//     Docker containers (journal and Docker followers), sshd, sudo & co.
//
// The /proc, cgroupfs and logind readers are fields so tests can fake them.
type procClass struct {
	self    int
	cache   map[int]procVerdict
	args    map[int]string // argv seen at exec
	cgroup  func(id uint64, pid int) string
	service func(session string) string // logind session -> PAM service
	ppid    func(pid int) int
	comm    func(pid int) string
	cmdline func(pid int) string
	// stdout: /proc/<pid>/fd/1's link; guestPipes: the pipes the guest
	// itself holds. Nil in tests that do not need them.
	stdout     func(pid int) string
	guestPipes func() map[string]bool
}

// platformOrProtocol: in an agent's tree, the platform's own tools (the
// computer-use MCP server under /opt/crowdvm) and a harness behind a wrapper
// (codex's node launcher execs the native binary) whose stdout goes straight
// to the guest: that is the agent protocol, not program output (Boat leaves
// out "the services that boat itself runs").
func (c *procClass) platformOrProtocol(ev procEvent) bool {
	cmd := c.args[ev.pid]
	if cmd == "" {
		cmd = c.cmdline(ev.pid)
	}
	if strings.HasPrefix(cmd, "/opt/crowdvm/") || ev.comm == "cua-driver" {
		return true
	}
	if c.stdout == nil || c.guestPipes == nil {
		return false
	}
	out := c.stdout(ev.pid)
	return strings.HasPrefix(out, "pipe:") && c.guestPipes()[out]
}

var workloadsCgroup = "/user.slice/" + workloadsUnit + "/"

func newProcClass(self int) *procClass {
	read := func(pid int, f string) string {
		b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/" + f)
		return string(b)
	}
	cg := &cgroupNames{root: cgroupRoot, m: map[uint64]string{}}
	return &procClass{self: self, cache: map[int]procVerdict{}, args: map[int]string{},
		cgroup: func(id uint64, pid int) string {
			if p := cg.path(id); p != "" {
				return p
			}
			for _, l := range strings.Split(read(pid, "cgroup"), "\n") {
				if p, ok := strings.CutPrefix(l, "0::"); ok {
					return p
				}
			}
			return ""
		},
		service: func(session string) string {
			b, _ := os.ReadFile("/run/systemd/sessions/" + session)
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "SERVICE="); ok {
					return v
				}
			}
			return ""
		},
		ppid: func(pid int) int {
			st, err := readStat(pid)
			if err != nil {
				return 0
			}
			return st.ppid
		},
		comm:    func(pid int) string { return strings.TrimSpace(read(pid, "comm")) },
		cmdline: func(pid int) string { return argv([]byte(read(pid, "cmdline"))) },
		stdout: func(pid int) string {
			l, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/1")
			return l
		},
		guestPipes: func() map[string]bool {
			m := map[string]bool{}
			ents, _ := os.ReadDir("/proc/self/fd")
			for _, e := range ents {
				if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && strings.HasPrefix(l, "pipe:") {
					m[l] = true
				}
			}
			return m
		},
	}
}

func (c *procClass) classify(ev procEvent) procVerdict {
	mode := c.mode(ev)
	if mode == modeSkip {
		return procVerdict{mode: modeSkip}
	}
	cmd := c.args[ev.pid]
	if cmd == "" {
		cmd = c.cmdline(ev.pid)
	}
	if cmd == "" {
		cmd = ev.comm
	}
	return procVerdict{mode: mode, command: cmd}
}

func (c *procClass) mode(ev procEvent) int {
	if ev.pid <= 1 || ev.pid == c.self || traceSkipIdent[ev.comm] {
		return modeSkip
	}
	cg := c.cgroup(ev.cgroup, ev.pid)
	if rest, ok := strings.CutPrefix(cg, workloadsCgroup); ok {
		if (rest == "agents" || strings.HasPrefix(rest, "agents/")) && ev.ppid != c.self {
			if c.platformOrProtocol(ev) {
				return modeSkip
			}
			return modeAll
		}
		return modeSkip
	}
	base := cg[strings.LastIndexByte(cg, '/')+1:]
	if id, ok := strings.CutPrefix(base, "session-"); ok && strings.HasSuffix(id, ".scope") {
		if c.service(strings.TrimSuffix(id, ".scope")) == "sshd" {
			return modeAll
		}
		return modeTTY
	}
	// No logind session (e.g. sshd without pam_systemd): an sshd descendant.
	for p, i := ev.ppid, 0; p > 1 && i < 64; p, i = c.ppid(p), i+1 {
		if p == c.self {
			return modeSkip
		}
		if comm := c.comm(p); comm == "sshd" || comm == "sshd-session" {
			return modeAll
		}
	}
	return modeSkip
}

// cgroupNames maps cgroup ids (the cgroupfs inode numbers) to paths.
type cgroupNames struct {
	root string
	m    map[uint64]string
}

func (c *cgroupNames) path(id uint64) string {
	if p, ok := c.m[id]; ok || id == 0 {
		return p
	}
	if len(c.m) > cacheSize {
		clear(c.m)
	}
	_ = filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				c.m[st.Ino] = "/" + strings.TrimPrefix(strings.TrimPrefix(p, c.root), "/")
			}
		}
		return nil
	})
	return c.m[id]
}
