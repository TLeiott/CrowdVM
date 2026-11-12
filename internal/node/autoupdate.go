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
// survive the restart.

import (
	"context"
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

// maybeAutoUpdate starts an update in the background when u and the
// auto-update setting call for one.
func (n *Node) maybeAutoUpdate(u *proto.NodeUpdate) {
	if u == nil {
		return
	}
	mode := firstNonEmpty(n.config().AutoUpdate, AutoUpdateRequired)
	required := u.State == compat.StateOutdated || u.State == compat.StateIncompatible
	if mode == AutoUpdateOff || !(required || (mode == AutoUpdateAlways && u.State == compat.StateAvailable)) {
		return
	}
	n.mu.Lock()
	ok := n.restarter != nil && time.Since(n.autoUpdateAt) >= autoUpdateEvery
	if ok {
		n.autoUpdateAt = time.Now()
	}
	n.mu.Unlock()
	if !ok {
		return
	}
	n.log.Info("auto-update", "state", u.State, "latest", u.Latest, "mode", mode)
	go autoUpdateRun(n)
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
