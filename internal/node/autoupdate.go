package node

// Auto-update: the daemon runs the same update as `crowdvm-node update`
// when the server says it should (heartbeat NodeUpdate, or the 426 a
// connect gets when the node is too old). By default ("required") only for
// updates the server requires: an outdated node gets no new sandboxes and
// loses its running ones after a grace period, so waiting for its owner
// costs everyone. "always" also installs optional releases (a bad build
// would then reach every such node at once, so it is opt-in); "off" leaves
// it to the owner. Only a supervised daemon (systemd, Windows service)
// updates itself, because only it can restart into the new build; its VMs
// survive the restart. Only from a server whose downloads are
// authenticated (TrustedUpdateServer), and never to a build that was
// rolled back here (update_pending.go).

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/proto"
)

const (
	AutoUpdateRequired = "required"
	AutoUpdateAlways   = "always"
	AutoUpdateOff      = "off"
)

// autoUpdateEvery spaces attempts (a failed download, a server whose
// downloads lag behind its requirement).
var autoUpdateEvery = time.Hour

// autoUpdateJitter: a random wait up to this before an update, so a
// release does not pull every node's download at once.
var autoUpdateJitter = 5 * time.Minute

// AutoUpdateStatus is the daemon's auto-update in Status. Unavailable says
// why it would not update by itself ("" when it would).
type AutoUpdateStatus struct {
	Mode        string `json:"mode"`
	Unavailable string `json:"unavailable,omitempty"`
}

// autoUpdateStatus for the server's latest release.
func (n *Node) autoUpdateStatus(latest string) AutoUpdateStatus {
	c := n.config()
	n.mu.Lock()
	supervised := n.restarter != nil
	n.mu.Unlock()
	s := AutoUpdateStatus{Mode: firstNonEmpty(c.AutoUpdate, AutoUpdateRequired)}
	switch {
	case s.Mode == AutoUpdateOff:
		s.Unavailable = "turned off"
	case !supervised:
		s.Unavailable = "not running as a service"
	case !TrustedUpdateServer(c.Server):
		s.Unavailable = "server is not https"
	case isSkipped(c.DataDir, latest):
		s.Unavailable = latest + " was rolled back here after it failed"
	}
	return s
}

// maybeAutoUpdate starts an update in the background when u and the
// auto-update setting call for one.
func (n *Node) maybeAutoUpdate(u *proto.NodeUpdate) {
	if u == nil {
		return
	}
	st := n.autoUpdateStatus(u.Latest)
	required := u.State == compat.StateOutdated || u.State == compat.StateIncompatible
	if st.Unavailable != "" || !(required || (st.Mode == AutoUpdateAlways && u.State == compat.StateAvailable)) {
		return
	}
	n.mu.Lock()
	// A newer release than the last attempt's is worth trying at once.
	ok := time.Since(n.autoUpdateAt) >= autoUpdateEvery || u.Latest != n.autoUpdateLatest
	if ok {
		n.autoUpdateAt, n.autoUpdateLatest = time.Now(), u.Latest
	}
	n.mu.Unlock()
	if !ok {
		return
	}
	var wait time.Duration
	if autoUpdateJitter > 0 {
		wait = rand.N(autoUpdateJitter)
	}
	n.log.Info("auto-update", "state", u.State, "latest", u.Latest, "mode", st.Mode, "in", wait.Round(time.Second))
	go func() {
		time.Sleep(wait)
		autoUpdateRun(n)
	}()
}

// autoUpdateRun installs the update (a variable for tests).
var autoUpdateRun = func(n *Node) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := n.Update(ctx, UpdateOptions{})
	switch {
	case err != nil:
		n.log.Warn("auto-update failed; retrying later", "err", err, "retry", autoUpdateEvery)
	case !res.Updated:
		n.log.Warn("auto-update: the server offers nothing newer than this build", "current", res.Current, "latest", res.Latest)
	}
}
