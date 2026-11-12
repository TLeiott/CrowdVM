package cli

import (
	"fmt"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
)

// outdatedNotice prints one line on stderr when the server marked this CLI
// outdated (X-Crowdvm-Client-Status, internal/compat) during this command.
// Unlike updateNotice it is not throttled: an outdated CLI misses an update
// the server requires. Never in JSON mode or off a terminal, so scripts
// and agents see nothing but the result.
func (a *App) outdatedNotice() {
	st := client.ClientStatus()
	if st == "" || a.jsonOut || !isTerminal(a.Err) {
		return
	}
	latest := ""
	for _, part := range strings.Split(st, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "latest="); ok {
			latest = " (latest: " + v + ")"
		}
	}
	fmt.Fprintf(a.Err, "\nThis crowdvm %s misses an update the server requires%s. Run `%s`.\n", client.Version, latest, updateCommand())
}
