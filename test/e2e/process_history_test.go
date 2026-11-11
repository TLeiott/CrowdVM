package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// testProcessHistory: the guest's eBPF writer tracer (CMD-021) records what
// ssh sessions write to a pipe or a tty, including a background writer the
// session left behind, and what they send to a socket (the journal through
// logger, a socketpair), as `process` history records with their exit code;
// commands run through exec are not recorded twice (CMD-022).
func testProcessHistory(t *testing.T, c *cluster) {
	id := c.newSandbox(t, "--name", "process-history")
	n := time.Now().UnixNano()
	sshMark, ttyMark, bgMark, execMark := fmt.Sprintf("ssh-out-%d", n), fmt.Sprintf("tty-out-%d", n),
		fmt.Sprintf("bg-tick-%d", n), fmt.Sprintf("exec-out-%d", n)
	logMark, sendMark, exitMark := fmt.Sprintf("log-msg-%d", n), fmt.Sprintf("sock-msg-%d", n), fmt.Sprintf("exit-three-%d", n)

	c.cliT(t, 2*time.Minute, "ssh", id, "--", "echo "+sshMark).ok(t)
	// a pty inside the session (the CLI's stdin is no terminal, so no ssh -t)
	c.cliT(t, 2*time.Minute, "ssh", id, "--", "script -qc 'echo "+ttyMark+"' /dev/null >/dev/null").ok(t)
	// a writer that outlives the session, writing to a pipe
	c.cliT(t, 2*time.Minute, "ssh", id, "--",
		`(setsid sh -c 'for i in 1 2 3 4 5 6 7 8; do echo `+bgMark+`-$i; sleep 1; done | cat >/dev/null' >/dev/null 2>&1 </dev/null &)`).ok(t)
	c.sh(t, id, "echo "+execMark)
	// sent to a socket: the journal (/dev/log, sendmsg) and a socketpair (send)
	c.cliT(t, 2*time.Minute, "ssh", id, "--", "logger "+logMark).ok(t)
	c.cliT(t, 2*time.Minute, "ssh", id, "--", `python3 -c "import socket; a, b = socket.socketpair(); a.send(b'`+sendMark+`')"`).ok(t)
	if r := c.cliT(t, 2*time.Minute, "ssh", id, "--", `sh -c "echo `+exitMark+`; exit 3"`); r.code != 3 {
		t.Errorf("ssh exit code %d, want 3: %s", r.code, tail(r.out+r.err, 400))
	}

	type rec struct{ id, mode, command, status, exitCode string }
	records := func() []rec {
		var out []rec
		for _, it := range list(c.cli(t, "history", id, "--limit", "100").ok(t).last(), "commands") {
			out = append(out, rec{gets(it, "commandId"), gets(it, "mode"), gets(it, "command"), gets(it, "status"),
				fmt.Sprint(get(it, "exitCode"))})
		}
		return out
	}
	output := func(cmdID string) string { return c.cli(t, "history", id, cmdID).out }
	// processRecord finds a process record whose stored output contains mark.
	processRecord := func(rs []rec, mark, cmdPart string) (rec, bool) {
		for _, r := range rs {
			if r.mode == "process" && strings.Contains(r.command, cmdPart) && strings.Contains(output(r.id), mark) {
				return r, true
			}
		}
		return rec{}, false
	}
	var rs []rec
	var echo, exit3 rec
	ok := eventually(t, 2*time.Minute, 5*time.Second, "process records of the ssh session, tty, background writer, socket sends and exits", func() (bool, string) {
		rs = records()
		var a, b, d, l, s, x bool
		echo, a = processRecord(rs, sshMark, sshMark)
		_, b = processRecord(rs, ttyMark, ttyMark)
		_, d = processRecord(rs, bgMark+"-8", bgMark)
		_, l = processRecord(rs, logMark, "logger")
		_, s = processRecord(rs, sendMark, "python3")
		exit3, x = processRecord(rs, exitMark, exitMark)
		done := echo.status == "exited" && exit3.status == "exited"
		return a && b && d && l && s && x && done,
			fmt.Sprintf("ssh=%v tty=%v bg=%v logger=%v send=%v exit3=%v exited=%v records=%v", a, b, d, l, s, x, done, rs)
	})
	expect(t, echo.exitCode == "0", "echo record exit code %s: %+v", echo.exitCode, echo)
	expect(t, exit3.exitCode == "3", "exit 3 record exit code %s: %+v", exit3.exitCode, exit3)
	if !ok {
		t.Logf("guest log: %s", tail(c.execR(t, id, "sudo journalctl -u crowdvm-guest --no-pager | grep -i tracer").out, 2000))
	}
	// the exec'd command: one record of its own, no process record
	var execRecs []string
	for _, r := range records() {
		if strings.Contains(r.command, execMark) {
			execRecs = append(execRecs, r.mode)
		}
	}
	expect(t, len(execRecs) == 1 && execRecs[0] != "process", "exec'd command records: %v", execRecs)
	var procs []string
	for _, r := range records() {
		if r.mode == "process" {
			procs = append(procs, tail(r.command, 80))
		}
	}
	t.Logf("process records: %q", procs)

	// A process an integrated agent starts (OpenCode's bash tool, free model).
	agentMark := fmt.Sprintf("agent-out-%d", n)
	r := c.cliT(t, 6*time.Minute, "prompt", id, "--provider", "opencode", "--model", freeModel, "--new",
		"Use your bash tool to run exactly this command, then reply DONE: echo "+agentMark)
	if done := r.event("done"); done == nil || gets(done, "status") != "finished" {
		t.Logf("agent step skipped: the free model's prompt did not finish: %s", tail(r.out+r.err, 400))
	} else if !strings.Contains(c.cli(t, "events", id).out, agentMark) {
		t.Logf("agent step skipped: the model did not run the command")
	} else {
		var ar rec
		if eventually(t, 2*time.Minute, 5*time.Second, "process record of the agent's command", func() (bool, string) {
			var ok bool
			ar, ok = processRecord(records(), agentMark, "")
			return ok, "no process record with " + agentMark
		}) {
			t.Logf("agent-started process recorded: %s %q", ar.id, tail(ar.command, 120))
		}
	}

	// The platform's own tools in an agent's tree (the computer-use MCP
	// server) are not user processes.
	for _, r := range records() {
		expect(t, !(r.mode == "process" && strings.Contains(r.command, "/opt/crowdvm/")), "platform process recorded: %q", r.command)
	}

	if os.Getenv("CROWDVM_E2E_OVERHEAD") == "1" {
		writerOverhead(t, c, id)
	}
}

// writerOverhead logs the cost of 2M one-byte pipe writes with the tracer
// on (from ssh: traced and sent to the guest; from exec: skipped in the
// kernel) and off (a sandbox with monitoring off runs no tracer).
func writerOverhead(t *testing.T, c *cluster, id string) {
	const probe = `g=$(pgrep -xo crowdvm-guest); cpu() { awk '{print $14+$15}' /proc/$g/stat; }; t0=$(cpu); s=$(date +%s%N); ` +
		`dd if=/dev/zero bs=1 count=2000000 2>/dev/null | cat >/dev/null; e=$(date +%s%N); ` +
		`echo "wall=$(( (e - s) / 1000000 ))ms guest_cpu=$(( ($(cpu) - t0) * 10 ))ms"`
	off := c.newSandbox(t, "--name", "process-history-off", "--no-monitoring")
	for i := 0; i < 2; i++ {
		t.Logf("overhead run %d: tracer on, ssh:  %s", i, strings.TrimSpace(c.cliT(t, 2*time.Minute, "ssh", id, "--", probe).out))
		t.Logf("overhead run %d: tracer on, exec: %s", i, strings.TrimSpace(c.sh(t, id, probe)))
		t.Logf("overhead run %d: tracer off, ssh: %s", i, strings.TrimSpace(c.cliT(t, 2*time.Minute, "ssh", off, "--", probe).out))
	}
}
