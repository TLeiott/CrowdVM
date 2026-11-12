//go:build linux

package qemu

// systemd's file descriptor store (sd_notify FDSTORE=1). A VM's QMP monitor
// and NIC are socketpairs handed to qemu at launch; the node keeps its ends
// here, so no socket path exists that another process of the node's user
// could connect to. With path sockets, a node user could stop the node,
// connect to qmp.sock and dump the guest's memory (secure layer, red team
// round 3). A restarted node gets the sockets back (LISTEN_FDS) and adopts
// its VMs as before. systemd drops a stored socket by itself when qemu exits
// (it polls for hangups).
//
// Used only when the unit enables it (CROWDVM_FDSTORE=1 next to
// FileDescriptorStoreMax=; see internal/node/service): otherwise systemd
// would silently drop the sockets and a restart would lose every VM.

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type fdStore struct {
	addr string
	mu   sync.Mutex
	got  map[string]*os.File // passed back at start, by name
}

func openFDStore() *fdStore {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" || os.Getenv("CROWDVM_FDSTORE") != "1" {
		return nil
	}
	s := &fdStore{addr: sock, got: map[string]*os.File{}}
	if pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID")); pid == os.Getpid() {
		n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
		names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
		for i := 0; i < n; i++ {
			fd := 3 + i
			syscall.CloseOnExec(fd)
			name := ""
			if i < len(names) {
				name = names[i]
			}
			if old := s.got[name]; old != nil { // a stale duplicate
				old.Close()
			}
			s.got[name] = os.NewFile(uintptr(fd), name)
		}
	}
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(k)
	}
	return s
}

func (s *fdStore) notify(msg string, f *os.File) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var oob []byte
	if f != nil {
		oob = syscall.UnixRights(int(f.Fd()))
	}
	return syscall.Sendmsg(fd, []byte(msg), oob, &syscall.SockaddrUnix{Name: s.addr}, 0)
}

// put stores f under name, replacing what was stored under it.
func (s *fdStore) put(name string, f *os.File) error {
	_ = s.remove(name)
	return s.notify("FDSTORE=1\nFDNAME="+name, f)
}

func (s *fdStore) remove(name string) error {
	return s.notify("FDSTOREREMOVE=1\nFDNAME="+name, nil)
}

// take returns the socket passed back for name at start (nil if none).
func (s *fdStore) take(name string) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.got[name]
	delete(s.got, name)
	return f
}

// socketPair returns two connected stream sockets (close-on-exec).
func socketPair() (*os.File, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "node"), os.NewFile(uintptr(fds[1]), "qemu"), nil
}
