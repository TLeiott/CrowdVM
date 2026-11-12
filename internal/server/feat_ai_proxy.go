package server

// The AI credentials proxy: /ai-proxy/{vendor}/<path> forwards to the
// vendor's API with the real key of the sandbox's profile. The sandbox only
// holds a per-start placeholder token (cvmai_...). Responses, SSE included,
// stream through untouched; token usage is read from them on the way.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
)

var aiUpstreams = map[string]string{
	"anthropic":  "https://api.anthropic.com",
	"openai":     "https://api.openai.com",
	"chatgpt":    "https://chatgpt.com/backend-api/codex",
	"openrouter": "https://openrouter.ai",
	"llmgateway": "https://api.llmgateway.io",
	"deepseek":   "https://api.deepseek.com",
	"moonshot":   "https://api.moonshot.ai",
	"mistral":    "https://api.mistral.ai",
	"xai":        "https://api.x.ai",
	"opencode":   "https://opencode.ai",
}

// aiUpstream: CROWDVM_AI_UPSTREAM_<VENDOR> overrides a vendor's base URL
// (LAN gateways, tests).
func aiUpstream(vendor string) string {
	if v := os.Getenv("CROWDVM_AI_UPSTREAM_" + strings.ToUpper(vendor)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return aiUpstreams[vendor]
}

var aiTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 10 * time.Minute,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   32,
	ForceAttemptHTTP2:     true,
}

// aiCustomTransport: custom API hosts are user-chosen DNS names, so they dial
// through the outbound guard (loopback, link-local and metadata addresses are
// refused at connect time, as for webhooks), with no proxy to route around it
// (bugs-r10-opus 7).
var aiCustomTransport = &http.Transport{
	DialContext:           guardedDial,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 10 * time.Minute,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   32,
	ForceAttemptHTTP2:     true,
}

var aiTokenRe = regexp.MustCompile(aiTokenPrefix + `[0-9a-z]{40}`)

// aiFindToken looks for the placeholder in the auth headers, any header,
// then the query string.
func aiFindToken(r *http.Request) string {
	for _, h := range []string{"X-Api-Key", "Authorization"} {
		if m := aiTokenRe.FindString(r.Header.Get(h)); m != "" {
			return m
		}
	}
	for _, vs := range r.Header {
		for _, v := range vs {
			if m := aiTokenRe.FindString(v); m != "" {
				return m
			}
		}
	}
	return aiTokenRe.FindString(r.URL.RawQuery)
}

func aiProxyFail(w http.ResponseWriter, status int, code, msg string) {
	api.WriteError(w, api.NewError(status, code, msg, nil))
}

func (s *Server) handleAIProxy(w http.ResponseWriter, r *http.Request) {
	vendor := r.PathValue("vendor")
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/ai-proxy/"+vendor)
	rest = strings.TrimPrefix(rest, "/")
	tok := aiFindToken(r)
	if tok == "" {
		aiProxyFail(w, 401, "invalid_proxy_token", "Missing AI proxy placeholder key.")
		return
	}
	var sbID, profileID string
	if err := s.DB.QueryRow(`SELECT sandbox_id, profile_id FROM ai_proxy_tokens WHERE token_hash=?`, hashToken(tok)).Scan(&sbID, &profileID); err != nil {
		aiProxyFail(w, 401, "invalid_proxy_token", "This placeholder key is not valid (it stops working when its sandbox stops).")
		return
	}
	sb, err := s.SandboxByID(sbID)
	if err != nil || sb.DeletedAt != nil {
		aiProxyFail(w, 401, "invalid_proxy_token", "This placeholder key is not valid.")
		return
	}
	if blocked, _ := sb.Extra["aiBlocked"].(bool); blocked {
		aiProxyFail(w, 403, "ai_access_blocked", "AI access is blocked for this sandbox.")
		return
	}

	var base, credID string
	var setAuth func(h http.Header)
	inXKey := strings.Contains(r.Header.Get("X-Api-Key"), tok)
	query := r.URL.RawQuery
	if vendor == "custom" {
		name, sub, _ := strings.Cut(rest, "/")
		var hit *aiCustomAPI
		for _, a := range s.aiCustomAPIs(profileID) {
			if a.Name == name {
				a := a
				hit = &a
			}
		}
		if hit == nil {
			aiProxyFail(w, 404, "unknown_custom_api", "The sandbox's profile has no custom API "+name+".")
			return
		}
		base, rest, credID = hit.BaseURL, sub, "custom:"+name
		// The secret replaces the placeholder in headers and the query, never the path.
		query = strings.ReplaceAll(query, tok, url.QueryEscape(hit.Value))
		setAuth = func(h http.Header) {
			for k, vs := range h {
				for i, v := range vs {
					vs[i] = strings.ReplaceAll(v, tok, hit.Value)
				}
				h[k] = vs
			}
		}
	} else {
		creds, ok := aiVendorCreds[vendor]
		if !ok {
			aiProxyFail(w, 404, "unknown_vendor", "Unknown AI proxy vendor "+vendor+".")
			return
		}
		active, renewed := s.aiActiveFresh(profileID) // sign-ins renew while in use
		if renewed {
			s.envPushProfile(profileID)
		}
		var sec aiSecret
		for _, c := range creds {
			if v, ok := active[c]; ok {
				credID, sec = c, v
				break
			}
		}
		if credID == "" {
			aiProxyFail(w, 409, "provider_not_configured", "The sandbox's credential profile has no credential for "+vendor+".")
			return
		}
		base = aiUpstream(vendor)
		bearer := func(h http.Header, v string) { h.Set("Authorization", "Bearer "+v) }
		switch credID {
		case "anthropic-api-key":
			setAuth = func(h http.Header) { h.Set("X-Api-Key", sec.Value) }
		case "claude-subscription":
			setAuth = func(h http.Header) {
				bearer(h, sec.Value)
				if b := h.Get("Anthropic-Beta"); !strings.Contains(b, "oauth-2025-04-20") {
					h.Set("Anthropic-Beta", strings.TrimPrefix(b+",oauth-2025-04-20", ","))
				}
			}
		case "chatgpt-subscription":
			access, acct := aiChatGPTToken(sec)
			base, rest = aiUpstream("chatgpt"), strings.TrimPrefix(rest, "v1/")
			setAuth = func(h http.Header) {
				bearer(h, access)
				if acct != "" {
					h.Set("ChatGPT-Account-Id", acct)
				}
			}
		default:
			setAuth = func(h http.Header) {
				if inXKey {
					h.Set("X-Api-Key", sec.Value)
				} else {
					bearer(h, sec.Value)
				}
			}
		}
	}
	target, err := url.Parse(base)
	if err != nil || base == "" {
		aiProxyFail(w, 502, "upstream_unconfigured", "No upstream for "+vendor+".")
		return
	}
	sub, err := url.PathUnescape(rest)
	if err != nil || strings.Contains(sub, "\\") || slices.Contains(strings.Split(sub, "/"), "..") {
		// Never above the provider's base path with the real secret
		// attached (bugs-r4-opus 3).
		aiProxyFail(w, 400, "invalid_path", "The path may not leave the provider's API.")
		return
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimPrefix(sub, "/")
	target.RawQuery = query

	reqHead := &aiCapture{max: 64 << 10}
	if r.Body != nil {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(r.Body, reqHead), r.Body}
	}
	var once sync.Once
	record := func(failed bool, t aiTok) {
		once.Do(func() {
			if t.model == "" {
				if m := aiModelRe.FindSubmatch(reqHead.Bytes()); m != nil {
					t.model = string(m[1])
				}
			}
			s.aiRecord(sb, profileID, credID, failed, t)
		})
	}
	transport := http.RoundTripper(aiTransport)
	if vendor == "custom" {
		transport = aiCustomTransport // bugs-r10-opus 7
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Accept-Encoding") // the transport decompresses, so usage can be read
			pr.Out.Header.Del("Cookie")
			if vendor != "custom" {
				pr.Out.Header.Del("X-Api-Key")
				pr.Out.Header.Del("Authorization")
			}
			setAuth(pr.Out.Header)
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			failed := resp.StatusCode >= 400
			sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
			resp.Body = &aiCounter{rc: resp.Body, sse: sse, done: func(t aiTok) { record(failed, t) }}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			record(true, aiTok{})
			// The detail (refused addresses, open ports) stays in the log
			// (bugs-r10-opus 7).
			s.Log.Warn("ai proxy upstream failed", "sandbox", sb.ID, "vendor", vendor, "err", err)
			aiProxyFail(w, 502, "upstream_unreachable", "The provider did not answer.")
		},
	}
	if vendor == "custom" {
		// CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1 (the admin's opt-in for LAN
		// and loopback custom APIs) and --webhook-allow-local lift the guard.
		allow := s.Cfg.WebhookAllowLocal || os.Getenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE") == "1"
		r = r.WithContext(withOutboundPolicy(r.Context(), allow))
		// bugs-r11-opus 12: without the opt-in a DNS name that resolves to a
		// LAN address is refused at dial time too (aiValidBaseURL only reads
		// the text of the URL).
		if !allow {
			r = r.WithContext(withNoPrivate(r.Context()))
		}
	}
	rp.ServeHTTP(w, r)
}

var aiModelRe = regexp.MustCompile(`"model"\s*:\s*"([^"]{1,200})"`)

type aiCapture struct {
	buf bytes.Buffer
	max int
	mu  sync.Mutex
}

func (c *aiCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	c.mu.Unlock()
	return len(p), nil
}

func (c *aiCapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf.Bytes())
}

// ---------------------------------------------------------------------------
// Usage accounting
// ---------------------------------------------------------------------------

// aiTok holds the usage a provider reported (fields keep the maximum seen,
// matching cumulative counts in streams).
type aiTok struct {
	model                       string
	in, out, cacheRead, cacheWr int64
}

func (t *aiTok) merge(m map[string]any) {
	if v, ok := m["model"].(string); ok && v != "" {
		t.model = v
	}
	for _, k := range []string{"message", "response"} {
		if sub, ok := m[k].(map[string]any); ok {
			t.merge(sub)
		}
	}
	u, ok := m["usage"].(map[string]any)
	if !ok {
		return
	}
	num := func(m map[string]any, keys ...string) int64 {
		for _, k := range keys {
			if f, ok := m[k].(float64); ok {
				return int64(f)
			}
		}
		return 0
	}
	t.in = max(t.in, num(u, "input_tokens", "prompt_tokens"))
	t.out = max(t.out, num(u, "output_tokens", "completion_tokens"))
	t.cacheWr = max(t.cacheWr, num(u, "cache_creation_input_tokens"))
	t.cacheRead = max(t.cacheRead, num(u, "cache_read_input_tokens"))
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			t.cacheRead = max(t.cacheRead, num(d, "cached_tokens"))
		}
	}
}

// aiCounter passes the body through and parses usage on the side.
type aiCounter struct {
	rc   io.ReadCloser
	sse  bool
	buf  []byte // SSE: current partial line; JSON: the whole body (capped)
	over bool
	tok  aiTok
	done func(aiTok)
	once sync.Once
}

const aiMaxJSON = 16 << 20

func (c *aiCounter) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.feed(p[:n])
	}
	if err == io.EOF {
		c.finish()
	}
	return n, err
}

func (c *aiCounter) Close() error {
	c.finish()
	return c.rc.Close()
}

func (c *aiCounter) feed(b []byte) {
	if !c.sse {
		if !c.over && len(c.buf)+len(b) <= aiMaxJSON {
			c.buf = append(c.buf, b...)
		} else {
			c.over, c.buf = true, nil
		}
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if len(c.buf)+len(b) <= 1<<20 {
				c.buf = append(c.buf, b...)
			}
			return
		}
		c.line(append(c.buf, b[:i]...))
		c.buf = c.buf[:0]
		b = b[i+1:]
	}
}

func (c *aiCounter) line(l []byte) {
	l = bytes.TrimSpace(l)
	if !bytes.HasPrefix(l, []byte("data:")) {
		return
	}
	l = bytes.TrimSpace(l[5:])
	if len(l) == 0 || l[0] != '{' {
		return
	}
	var m map[string]any
	if json.Unmarshal(l, &m) == nil {
		c.tok.merge(m)
	}
}

func (c *aiCounter) finish() {
	c.once.Do(func() {
		if c.sse {
			if len(c.buf) > 0 {
				c.line(c.buf)
			}
		} else if !c.over && len(c.buf) > 0 {
			var m map[string]any
			if json.Unmarshal(c.buf, &m) == nil {
				c.tok.merge(m)
			}
		}
		c.buf = nil
		c.done(c.tok)
	})
}

func (s *Server) aiRecord(sb *Sandbox, profileID, cred string, failed bool, t aiTok) {
	_, err := s.DB.Exec(`INSERT INTO ai_usage(day,sandbox_id,wallet,profile_id,credential,model,requests,errors,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens)
		VALUES(?,?,?,?,?,?,1,?,?,?,?,?)
		ON CONFLICT(day,sandbox_id,profile_id,credential,model) DO UPDATE SET requests=requests+1, errors=errors+excluded.errors,
		input_tokens=input_tokens+excluded.input_tokens, output_tokens=output_tokens+excluded.output_tokens,
		cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens, cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens`,
		time.Now().UTC().Format("2006-01-02"), sb.ID, sb.Wallet(), profileID, cred, t.model, boolInt(failed), t.in, t.out, t.cacheRead, t.cacheWr)
	if err != nil {
		s.Log.Warn("ai usage", "sandbox", sb.ID, "err", err)
	}
}
