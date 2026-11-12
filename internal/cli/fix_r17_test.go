package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// bugs-r17-opus 8: `desktop ID` sends publicAccess:false (so a public desktop
// becomes private again), `--public` sends true, and a public answer is said
// on stderr.
func TestFixR17DesktopSendsPublicAccessFalse(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	var body string
	public := false
	e.f.On("POST /api/v1/sandboxes/{id}/desktop", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		clienttest.OK(w, "desktop.url", map[string]any{"desktopUrl": "http://server/desktop/" + r.PathValue("id"), "mode": "vnc", "publicAccess": public})
	})
	r := e.mustRun("desktop", id, "--no-open")
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil || req["publicAccess"] != false {
		t.Fatalf("without --public the body must carry publicAccess:false, got %q", body)
	}
	if strings.Contains(r.err, "public") {
		t.Errorf("a private desktop says it is public: %q", r.err)
	}
	public = true
	r = e.mustRun("desktop", id, "--no-open", "--public")
	if err := json.Unmarshal([]byte(body), &req); err != nil || req["publicAccess"] != true {
		t.Fatalf("--public must send publicAccess:true, got %q", body)
	}
	if !strings.Contains(r.err, "public") {
		t.Errorf("a public desktop is not announced on stderr: %q", r.err)
	}
	// Whatever the flag, a public answer is announced.
	if r := e.mustRun("desktop", id, "--no-open"); !strings.Contains(r.err, "public") {
		t.Errorf("a public answer is not announced: %q", r.err)
	}
}

// bugs-r17-opus 10: EPIPE on stdout ends quietly with 141; the "server
// reachable?" hint is for real network errors only.
func TestFixR17BrokenPipeIsQuiet(t *testing.T) {
	report := func(err error) (int, string) {
		var errb bytes.Buffer
		a := &App{Err: &errb, Out: io.Discard}
		return a.reportError(err), errb.String()
	}
	pipe := &fs.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}
	if code, msg := report(pipe); code != 141 || msg != "" {
		t.Errorf("EPIPE on stdout: code %d stderr %q, want 141 and nothing", code, msg)
	}
	const hint = "is the CrowdVM server reachable"
	if code, msg := report(&fs.PathError{Op: "write", Path: "/tmp/x", Err: syscall.EACCES}); code != 1 || strings.Contains(msg, hint) {
		t.Errorf("a plain errno got code %d and %q", code, msg)
	}
	if _, msg := report(&url.Error{Op: "Get", URL: "http://x", Err: errors.New("boom")}); !strings.Contains(msg, hint) {
		t.Errorf("a url.Error lost the network hint: %q", msg)
	}
}

// exec to a closed stdout end to end: the writer fails with EPIPE.
type epipeWriter struct{}

func (epipeWriter) Write([]byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}
}

func TestFixR17ExecToClosedPipe(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	var errb bytes.Buffer
	a := &App{In: strings.NewReader(""), Out: epipeWriter{}, Err: &errb, Getenv: func(k string) string { return e.vars[k] },
		Exe: "/opt/crowdvm/crowdvm", Poll: 2 * time.Millisecond}
	code := a.Execute(context.Background(), []string{"exec", id, "--", "echo", "hello"})
	if code != 141 || strings.Contains(errb.String(), "reachable") || strings.Contains(errb.String(), "error:") {
		t.Fatalf("exec into a closed pipe: code %d stderr %q, want 141 and no error line", code, errb.String())
	}
}
