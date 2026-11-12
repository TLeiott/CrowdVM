//go:build linux

package guest

import (
	"os"
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
