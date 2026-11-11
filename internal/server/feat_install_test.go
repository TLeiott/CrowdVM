package server_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// The embedded installers must stay identical to scripts/.
func TestInstallScriptsInSync(t *testing.T) {
	for _, n := range []string{"install.sh", "install.ps1"} {
		a, err1 := os.ReadFile(filepath.Join("..", "..", "scripts", n))
		b, err2 := os.ReadFile(filepath.Join("feat_install_files", n))
		if err1 != nil || err2 != nil || string(a) != string(b) {
			t.Fatalf("internal/server/feat_install_files/%s differs from scripts/%s: cp scripts/%s internal/server/feat_install_files/", n, n, n)
		}
	}
}

func instGet(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestInstallAndDownloads(t *testing.T) {
	env := servertest.New(t)
	for _, p := range []string{"/install", "/install.sh", "/install.ps1"} {
		st, body, _ := instGet(t, env.URL+p)
		if st != 200 || strings.Contains(body, "__CROWDVM_SERVER__") || !strings.Contains(body, env.URL) {
			t.Fatalf("%s: %d %.200s", p, st, body)
		}
	}

	// Import a dist/ tree as made by scripts/dist.sh.
	dist := t.TempDir()
	for _, f := range []string{"crowdvm_v1_linux_amd64/crowdvm", "crowdvm_v1_linux_amd64/crowdvm-node", "crowdvm_v1_linux_amd64/crowdvm-guest",
		"crowdvm_v1_windows_amd64/crowdvm.exe", "crowdvm_v1_windows_amd64/crowdvm-linux-amd64", "crowdvm_v1_darwin_arm64/crowdvm"} {
		p := filepath.Join(dist, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("bin:"+f), 0o755)
	}
	files, err := server.ImportDownloads(dist, env.S.DownloadsDir())
	if err != nil || len(files) != 6 {
		t.Fatalf("import: %v %v", files, err)
	}
	if st, body, h := instGet(t, env.URL+"/downloads/linux/amd64/crowdvm-node"); st != 200 || body != "bin:crowdvm_v1_linux_amd64/crowdvm-node" || h.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download: %d %s", st, body)
	}
	if st, body, _ := instGet(t, env.URL+"/downloads/windows/amd64/crowdvm.exe"); st != 200 || !strings.HasSuffix(body, "crowdvm.exe") {
		t.Fatalf("windows: %d %s", st, body)
	}
	// The in-sandbox CLI that Windows nodes put on the config drive (never .exe).
	if st, body, _ := instGet(t, env.URL+"/downloads/windows/amd64/crowdvm-linux-amd64"); st != 200 || !strings.HasSuffix(body, "crowdvm-linux-amd64") {
		t.Fatalf("in-sandbox cli: %d %s", st, body)
	}
	if st, _, _ := instGet(t, env.URL+"/downloads/windows/amd64/crowdvm-linux-amd64.exe"); st != 404 {
		t.Fatalf("cli .exe: %d", st)
	}
	if st, body, _ := instGet(t, env.URL+"/downloads"); st != 200 || !strings.Contains(body, `"name":"crowdvm-linux-amd64"`) || !strings.Contains(body, "/downloads/windows/amd64/crowdvm-linux-amd64") {
		t.Fatalf("list: %d %s", st, body)
	}
	if st, _, _ := instGet(t, env.URL+"/downloads/linux/arm64/crowdvm"); st != 404 {
		t.Fatalf("missing: %d", st)
	}
	if st, _, _ := instGet(t, env.URL+"/downloads/linux/amd64/..%2Fsecret"); st != 404 {
		t.Fatalf("traversal: %d", st)
	}
	if st, body, _ := instGet(t, env.URL+"/api/crowdvm/cli/download?platform=darwin-arm64"); st != 200 || body != "bin:crowdvm_v1_darwin_arm64/crowdvm" {
		t.Fatalf("cli download: %d %s", st, body)
	}
	if st, _, _ := instGet(t, env.URL+"/api/crowdvm/cli/download?platform=plan9-x64"); st != 400 {
		t.Fatalf("bad platform: %d", st)
	}
}
