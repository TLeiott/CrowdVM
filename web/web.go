// Package web holds the CrowdVM dashboard: plain HTML, CSS and ES modules with
// no build step. The server mounts FS at / and falls back to index.html for
// unknown paths (client-side routing). See README.md for the API it expects.
package web

import "embed"

//go:embed index.html favicon.svg css js vendor
var FS embed.FS
