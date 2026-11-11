package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
	"github.com/spf13/cobra"
)

// completionTTL is how long completed sandbox ids are reused (Boat: 15 s).
const completionTTL = 15 * time.Second

type completionCache struct {
	Key   string    `json:"key"`
	At    time.Time `json:"at"`
	Items []string  `json:"items"`
}

// addIDCompletion completes live sandbox ids (plus current and self) for
// every command whose first argument is a sandbox id.
func (a *App) addIDCompletion(root *cobra.Command) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		fields := strings.Fields(c.Use)
		if c.ValidArgsFunction != nil || len(fields) < 2 {
			return
		}
		first := strings.Trim(fields[1], "[]")
		if first != "ID" && first != "SANDBOX_ID" {
			return
		}
		c.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveDefault
			}
			return a.sandboxCompletions(), cobra.ShellCompDirectiveNoFileComp
		}
	}
	walk(root)
}

// sandboxCompletions returns "id\tdescription" entries for live sandboxes.
func (a *App) sandboxCompletions() []string {
	out := []string{"current\tthe last sandbox created in this shell", "self\tthe sandbox this CLI runs in"}
	srv, key := a.server(), a.apiKey()
	if srv == "" || key == "" {
		return out
	}
	cacheKey := srv + "|" + a.profile + "|" + firstNonEmpty(a.org, a.Getenv("CROWDVM_ORG"), a.Getenv("BOAT_ORG"))
	path := filepath.Join(a.configDir(), "completion-cache.json")
	var cache completionCache
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cache) == nil &&
		cache.Key == cacheKey && time.Since(cache.At) < completionTTL {
		return append(out, cache.Items...)
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c := a.newClient(srv, key)
	c.MaxRetries = 0
	list, err := c.ListAllSandboxes(ctx, client.ListSandboxesOptions{})
	if err != nil {
		return out
	}
	items := []string{}
	for _, sb := range list {
		if isFinalBad(sb.State) && sb.State != "archived" {
			continue
		}
		desc := displayState(sb.State)
		if sb.Name != "" {
			desc = sb.Name + " (" + desc + ")"
		}
		items = append(items, sb.ID+"\t"+desc)
	}
	if b, err := json.Marshal(completionCache{Key: cacheKey, At: time.Now(), Items: items}); err == nil {
		_ = os.MkdirAll(a.configDir(), 0o700)
		_ = os.WriteFile(path, b, 0o600)
	}
	return append(out, items...)
}
