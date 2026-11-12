//go:build linux

package qemu

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// A node restarted while one of its VMs' qemu had died. adoptAll finds no QMP
// socket for it (systemd dropped the hung-up socketpair ends), marks the VM
// `error` and moves on. The boot ticket the node stored in the fd store is a
// memfd: systemd never sees a hangup on it, so it stays in the store, and
// nothing names it again once the VM is relaunched under a new pid (a recover
// of that VM: `Restart` → `launch`; `remove` later only knows the new pid).
// bugs-r15-sonnet 3 promised "the entries named after a qemu pid are removed
// when that qemu is gone".
func TestBughuntR16AdoptOfDeadQemuForgetsItsFDStoreEntries(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "notify")
	pc, err := net.ListenPacket("unixgram", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	t.Setenv("NOTIFY_SOCKET", sock)
	t.Setenv("CROWDVM_FDSTORE", "1")
	fds := openFDStore()
	if fds == nil {
		t.Fatal("no fd store")
	}
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Skip(err)
	}
	dead := c.Process.Pid // exited and reaped: no such process now
	vmdir := filepath.Join(dir, "vms", "bx_dead")
	_ = os.MkdirAll(vmdir, 0o755)
	rec := vmRecord{Spec: proto.VMSpec{SandboxID: "bx_dead"}, Disk: "disks/bx_dead.qcow2", QMPNet: "fd", PID: dead}
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(filepath.Join(vmdir, "vm.json"), b, 0o600)
	d := &Driver{opt: Options{DataDir: dir, Logf: func(string, ...any) {}}, vms: map[string]*machine{}, fds: fds}

	d.adoptAll()

	if m := d.vms["bx_dead"]; m == nil || m.st.State != proto.VMError {
		t.Fatalf("setup: the VM should be in error after the failed adoption: %+v", m)
	}
	_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var got []string
	buf := make([]byte, 4096)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		got = append(got, strings.ReplaceAll(string(buf[:n]), "\n", " "))
	}
	found := false
	for _, g := range got {
		if strings.Contains(g, "FDSTOREREMOVE=1") && strings.Contains(g, "FDNAME=ticket-") {
			found = true
		}
	}
	if !found {
		t.Errorf("the ticket memfd of the dead qemu (pid %d) stays in systemd's fd store for good (messages sent to systemd: %q)", dead, got)
	}
}
