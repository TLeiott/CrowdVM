//go:build linux

package bench

import (
	"math"
	"os"
	"strconv"
	"strings"
)

// cpuTimes reads the aggregate "cpu" line of /proc/stat (in jiffies): busy
// = user nice system irq softirq steal (these only grow; idle and iowait
// may step back), total = busy + idle + iowait. Guest time is already part
// of user and nice.
func cpuTimes() (busy, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	fs := strings.Fields(line)
	if len(fs) < 5 || fs[0] != "cpu" {
		return 0, 0, false
	}
	for i, f := range fs[1:min(len(fs), 9)] { // user nice system idle iowait irq softirq steal
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i != 3 && i != 4 {
			busy += v
		}
	}
	return busy, total, true
}

// pressure reads PSI "some avg10" for CPU and memory (Linux 4.20+, nil
// where the kernel has PSI disabled).
func pressure() (cpu, mem *float64) {
	return psi("/proc/pressure/cpu"), psi("/proc/pressure/memory")
}

func psi(path string) *float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	line, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(line, "some ") {
		return nil
	}
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "avg10="); ok {
			if x, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(x) && !math.IsInf(x, 0) {
				return &x
			}
		}
	}
	return nil
}
