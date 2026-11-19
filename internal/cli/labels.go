package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crowdvm/crowdvm/internal/client"
)

// Labels let parallel agents tell their sandboxes apart (issue #17):
// CROWDVM_LABEL=KEY=VALUE[,KEY=VALUE] is added to every sandbox the CLI
// creates, and delete/stop refuse a sandbox that lacks it. Without
// CROWDVM_LABEL nothing changes.

// envLabels parses CROWDVM_LABEL (nil when unset).
func (a *App) envLabels() (map[string]string, error) {
	v := strings.TrimSpace(a.Getenv("CROWDVM_LABEL"))
	if v == "" {
		return nil, nil
	}
	return parseKV("CROWDVM_LABEL", []string{v})
}

// newLabels is CROWDVM_LABEL plus the --label flags (flags win).
func (a *App) newLabels(flags []string) (map[string]string, error) {
	m, err := a.envLabels()
	if err != nil {
		return nil, err
	}
	f, err := parseKV("--label", flags)
	if err != nil || m == nil {
		return f, err
	}
	maps.Copy(m, f)
	return m, nil
}

// guardLabels refuses sandbox id when it lacks a CROWDVM_LABEL label: it
// likely belongs to another agent. how tells the user how to go ahead anyway.
func (a *App) guardLabels(ctx context.Context, c *client.Client, id, how string) error {
	want, err := a.envLabels()
	if err != nil || len(want) == 0 {
		return err
	}
	s, err := c.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	for k, v := range want {
		if got, ok := s.Labels[k]; !ok || got != v {
			return &client.Error{Status: 409, Code: "label_mismatch", Message: fmt.Sprintf(
				"%s is not labeled %s (CROWDVM_LABEL); its labels: %s. It is probably another agent's sandbox. %s",
				id, formatLabels(want), firstNonEmpty(formatLabels(s.Labels), "none"), how)}
		}
	}
	return nil
}

// formatLabels renders labels as sorted k=v,k=v.
func formatLabels(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
