package image

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteISO(t *testing.T) {
	files := []ISOFile{
		{"user-data", []byte("#cloud-config\n{}\n")},
		{"meta-data", []byte("instance-id: x\n")},
		{"crowdvm-guest", bytes.Repeat([]byte{0xAB}, 5000)},
		{"empty", nil},
	}
	var buf bytes.Buffer
	if err := WriteISO(&buf, "CROWDVM", files); err != nil {
		t.Fatal(err)
	}
	img := buf.Bytes()
	if len(img)%2048 != 0 || string(img[16*2048+1:16*2048+6]) != "CD001" {
		t.Fatal("not an ISO9660 image")
	}
	pvd := img[16*2048:]
	if got := strings.TrimSpace(string(pvd[40:72])); got != "CROWDVM" {
		t.Fatalf("label %q", got)
	}
	if total := binary.LittleEndian.Uint32(pvd[80:]); int(total)*2048 != len(img) {
		t.Fatalf("volume size %d sectors, image %d bytes", total, len(img))
	}
	// Walk the root directory ourselves.
	root := pvd[156:]
	lba, size := binary.LittleEndian.Uint32(root[2:]), binary.LittleEndian.Uint32(root[10:])
	dir := img[lba*2048 : lba*2048+size]
	got := map[string][]byte{}
	for off := 0; off < len(dir); {
		n := int(dir[off])
		if n == 0 {
			off = (off/2048 + 1) * 2048
			continue
		}
		r := dir[off : off+n]
		name := string(r[33 : 33+int(r[32])])
		if r[25]&2 == 0 {
			l, s := binary.LittleEndian.Uint32(r[2:]), binary.LittleEndian.Uint32(r[10:])
			got[name] = img[l*2048 : l*2048+s]
		}
		off += n
	}
	for _, f := range files {
		if !bytes.Equal(got[f.Name], f.Data) {
			t.Errorf("%s: content mismatch (%d bytes)", f.Name, len(got[f.Name]))
		}
	}
	// Cross-check with xorriso when available.
	if x, err := exec.LookPath("xorriso"); err == nil {
		p := filepath.Join(t.TempDir(), "t.iso")
		os.WriteFile(p, img, 0o644)
		out, err := exec.Command(x, "-indev", p, "-ls", "/").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "'user-data'") || !strings.Contains(string(out), "'crowdvm-guest'") {
			t.Fatalf("xorriso: %v\n%s", err, out)
		}
	}
}

func TestUserData(t *testing.T) {
	ud, err := UserData("full")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ud, []byte("#cloud-config\n")) {
		t.Fatal("missing #cloud-config header")
	}
	var cfg struct {
		WriteFiles []struct{ Path, Permissions string } `json:"write_files"`
		Runcmd     [][]string                           `json:"runcmd"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(ud, []byte("#cloud-config\n")), &cfg); err != nil {
		t.Fatal(err)
	}
	perms := map[string]string{}
	for _, f := range cfg.WriteFiles {
		perms[f.Path] = f.Permissions
	}
	for p, want := range map[string]string{
		guestDir + "/scripts/build.sh":                                        "0755",
		guestDir + "/rootfs/minimal/usr/local/lib/crowdvm/guest-prepare":      "0755",
		guestDir + "/rootfs/minimal/etc/systemd/system/crowdvm-guest.service": "0644",
	} {
		if perms[p] != want {
			t.Errorf("%s: perms %q, want %q", p, perms[p], want)
		}
	}
	if len(cfg.Runcmd) != 1 || cfg.Runcmd[0][2] != "full" {
		t.Errorf("runcmd %v", cfg.Runcmd)
	}
}

func TestParseResultAndList(t *testing.T) {
	dir := t.TempDir()
	res := filepath.Join(dir, "r.txt")
	os.WriteFile(res, []byte("node=24.1.0\r\nrustc=1.90.0\r\n"+okMarker+"\r\n"), 0o644)
	tools, err := parseResult(res)
	if err != nil || len(tools) != 2 || tools[0] != (Tool{"node", "24.1.0"}) {
		t.Fatalf("tools %v err %v", tools, err)
	}
	os.WriteFile(res, []byte("x=1\n"+failMarker+" line 3: false\n"), 0o644)
	if _, err := parseResult(res); err == nil {
		t.Fatal("want failure")
	}

	for _, id := range []string{"b", "a", "orphan"} {
		js, _ := json.Marshal(Metadata{ID: id})
		os.WriteFile(filepath.Join(dir, id+".json"), js, 0o644)
		if id != "orphan" {
			os.WriteFile(filepath.Join(dir, id+".qcow2"), nil, 0o644)
		}
	}
	list, err := List(dir)
	if err != nil || len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("list %+v err %v", list, err)
	}
}

func TestDefaults(t *testing.T) {
	if DefaultID("full") != "ubuntu-24.04-crowdvm-v4" || DefaultID("minimal") != "ubuntu-24.04-crowdvm-minimal-v4" {
		t.Fatal(DefaultID("full"), DefaultID("minimal"))
	}
	o := Options{Profile: "bogus", CloudImage: "x", OutDir: "y"}
	if o.defaults() == nil {
		t.Fatal("bad profile accepted")
	}
}
