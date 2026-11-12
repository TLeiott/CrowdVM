package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"github.com/crowdvm/crowdvm/internal/client/clienttest"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Nightly builds are named <base>-nightly.<yyyymmdd>.<run> (scripts/release/version.mjs).
// compareVersions treats every pre-release suffix as equal, so a nightly
// never sees a newer nightly: `self-update` says "up to date" and the notice
// stays silent until the base version changes.
func TestBughuntR16NightlyVersionsAreOrdered(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.1.1-nightly.20261112.3", "0.1.1-nightly.20261111.9", 1},
		{"0.1.1-nightly.20261112.4", "0.1.1-nightly.20261112.3", 1},
		{"0.1.1-nightly.20261110.1", "0.1.1-nightly.20261112.3", -1},
		{"0.1.1-nightly.20261112.10", "0.1.1-nightly.20261112.9", 1},
	} {
		got, ok := compareVersions(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, %v; want %d", c.a, c.b, got, ok, c.want)
		}
	}
}

// ssh-config writes `IdentityFile <path>` and `UserKnownHostsFile <path>`
// unquoted. OpenSSH splits an unquoted value at a space (UserKnownHostsFile
// even takes several files), so a config or home directory with a space
// (C:\Users\First Last\...) gives a config that points at the wrong files.
func TestBughuntR16SSHConfigQuotesPathsWithSpaces(t *testing.T) {
	e := newEnv(t)
	e.vars["CROWDVM_CONFIG_DIR"] = filepath.Join(e.dir, "my config")
	id := e.f.AddSandbox("running")
	r := e.mustRun("ssh-config", id, "--print")
	for _, line := range strings.Split(r.out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		if (k == "IdentityFile" || k == "UserKnownHostsFile") && strings.Contains(v, " ") && !strings.HasPrefix(v, `"`) {
			t.Errorf("%s value has a space and is not quoted: %q", k, v)
		}
	}
}

// `snapshot pull` extracts the server's tar (entries rooted at the folder's
// name) into DIR/home_user and then "flattens" it, but only when that was
// the only entry in the folder. Pulling again into the same -o directory
// (a refreshed backup) finds the earlier files and leaves the new copy one
// level down: home_user/project/project/a.txt next to a stale
// home_user/project/a.txt.
func TestBughuntR16SnapshotPullTwiceKeepsLayout(t *testing.T) {
	e := newEnv(t)
	content := ""
	e.f.On("GET /api/v1/snapshots/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, f := range []struct{ name, body string }{{"project/", ""}, {"project/a.txt", content}} {
			h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
			if strings.HasSuffix(f.name, "/") {
				h.Typeflag, h.Mode = tar.TypeDir, 0o755
			}
			_ = tw.WriteHeader(h)
			_, _ = tw.Write([]byte(f.body))
		}
		_ = tw.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(buf.Bytes())
	})
	pull := func(c string) {
		content = c
		e.mustRun("snapshot", "pull", "snap_1", "-o", filepath.Join(e.dir, "out"), "--path", "/home/user/project")
	}
	pull("one")
	pull("two")
	got, err := os.ReadFile(filepath.Join(e.dir, "out", "home_user", "project", "a.txt"))
	if err != nil || string(got) != "two" {
		t.Fatalf("home_user/project/a.txt after the second pull: %q, %v (want \"two\")", got, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "out", "home_user", "project", "project")); err == nil {
		t.Fatal("the second pull nested the folder: home_user/project/project exists")
	}
}

// `direct-ports ID PORT --clear` is documented as "remove the ports asked
// for". The code builds the list from the arguments and PUTs it, whatever
// --clear says: the port the user wanted gone is published (and the guest
// firewall is opened for it).
func TestBughuntR16DirectPortsClearRemovesNamedPort(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	e.f.On("GET /api/v1/sandboxes/{id}/direct-ports", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "direct_ports", map[string]any{"sandboxId": id, "ports": []any{
			map[string]any{"port": 25565, "proto": "tcp"}, map[string]any{"port": 27015, "proto": "udp"}}})
	})
	e.f.On("PUT /api/v1/sandboxes/{id}/direct-ports", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "direct_ports", map[string]any{"sandboxId": id, "ports": []any{}})
	})
	e.run("direct-ports", id, "27015/udp", "--clear")
	put := e.f.Find("PUT", "/api/v1/sandboxes/"+id+"/direct-ports")
	if put == nil {
		t.Fatal("no PUT sent")
	}
	for _, p := range reqBody(t, put)["ports"].([]any) {
		if p.(map[string]any)["port"] == float64(27015) {
			t.Fatalf("--clear 27015/udp published port 27015: %s", put.Body)
		}
	}
}

// Two crowdvm commands started together for the first time (a script that
// runs `crowdvm ssh-config` for several sandboxes in parallel) both find no
// key pair and both generate one. Each writes the private key and the public
// key without a lock or an exclusive create, so the last writer wins on disk
// while the other command already authorized its own public key on the
// sandbox and runs ssh with a private key that does not match it.
func TestBughuntR16SSHKeyGenerationIsRaceFree(t *testing.T) {
	for round := 0; round < 30; round++ {
		dir := t.TempDir()
		start := make(chan struct{})
		pubs := make([]string, 8)
		var wg sync.WaitGroup
		for i := range pubs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a := &App{ConfigDir: dir, Getenv: func(string) string { return "" }}
				<-start
				_, pubs[i], _ = a.sshKey()
			}()
		}
		close(start)
		wg.Wait()
		onDisk, _ := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
		for i, p := range pubs {
			if p != strings.TrimSpace(string(onDisk)) {
				t.Fatalf("round %d: command %d got a public key that is not the one on disk (it authorizes a key whose private half was overwritten)", round, i)
			}
		}
	}
}

// `Include crowdvm/*.conf` is looked for as a substring of ~/.ssh/config, so
// a commented-out line counts: ssh-config then says "included" and prints no
// hint, while ssh ignores the entry.
func TestBughuntR16SSHConfigIncludeInCommentIsNotIncluded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("# Include crowdvm/*.conf\nHost x\n  User y\n"), 0o600)
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	r := e.mustRun("ssh-config", id)
	if r.last(t)["included"] != false {
		t.Fatalf("a commented-out Include counts as included: %v", r.last(t))
	}
}

// `run --json` ends with {"event":"summary","exitCode":N,...}. When the run
// itself fails (the sandbox never answers, a push fails, the follow loop
// breaks) the command never produced an exit code, but the summary says
// exitCode 0 (the zero value of the failed closure's result). A script or
// agent that reads the summary instead of the process status takes it for a
// success.
func TestBughuntR16RunSummaryDoesNotReportExitZeroForAFailedRun(t *testing.T) {
	e := newEnv(t)
	e.f.On("POST /api/v1/sandboxes/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		clienttest.Error(w, 400, "bad_request", "the sandbox cannot run this")
	})
	r := e.run("run", "--json", "--", "make test")
	if r.code == 0 {
		t.Fatalf("setup: the run should fail: %+v", r)
	}
	for _, l := range r.lines(t) {
		if l["event"] == "summary" && l["exitCode"] == float64(0) {
			t.Fatalf("summary reports exitCode 0 for a run whose command never ran (process exit %d): %v", r.code, l)
		}
	}
}

// `crowdvm exec ID --timeout 0 "tail -f log" | grep -m1 ready`: the reader
// closes its end after the first line. The CLI ignores every write error
// (io.WriteString results are dropped) and, on a real stdout, is killed by
// the Go runtime's SIGPIPE default (cancelSignals does not include SIGPIPE),
// so in neither case does runLong's cleanup stop the remote command. With
// --timeout 0 nothing else ever does: the command runs on in the sandbox.
func TestBughuntR16ExecStopsRemoteCommandWhenOutputReaderIsGone(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"type": "stdout", "data": "ready\n", "nextOffset": 6})
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_ = enc.Encode(map[string]any{"type": "exit", "exitCode": 0})
	})
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pr.Close() // the reader (head, grep -m1) is gone
	defer pw.Close()
	a := &App{In: strings.NewReader(""), Out: pw, Err: io.Discard, Getenv: func(k string) string { return e.vars[k] },
		Exe: "/opt/crowdvm/crowdvm", Poll: 2 * time.Millisecond}
	a.Execute(context.Background(), []string{"exec", id, "--json=false", "--timeout", "0", "tail -f log"})
	if e.f.Count("DELETE /api/v1/sandboxes/"+id+"/commands/") == 0 {
		t.Fatal("the output reader closed, but the remote command was not stopped: it runs on in the sandbox")
	}
}

// Same cause on the command line of `crowdvm ssh`: `-o UserKnownHostsFile=<path>`
// goes through ssh's config parser, which splits an unquoted value at spaces
// (the option takes several files). With the config directory under a
// profile with a space (C:\Users\First Last\AppData\Roaming\crowdvm) the
// pinned host key file is never read and StrictHostKeyChecking=yes fails.
func TestBughuntR16SSHOptionQuotesKnownHostsWithSpaces(t *testing.T) {
	e := newEnv(t)
	e.vars["CROWDVM_CONFIG_DIR"] = filepath.Join(e.dir, "my config")
	e.tools["ssh"] = true
	id := e.f.AddSandbox("running")
	e.mustRun("ssh", id)
	if len(e.ran) == 0 {
		t.Fatal("ssh was not run")
	}
	for _, arg := range e.ran[0].Args {
		if v, ok := strings.CutPrefix(arg, "UserKnownHostsFile="); ok && strings.Contains(v, " ") && !strings.HasPrefix(v, `"`) {
			t.Fatalf("-o %s: the path has a space and is not quoted, ssh reads it as two files", arg)
		}
	}
}

// The fast path of sshKey reads the key pair without the lock. With plain
// os.WriteFile the .pub exists empty for a moment while the first command
// writes it, and a command reading in that gap gets an empty or cut-off
// public key and no error.
func TestFixR16SSHKeyFastPathNeverSeesPartialKey(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		var bad atomic.Value
		done := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a := &App{ConfigDir: dir, Getenv: func(string) string { return "" }}
				for {
					select {
					case <-done:
						return
					default:
					}
					if !fileExists(filepath.Join(dir, "id_ed25519.pub")) {
						continue
					}
					if _, pub, err := a.sshKey(); err == nil && !strings.HasSuffix(pub, " crowdvm") {
						bad.Store(pub)
					}
				}
			}()
		}
		a := &App{ConfigDir: dir, Getenv: func(string) string { return "" }}
		if _, _, err := a.sshKey(); err != nil {
			t.Fatal(err)
		}
		close(done)
		wg.Wait()
		if v := bad.Load(); v != nil {
			t.Fatalf("round %d: a command read a partial public key %q", round, v)
		}
	}
}
