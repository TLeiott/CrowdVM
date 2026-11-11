package guest

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestFiles(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	// write + read utf8 / base64, parents created
	w := e.mustOK("PUT", "/v1/files", map[string]any{"path": "a/b/hello.txt", "content": "hello world\nsecond hello\n"})
	if w["type"] != "file.written" || w["path"] != "/home/user/a/b/hello.txt" || num(w["size"]) != 25 {
		t.Fatalf("write %v", w)
	}
	bin := []byte{0, 1, 2, 255}
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "/home/user/a/bin.dat", "content": base64.StdEncoding.EncodeToString(bin), "encoding": "base64"})
	r := e.mustOK("GET", "/v1/files?path="+q("a/b/hello.txt"), nil)
	if r["content"] != "hello world\nsecond hello\n" || r["encoding"] != "utf8" {
		t.Fatalf("read %v", r)
	}
	r = e.mustOK("GET", "/v1/files?encoding=base64&path="+q("/home/user/a/bin.dat"), nil)
	if r["content"] != base64.StdEncoding.EncodeToString(bin) {
		t.Fatalf("read b64 %v", r)
	}
	// /tmp maps to the agent's tmp dir
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "/tmp/x/t.txt", "content": "tmp"})
	if b, _ := os.ReadFile(filepath.Join(e.a.tmp, "x/t.txt")); string(b) != "tmp" {
		t.Fatalf("tmp mapping: %q", b)
	}
	// invalid paths
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "/home/userx/a"} {
		if st, m := e.do("GET", "/v1/files?path="+q(p), nil); st != 400 || m["code"] != "invalid_path" {
			t.Fatalf("%s: %d %v", p, st, m)
		}
	}
	// symlink escaping the home is refused; the link itself can be deleted
	os.Symlink("/etc", filepath.Join(e.home, "etc-link"))
	if st, _ := e.do("GET", "/v1/files?path=etc-link/hostname", nil); st != 400 {
		t.Fatalf("symlink escape: %d", st)
	}
	if st, _ := e.do("GET", "/v1/files?path=missing.txt", nil); st != 404 {
		t.Fatalf("missing: %d", st)
	}
	// list
	l := e.mustOK("GET", "/v1/files/list?path=a", nil)
	files := l["files"].([]any)
	if len(files) != 2 || files[0].(map[string]any)["name"] != "b" || files[0].(map[string]any)["isDir"] != true ||
		files[1].(map[string]any)["path"] != "/home/user/a/bin.dat" || num(files[1].(map[string]any)["size"]) != 4 {
		t.Fatalf("list %v", l)
	}
	l = e.mustOK("GET", "/v1/files/list", nil)
	if l["path"] != "/home/user" {
		t.Fatalf("default list %v", l)
	}
	// info + permissions + folder
	i := e.mustOK("GET", "/v1/files/info?path=etc-link", nil)
	if i["isSymlink"] != true || i["type"] != "file.info" {
		t.Fatalf("info %v", i)
	}
	p := e.mustOK("POST", "/v1/files/permissions", map[string]any{"path": "a/bin.dat", "mode": "750"})
	if p["mode"] != "0750" || p["type"] != "file.permissions_set" {
		t.Fatalf("perm %v", p)
	}
	if st, _ := e.do("POST", "/v1/files/permissions", map[string]any{"path": "a/bin.dat", "mode": "99"}); st != 400 {
		t.Fatal("bad mode accepted")
	}
	f := e.mustOK("POST", "/v1/files/folder", map[string]any{"path": "new/deep/dir", "mode": "700"})
	if f["type"] != "folder.created" || f["isDir"] != true || f["mode"] != "0700" {
		t.Fatalf("folder %v", f)
	}
	e.mustOK("POST", "/v1/files/folder", map[string]any{"path": "new/deep/dir"}) // exists: fine
	// search + find
	s := e.mustOK("GET", "/v1/files/search?pattern="+q("*.txt"), nil)
	if got := s["files"].([]any); len(got) != 1 || got[0] != "/home/user/a/b/hello.txt" {
		t.Fatalf("search %v", s)
	}
	fd := e.mustOK("GET", "/v1/files/find?path=a&pattern=hello", nil)
	ms := fd["matches"].([]any)
	if len(ms) != 2 || num(ms[1].(map[string]any)["line"]) != 2 || ms[0].(map[string]any)["file"] != "/home/user/a/b/hello.txt" {
		t.Fatalf("find %v", fd)
	}
	fd = e.mustOK("GET", "/v1/files/find?regex=true&pattern="+q("^sec(ond)? h"), nil)
	if len(fd["matches"].([]any)) != 1 {
		t.Fatalf("find regex %v", fd)
	}
	if st, m := e.do("GET", "/v1/files/find?regex=true&pattern="+q("(("), nil); st != 400 || m["code"] != "invalid_regex" {
		t.Fatalf("bad regex %d %v", st, m)
	}
	// replace
	rp := e.mustOK("POST", "/v1/files/replace", map[string]any{"paths": []string{"a/b/hello.txt", "nope.txt"}, "pattern": "hello", "newValue": "bye"})
	res := rp["results"].([]any)
	if res[0].(map[string]any)["success"] != true || num(res[0].(map[string]any)["replacements"]) != 2 || res[1].(map[string]any)["success"] != false {
		t.Fatalf("replace %v", rp)
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, "a/b/hello.txt")); string(b) != "bye world\nsecond bye\n" {
		t.Fatalf("replaced content %q", b)
	}
	// move (+ overwrite) across home and /tmp
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "m1.txt", "content": "1"})
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "m2.txt", "content": "2"})
	if st, m := e.do("POST", "/v1/files/move", map[string]any{"source": "m1.txt", "destination": "m2.txt"}); st != 409 {
		t.Fatalf("move without overwrite %d %v", st, m)
	}
	mv := e.mustOK("POST", "/v1/files/move", map[string]any{"source": "m1.txt", "destination": "/tmp/moved/m.txt", "overwrite": true})
	if mv["destination"] != "/tmp/moved/m.txt" || mv["path"] != "/home/user/m1.txt" {
		t.Fatalf("move %v", mv)
	}
	e.mustOK("POST", "/v1/files/move", map[string]any{"source": "/tmp/moved/m.txt", "destination": "m2.txt", "overwrite": true})
	if b, _ := os.ReadFile(filepath.Join(e.home, "m2.txt")); string(b) != "1" {
		t.Fatalf("moved content %q", b)
	}
	// delete
	if st, _ := e.do("DELETE", "/v1/files?path=a", nil); st != 400 {
		t.Fatal("dir delete without recursive")
	}
	e.mustOK("DELETE", "/v1/files?path=etc-link", nil)
	if _, err := os.Stat("/etc"); err != nil {
		t.Fatal("symlink target deleted")
	}
	e.mustOK("DELETE", "/v1/files?recursive=true&path=new", nil)
	if st, _ := e.do("DELETE", "/v1/files?recursive=true&path=/home/user", nil); st != 400 {
		t.Fatal("home deleted")
	}
	if _, err := os.Stat(filepath.Join(e.home, "new")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
}

func TestArtifactsAndRaw(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "out/r.txt", "content": "report"})
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "out/sub/x.txt", "content": "x"})
	resp := e.raw("GET", "/v1/artifacts?path=out/r.txt", nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "report" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("artifact %d %q", resp.StatusCode, b)
	}
	resp = e.raw("GET", "/v1/artifacts?path=out", nil)
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	resp.Body.Close()
	sort.Strings(names)
	if strings.Join(names, ",") != "out/,out/r.txt,out/sub/,out/sub/x.txt" {
		t.Fatalf("tar %v", names)
	}
	// raw upload / download (no size limit)
	big := strings.Repeat("z", 6<<20)
	resp = e.raw("PUT", "/v1/files/raw?path="+q("big/file.bin"), []byte(big))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("raw put %d", resp.StatusCode)
	}
	if st, m := e.do("GET", "/v1/files?path=big/file.bin", nil); st != 400 || m["code"] != "file_too_large" {
		t.Fatalf("5 MiB limit %d %v", st, m)
	}
	resp = e.raw("GET", "/v1/files/raw?path="+q("big/file.bin"), nil)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) != len(big) {
		t.Fatalf("raw get %d bytes", len(b))
	}
}

func TestTunnel(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	dial := func(p int) (net.Conn, error) {
		c, err := net.Dial("tcp", strings.TrimPrefix(e.srv.URL, "http://"))
		if err != nil {
			return nil, err
		}
		return mux.DialUpgrade(c, "guest", "/v1/tunnel?port="+strconv.Itoa(p), map[string][]string{"Authorization": {"Bearer " + testToken}})
	}
	c, err := dial(port)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.Close()
	// closed port -> 502 before upgrading
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if _, err := dial(dead); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("dead port: %v", err)
	}
}

func TestPrepareStop(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	mk := func(p, content string) {
		full := filepath.Join(e.home, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	mk(".boatignore", "*.log\n!keep.log\n")
	mk("app/.crowdvmignore", "node_modules/\n/dist\nbuild\n")
	mk("app/.boatignore", "src\n") // lower priority than .crowdvmignore: ignored
	mk("app/node_modules/x/index.js", "x")
	mk("app/dist/bundle.js", "x")
	mk("app/src/dist/keep.js", "x") // /dist is anchored to app/
	mk("app/src/main.go", "x")
	mk("app/build", "file named build")
	mk("app/.git/objects/build", "git is kept")
	mk("app/debug.log", "x")
	mk("keep.log", "x")
	mk(".cache/pip/x", "x")
	mk("notes.txt", "x")
	os.MkdirAll(filepath.Join(e.a.tmp, "junk"), 0o755)
	os.WriteFile(filepath.Join(e.a.tmp, "junk/f"), []byte("x"), 0o644)

	m := e.mustOK("POST", "/v1/internal/prepare-stop", nil)
	if num(m["removed"]) == 0 {
		t.Fatalf("prepare-stop %v", m)
	}
	gone := []string{"app/node_modules", "app/dist", "app/build", "app/debug.log", ".cache/pip"}
	kept := []string{"app/src/dist/keep.js", "app/src/main.go", "app/.git/objects/build", "keep.log", "notes.txt", ".boatignore", ".cache"}
	for _, p := range gone {
		if _, err := os.Lstat(filepath.Join(e.home, p)); !os.IsNotExist(err) {
			t.Errorf("%s not deleted", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(filepath.Join(e.home, p)); err != nil {
			t.Errorf("%s deleted", p)
		}
	}
	if ents, _ := os.ReadDir(e.a.tmp); len(ents) != 0 {
		t.Errorf("tmp not emptied: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(e.data, "history")); err != nil {
		t.Error("agent data removed")
	}
	e.mustOK("POST", "/v1/internal/sync", nil)
}

func TestIgnoreRules(t *testing.T) {
	rules := parseIgnore("/h", "# c\n*.o\n/top\nlogs/\na/**/z\n!keep.o\n\\#hash\nfoo/*.txt\n[ab]?.md\n")
	cases := map[string]bool{
		"/h/x.o": true, "/h/d/e/x.o": true, "/h/keep.o": false, "/h/top": true, "/h/d/top": false,
		"/h/a/z": true, "/h/a/b/c/z": true, "/h/#hash": true, "/h/foo/a.txt": true, "/h/foo/b/a.txt": false,
		"/h/ax.md": true, "/h/cx.md": false, "/h/other": false,
	}
	for p, want := range cases {
		if got := ignored(rules, p, false); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
	if !ignored(rules, "/h/x/logs", true) || ignored(rules, "/h/x/logs", false) {
		t.Error("dir-only rule")
	}
}

func TestSSHKey(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, _ := ssh.NewPublicKey(pub)
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " me@laptop"
	m := e.mustOK("POST", "/v1/sshkey", map[string]any{"key": key})
	if m["type"] != "ssh_key.configured" || m["success"] != true {
		t.Fatalf("sshkey %v", m)
	}
	e.mustOK("POST", "/v1/sshkey", map[string]any{"key": key}) // dedup
	b, _ := os.ReadFile(filepath.Join(e.home, ".ssh/authorized_keys"))
	if strings.Count(string(b), "ssh-ed25519") != 1 {
		t.Fatalf("authorized_keys %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(e.home, ".ssh/authorized_keys")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	for _, k := range []string{"nope", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc"} {
		if st, _ := e.do("POST", "/v1/sshkey", map[string]any{"key": k}); st != 400 {
			t.Fatalf("%q accepted", k)
		}
	}
}

func TestRootPaths(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	if i := e.mustOK("GET", "/v1/files/info?path=/home/user", nil); i["isDir"] != true || i["path"] != "/home/user" {
		t.Fatalf("home info %v", i)
	}
	if st, m := e.do("DELETE", "/v1/files?recursive=true&path=/tmp", nil); st != 400 || m["code"] != "invalid_path" {
		t.Fatalf("tmp delete %d %v", st, m)
	}
	if st, _ := e.do("POST", "/v1/files/move", map[string]any{"source": "/home/user", "destination": "x"}); st != 400 {
		t.Fatal("home moved")
	}
}
