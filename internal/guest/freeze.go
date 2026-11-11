package guest

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// freezer FIFREEZEs the writable block filesystems for a consistent disk
// image (stop: the node kills the VM right after; live snapshot: thaw
// follows). While frozen the agent must not write to disk: the handlers only
// touch memory, and an auto-thaw timer guards against a lost thaw.
type freezer struct {
	mu     sync.Mutex
	frozen []string // mount points, in freeze order
	timer  *time.Timer
}

const (
	fiFreeze = 0xC0045877 // _IOWR('X', 119, int)
	fiThaw   = 0xC0045878 // _IOWR('X', 120, int)
)

// freezableMounts lists writable mounts of block-device filesystems, deepest
// first (children are frozen before their parents).
func freezableMounts() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	ok := map[string]bool{"ext4": true, "ext3": true, "ext2": true, "xfs": true, "btrfs": true, "vfat": false}
	seenDev := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// id parent major:minor root mountpoint opts ... - fstype source superopts
		l := sc.Text()
		pre, post, found := strings.Cut(l, " - ")
		if !found {
			continue
		}
		a, b := strings.Fields(pre), strings.Fields(post)
		if len(a) < 6 || len(b) < 2 {
			continue
		}
		mp, opts, fstype, src := unescapeMount(a[4]), a[5], b[0], b[1]
		if !ok[fstype] || !strings.HasPrefix(src, "/dev/") || strings.Split(opts, ",")[0] == "ro" {
			continue
		}
		if seenDev[a[2]] { // bind mounts of the same filesystem
			continue
		}
		seenDev[a[2]] = true
		out = append(out, mp)
	}
	// deepest first
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if strings.Count(out[j], "/") > strings.Count(out[i], "/") || (out[i] == "/" && out[j] != "/") {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// freeze freezes (idempotent) and arms the auto-thaw (default 60 s).
func (z *freezer) freeze(a *Agent, timeoutSec int) ([]string, error) {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.timer != nil {
		z.timer.Stop()
	}
	var errs []string
	if len(z.frozen) == 0 {
		for _, mp := range freezableMounts() {
			if err := ioctlMount(mp, fiFreeze); err != nil {
				if errors.Is(err, unix.EBUSY) { // already frozen by someone else
					continue
				}
				errs = append(errs, mp+": "+err.Error())
				continue
			}
			z.frozen = append(z.frozen, mp)
		}
	}
	z.timer = time.AfterFunc(time.Duration(timeoutSec)*time.Second, func() {
		if m := z.thaw(); len(m) > 0 {
			a.log.Printf("fsfreeze: auto-thawed %v after %ds", m, timeoutSec)
		}
	})
	out := append([]string(nil), z.frozen...)
	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// thaw thaws everything this agent froze (reverse order).
func (z *freezer) thaw() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.timer != nil {
		z.timer.Stop()
		z.timer = nil
	}
	out := []string{}
	for i := len(z.frozen) - 1; i >= 0; i-- {
		if err := ioctlMount(z.frozen[i], fiThaw); err == nil || errors.Is(err, unix.EINVAL) {
			out = append(out, z.frozen[i])
		}
	}
	z.frozen = nil
	return out
}

func ioctlMount(mp string, req uint) error {
	fd, err := unix.Open(mp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), 0)
	if e != 0 {
		return e
	}
	return nil
}
