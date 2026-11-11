package guest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Samples (CMD-025/027, OTEL-005, LIFE-044): every 30 s the guest writes
// /run/crowdvm/df ("<used> <total>" bytes of the root filesystem, read by the
// server's OpenTelemetry exporter through GET /v1/files) and, while
// monitoring is on, takes a GuestSample (VM CPU/RAM/disk/IO + the 12 busiest
// processes) for the server to pull via history/export, and raises
// disk.usage events at 85 % / 95 % (at 95 % it also frees caches).

const (
	sampleEvery    = 30 * time.Second
	sampleTopProcs = 12
	samplesPending = 2880 // 24 h of unexported samples
	EvDiskUsage    = "disk.usage"
)

type sampleRec struct {
	Seq int64 `json:"seq"`
	proto.GuestSample
}

type sampler struct {
	a  *Agent
	mu sync.Mutex

	seq     int64
	pend    []sampleRec
	last    *proto.GuestSample
	cpuPrev [2]uint64      // busy, total
	ticks   map[int]uint64 // per-pid cpu ticks at the previous sample
	tickAt  time.Time

	diskLevel   int // last alerted level (0, 85, 95)
	diskAlertAt map[int]time.Time
	cleanedAt   time.Time
}

func newSampler(a *Agent) *sampler { return &sampler{a: a, diskAlertAt: map[int]time.Time{}} }

// dfPath is where the df gauge is written (tmpfs in a VM).
func (a *Agent) dfPath() string {
	if a.unpriv {
		return filepath.Join(a.data, "run", "df")
	}
	return "/run/crowdvm/df"
}

func (s *sampler) loop(stop <-chan struct{}) {
	s.tick(time.Now())
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

func (s *sampler) tick(now time.Time) {
	a := s.a
	used, total := diskUsage(a.diskRoot())
	if total > 0 {
		p := a.dfPath()
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p+".tmp", []byte(strconv.FormatInt(used, 10)+" "+strconv.FormatInt(total, 10)+"\n"), 0o644)
		_ = os.Rename(p+".tmp", p)
	}
	if a.hist.unmonitor {
		return // monitoring off: no samples, no disk alerts (Boat)
	}
	sm := proto.GuestSample{Time: now.UTC(), DiskUsedBytes: used, DiskTotalBytes: total}
	if busy, tot := cpuTotals(); tot > 0 {
		s.mu.Lock()
		if s.cpuPrev[1] > 0 && tot > s.cpuPrev[1] {
			sm.CPUUtilization = float64(busy-s.cpuPrev[0]) / float64(tot-s.cpuPrev[1])
		}
		s.cpuPrev = [2]uint64{busy, tot}
		s.mu.Unlock()
	}
	mi := meminfo()
	sm.MemoryTotalBytes = mi["MemTotal"]
	sm.MemoryUsedBytes = mi["MemTotal"] - mi["MemAvailable"]
	sm.IOPressure = ioPressure()
	s.mu.Lock()
	prev, prevAt := s.ticks, s.tickAt
	s.mu.Unlock()
	window := now.Sub(prevAt)
	if prev == nil || window <= 0 {
		window = 0
	}
	rows, ticks := a.collectProcs(prev, window)
	if len(rows) > sampleTopProcs {
		rows = rows[:sampleTopProcs]
	}
	sm.Processes = rows
	s.mu.Lock()
	s.ticks, s.tickAt = ticks, now
	s.seq++
	s.pend = append(s.pend, sampleRec{Seq: s.seq, GuestSample: sm})
	if len(s.pend) > samplesPending {
		s.pend = s.pend[len(s.pend)-samplesPending:]
	}
	s.last = &sm
	s.mu.Unlock()
	s.diskAlert(now, used, total)
}

// pending returns up to n unacked samples, oldest first.
func (s *sampler) pending(n int) []sampleRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pend) > n {
		return append([]sampleRec(nil), s.pend[:n]...)
	}
	return append([]sampleRec(nil), s.pend...)
}

func (s *sampler) ack(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := 0
	for i < len(s.pend) && s.pend[i].Seq <= seq {
		i++
	}
	s.pend = append(s.pend[:0], s.pend[i:]...)
}

// diskAlert emits disk.usage at 85 % / 95 % (again at most hourly per
// level; the server sends at most one notification per level per day) and
// at 95 % cleans the apt cache, the journal and unused Docker images.
func (s *sampler) diskAlert(now time.Time, used, total int64) {
	if total <= 0 {
		return
	}
	pct := float64(used) / float64(total) * 100
	level := 0
	switch {
	case pct >= 95:
		level = 95
	case pct >= 85:
		level = 85
	}
	s.mu.Lock()
	fire := level > 0 && (level > s.diskLevel || now.Sub(s.diskAlertAt[level]) > time.Hour)
	clean := level == 95 && now.Sub(s.cleanedAt) > time.Hour
	s.diskLevel = level
	if fire {
		s.diskAlertAt[level] = now
	}
	if clean {
		s.cleanedAt = now
	}
	s.mu.Unlock()
	if fire {
		s.a.Emit(EvDiskUsage, map[string]any{"level": level, "percent": pct, "usedBytes": used, "totalBytes": total})
	}
	if clean && !s.a.unpriv {
		go s.a.freeDiskSpace()
	}
}

func (a *Agent) freeDiskSpace() {
	var errs []string
	a.runQuiet(&errs, "apt-get", "clean")
	a.runQuiet(&errs, "journalctl", "--vacuum-size=64M")
	if a.dockerOK() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		a.pruneImages(ctx, true) // pulled images only: never what the user built
		cancel()
	}
	if len(errs) > 0 {
		a.log.Printf("disk cleanup: %s", strings.Join(errs, "; "))
	}
}

func (a *Agent) diskRoot() string {
	if a.unpriv {
		return a.home
	}
	return "/"
}

func diskUsage(path string) (used, total int64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	used = (int64(st.Blocks) - int64(st.Bfree)) * bs
	return used, total
}

// cpuTotals returns busy and total jiffies from the first line of /proc/stat.
func cpuTotals() (busy, total uint64) {
	b, _ := os.ReadFile("/proc/stat")
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	var idle uint64
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		if i >= 8 { // guest, guest_nice are already in user/nice
			break
		}
		total += n
		if i == 3 || i == 4 {
			idle += n
		}
	}
	return total - idle, total
}

func meminfo() map[string]int64 {
	out := map[string]int64{}
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if f := strings.Fields(v); len(f) > 0 {
			n, _ := strconv.ParseInt(f[0], 10, 64)
			out[k] = n << 10
		}
	}
	return out
}

func ioPressure() float64 {
	b, _ := os.ReadFile("/proc/pressure/io")
	line, _, _ := strings.Cut(string(b), "\n")
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "avg60="); ok {
			n, _ := strconv.ParseFloat(v, 64)
			return n / 100
		}
	}
	return 0
}
