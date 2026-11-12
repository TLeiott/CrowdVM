package cli

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// execWithStdin runs `crowdvm exec ID -- true` with in as the process stdin.
func execWithStdin(e *env, in *os.File) result {
	id := e.f.AddSandbox("ready")
	var out, errb strings.Builder
	a := &App{In: in, Out: &out, Err: &errb, Getenv: func(k string) string { return e.vars[k] },
		Exe: "/opt/crowdvm/crowdvm", Poll: 2 * time.Millisecond}
	code := a.Execute(context.Background(), []string{"exec", id, "--", "true"})
	return result{out.String(), errb.String(), code}
}

// bugs-r16-opus 4: exec drops stdin; a pipe or a non-empty file gets a
// warning, /dev/null and an empty file stay silent.
func TestFixR16ExecWarnsAboutDroppedStdin(t *testing.T) {
	const warn = "does not pass stdin"
	e := newEnv(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	if r := execWithStdin(e, pr); !strings.Contains(r.err, warn) || !strings.Contains(r.err, "crowdvm ssh ID -- CMD") {
		t.Errorf("pipe: want the warning, got code %d stderr %q", r.code, r.err)
	}

	data := filepath.Join(e.dir, "data")
	if err := os.WriteFile(data, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(data)
	defer f.Close()
	if r := execWithStdin(e, f); !strings.Contains(r.err, warn) {
		t.Errorf("non-empty file: want the warning, got %q", r.err)
	}

	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	if r := execWithStdin(e, null); strings.Contains(r.err, warn) {
		t.Errorf("/dev/null: want no warning, got code %d stderr %q", r.code, r.err)
	}

	empty := filepath.Join(e.dir, "empty")
	_ = os.WriteFile(empty, nil, 0o600)
	ef, _ := os.Open(empty)
	defer ef.Close()
	if r := execWithStdin(e, ef); strings.Contains(r.err, warn) {
		t.Errorf("empty file: want no warning, got %q", r.err)
	}

	// The default test stdin (a string reader, like a terminal here) is silent too.
	if r := e.run("exec", e.f.AddSandbox("ready"), "--", "true"); strings.Contains(r.err, warn) {
		t.Errorf("non-file stdin: want no warning, got %q", r.err)
	}
	if h := e.mustRun("exec", "--help"); !strings.Contains(h.out, "Stdin is not passed") {
		t.Errorf("exec --help does not say stdin is dropped:\n%s", h.out)
	}
}

// bugs-r16-opus 5: `tunnel --pin` writes the sandbox's current host key to
// known_hosts before relaying, and prints nothing of its own to stdout (stdout
// is the ssh transport).
func TestFixR16TunnelPinWritesCurrentHostKey(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	kh := filepath.Join(e.dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(id+" ssh-ed25519 AAAAOLDKEY\nother ssh-ed25519 AAAAOTHER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.f.On("POST /api/v1/sandboxes/{id}/sshkey", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "ssh_key.configured", map[string]any{"success": true, "sshUser": "user", "hostKey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINEWKEY0"})
	})
	e.f.On("GET /api/v1/sandboxes/{id}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		line, _ := bufio.NewReader(nc).ReadString('\n')
		nc.Write([]byte(line))
		nc.Close()
	})
	r := e.runIn("ping\n", "tunnel", "--pin", id, "22")
	if r.code != 0 {
		t.Fatalf("exit %d: %q %q", r.code, r.err, r.out)
	}
	if r.out != "ping\n" { // only the relayed bytes (the fake echoes)
		t.Errorf("stdout has more than the relayed data: %q", r.out)
	}
	b, _ := os.ReadFile(kh)
	got := string(b)
	if !strings.Contains(got, id+" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINEWKEY0") || strings.Contains(got, "OLDKEY") || !strings.Contains(got, "other ssh-ed25519") {
		t.Errorf("known_hosts not refreshed (only this host's line may change):\n%s", got)
	}

	// Without --pin the file stays as it is.
	e.f.AddSandbox("ready")
	before, _ := os.ReadFile(kh)
	e.runIn("x\n", "tunnel", id, "22")
	if after, _ := os.ReadFile(kh); string(after) != string(before) {
		t.Errorf("tunnel without --pin changed known_hosts")
	}
}

// bugs-r16-opus 5: the ssh-config entry's ProxyCommand pins on every connect,
// and still checks host keys strictly.
func TestFixR16SSHConfigProxyCommandPins(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	r := e.mustRun("ssh-config", id, "--print")
	var proxy string
	for _, l := range strings.Split(r.out, "\n") {
		if k, v, _ := strings.Cut(strings.TrimSpace(l), " "); k == "ProxyCommand" {
			proxy = v
		}
	}
	if !strings.Contains(proxy, "tunnel --pin %h 22") {
		t.Errorf("ProxyCommand does not pin: %q", proxy)
	}
	if !strings.Contains(r.out, "StrictHostKeyChecking yes") {
		t.Errorf("StrictHostKeyChecking must stay yes:\n%s", r.out)
	}
	// `crowdvm ssh` already pins through the API; its ProxyCommand stays plain.
	e.tools["ssh"] = true
	e.mustRun("ssh", id, "--", "true")
	if len(e.ran) == 0 || strings.Contains(strings.Join(e.ran[len(e.ran)-1].Args, " "), "--pin") {
		t.Errorf("ssh's own ProxyCommand should not pin: %v", e.ran)
	}
}

// bugs-r16-opus 6: `run` deletes its sandbox itself, so an interrupted wait
// must not say "Remove it with: crowdvm delete".
func TestFixR16RunDoesNotHintDelete(t *testing.T) {
	e := newEnv(t)
	polled := make(chan struct{}, 100)
	e.f.On("GET /api/v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		select {
		case polled <- struct{}{}:
		default:
		}
		clienttest.OK(w, "sandbox", map[string]any{"sandbox": map[string]any{"id": r.PathValue("id"), "state": "provisioning"}})
	})
	run := func(args ...string) result {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		for len(polled) > 0 {
			<-polled
		}
		go func() { // interrupt while run waits: after it saw the sandbox once
			for i := 0; i < 2; i++ {
				select {
				case <-polled:
				case <-time.After(10 * time.Second):
				}
			}
			cancel()
		}()
		return e.runCtx(ctx, "", append([]string{"run"}, args...)...)
	}
	if r := run("--", "true"); strings.Contains(r.err, "Remove it with") {
		t.Errorf("run printed the delete hint: %q", r.err)
	}
	// With --keep the sandbox stays, so the hint is still right.
	if r := run("--keep", "--", "true"); !strings.Contains(r.err, "Remove it with") {
		t.Errorf("run --keep lost the delete hint: %q", r.err)
	}
}
