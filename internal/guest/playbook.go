package guest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Startup playbook (LIFE-018, Boat's ~/.ascii/playbook.json). While a
// sandbox starts from a snapshot (resume, fork, deploy of a saved
// snapshot: the disk was booted before), the guest records the files that
// programs open, in first-open order, for playbookWindow after boot
// (fanotify FAN_OPEN on the root filesystem; root only). The run is merged
// into ~/.crowdvm/playbook.json: this run's paths first, then older ones
// it did not open (the newest run counts most), at most playbookMax paths.
// The file is an ordinary file, so forks and deploys carry it.
//
// At the next start from a snapshot the guest first reads the listed files
// (GuestConfig.Playbook from the server, then the disk's own list): their
// blocks are fetched from the snapshot chain (and the node's lazy chain
// cache) before programs ask for them. ready does not wait for this.

const (
	EvPlaybookRecorded = "playbook.recorded" // {"path","paths","recorded"}
	playbookMax        = 20000
	playbookRel        = ".crowdvm/playbook.json"
)

var (
	playbookWindow        = 2 * time.Minute
	prefetchBudget  int64 = 2 << 30   // bytes read by one prefetch
	prefetchMaxFile int64 = 256 << 20 // larger files are skipped
	prefetchWorkers       = 4
)

type playbookFile struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Paths     []string  `json:"paths"`
}

func (a *Agent) playbookPath() string { return filepath.Join(a.home, playbookRel) }

func readPlaybook(p string) []string {
	var f playbookFile
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	return f.Paths
}

// mergePlaybook puts run first, then the older paths it does not contain.
func mergePlaybook(run, old []string) []string {
	seen := make(map[string]bool, len(run))
	out := make([]string, 0, min(len(run)+len(old), playbookMax))
	for _, l := range [][]string{run, old} {
		for _, p := range l {
			if len(out) == playbookMax {
				return out
			}
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// playbookSkip: paths that are not disk content worth recording.
func playbookSkip(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, " (deleted)") {
		return true
	}
	for _, pre := range []string{"/proc/", "/sys/", "/dev/", "/run/", "/tmp/", "/var/tmp/", "/var/lib/crowdvm/",
		"/var/log/journal/", "/home/user/.cache/", "/home/user/" + playbookRel} {
		if strings.HasPrefix(p, pre) || p == strings.TrimSuffix(pre, "/") {
			return true
		}
	}
	return false
}

// playbookRun is this boot's recording (nil when none runs).
type playbookRun struct {
	stop     chan struct{} // closed to end recording early
	stopOnce sync.Once
	done     chan struct{} // closed once the file is written (or nothing was)
	result   map[string]any
}

// startPlaybook runs prefetch and recording for a boot from a snapshot.
func (a *Agent) startPlaybook(ctx context.Context, fromSnapshot bool) {
	if !fromSnapshot || a.unpriv || a.inspect() {
		return
	}
	rec, err := newOpenRecorder()
	if err != nil {
		a.log.Printf("playbook: recorder: %v", err)
	}
	run := &playbookRun{stop: make(chan struct{}), done: make(chan struct{}), result: map[string]any{"recorded": 0}}
	if rec == nil {
		close(run.done)
	}
	a.mu.Lock()
	a.playbook = run
	a.mu.Unlock()
	go func() {
		list := mergePlaybook(a.config().Playbook, readPlaybook(a.playbookPath()))
		if n, bytes := a.prefetch(ctx, list); n > 0 {
			a.log.Printf("playbook: prefetched %d files (%d MiB)", n, bytes>>20)
		}
	}()
	if rec == nil {
		return
	}
	go func() {
		defer close(run.done)
		paths := rec.record(ctx, run.stop, playbookWindow, playbookMax)
		if len(paths) == 0 {
			return
		}
		all := mergePlaybook(paths, readPlaybook(a.playbookPath()))
		b, _ := json.MarshalIndent(playbookFile{Version: 1, UpdatedAt: time.Now().UTC(), Paths: all}, "", " ")
		p := a.playbookPath()
		if err := a.mkdirAll(filepath.Dir(p), 0o755); err != nil {
			a.log.Printf("playbook: %v", err)
			return
		}
		if err := a.writeFileAtomic(p, append(b, '\n'), 0o644, true); err != nil {
			a.log.Printf("playbook: %v", err)
			return
		}
		run.result = map[string]any{"path": a.toVirt(p), "paths": len(all), "recorded": len(paths)}
		a.Emit(EvPlaybookRecorded, run.result)
	}()
}

// POST /v1/internal/playbook/finish: end this boot's recording now (the
// node calls it when the lazy chain download ended) and write the file.
// -> playbook.finished {recording, path?, paths?, recorded}.
func (a *Agent) handlePlaybookFinish(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	run := a.playbook
	a.mu.Unlock()
	if run == nil {
		ok(w, "playbook.finished", map[string]any{"recording": false, "recorded": 0})
		return
	}
	run.stopOnce.Do(func() { close(run.stop) })
	select {
	case <-run.done:
	case <-r.Context().Done():
		return
	case <-time.After(30 * time.Second):
	}
	res := map[string]any{"recording": true}
	select {
	case <-run.done:
		for k, v := range run.result {
			res[k] = v
		}
	default: // still writing
	}
	ok(w, "playbook.finished", res)
}

// prefetch reads the files of list in order (a few at a time) within the
// byte budget. Returns files and bytes read.
func (a *Agent) prefetch(ctx context.Context, list []string) (int, int64) {
	var mu sync.Mutex
	var files int
	var total int64
	next := make(chan string)
	var wg sync.WaitGroup
	for range prefetchWorkers {
		wg.Go(func() {
			buf := make([]byte, 1<<20)
			for p := range next {
				n, ok := prefetchFile(p, buf)
				mu.Lock()
				if ok {
					files++
					total += n
				}
				mu.Unlock()
			}
		})
	}
	for _, p := range list {
		mu.Lock()
		over := total >= prefetchBudget
		mu.Unlock()
		if over || ctx.Err() != nil {
			break
		}
		next <- p
	}
	close(next)
	wg.Wait()
	return files, total
}

func prefetchFile(p string, buf []byte) (int64, bool) {
	f, fi, err := openFileNB(p) // bugs-r13-opus 7: a FIFO must not block the open
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if !fi.Mode().IsRegular() || fi.Size() > prefetchMaxFile {
		return 0, false
	}
	n, _ := io.CopyBuffer(io.Discard, struct{ io.Reader }{f}, buf)
	return n, true
}

// openRecorder reports files opened on the root filesystem (fanotify).
type openRecorder struct {
	f *os.File
}

func newOpenRecorder() (*openRecorder, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK, unix.O_RDONLY|unix.O_LARGEFILE|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	err = unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, unix.FAN_OPEN, unix.AT_FDCWD, "/")
	if err != nil { // kernels < 4.20: the root mount only
		err = unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT, unix.FAN_OPEN, unix.AT_FDCWD, "/")
	}
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &openRecorder{f: os.NewFile(uintptr(fd), "fanotify")}, nil
}

// record collects paths in first-open order until window ends, max paths
// or ctx is done. Opens by this process (prefetch) are ignored.
func (r *openRecorder) record(ctx context.Context, stop <-chan struct{}, window time.Duration, max int) []string {
	defer r.f.Close()
	self := int32(os.Getpid())
	deadline := time.Now().Add(window)
	seen := map[string]bool{}
	var out []string
	buf := make([]byte, 64<<10)
	const metaLen = int(unsafe.Sizeof(unix.FanotifyEventMetadata{}))
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	for len(out) < max && time.Now().Before(deadline) && ctx.Err() == nil && !stopped() {
		_ = r.f.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, err := r.f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			break
		}
		for off := 0; off+metaLen <= n; {
			m := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))
			if m.Event_len < uint32(metaLen) {
				break
			}
			off += int(m.Event_len)
			if m.Fd < 0 {
				continue
			}
			p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(m.Fd)))
			unix.Close(int(m.Fd))
			if err != nil || m.Pid == self || seen[p] || playbookSkip(p) || len(out) >= max {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// fromSnapshot: this disk was booted before (resume, fork, deploy of a
// saved snapshot), and not just restarted (recover).
func fromSnapshot(prev bootState, boot proto.BootKind) bool {
	return prev.BootID != "" && boot != proto.BootRecover && boot != proto.BootInspect
}
