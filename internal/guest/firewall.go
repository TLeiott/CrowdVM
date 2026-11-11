package guest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Direct ports (URL-012, Boat hosting#raw-tcp-or-udp-ports): "`host <id>
// <port> --public` also opens that TCP port on the IP address. boat opens it
// again after a resume." The node publishes the port and calls
//
//	PUT /v1/internal/firewall {"open": ["8000/tcp", ...]}
//
// whenever the set changes and every time the agent (re)connects, so the
// rules come back after the boot's firewall reset. The guest allows each
// spec in ufw, and deletes the rules it added for specs that left the set.
// Rules the user had already added are never deleted.

type firewallState struct {
	mu    sync.Mutex
	ready chan struct{} // closed once this boot's firewall reset ran
	once  sync.Once
}

func (f *firewallState) markReady() { f.once.Do(func() { close(f.ready) }) }

var fwSpecRe = regexp.MustCompile(`^([1-9][0-9]{0,4})/(tcp|udp)$`)

// fwOpenedPath (tmpfs: rules do not survive a boot) holds the specs the
// guest added itself.
func (a *Agent) fwOpenedPath() string { return filepath.Join(a.runDir(), "firewall-opened.json") }

// PUT /v1/internal/firewall {open:[spec]} -> guest.firewall {open, added, removed, active}
func (a *Agent) handleFirewall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Open []string `json:"open"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	want := []string{}
	for _, s := range req.Open {
		m := fwSpecRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
		if m == nil || len(m[1]) > 5 || atoi(m[1]) > 65535 {
			writeErr(w, 400, "invalid_port", fmt.Sprintf("%q: want <port>/tcp or <port>/udp", s))
			return
		}
		if !slices.Contains(want, m[0]) {
			want = append(want, m[0])
		}
	}
	slices.Sort(want)
	select { // the boot's firewall reset would undo rules added before it
	case <-a.fw.ready:
	case <-r.Context().Done():
		return
	case <-time.After(60 * time.Second):
	}
	added, removed, active := a.applyFirewall(want)
	ok(w, "guest.firewall", map[string]any{"open": want, "added": added, "removed": removed, "active": active})
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// applyFirewall makes ufw allow exactly want among the rules the guest
// added. Unprivileged or without ufw it only records the set.
func (a *Agent) applyFirewall(want []string) (added, removed []string, active bool) {
	a.fw.mu.Lock()
	defer a.fw.mu.Unlock()
	var opened []string
	if b, err := os.ReadFile(a.fwOpenedPath()); err == nil {
		_ = json.Unmarshal(b, &opened)
	}
	_, err := exec.LookPath("ufw")
	active = !a.unpriv && err == nil
	next := []string{}
	for _, s := range opened {
		if slices.Contains(want, s) {
			next = append(next, s)
			continue
		}
		if active && a.run("ufw", "delete", "allow", s) != nil {
			next = append(next, s) // try again next time
			continue
		}
		removed = append(removed, s)
	}
	for _, s := range want {
		if slices.Contains(opened, s) {
			continue
		}
		if !active {
			next = append(next, s)
			added = append(added, s)
			continue
		}
		out, err := exec.Command("ufw", "allow", s).CombinedOutput()
		if err != nil {
			a.log.Printf("ufw allow %s: %v: %s", s, err, strings.TrimSpace(lastLines(string(out), 2)))
			continue
		}
		if strings.Contains(string(out), "Skipping") { // the user's own rule: never ours to delete
			continue
		}
		next = append(next, s)
		added = append(added, s)
	}
	b, _ := json.Marshal(next)
	_ = os.MkdirAll(filepath.Dir(a.fwOpenedPath()), 0o755)
	_ = os.WriteFile(a.fwOpenedPath(), b, 0o644)
	return added, removed, active
}
