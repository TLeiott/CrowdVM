package guest

import (
	"os"
	"testing"
)

// bugs-r13-opus 1: adopted.json (the adopted sandbox's config with its API
// key and env secrets) stayed on the disk and went on in snapshots and
// share codes. A boot that is not from the warm config drive removes it.
func TestR13ClearAdoptedRemovesSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(adoptedPath(dir), []byte(`{"sandboxId":"sb1","token":"secret","boot":"create"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadAdopted(dir); !ok {
		t.Fatal("setup: adopted config not loadable")
	}
	if err := ClearAdopted(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(adoptedPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("adopted.json still there: %v", err)
	}
	if err := ClearAdopted(dir); err != nil { // no file: fine
		t.Fatal(err)
	}
}
