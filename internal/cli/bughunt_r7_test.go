package cli

import (
	"archive/tar"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// The documented guarantee of extractTar is that a symlink pointing outside
// the destination is skipped. linkInside checks the target lexically against
// the links that exist when the entry comes. `b -> a/..` is accepted while "a"
// does not exist yet, and `a -> .` is accepted next; afterwards b resolves
// through a (the destination) and ".." to the destination's parent.
func TestBughuntR7ExtractTarLinkChainEscapesDest(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "xfer", "d")
	archive := tarOf(t, []tar.Header{
		{Name: "d/b", Typeflag: tar.TypeSymlink, Linkname: "a/.."},
		{Name: "d/a", Typeflag: tar.TypeSymlink, Linkname: "."},
	}, nil)
	if _, err := extractTar(archive, dest, "d", nil); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dest, "b"))
	if err != nil {
		return // refused or broken: fine
	}
	realDest, _ := filepath.EvalSymlinks(dest)
	if rel, err := filepath.Rel(realDest, real); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		t.Fatalf("symlink d/b was created and resolves to %s, outside the destination %s", real, realDest)
	}
}

// A long command that follows the attach stream must survive any number of
// network cuts ("surviving network cuts" is the documented contract), as long
// as each reconnect makes progress. The failure counter in attach() is never
// reset by a stream that delivered data, so the 21st cut aborts the exec
// (exit 1, "attach: stream keeps ending") while the command keeps running.
func TestBughuntR7ExecAttachGivesUpAfter21Cuts(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	var calls atomic.Int32
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}/stream", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		enc := json.NewEncoder(w)
		if n <= 30 {
			enc.Encode(map[string]any{"type": "stdout", "data": "x", "nextOffset": n}) // data, then the connection is cut
			return
		}
		enc.Encode(map[string]any{"type": "exit", "exitCode": 0})
	})
	e.f.On("GET /api/v1/sandboxes/{id}/commands/{pid}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "command.status", map[string]any{"processId": 4242, "status": "running", "running": true})
	})
	r := e.run("exec", id, "--json=false", "--timeout", "0", "make")
	if r.code != 0 || r.out != strings.Repeat("x", 30) {
		t.Fatalf("exit %d, stdout %q, stderr %q (stream opened %d times)", r.code, r.out, r.err, calls.Load())
	}
}
