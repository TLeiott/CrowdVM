// Package docs embeds the CrowdVM user guide (guide/*.md) so the server can
// serve it to agents: /llms.txt, /llms-full.txt, /docs/<page>.md and the
// docs MCP server at /mcp (internal/server/feat_docs.go).
//
// guide/cli-reference.md is generated from the crowdvm CLI's command tree;
// run `go generate ./docs` (or `make docs`) after changing CLI commands.
package docs

import "embed"

//go:generate go run ../internal/server/gendocs -out guide/cli-reference.md

//go:embed guide/*.md
var Guide embed.FS
