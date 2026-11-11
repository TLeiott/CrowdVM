package guest

import (
	"os"
	"path/filepath"
	"testing"
)

// A secret file written into a repo folder before the clone survives, and
// the clone's files and .git land next to it (opus-10).
func TestMergeInto(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "clone"), filepath.Join(root, "backend")
	for p, c := range map[string]string{".git/HEAD": "ref", "README.md": "readme", ".env": "from-repo", "cfg/app.yml": "a"} {
		os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755)
		os.WriteFile(filepath.Join(src, p), []byte(c), 0o644)
	}
	os.MkdirAll(filepath.Join(dst, "cfg"), 0o755)
	os.WriteFile(filepath.Join(dst, ".env"), []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(dst, "cfg", "local.yml"), []byte("mine"), 0o600)
	if err := mergeInto(src, dst); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{".git/HEAD": "ref", "README.md": "readme", ".env": "secret", "cfg/app.yml": "a", "cfg/local.yml": "mine"} {
		if b, _ := os.ReadFile(filepath.Join(dst, p)); string(b) != want {
			t.Errorf("%s = %q, want %q", p, b, want)
		}
	}
}

func TestUserSetsHostname(t *testing.T) {
	dir := t.TempDir()
	if userSetsHostname(dir) {
		t.Fatal("empty dir")
	}
	os.MkdirAll(filepath.Join(dir, "multi-user.target.wants"), 0o755)
	unit := filepath.Join(dir, "set-hostname.service")
	os.WriteFile(unit, []byte("[Service]\nType=oneshot\nExecStart=/usr/bin/hostnamectl set-hostname my-sandbox\n"), 0o644)
	if userSetsHostname(dir) {
		t.Fatal("a unit that is not enabled counts")
	}
	os.Symlink(unit, filepath.Join(dir, "multi-user.target.wants", "set-hostname.service"))
	if !userSetsHostname(dir) {
		t.Fatal("Boat's recipe not detected")
	}
}
