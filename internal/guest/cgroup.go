package guest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Workloads (LIFE-043, CMD-001/010): every command, setup script and
// integrated-agent harness runs in its own cgroup under
//
//	/sys/fs/cgroup/user.slice/crowdvm-workloads.service/<group>
//
// crowdvm-workloads.service is a transient, delegated unit (Delegate=yes)
// whose only own process is a `sleep infinity` keeper. Children are placed
// with clone3(CLONE_INTO_CGROUP), so:
//   - user.slice has MemoryMax = RAM - 512 MiB (set every boot, the RAM size
//     changes with the sandbox type): platform services keep 512 MiB and an
//     out-of-memory kill only hits workloads;
//   - memory.events of a command's cgroup tells whether the OOM killer hit
//     it (oomKilled, exit 137), even after the process is gone;
//   - detached commands live outside crowdvm-guest.service, so they survive
//     an agent restart or upgrade without KillMode=process.
//
// Unprivileged, or when systemd/cgroup v2 are missing, everything falls back
// to a plain fork in the agent's own cgroup.

const (
	cgroupRoot        = "/sys/fs/cgroup"
	workloadsUnit     = "crowdvm-workloads.service"
	platformReserve   = 512 << 20
	minWorkloadMemory = 256 << 20
)

type workloads struct {
	mu   sync.Mutex
	root string // real cgroup dir, "" = disabled
	// taskCap: pids.max of each workload cgroup, 3/4 of what all workloads
	// share, so one fork bomb leaves room to exec and kill it
	// (bugs-r7-opus 4). 0 = no limit known.
	taskCap int64
}

// sharedTaskLimit: the smallest pids.max from dir up to the cgroup root.
func sharedTaskLimit(dir string) int64 {
	var min int64
	for d := dir; strings.HasPrefix(d, cgroupRoot) && d != cgroupRoot; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "pids.max"))
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && (min == 0 || n < min) {
			min = n
		}
	}
	if min == 0 { // no cgroup limit: the kernel's
		if b, err := os.ReadFile("/proc/sys/kernel/threads-max"); err == nil {
			min, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		}
	}
	return min
}

// killGroup kills every process of a workload cgroup, also when its first
// process already exited.
func (a *Agent) killGroup(group string) {
	dir := a.groupDir(group)
	if dir == "" {
		return
	}
	if os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644) == nil {
		return
	}
	b, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// initWorkloads sets the user.slice memory cap and makes sure the delegated
// workloads cgroup exists (root only). Safe to call again after a restart.
func (a *Agent) initWorkloads() {
	if a.unpriv || os.Geteuid() != 0 {
		return
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return // not cgroup v2
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	if total := memTotal(); total > 0 {
		max := total - platformReserve
		if max < minWorkloadMemory {
			max = minWorkloadMemory
		}
		_ = a.run("systemctl", "set-property", "--runtime", "user.slice", "MemoryAccounting=yes", "MemoryMax="+strconv.FormatInt(max, 10))
	}
	root := filepath.Join(cgroupRoot, "user.slice", workloadsUnit)
	if _, err := os.Stat(filepath.Join(root, "keeper")); err != nil {
		_ = exec.Command("systemctl", "reset-failed", workloadsUnit).Run()
		if a.run("systemd-run", "--quiet", "--unit="+workloadsUnit, "--slice=user.slice",
			"-p", "Delegate=yes", "-p", "KillMode=process", "-p", "RemainAfterExit=yes",
			"-p", "OOMScoreAdjust=-1000", "-p", "Type=exec", "-p", "MemoryAccounting=yes",
			"--description=CrowdVM user workloads (commands, setup scripts, agents)",
			"sleep", "infinity") != nil {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(root, "cgroup.procs")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				a.log.Printf("workloads: %s did not appear", root)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		// cgroup v2 "no internal processes": move the keeper into a leaf
		// before enabling controllers for the children.
		if err := os.MkdirAll(filepath.Join(root, "keeper"), 0o755); err != nil {
			a.log.Printf("workloads: %v", err)
			return
		}
		b, _ := os.ReadFile(filepath.Join(root, "cgroup.procs"))
		for _, pid := range strings.Fields(string(b)) {
			_ = os.WriteFile(filepath.Join(root, "keeper", "cgroup.procs"), []byte(pid), 0o644)
		}
	}
	ctl := filepath.Join(root, "cgroup.subtree_control")
	if err := os.WriteFile(ctl, []byte("+memory +pids +cpu"), 0o644); err != nil {
		if err := os.WriteFile(ctl, []byte("+memory"), 0o644); err != nil {
			a.log.Printf("workloads: enable memory controller: %v", err)
			return
		}
	}
	a.wl.mu.Lock()
	a.wl.root = root
	if n := sharedTaskLimit(root); n > 0 {
		a.wl.taskCap = max(64, n*3/4)
	}
	a.wl.mu.Unlock()
	// Probe clone3(CLONE_INTO_CGROUP) (Linux >= 5.7) once, so spawn never
	// has to retry a failed exec.Cmd.Start.
	probe := exec.Command("/bin/true")
	if err := a.spawn(probe, "probe"); err != nil {
		a.log.Printf("workloads: CLONE_INTO_CGROUP unavailable (%v); commands stay in the agent's cgroup", err)
		a.wl.mu.Lock()
		a.wl.root = ""
		a.wl.mu.Unlock()
		return
	}
	_ = probe.Wait()
	_ = syscall.Rmdir(filepath.Join(root, "probe"))
	go a.workloadsJanitor()
}

func memTotal() int64 {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "MemTotal:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n << 10
			}
		}
	}
	return 0
}

// groupDir returns the cgroup dir for group ("" when workloads are disabled).
func (a *Agent) groupDir(group string) string {
	a.wl.mu.Lock()
	root := a.wl.root
	a.wl.mu.Unlock()
	if root == "" || group == "" || strings.ContainsAny(group, "/.") {
		return ""
	}
	return filepath.Join(root, group)
}

// spawn starts cmd in the workload cgroup named group (created if missing)
// and resets the child's OOM score adjustment, so the OOM killer can pick
// it even when crowdvm-guest itself is protected. Without workload cgroups
// (unprivileged, no cgroup v2/systemd, old kernel) it is a plain
// cmd.Start(). cmd.SysProcAttr is kept (Setsid, Credential, ...).
func (a *Agent) spawn(cmd *exec.Cmd, group string) error {
	if dir := a.groupDir(group); dir != "" {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			a.wl.mu.Lock()
			taskCap := a.wl.taskCap
			a.wl.mu.Unlock()
			if taskCap > 0 {
				_ = os.WriteFile(filepath.Join(dir, "pids.max"), []byte(strconv.FormatInt(taskCap, 10)), 0o644)
			}
		}
		if err == nil || errors.Is(err, os.ErrExist) {
			if fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0); err == nil {
				attr := cmd.SysProcAttr
				if attr == nil {
					attr = &syscall.SysProcAttr{}
				}
				withCG := *attr
				withCG.UseCgroupFD, withCG.CgroupFD = true, fd
				cmd.SysProcAttr = &withCG
				err := cmd.Start()
				syscall.Close(fd)
				cmd.SysProcAttr = attr
				if err == nil {
					a.resetOOMScore(cmd.Process.Pid)
				}
				return err
			}
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	a.resetOOMScore(cmd.Process.Pid)
	return nil
}

// SpawnAgent is the spawn helper for the integrated agents package
// (agents.Options.Spawn): harness processes share the "agents" cgroup.
func (a *Agent) SpawnAgent(cmd *exec.Cmd) error { return a.spawn(cmd, "agents") }

func (a *Agent) resetOOMScore(pid int) {
	if a.unpriv {
		return
	}
	if b, err := os.ReadFile("/proc/self/oom_score_adj"); err == nil && strings.TrimSpace(string(b)) != "0" {
		_ = os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte("0"), 0o644)
	}
}

// oomKills returns the oom_kill counter of a workload cgroup (0 if unknown).
func (a *Agent) oomKills(group string) int {
	dir := a.groupDir(group)
	if dir == "" {
		return 0
	}
	f, err := os.Open(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// groupPIDs lists the processes in a workload cgroup (and its children).
func (a *Agent) groupPIDs(group string) []int {
	dir := a.groupDir(group)
	if dir == "" {
		return nil
	}
	var pids []int
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(filepath.Join(p, "cgroup.procs"))
		for _, s := range strings.Fields(string(b)) {
			if n, err := strconv.Atoi(s); err == nil {
				pids = append(pids, n)
			}
		}
		return nil
	})
	return pids
}

// cgroupOf returns the workload group of pid ("" when it is not a workload).
func (a *Agent) cgroupOf(pid int) string {
	a.wl.mu.Lock()
	root := a.wl.root
	a.wl.mu.Unlock()
	if root == "" {
		return ""
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	rel := strings.TrimPrefix(root, cgroupRoot)
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			if rest, ok := strings.CutPrefix(p, rel+"/"); ok {
				g, _, _ := strings.Cut(rest, "/")
				return g
			}
		}
	}
	return ""
}

// workloadsJanitor removes empty command cgroups of finished commands.
func (a *Agent) workloadsJanitor() {
	for {
		time.Sleep(time.Minute)
		a.cleanWorkloads()
	}
}

func (a *Agent) cleanWorkloads() {
	a.wl.mu.Lock()
	root := a.wl.root
	a.wl.mu.Unlock()
	if root == "" {
		return
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "cmd_") {
			continue
		}
		if m := a.hist.get(e.Name()); m != nil && a.hist.record(m).Status == "running" {
			continue
		}
		_ = syscall.Rmdir(filepath.Join(root, e.Name())) // EBUSY while populated
	}
}
