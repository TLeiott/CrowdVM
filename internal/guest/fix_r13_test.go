package guest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r13-opus 4: find keeps at most findLineCap bytes of a line.
func TestFindHugeLine(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	dir := filepath.Join(e.home, "big")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 8 MiB single line: a match near the start, one far beyond the cap; then
	// a normal second line.
	line := bytes.Repeat([]byte("a"), 8<<20)
	copy(line[100:], "needle")
	copy(line[4<<20:], "latemark")
	line = append(line, "\nsecond needle\r\n"...)
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), line, 0o644); err != nil {
		t.Fatal(err)
	}
	find := func(pat string) []any {
		m := e.mustOK("GET", "/v1/files/find?path=big&pattern="+q(pat), nil)
		return m["matches"].([]any)
	}
	ms := find("needle")
	if len(ms) != 2 || num(ms[0].(map[string]any)["line"]) != 1 || num(ms[1].(map[string]any)["line"]) != 2 ||
		ms[1].(map[string]any)["content"] != "second needle" || len(ms[0].(map[string]any)["content"].(string)) > maxFindLine {
		t.Fatalf("needle: %v", ms)
	}
	if ms := find("latemark"); len(ms) != 0 { // beyond the cap: not looked at
		t.Fatalf("match beyond the line cap: %v", ms)
	}
}

// bugs-r13-opus 4: the reader drops the rest of a long line as it goes.
func TestReadCappedLine(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	br := bufio.NewReaderSize(strings.NewReader(long+"\nshort\nlast"), 4096)
	l, err := readCappedLine(br, nil, 1000)
	if err != nil || len(l) != 1000 {
		t.Fatalf("long line: %d bytes, %v", len(l), err)
	}
	if l, err = readCappedLine(br, l[:0], 1000); err != nil || string(l) != "short\n" {
		t.Fatalf("second line: %q %v", l, err)
	}
	if l, err = readCappedLine(br, l[:0], 1000); err != io.EOF || string(l) != "last" {
		t.Fatalf("last line: %q %v", l, err)
	}
	if l, err = readCappedLine(br, l[:0], 1000); err != io.EOF || len(l) != 0 {
		t.Fatalf("eof: %q %v", l, err)
	}
}

// bugs-r13-opus 7: a FIFO is refused at once (a reader blocked in open() leaks a thread).
func TestFifoRefused(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	fifo := filepath.Join(e.home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // release a reader that a regression left blocked
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
	cl := &http.Client{Timeout: 3 * time.Second}
	call := func(method, path string, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	for _, p := range []string{"/v1/files?path=fifo", "/v1/files/raw?path=fifo", "/v1/artifacts?path=fifo"} {
		if st, m := call("GET", p, ""); st != 400 || m["code"] != "not_a_regular_file" {
			t.Fatalf("GET %s: %d %v", p, st, m)
		}
	}
	st, m := call("POST", "/v1/files/replace", `{"paths":["fifo"],"pattern":"a","newValue":"b"}`)
	res, _ := m["results"].([]any)
	if st != 200 || len(res) != 1 || res[0].(map[string]any)["success"] != false ||
		!strings.Contains(res[0].(map[string]any)["error"].(string), "not a regular file") {
		t.Fatalf("replace: %d %v", st, m)
	}
	// find and folder download pass a FIFO by
	if err := os.WriteFile(filepath.Join(e.home, "ok.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, m := call("GET", "/v1/files/find?pattern=needle", ""); st != 200 || len(m["matches"].([]any)) != 1 {
		t.Fatalf("find: %d %v", st, m)
	}
	var buf bytes.Buffer
	if err := writeTarGz(&buf, e.home, "home"); err != nil {
		t.Fatalf("tar with a FIFO: %v", err)
	}
	if _, err := io.Copy(io.Discard, mustGz(t, &buf)); err != nil {
		t.Fatal(err)
	}
}

func mustGz(t *testing.T, r io.Reader) io.Reader {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

// bugs-r13-opus 9: files that shrink or grow while they are written keep the archive valid.
func TestTarShrinkGrow(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	dir := filepath.Join(e.home, "art")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := bytes.Repeat([]byte("0123456789abcdef"), 8192) // 128 KiB
	for name, data := range map[string][]byte{"a.bin": orig, "m.txt": orig[:5000], "z.txt": []byte("last")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tarAfterHeader = func(p string) {
		switch filepath.Base(p) {
		case "a.bin":
			os.Truncate(p, 1000)
		case "m.txt":
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			f.WriteString("grown tail")
			f.Close()
		}
	}
	t.Cleanup(func() { tarAfterHeader = nil })

	resp := e.raw("GET", "/v1/artifacts?path=art", nil)
	defer resp.Body.Close()
	tr := tar.NewReader(mustGz(t, resp.Body))
	got := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("archive broken: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("%s: %v", h.Name, err)
		}
		got[h.Name] = b
	}
	if len(got) != 4 || string(got["art/z.txt"]) != "last" { // art/, a.bin, m.txt, z.txt
		t.Fatalf("entries: %v", len(got))
	}
	want := append(append([]byte{}, orig[:1000]...), make([]byte, len(orig)-1000)...)
	if !bytes.Equal(got["art/a.bin"], want) {
		t.Fatalf("shrunk file: %d bytes, want %d zero-padded", len(got["art/a.bin"]), len(want))
	}
	if !bytes.Equal(got["art/m.txt"], orig[:5000]) {
		t.Fatalf("grown file: %d bytes, want 5000", len(got["art/m.txt"]))
	}
}

type failWriter struct{ left int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.left -= len(p); f.left < 0 {
		return 0, errors.New("client gone")
	}
	return len(p), nil
}

// bugs-r13-opus 9: a real write error is returned (the handler then aborts).
func TestTarWriteError(t *testing.T) {
	dir := t.TempDir()
	rnd := make([]byte, 1<<20)
	for i := range rnd {
		rnd[i] = byte(i * 7919 >> 3)
	}
	if err := os.WriteFile(filepath.Join(dir, "r.bin"), rnd, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTarGz(&failWriter{left: 1000}, dir, "d"); err == nil {
		t.Fatal("write error swallowed")
	}
}
