package node

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// keepAwakeLoop holds a logind sleep inhibitor while this node hosts
// sandboxes, so the machine does not idle-suspend under them (field test: a
// laptop node went to sleep, and its sandbox had to move to another node,
// losing what changed since its last snapshot). Closing a laptop's lid still
// suspends: logind ignores inhibitors for the lid switch by default. Linux
// with systemd only; `crowdvm-node config set keep-awake false` turns it off.
func (n *Node) keepAwakeLoop(ctx context.Context) {
	if runtime.GOOS != "linux" || n.config().NoKeepAwake {
		return
	}
	bin, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return // no systemd: nothing to hold
	}
	var cmd *exec.Cmd
	var exited chan struct{}
	var stderr bytes.Buffer
	var retryAt time.Time
	release := func() {
		if cmd == nil {
			return
		}
		_ = cmd.Process.Kill()
		<-exited
		cmd = nil
		n.log.Info("no sandboxes here: the machine may sleep again")
	}
	defer release()
	t := time.NewTicker(awakeTick)
	defer t.Stop()
	for {
		n.mu.Lock()
		hosting := len(n.vms) > 0
		n.mu.Unlock()
		switch {
		case hosting && cmd == nil && time.Now().After(retryAt):
			stderr.Reset()
			c := exec.Command(bin, "--what=sleep", "--mode=block", "--who=CrowdVM node",
				"--why=Hosting sandboxes for the CrowdVM pool (crowdvm-node config set keep-awake false allows sleep)", "sleep", "infinity")
			c.Stderr = &stderr
			dieWithNode(c)
			if err := c.Start(); err != nil {
				n.log.Warn("cannot keep the machine awake", "err", err)
				retryAt = time.Now().Add(10 * time.Minute)
				break
			}
			cmd, exited = c, make(chan struct{})
			go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); close(done) }(c, exited)
			n.log.Info("hosting sandboxes: keeping the machine awake (closing the lid still suspends)")
		case !hosting:
			release()
		}
		if cmd != nil {
			select {
			case <-exited: // refused (polkit) or killed from outside
				n.log.Warn("cannot keep the machine awake: the sleep inhibitor ended", "err", strings.TrimSpace(stderr.String()))
				cmd, retryAt = nil, time.Now().Add(10*time.Minute)
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

var awakeTick = 10 * time.Second
