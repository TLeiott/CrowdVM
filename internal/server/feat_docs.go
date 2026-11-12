package server

// Docs for agents (DOCS-001/002, Boat ai-tools §2), served by the
// coordinator without auth:
//
//   - GET /llms.txt         index: every page with a one-line summary
//   - GET /llms-full.txt    every page in one file
//   - GET /docs/<page>.md   one page as Markdown (/docs and /docs/ = index)
//   - POST /mcp             docs MCP server (streamable HTTP transport,
//     JSON responses): tools search_docs, read_doc, list_docs; every page
//     is also a resource (crowdvm-docs://<page>)
//
// Pages: the user guide embedded from docs/guide (README = "index"), the
// CLI reference generated from the CLI's command tree (go generate ./docs)
// and an API reference rendered from this server's live OpenAPI document.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/crowdvm/crowdvm/docs"
)

type docPage struct {
	Slug, Title, Summary, Body string
}

type docSection struct {
	Page    *docPage
	Heading string
	Anchor  string
	Text    string
}

type docsState struct {
	s      *Server
	once   sync.Once
	pages  []*docPage
	bySlug map[string]*docPage
	secs   []docSection
}

// docOrder is the reading order; pages not listed follow alphabetically.
var docOrder = []string{"index", "getting-started", "cli-for-agents", "sandboxes", "public-urls", "security", "node-owners", "server-on-a-pi", "updates",
	"api-and-sdks", "troubleshooting", "migrating-from-boat", "cli-reference", "api-reference"}

var docAliases = map[string]string{"readme": "index", "quickstart": "getting-started", "cli": "cli-reference", "api": "api-reference",
	"introduction": "index", "llms": "index"}

func init() {
	RegisterFeature("docs", func(s *Server) error {
		d := &docsState{s: s}
		s.router.HandleFunc("GET /llms.txt", d.handleIndex)
		s.router.HandleFunc("GET /llms-full.txt", d.handleFull)
		s.router.HandleFunc("GET /docs", d.handleIndexMD)
		s.router.HandleFunc("GET /docs/{$}", d.handleIndexMD)
		s.router.HandleFunc("GET /docs/{page}", d.handlePage)
		s.router.HandleFunc("/mcp", d.handleMCP)
		return nil
	})
}

func (d *docsState) load() {
	d.once.Do(func() {
		d.bySlug = map[string]*docPage{}
		ents, _ := fs.ReadDir(docs.Guide, "guide")
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, err := fs.ReadFile(docs.Guide, "guide/"+e.Name())
			if err != nil {
				continue
			}
			slug := strings.TrimSuffix(e.Name(), ".md")
			if strings.EqualFold(slug, "README") {
				slug = "index"
			}
			d.add(slug, string(b))
		}
		d.add("api-reference", d.apiReference())
		rank := map[string]int{}
		for i, s := range docOrder {
			rank[s] = i + 1
		}
		sort.SliceStable(d.pages, func(i, j int) bool {
			ri, rj := rank[d.pages[i].Slug], rank[d.pages[j].Slug]
			if ri == 0 {
				ri = 1000
			}
			if rj == 0 {
				rj = 1000
			}
			if ri != rj {
				return ri < rj
			}
			return d.pages[i].Slug < d.pages[j].Slug
		})
		for _, p := range d.pages {
			d.secs = append(d.secs, splitSections(p)...)
		}
	})
}

func (d *docsState) add(slug, body string) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	p := &docPage{Slug: slug, Title: slug, Body: body}
	lines := strings.Split(body, "\n")
	var para []string
	inCode := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		if strings.HasPrefix(t, "# ") && p.Title == slug {
			p.Title = strings.TrimSpace(t[2:])
			continue
		}
		if p.Title == slug && t == "" {
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "- ") {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, t)
	}
	p.Summary = oneSentence(strings.Join(para, " "))
	d.pages = append(d.pages, p)
	d.bySlug[slug] = p
}

var mdLinkRe = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)

// oneSentence shortens a paragraph to its first sentence (at most ~200 chars).
func oneSentence(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "**", "")
	if i := strings.Index(s, ". "); i > 0 && i < 220 {
		return s[:i+1]
	}
	if len(s) > 200 {
		cut := strings.LastIndex(s[:200], " ")
		if cut < 100 {
			cut = 200
		}
		return s[:cut] + "…"
	}
	return s
}

var anchorStrip = regexp.MustCompile(`[^a-z0-9 _-]+`)

func headingAnchor(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = anchorStrip.ReplaceAllString(h, "")
	return strings.ReplaceAll(h, " ", "-")
}

func splitSections(p *docPage) []docSection {
	var out []docSection
	cur := docSection{Page: p, Heading: p.Title}
	var buf strings.Builder
	inCode := false
	flush := func() {
		cur.Text = strings.TrimSpace(buf.String())
		if cur.Text != "" || cur.Heading != "" {
			out = append(out, cur)
		}
		buf.Reset()
	}
	for _, l := range strings.Split(p.Body, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
		}
		if !inCode && strings.HasPrefix(t, "#") {
			h := strings.TrimSpace(strings.TrimLeft(t, "#"))
			if h != "" {
				flush()
				cur = docSection{Page: p, Heading: h, Anchor: headingAnchor(h)}
				continue
			}
		}
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	flush()
	return out
}

func (d *docsState) base() string { return strings.TrimRight(d.s.Cfg.PublicURL, "/") }

func (d *docsState) pageURL(p *docPage) string { return d.base() + "/docs/" + p.Slug + ".md" }

// lookup finds a page by slug, file name, alias or URL.
func (d *docsState) lookup(ref string) *docPage {
	d.load()
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "/docs/"); i >= 0 {
		ref = ref[i+len("/docs/"):]
	}
	ref, _, _ = strings.Cut(ref, "#")
	ref = strings.ToLower(strings.TrimSuffix(path.Base(ref), ".md"))
	if a, ok := docAliases[ref]; ok {
		ref = a
	}
	return d.bySlug[ref]
}

// ---------------------------------------------------------------------------
// HTTP: llms.txt, llms-full.txt, /docs/<page>.md
// ---------------------------------------------------------------------------

func (d *docsState) indexText() string {
	d.load()
	var b strings.Builder
	b.WriteString("# CrowdVM\n\n")
	b.WriteString("> CrowdVM runs persistent Ubuntu VMs (\"sandboxes\") for people and coding agents on machines a team already owns (LAN or Tailscale), with a Boat-compatible CLI (`crowdvm`), HTTP API (" + d.base() + "/api/v1) and SDKs.\n\n")
	b.WriteString("Give a coding agent the `crowdvm` CLI (signed in with `echo \"$CROWDVM_API_KEY\" | crowdvm login --key-stdin --server " + d.base() + "`), these docs, and the rule from `crowdvm agent-instructions` in AGENTS.md or CLAUDE.md.\n\n")
	b.WriteString("- Docs MCP server (search and read pages): " + d.base() + "/mcp (for example `claude mcp add --transport http crowdvm-docs " + d.base() + "/mcp`)\n")
	b.WriteString("- All pages in one file: " + d.base() + "/llms-full.txt\n")
	b.WriteString("- One page as Markdown: " + d.base() + "/docs/<page>.md\n")
	b.WriteString("- OpenAPI document: " + d.base() + "/openapi/crowdvm-v1.yaml\n\n")
	b.WriteString("## Docs\n\n")
	for _, p := range d.pages {
		fmt.Fprintf(&b, "- [%s](%s): %s\n", p.Title, d.pageURL(p), p.Summary)
	}
	return b.String()
}

func (d *docsState) fullText() string {
	d.load()
	var b strings.Builder
	b.WriteString("# CrowdVM documentation (all pages)\n\nIndex: " + d.base() + "/llms.txt\n\n")
	for _, p := range d.pages {
		fmt.Fprintf(&b, "\n---\n\nSource: %s\n\n%s\n", d.pageURL(p), strings.TrimSpace(p.Body))
	}
	return b.String()
}

func writeDoc(w http.ResponseWriter, ctype, body string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = io.WriteString(w, body)
}

func (d *docsState) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeDoc(w, "text/plain; charset=utf-8", d.indexText())
}

func (d *docsState) handleIndexMD(w http.ResponseWriter, r *http.Request) {
	writeDoc(w, "text/markdown; charset=utf-8", d.indexText())
}

func (d *docsState) handleFull(w http.ResponseWriter, r *http.Request) {
	writeDoc(w, "text/plain; charset=utf-8", d.fullText())
}

func (d *docsState) handlePage(w http.ResponseWriter, r *http.Request) {
	p := d.lookup(r.PathValue("page"))
	if p == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "No such page. The index of every page is at "+d.base()+"/llms.txt\n")
		return
	}
	writeDoc(w, "text/markdown; charset=utf-8", p.Body)
}

// apiReference renders the live OpenAPI document as Markdown.
func (d *docsState) apiReference() string {
	doc := d.s.openAPIDoc()
	paths, _ := doc["paths"].(map[string]any)
	type opRow struct{ tag, method, path, summary, action, id string }
	var rows []opRow
	for p, item := range paths {
		m, _ := item.(map[string]any)
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			op, ok := m[method].(map[string]any)
			if !ok {
				continue
			}
			tag := "CrowdVM"
			if tags, ok := op["tags"].([]any); ok && len(tags) > 0 {
				tag, _ = tags[0].(string)
			}
			full := p
			if _, abs := m["servers"]; !abs {
				full = "/api/v1" + p
			}
			sum, _ := op["summary"].(string)
			act, _ := op["x-crowdvm-action"].(string)
			id, _ := op["operationId"].(string)
			rows = append(rows, opRow{tag, strings.ToUpper(method), full, sum, act, id})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].tag != rows[j].tag {
			return rows[i].tag < rows[j].tag
		}
		if rows[i].path != rows[j].path {
			return rows[i].path < rows[j].path
		}
		return rows[i].method < rows[j].method
	})
	var b strings.Builder
	b.WriteString("# API reference\n\n")
	b.WriteString("Every operation of this server's HTTP API, generated from its live OpenAPI document (" + d.base() + "/openapi/crowdvm-v1.yaml). ")
	b.WriteString("Paths, bodies, envelopes and error codes follow Boat's v1 API; operations tagged CrowdVM are extensions. Read [API and SDKs](api-and-sdks.md) for auth, the envelope and errors.\n\n")
	b.WriteString("Authenticate with `Authorization: Bearer <key>` (an API key `cvm_…`, or a CLI session token `cvms_…`). Scope is the API key action the operation needs.\n")
	tag := ""
	for _, r := range rows {
		if r.tag != tag {
			tag = r.tag
			fmt.Fprintf(&b, "\n## %s\n\n| Method | Path | Summary | Scope |\n|---|---|---|---|\n", tag)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", r.method, r.path, strings.ReplaceAll(r.summary, "|", "/"), r.action)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

type docHit struct {
	Sec   docSection
	Score int
}

var termRe = regexp.MustCompile(`[\p{L}\p{N}_.-]+`)

func (d *docsState) search(q string, limit int) []docHit {
	d.load()
	terms := []string{}
	for _, t := range termRe.FindAllString(strings.ToLower(q), -1) {
		t = strings.Trim(t, ".-")
		if len(t) >= 2 {
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 {
		return nil
	}
	phrase := strings.ToLower(strings.TrimSpace(q))
	var hits []docHit
	for _, sec := range d.secs {
		text := strings.ToLower(sec.Text)
		head := strings.ToLower(sec.Heading + " " + sec.Page.Title)
		score, matched := 0, 0
		for _, t := range terms {
			n := min(strings.Count(text, t), 5) + 6*strings.Count(head, t)
			if n > 0 {
				matched++
			}
			score += n
		}
		if matched == 0 {
			continue
		}
		score += 10 * matched // sections matching more terms first
		if len(terms) > 1 && strings.Contains(text+" "+head, phrase) {
			score += 15
		}
		if l := len(text); l > 3000 { // long tables match everything a little
			score = score * 3000 / l
		}
		hits = append(hits, docHit{sec, score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func snippet(text string, terms []string) string {
	low := strings.ToLower(text)
	at := -1
	for _, t := range terms {
		if i := strings.Index(low, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		at = 0
	}
	start := max(0, at-120)
	end := min(len(text), at+260)
	for start > 0 && !isRuneStart(text[start]) {
		start--
	}
	for end < len(text) && !isRuneStart(text[end]) {
		end++
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ---------------------------------------------------------------------------
// MCP (streamable HTTP, JSON responses, stateless)
// ---------------------------------------------------------------------------

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (d *docsState) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	switch r.Method {
	case http.MethodPost:
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Mcp-Session-Id, Mcp-Protocol-Version, Authorization")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		// No server-initiated stream (GET) and no sessions (DELETE).
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "The CrowdVM docs MCP server speaks the streamable HTTP transport: POST JSON-RPC messages to this URL.", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	var msgs []rpcMsg
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &msgs)
	} else {
		var m rpcMsg
		err = json.Unmarshal(body, &m)
		msgs = []rpcMsg{m}
	}
	if err != nil || len(msgs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcErr{-32700, "Parse error: send a JSON-RPC 2.0 message."}})
		return
	}
	var out []map[string]any
	for _, m := range msgs {
		if len(m.ID) == 0 || string(m.ID) == "null" {
			continue // notification or response: nothing to answer
		}
		res, rerr := d.mcpCall(m)
		resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = res
		}
		out = append(out, resp)
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if batch {
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	_ = json.NewEncoder(w).Encode(out[0])
}

var mcpTools = []map[string]any{
	{"name": "search_docs", "title": "Search the CrowdVM docs",
		"description": "Search the CrowdVM documentation (user guide, CLI reference, API reference). Returns the best matching sections with their page, URL and a snippet. Use read_doc to read a whole page.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Words to look for, e.g. \"resume snapshot_data_expired\" or \"public url vite\"."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Results to return (default 5)."}}},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false}},
	{"name": "read_doc", "title": "Read a CrowdVM docs page",
		"description": "Return one CrowdVM docs page as Markdown. page is a slug from list_docs (e.g. \"sandboxes\", \"cli-reference\"), a file name or a page URL. section (optional) returns only the section under that heading.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"page"}, "properties": map[string]any{
			"page":    map[string]any{"type": "string", "description": "Page slug, file name or URL."},
			"section": map[string]any{"type": "string", "description": "Heading text (or its #anchor) to return only that section."}}},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false}},
	{"name": "list_docs", "title": "List the CrowdVM docs pages",
		"description": "List every CrowdVM docs page with its slug, URL and a one-line summary.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false}},
}

func textResult(s string, isErr bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isErr}
}

func (d *docsState) mcpCall(m rpcMsg) (any, *rpcErr) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := mcpVersions[0]
		for _, x := range mcpVersions {
			if x == p.ProtocolVersion {
				v = x
			}
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}, "resources": map[string]any{"listChanged": false, "subscribe": false}},
			"serverInfo":      map[string]any{"name": "crowdvm-docs", "title": "CrowdVM docs", "version": Version},
			"instructions":    "Search and read the documentation of this CrowdVM server (persistent Ubuntu sandboxes on a team's own machines, Boat-compatible CLI and API). Start with search_docs; read_doc returns whole pages.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcErr{-32602, "Invalid params: " + err.Error()}
		}
		return d.mcpTool(p.Name, p.Arguments)
	case "resources/list":
		d.load()
		var res []any
		for _, p := range d.pages {
			res = append(res, map[string]any{"uri": "crowdvm-docs://" + p.Slug, "name": p.Slug, "title": p.Title, "description": p.Summary, "mimeType": "text/markdown"})
		}
		return map[string]any{"resources": res}, nil
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(m.Params, &p)
		pg := d.lookup(strings.TrimPrefix(p.URI, "crowdvm-docs://"))
		if pg == nil {
			return nil, &rpcErr{-32002, "Resource not found: " + p.URI}
		}
		return map[string]any{"contents": []any{map[string]any{"uri": "crowdvm-docs://" + pg.Slug, "mimeType": "text/markdown", "text": pg.Body}}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	}
	return nil, &rpcErr{-32601, "Method not found: " + m.Method}
}

func (d *docsState) mcpTool(name string, raw json.RawMessage) (any, *rpcErr) {
	var a struct {
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		Page    string `json:"page"`
		Section string `json:"section"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &rpcErr{-32602, "Invalid arguments: " + err.Error()}
		}
	}
	switch name {
	case "list_docs":
		d.load()
		var b strings.Builder
		for _, p := range d.pages {
			fmt.Fprintf(&b, "- %s (%s): %s\n  %s\n", p.Slug, p.Title, p.Summary, d.pageURL(p))
		}
		return textResult(b.String(), false), nil
	case "search_docs":
		if strings.TrimSpace(a.Query) == "" {
			return textResult("query is required.", true), nil
		}
		if a.Limit <= 0 {
			a.Limit = 5
		}
		hits := d.search(a.Query, min(a.Limit, 20))
		if len(hits) == 0 {
			return textResult("No section matches "+fmt.Sprintf("%q", a.Query)+". Try other words, or list_docs.", false), nil
		}
		terms := termRe.FindAllString(strings.ToLower(a.Query), -1)
		var b strings.Builder
		for i, h := range hits {
			u := d.pageURL(h.Sec.Page)
			if h.Sec.Anchor != "" {
				u += "#" + h.Sec.Anchor
			}
			fmt.Fprintf(&b, "%d. %s › %s\n   page: %s  url: %s\n   %s\n\n", i+1, h.Sec.Page.Title, h.Sec.Heading, h.Sec.Page.Slug, u, snippet(h.Sec.Text, terms))
		}
		return textResult(strings.TrimSpace(b.String()), false), nil
	case "read_doc":
		p := d.lookup(a.Page)
		if p == nil {
			return textResult("No page "+fmt.Sprintf("%q", a.Page)+". Call list_docs for the slugs.", true), nil
		}
		if a.Section == "" {
			return textResult(p.Body, false), nil
		}
		want := headingAnchor(strings.TrimPrefix(a.Section, "#"))
		for _, sec := range splitSections(p) {
			if sec.Anchor == want || strings.EqualFold(sec.Heading, a.Section) {
				return textResult("## "+sec.Heading+"\n\n"+sec.Text, false), nil
			}
		}
		return textResult("Page "+p.Slug+" has no section "+fmt.Sprintf("%q", a.Section)+"; returning nothing. Read the whole page without section.", true), nil
	}
	return nil, &rpcErr{-32602, "Unknown tool: " + name}
}
