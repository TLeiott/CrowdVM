package guest

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

type procStat struct {
	pid, ppid, pgrp, session int
	state                    string
	ticks                    uint64 // utime+stime
	startTime                uint64
	rssPages                 int64
}

func readStat(pid int) (procStat, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procStat{}, errors.New("bad stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 22 {
		return procStat{}, errors.New("bad stat")
	}
	n := func(k int) uint64 { v, _ := strconv.ParseUint(f[k], 10, 64); return v }
	return procStat{pid: pid, state: f[0], ppid: int(n(1)), pgrp: int(n(2)), session: int(n(3)),
		ticks: n(11) + n(12), startTime: n(19), rssPages: int64(n(21))}, nil
}

const clkTck = 100 // USER_HZ on Linux

func bootTime() time.Time {
	b, _ := os.ReadFile("/proc/stat")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			s, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return time.Unix(s, 0).UTC()
		}
	}
	return time.Time{}
}

var uidNames sync.Map

func uidName(uid uint32) string {
	if v, ok := uidNames.Load(uid); ok {
		return v.(string)
	}
	name := strconv.Itoa(int(uid))
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	uidNames.Store(uid, name)
	return name
}

func listPIDs() []int {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// GET /v1/processes
func (a *Agent) handleProcesses(w http.ResponseWriter, r *http.Request) {
	const window = 250 * time.Millisecond
	first := map[int]uint64{}
	for _, pid := range listPIDs() {
		if st, err := readStat(pid); err == nil {
			first[pid] = st.ticks
		}
	}
	time.Sleep(window)
	sampled := time.Now().UTC()
	rows, _ := a.collectProcs(first, window)
	total := len(rows)
	if len(rows) > 500 {
		rows = rows[:500]
	}
	ok(w, "process.list", map[string]any{"live": true, "sampledAt": sampled, "total": total, "processes": rows})
}

// collectProcs lists user-visible processes (no kernel threads), busiest
// first. CPU % is measured against prev (ticks per pid) over window; with a
// nil prev or zero window it is 0. Returns the rows and the current ticks.
func (a *Agent) collectProcs(prev map[int]uint64, window time.Duration) ([]proto.ProcessInfo, map[int]uint64) {
	// running commands: session leader pid -> commandId
	cmdBy := map[int]string{}
	for _, m := range a.hist.all() {
		if m.Status == "running" && m.PID > 0 && m.Mode != "process" {
			cmdBy[m.PID] = m.CommandID
		}
	}
	bt := bootTime()
	page := int64(os.Getpagesize())
	ticks := map[int]uint64{}
	rows := []proto.ProcessInfo{}
	for _, pid := range listPIDs() {
		st, err := readStat(pid)
		if err != nil || pid == 2 || st.ppid == 2 {
			continue // kernel threads
		}
		ticks[pid] = st.ticks
		cl, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		cmdline := strings.TrimSpace(string(bytes.ReplaceAll(cl, []byte{0}, []byte{' '})))
		if cmdline == "" {
			continue
		}
		if len(cmdline) > 1000 {
			cmdline = cmdline[:1000]
		}
		rw := proto.ProcessInfo{Pid: pid, ParentPid: st.ppid, Command: cmdline,
			MemoryMB:  float64(st.rssPages*page) / (1 << 20),
			StartedAt: bt.Add(time.Duration(st.startTime) * time.Second / clkTck)}
		if p, ok := prev[pid]; ok && window > 0 && st.ticks >= p {
			rw.CPUPercent = float64(st.ticks-p) / clkTck / window.Seconds() * 100
		}
		if fi, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
			if s, ok := fi.Sys().(*syscall.Stat_t); ok {
				rw.User = uidName(s.Uid)
			}
		}
		if id, ok := cmdBy[st.session]; ok {
			rw.CommandID = ptr(id)
		} else if id, ok := cmdBy[st.pgrp]; ok {
			rw.CommandID = ptr(id)
		} else if g := a.cgroupOf(pid); strings.HasPrefix(g, "cmd_") {
			rw.CommandID = ptr(g)
		}
		rows = append(rows, rw)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPUPercent != rows[j].CPUPercent {
			return rows[i].CPUPercent > rows[j].CPUPercent
		}
		return rows[i].MemoryMB > rows[j].MemoryMB
	})
	return rows, ticks
}
