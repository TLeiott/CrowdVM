//go:build !windows

package server

//go:generate go run ./gencatalog provider_models.json

import (
	"encoding/json"

	"github.com/crowdvm/crowdvm/internal/guest/agents"
)

// builtinCatalog is the guest's harness/model catalog as JSON.
func builtinCatalog() []byte {
	b, _ := json.Marshal(agents.Catalog())
	return b
}
