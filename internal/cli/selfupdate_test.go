package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A download that does not match the list's sha256 is refused before it
// runs, and nothing is left next to the binary.
func TestSelfUpdateChecksumMismatch(t *testing.T) {
	t.Setenv("CROWDVM_NPM_LAUNCHER", "")
	e := newEnv(t)
	name := "crowdvm"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	blob := bytes.Repeat([]byte{1}, 2<<20)
	e.f.On("GET /downloads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"v9.9.9","downloads":[{"os":"` + runtime.GOOS + `","arch":"` + runtime.GOARCH +
			`","name":"` + name + `","size":2097152,"sha256":"0000000000000000000000000000000000000000000000000000000000000000"}]}`))
	})
	e.f.On("GET /downloads/{os}/{arch}/{name}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(blob) })
	r := e.run("self-update", "--check")
	if r.code == 0 || !strings.Contains(r.err+r.out, "checksum") {
		t.Fatalf("mismatch accepted: %d %s %s", r.code, r.out, r.err)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("left the download behind")
	}
}
