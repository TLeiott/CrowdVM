package guest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestBughuntR4SolMoveViaSymlinkPreservesSource(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	e.mustOK("PUT", "/v1/files", map[string]any{"path": "a/b/keep.txt", "content": "irreplaceable"})
	if err := os.Symlink(filepath.Join(e.home, "a"), filepath.Join(e.home, "alias")); err != nil {
		t.Fatal(err)
	}
	status, response := e.do("POST", "/v1/files/move", map[string]any{"source": "alias/b", "destination": "a", "overwrite": true})
	b, err := os.ReadFile(filepath.Join(e.home, "a/b/keep.txt"))
	if err != nil || string(b) != "irreplaceable" {
		t.Fatalf("move via alias deleted source before failing: status=%d response=%v content=%q err=%v", status, response, b, err)
	}
}
