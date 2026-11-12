package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bugs-r13-opus 12: a killed qemu reports its signal, not the overlay
// "Formatting" lines, and no absolute path of the node.
func TestExitDetailNoFormattingLines(t *testing.T) {
	log := filepath.Join(t.TempDir(), "qemu.log")
	os.WriteFile(log, []byte("--- launch 2026-01-01T00:00:00Z: qemu -drive file=/home/tl/.local/share/x/n1/vm/disks/a.qcow2\n"+
		"Formatting '/home/tl/.local/share/x/n1/vm/disks/bx_1-efffde64.qcow2', fmt=qcow2 cluster_size=65536 extended_l2=off compression_type=zlib size=67645734912 backing_file=/home/tl/.local/share/x/n1/vm/disks/bx_1-0719fa5c.qcow2 backing_fmt=qcow2 lazy_refcounts=off refcount_bits=16\n"+
		"qemu-system-x86_64: warning: host doesn't support requested feature at /home/tl/.cache/x\n"), 0o600)

	cmd := exec.Command("sh", "-c", "kill -9 $$")
	werr := cmd.Run()
	r := &run{log: log, waited: true, waitErr: werr}
	got := exitDetail(r)
	if !strings.Contains(got, "killed by signal: killed") {
		t.Fatalf("no signal in %q", got)
	}
	for _, bad := range []string{"Formatting", "backing_file", "size=", "/home/tl", "qcow2"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "warning: host doesn't support") {
		t.Fatalf("qemu's own line dropped: %q", got)
	}

	exit := exec.Command("sh", "-c", "exit 3").Run()
	if got := exitDetail(&run{log: filepath.Join(t.TempDir(), "none"), waited: true, waitErr: exit}); got != ": exit status 3" {
		t.Fatalf("exit status: %q", got)
	}
	if got := exitDetail(&run{log: filepath.Join(t.TempDir(), "none")}); got != "" { // adopted run: nothing known
		t.Fatalf("adopted: %q", got)
	}
}
