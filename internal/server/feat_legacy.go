package server

// Legacy Box API aliases (Boat parity API-137, EVAL-004/005).
//
// /api/box/v1/<rest> is a rename shim over /api/v1: /boxes -> /sandboxes,
// X-Box-Org -> X-Boat-Org, the 14 renamed JSON keys in request and response
// bodies, the 13 envelope types and the 18 error codes. Responses carry
// Deprecation, Sunset and Link headers like Boat's legacy routes. Streaming
// responses (NDJSON, raw files) pass through unchanged. This is what
// harbor-box (base_url) and @asciidev/eve-box (baseUrl) need.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	boxDeprecation = "@1789430400"                   // 2026-09-15, the rename
	boxSunset      = "Sat, 31 Oct 2026 23:59:59 GMT" // Boat's announced (not enforced) date
)

// boxKeys maps Box JSON keys to their current names.
var boxKeys = map[string]string{
	"activeBoxes": "activeSandboxes", "box": "sandbox", "boxCount": "sandboxCount", "boxId": "sandboxId",
	"boxIds": "sandboxIds", "boxLifecycle": "sandboxLifecycle", "boxPlanDollars": "sandboxPlanDollars",
	"boxPlanKey": "sandboxPlanKey", "boxType": "sandboxType", "boxes": "sandboxes", "maxActiveBoxes": "maxActiveSandboxes",
	"passBoxCredentials": "passSandboxCredentials", "queuedBoxes": "queuedSandboxes", "sourceBoxId": "sourceSandboxId",
}

var boxTypes = []string{"create", "created", "deleting", "error", "fork", "forking", "info", "list", "resume", "resuming", "stopping", "updated", "usage"}

var boxCodes = []string{"billing_not_configured", "busy", "deletion_conflict", "direct_failed", "email_auth_not_configured", "not_archivable",
	"not_found", "not_promptable", "not_snapshotable", "purge_pending", "recent_snapshot_required", "required", "restoring", "resuming",
	"running_stop_first", "securing", "starting", "stopping"}

var (
	boxKeysRev  = map[string]string{}
	boxTypesRev = map[string]string{}
	boxCodesRev = map[string]string{}
)

func init() {
	for k, v := range boxKeys {
		boxKeysRev[v] = k
	}
	for _, t := range boxTypes {
		boxTypesRev["sandbox."+t] = "box." + t
	}
	for _, c := range boxCodes {
		boxCodesRev["sandbox_"+c] = "box_" + c
	}
	RegisterFeature("legacy-box", func(s *Server) error {
		s.Router().HandleFunc("/api/box/v1/{rest...}", s.serveBoxAlias)
		return nil
	})
}

// renameKeys renames object keys recursively with m.
func renameKeys(v any, m map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if n, ok := m[k]; ok {
				if _, clash := x[n]; !clash {
					k = n
				}
			}
			out[k] = renameKeys(val, m)
		}
		return out
	case []any:
		for i := range x {
			x[i] = renameKeys(x[i], m)
		}
	}
	return v
}

func (s *Server) serveBoxAlias(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if rest == "boxes" || strings.HasPrefix(rest, "boxes/") {
		rest = "sandboxes" + strings.TrimPrefix(rest, "boxes")
	}
	h := w.Header()
	h.Set("Deprecation", boxDeprecation)
	h.Set("Sunset", boxSunset)
	h.Set("Link", "<"+strings.TrimRight(s.Cfg.PublicURL, "/")+"/api/v1>; rel=\"successor-version\"")

	r2 := r.Clone(r.Context())
	r2.URL.Path = "/api/v1/" + rest
	r2.URL.RawPath = ""
	r2.RequestURI = r2.URL.RequestURI()
	if v := r.Header.Get("X-Box-Org"); v != "" && r.Header.Get("X-Boat-Org") == "" {
		r2.Header.Set("X-Boat-Org", v)
	}
	if r.Body != nil {
		// Same cap as Ctx.bind; the inner route enforces it for the body it reads.
		r2.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	}
	// Only rewrite (parse) the body of an authenticated caller: an anonymous
	// request is passed through unread and refused by the inner route.
	if _, err := s.authenticate(w, r); err == nil && strings.Contains(r.Header.Get("Content-Type"), "json") && r.Body != nil {
		body, err := io.ReadAll(r2.Body)
		if err != nil {
			http.Error(w, "could not read body", http.StatusRequestEntityTooLarge)
			return
		}
		var v any
		if json.Unmarshal(body, &v) == nil {
			body, _ = json.Marshal(renameKeys(v, boxKeys))
		}
		r2.Body = io.NopCloser(bytes.NewReader(body))
		r2.ContentLength = int64(len(body))
		r2.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	bw := &boxWriter{w: w}
	s.router.mux.ServeHTTP(bw, r2) // middleware (request id, IP limit) already ran
	bw.finish()
}

// boxWriter buffers JSON responses to rename them and streams the rest.
type boxWriter struct {
	w       http.ResponseWriter
	status  int
	buffer  bool
	decided bool
	buf     bytes.Buffer
}

func (b *boxWriter) Header() http.Header { return b.w.Header() }

func (b *boxWriter) WriteHeader(code int) {
	if b.decided {
		return
	}
	b.decided, b.status = true, code
	ct := b.w.Header().Get("Content-Type")
	b.buffer = strings.HasPrefix(ct, "application/json") && code != http.StatusSwitchingProtocols
	if !b.buffer {
		b.w.WriteHeader(code)
	}
}

func (b *boxWriter) Write(p []byte) (int, error) {
	if !b.decided {
		b.WriteHeader(http.StatusOK)
	}
	if b.buffer {
		return b.buf.Write(p)
	}
	return b.w.Write(p)
}

func (b *boxWriter) Flush() {
	if b.decided && !b.buffer {
		_ = http.NewResponseController(b.w).Flush()
	}
}

func (b *boxWriter) Unwrap() http.ResponseWriter { return b.w }

func (b *boxWriter) finish() {
	if !b.decided || !b.buffer {
		return
	}
	out := b.buf.Bytes()
	var v any
	if json.Unmarshal(out, &v) == nil {
		if m, ok := renameKeys(v, boxKeysRev).(map[string]any); ok {
			if t, ok := m["type"].(string); ok {
				if n, ok := boxTypesRev[t]; ok {
					m["type"] = n
				}
			}
			if c, ok := m["code"].(string); ok {
				if n, ok := boxCodesRev[c]; ok {
					m["code"] = n
				}
			}
			if e, ok := m["error"].(map[string]any); ok {
				if c, ok := e["code"].(string); ok {
					if n, ok := boxCodesRev[c]; ok {
						e["code"] = n
					}
				}
			}
			v = m
		}
		out, _ = json.Marshal(v)
	}
	b.w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	b.w.WriteHeader(b.status)
	_, _ = b.w.Write(out)
}
