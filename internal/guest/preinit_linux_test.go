//go:build linux

package guest

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// / becomes an overlay: its fstab line must go (systemd would fsck and remount
// it with ext4 options); /boot and /boot/efi stay.
func TestDropRootFromFstab(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fstab")
	in := "LABEL=cloudimg-rootfs\t/\text4\tdiscard,commit=30,errors=remount-ro\t0 1\nLABEL=BOOT\t/boot\text4\tdefaults\t0 2\nLABEL=UEFI\t/boot/efi\tvfat\tumask=0077\t0 1\n"
	if err := os.WriteFile(p, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dropRootFromFstab(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	out := string(b)
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == "/" {
			t.Fatalf("root still mounted from fstab:\n%s", out)
		}
	}
	if !strings.Contains(out, "\t/boot\t") || !strings.Contains(out, "/boot/efi") {
		t.Fatalf("lost /boot:\n%s", out)
	}
	if err := dropRootFromFstab(p); err != nil { // idempotent
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(p); string(b2) != out {
		t.Fatal("second run changed the file")
	}
}

// An interrupted re-encryption shows in luksDump's Requirements line
// (cryptsetup 2.8 output); a finished one does not.
func TestReencryptPending(t *testing.T) {
	busy := "LUKS header information\nVersion:       \t2\nFlags:       \t(no flags)\nRequirements:\tonline-reencrypt-v2 \n\nData segments:\n"
	done := "LUKS header information\nVersion:       \t2\nFlags:       \t(no flags)\n\nData segments:\n  0: crypt\n"
	if !reencryptPending(busy) || reencryptPending(done) || reencryptPending("") {
		t.Fatal("reencryptPending")
	}
}

func TestProgressLine(t *testing.T) {
	l := `{"device":"/dev/vda2","device_bytes":"1073741824","device_size":"4294967296","speed":"491875832","eta_ms":"4331","time_ms":"2182"}`
	if got := progressLine(l); got != "re-encrypting: 25% of 4.3 GB, 492 MB/s, 4s left" {
		t.Fatalf("progressLine: %q", got)
	}
	if progressLine("Key slot 0 unlocked.") != "" || progressLine(`{"device_size":"0"}`) != "" {
		t.Fatal("not a progress line")
	}
}

// A passphrase change cut off after the new passphrase was added: both
// open. The next boot removes the old one and leaves the volume alone.
func TestRekeyDataFinishesACutOffPassphraseChange(t *testing.T) {
	if _, err := exec.LookPath("cryptsetup"); err != nil {
		t.Skip("no cryptsetup")
	}
	dir := t.TempDir()
	vol := filepath.Join(dir, "vol")
	old, key := bytes.Repeat([]byte{1}, 64), bytes.Repeat([]byte{2}, 64)
	if err := os.WriteFile(vol, make([]byte, 32<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(old, "cryptsetup", "luksFormat", "--batch-mode", "--type", "luks2", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000",
		"--sector-size", "4096", "--key-file=-", vol); err != nil {
		t.Skip(err) // no permission here
	}
	if err := runKeys([][]byte{old, key}, "cryptsetup", "luksAddKey", "--batch-mode", "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000",
		"--key-file=/proc/self/fd/3", vol, "/proc/self/fd/4"); err != nil {
		t.Fatal(err)
	}
	if !opens(vol, old) || !opens(vol, key) {
		t.Fatal("setup: both passphrases should open")
	}
	if err := rekeyData(vol, key, old, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	if opens(vol, old) || !opens(vol, key) {
		t.Fatalf("after rekeyData: old opens %v, new opens %v", opens(vol, old), opens(vol, key))
	}
}
