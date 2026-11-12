//go:build linux

package guest

import (
	"os"
	"path/filepath"
	"testing"
)

// bugs-r15-opus 4: a disk that already booted unencrypted (its image root
// has a machine-id) must not get a data partition hiding its data.
func TestR15BootedPlain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machine-id")
	if bootedPlain(p) {
		t.Fatal("missing machine-id counts as booted")
	}
	os.WriteFile(p, []byte("\n"), 0o644) // the image ships it empty
	if bootedPlain(p) {
		t.Fatal("an empty machine-id (fresh image) counts as booted")
	}
	os.WriteFile(p, []byte("0123456789abcdef0123456789abcdef\n"), 0o644)
	if !bootedPlain(p) {
		t.Fatal("a root that ran as a plain system is not recognized")
	}
}
