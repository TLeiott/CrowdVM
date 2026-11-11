package guest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestPlaybookMerge(t *testing.T) {
	got := mergePlaybook([]string{"/b", "/c", "/b"}, []string{"/a", "/c", "", "/d"})
	if !slices.Equal(got, []string{"/b", "/c", "/a", "/d"}) {
		t.Fatalf("merge: %v", got)
	}
	long := make([]string, playbookMax+5)
	for i := range long {
		long[i] = "/f" + string(rune('a'+i%26)) + string(rune(i))
	}
	if n := len(mergePlaybook(long, []string{"/x"})); n != playbookMax {
		t.Fatalf("cap: %d", n)
	}
	for p, skip := range map[string]bool{"/usr/bin/node": false, "/home/user/app/x.js": false, "/proc/1/maps": true,
		"/tmp/x": true, "/home/user/.cache/a": true, "/home/user/.crowdvm/playbook.json": true, "/a (deleted)": true} {
		if playbookSkip(p) != skip {
			t.Errorf("skip %s = %v", p, !skip)
		}
	}
	if !fromSnapshot(bootState{BootID: "x"}, proto.BootResume) || fromSnapshot(bootState{}, proto.BootCreate) ||
		fromSnapshot(bootState{BootID: "x"}, proto.BootRecover) {
		t.Fatal("fromSnapshot")
	}
}

func TestPrefetch(t *testing.T) {
	dir := t.TempDir()
	var list []string
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, n)
		os.WriteFile(p, make([]byte, 1000), 0o644)
		list = append(list, p)
	}
	list = append(list, filepath.Join(dir, "missing"), dir)
	a := &Agent{}
	if n, b := a.prefetch(context.Background(), list); n != 3 || b != 3000 {
		t.Fatalf("prefetch: %d files %d bytes", n, b)
	}
}
