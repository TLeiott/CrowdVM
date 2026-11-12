//go:build linux

package qemu

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The node keeps each VM's QMP socket, NIC socket and boot ticket in
// systemd's fd store, named by the qemu pid ("qmp-<pid>", "net-<pid>",
// "ticket-<pid>", e453353). Nothing ever sends FDSTOREREMOVE for them: not
// when the VM is destroyed, not when qemu exits (a reboot, a recover, a stop).
// Every launch leaves up to three descriptors in the store for good
// (FileDescriptorStorePreserve=yes keeps them even across a stop of the
// unit). The store holds 1024 (FileDescriptorStoreMax): after a few hundred
// boots systemd drops new ones without an error, and the next node restart
// cannot adopt a VM ("its QMP socket was not passed back"): it is killed.
func TestBughuntR15FDStoreKeepsDestroyedVMsSockets(t *testing.T) {
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
	d := &Driver{opt: Options{DataDir: dir}, vms: map[string]*machine{}, fds: fds}
	m := &machine{id: "bx_r15", rec: vmRecord{PID: 4242, QMPNet: "fd"}}
	d.vms[m.id] = m

	d.remove(m) // the sandbox is deleted

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
	for _, name := range []string{"qmp-4242", "net-4242", "ticket-4242"} {
		found := false
		for _, g := range got {
			if strings.Contains(g, "FDSTOREREMOVE=1") && strings.Contains(g, "FDNAME="+name) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s stays in the fd store after its VM is destroyed (messages sent to systemd: %q)", name, got)
		}
	}
}
