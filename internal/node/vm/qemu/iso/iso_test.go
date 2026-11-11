package iso

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// readTree parses the root directory of the descriptor at lba (16 = primary,
// 17 = Joliet) and returns name -> content.
func readTree(t *testing.T, img []byte, lba int) map[string]string {
	t.Helper()
	vd := img[lba*sector:]
	if string(vd[1:6]) != "CD001" {
		t.Fatalf("no volume descriptor at %d", lba)
	}
	root := vd[156:]
	ext, size := binary.LittleEndian.Uint32(root[2:]), binary.LittleEndian.Uint32(root[10:])
	dir := img[int(ext)*sector : int(ext)*sector+int(size)]
	out := map[string]string{}
	for off := 0; off < len(dir); {
		l := int(dir[off])
		if l == 0 {
			off = (off/sector + 1) * sector
			continue
		}
		r := dir[off : off+l]
		off += l
		name := r[33 : 33+int(r[32])]
		if len(name) == 1 && name[0] <= 1 {
			continue
		}
		if lba == 17 {
			u := make([]uint16, len(name)/2)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(name[2*i:])
			}
			name = []byte(string(utf16.Decode(u)))
		}
		e, n := binary.LittleEndian.Uint32(r[2:]), binary.LittleEndian.Uint32(r[10:])
		out[string(name)] = string(img[int(e)*sector : int(e)*sector+int(n)])
	}
	return out
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	big := bytes.Repeat([]byte("x"), 5000)
	if err := os.WriteFile(bin, big, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	files := []File{
		{Name: "config.json", Data: []byte(`{"a":1}`)},
		{Name: "crowdvm-guest", Path: bin},
		{Name: "empty", Data: nil},
	}
	if err := Write(&buf, "CROWDVM", files); err != nil {
		t.Fatal(err)
	}
	img := buf.Bytes()
	if len(img)%sector != 0 {
		t.Fatalf("size %d not sector aligned", len(img))
	}
	if got := strings.TrimSpace(string(img[16*sector+40 : 16*sector+72])); got != "CROWDVM" {
		t.Fatalf("label %q", got)
	}
	j := readTree(t, img, 17)
	if j["config.json"] != `{"a":1}` || j["crowdvm-guest"] != string(big) || len(j) != 3 {
		t.Fatalf("joliet tree wrong: %v", keys(j))
	}
	p := readTree(t, img, 16)
	if _, ok := p["CROWDVM_GUEST.;1"]; !ok || p["CONFIG.JSON;1"] != `{"a":1}` {
		t.Fatalf("primary tree wrong: %v", keys(p))
	}

	// Cross-check with independent readers when installed.
	f := filepath.Join(dir, "x.iso")
	os.WriteFile(f, img, 0o644)
	if blkid, err := exec.LookPath("blkid"); err == nil {
		out, _ := exec.Command(blkid, "-p", "-o", "value", "-s", "LABEL", f).Output()
		if strings.TrimSpace(string(out)) != "CROWDVM" {
			t.Fatalf("blkid label %q", out)
		}
	}
	if bsdtar, err := exec.LookPath("bsdtar"); err == nil {
		out, err := exec.Command(bsdtar, "-tf", f).CombinedOutput()
		if err != nil {
			t.Fatalf("bsdtar: %v %s", err, out)
		}
		for _, want := range []string{"config.json", "crowdvm-guest", "empty"} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("bsdtar listing %q lacks %s", out, want)
			}
		}
	}
}

func TestBadNames(t *testing.T) {
	for _, fs := range [][]File{
		{{Name: "a/b"}},
		{{Name: ""}},
		{{Name: "a-b"}, {Name: "a_b"}}, // collide in the primary tree
	} {
		if err := Write(&bytes.Buffer{}, "X", fs); err == nil {
			t.Fatalf("expected error for %v", fs)
		}
	}
}

func keys(m map[string]string) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	return k
}
