package server

import _ "embed"

// internal/guest/agents does not build on Windows: Windows servers serve a
// snapshot of the catalog (regenerate with `go generate ./internal/server`
// on Linux), unless an admin set one (PUT /admin/provider-models).
//
//go:embed provider_models.json
var providerModelsSnapshot []byte

func builtinCatalog() []byte { return providerModelsSnapshot }
