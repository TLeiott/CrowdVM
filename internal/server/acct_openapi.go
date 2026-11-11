package server

// The OpenAPI document of this server (SDK-009, API-131, API-133), built
// from the live route table so it never drifts from what the server serves:
//
//	GET /openapi/crowdvm-v1.yaml   GET /openapi/crowdvm-v1.json
//	GET /api/v1/openapi.yaml       GET /api/v1/openapi.json
//
// Routes that mirror Boat's v1 API keep Boat's operation ids, summaries,
// tags and query/header parameters (boatOps, acct_openapi_ops.go). CrowdVM
// extensions get generated operation ids and the tag "CrowdVM". Guest
// pass-through routes (one wildcard registration per prefix) are expanded
// to Boat's operations under them. Bodies use the shared envelope schemas;
// x-crowdvm-action names the API key scope each operation needs.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type opParam struct{ In, Name string }

type boatOp struct {
	Method, Path, OperationID, Summary, Tag string
	Params                                  []opParam
	Body                                    bool
}

func init() {
	RegisterFeature("openapi", func(s *Server) error {
		// /openapi/boat-v1: Boat's name, for generators pointed at a Boat URL.
		for _, p := range []string{"/openapi/crowdvm-v1", "/openapi/boat-v1", "/api/v1/openapi"} {
			s.router.HandleFunc("GET "+p+".yaml", s.handleOpenAPI)
			s.router.HandleFunc("GET "+p+".json", s.handleOpenAPI)
		}
		return nil
	})
}

var openapiCache struct {
	sync.Mutex
	doc map[string]any
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	openapiCache.Lock()
	if openapiCache.doc == nil {
		openapiCache.doc = s.openAPIDoc()
	}
	doc := openapiCache.doc
	openapiCache.Unlock()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if strings.HasSuffix(r.URL.Path, ".json") {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(doc)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	var b strings.Builder
	writeYAML(&b, doc, 0)
	_, _ = w.Write([]byte(b.String()))
}

var pathParamRe = wildRe

func (s *Server) openAPIDoc() map[string]any {
	boat := map[string]boatOp{}
	for _, op := range boatOps {
		boat[op.Method+" "+normPattern(op.Path)] = op
	}
	paths := map[string]map[string]any{}
	used := map[string]bool{}
	add := func(method, path, action string, rel bool, bo *boatOp) {
		key := path
		if !rel {
			key = path // absolute routes get a path-level server
		}
		item := paths[key]
		if item == nil {
			item = map[string]any{}
			if !rel {
				item["servers"] = []any{map[string]any{"url": s.Cfg.PublicURL}}
			}
			paths[key] = item
		}
		m := strings.ToLower(method)
		if _, dup := item[m]; dup {
			return
		}
		op := map[string]any{"x-crowdvm-action": actionName(action)}
		var params []any
		for _, name := range pathParams(path) {
			params = append(params, map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		hasBody := method == "POST" || method == "PUT" || method == "PATCH"
		if bo != nil {
			op["operationId"] = bo.OperationID
			op["summary"] = strings.ReplaceAll(bo.Summary, "Boat", "CrowdVM")
			op["tags"] = []any{tagName(bo.Tag)}
			for _, p := range bo.Params {
				params = append(params, map[string]any{"name": p.Name, "in": p.In, "required": false, "schema": map[string]any{"type": "string"}})
			}
			hasBody = bo.Body
		} else {
			op["operationId"] = genOperationID(method, path)
			op["summary"] = genSummary(method, path)
			op["tags"] = []any{"CrowdVM"}
			op["x-crowdvm-extension"] = true
		}
		id := op["operationId"].(string)
		for i := 2; used[id]; i++ {
			id = op["operationId"].(string) + strconv.Itoa(i)
		}
		used[id] = true
		op["operationId"] = id
		if len(params) > 0 {
			op["parameters"] = params
		}
		if hasBody {
			op["requestBody"] = map[string]any{"required": false, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": true}}}}
		}
		if action == ActionPublic {
			op["security"] = []any{}
		}
		op["responses"] = map[string]any{
			"2XX":     map[string]any{"description": "Success envelope ({ok: true, type, ...fields}).", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Envelope"}}}},
			"default": map[string]any{"description": "Error envelope.", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"}}}},
		}
		item[m] = op
	}
	for _, rt := range s.router.apis {
		methods := []string{rt.Method}
		if rt.Method == "" {
			methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
		}
		path := rt.Path
		if !rt.Rel {
			if rel, ok := strings.CutPrefix(path, "/api/v1/"); ok {
				path, rt.Rel = "/"+rel, true
			}
		}
		if strings.Contains(path, "...}") {
			continue
		}
		for _, m := range methods {
			var bo *boatOp
			if rt.Rel {
				if b, ok := boat[m+" "+normPattern(path)]; ok {
					bo = &b
				}
			}
			if rt.Method == "" && bo == nil {
				continue // method-less route: only document Boat's methods
			}
			add(m, path, rt.Action, rt.Rel, bo)
		}
	}
	// Guest pass-through: Boat operations under the wildcard prefixes.
	for _, op := range boatOps {
		rest, ok := strings.CutPrefix(op.Path, "/sandboxes/{sandboxId}/")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, "/")
		isGuest := false
		for _, g := range guestPrefixes {
			isGuest = isGuest || g == first
		}
		if !isGuest {
			continue
		}
		op := op
		add(op.Method, op.Path, guestAction(rest, op.Method), true, &op)
	}
	ps := map[string]any{}
	for k, v := range paths {
		ps[k] = v
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "CrowdVM API v1",
			"version": Version,
			"description": "The /api/v1 API of this CrowdVM server. It mirrors Boat's public v1 API (same paths, bodies, envelopes and error codes) " +
				"and adds CrowdVM extensions (tag CrowdVM, x-crowdvm-extension: true): nodes, credits, admin, organizations and account settings. " +
				"x-crowdvm-action is the API key scope an operation needs. Generated from the server's route table.",
		},
		"servers":  []any{map[string]any{"url": s.Cfg.PublicURL + "/api/v1"}},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths":    ps,
		"components": map[string]any{
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer",
				"description": "An API key (cvm_...; boat_/box_ prefixes are accepted), a CLI session token (cvms_...), or the dashboard cookie with X-CrowdVM-CSRF: 1."}},
			"schemas": map[string]any{
				"Envelope": map[string]any{"type": "object", "required": []any{"ok", "type"}, "additionalProperties": true, "properties": map[string]any{
					"ok": map[string]any{"type": "boolean", "const": true}, "type": map[string]any{"type": "string"}}},
				"Error": map[string]any{"type": "object", "required": []any{"code", "message", "status"}, "properties": map[string]any{
					"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "status": map[string]any{"type": "integer"},
					"details": map[string]any{"type": "object", "additionalProperties": true}}},
				"ErrorEnvelope": map[string]any{"type": "object", "required": []any{"ok", "type", "status", "code", "message", "error"}, "properties": map[string]any{
					"ok": map[string]any{"type": "boolean", "const": false}, "type": map[string]any{"type": "string", "const": "sandbox.error"},
					"status": map[string]any{"type": "integer"}, "code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
					"error": map[string]any{"$ref": "#/components/schemas/Error"}, "requestId": map[string]any{"type": "string"}}},
			},
		},
	}
}

func actionName(a string) string {
	switch a {
	case ActionAny:
		return "any"
	case ActionPublic, ActionSession, ActionAdmin:
		return a
	}
	return a
}

func tagName(t string) string {
	if t == "" || t == "Boat" {
		return "Account"
	}
	return t
}

func pathParams(p string) []string {
	var out []string
	for _, m := range pathParamRe.FindAllString(p, -1) {
		out = append(out, strings.Trim(m, "{}."))
	}
	return out
}

// genOperationID: "GET /admin/users/{userId}/merge" -> "getAdminUsersMerge".
func genOperationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || strings.HasPrefix(seg, "{") || strings.HasPrefix(seg, ".") {
			continue
		}
		for _, w := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			b.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	return b.String()
}

func genSummary(method, path string) string {
	verb := map[string]string{"GET": "Get", "POST": "Post", "PUT": "Set", "PATCH": "Update", "DELETE": "Delete"}[method]
	return fmt.Sprintf("%s %s (CrowdVM)", verb, path)
}

// writeYAML emits v as block YAML (sorted keys, JSON-quoted strings).
func writeYAML(b *strings.Builder, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(pad + yamlScalar(k) + ":")
			writeYAMLValue(b, x[k], indent)
		}
	case []any:
		for _, e := range x {
			b.WriteString(pad + "-")
			writeYAMLValue(b, e, indent)
		}
	}
}

func writeYAMLValue(b *strings.Builder, v any, indent int) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			b.WriteString(" {}\n")
			return
		}
		b.WriteString("\n")
		writeYAML(b, x, indent+1)
	case []any:
		if len(x) == 0 {
			b.WriteString(" []\n")
			return
		}
		b.WriteString("\n")
		writeYAML(b, x, indent+1)
	default:
		b.WriteString(" " + yamlScalar(x) + "\n")
	}
}

func yamlScalar(v any) string {
	switch x := v.(type) {
	case string:
		q, _ := json.Marshal(x)
		return string(q)
	case nil:
		return "null"
	default:
		q, _ := json.Marshal(x)
		return string(q)
	}
}
