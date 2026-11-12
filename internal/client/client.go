// Package client is the Go client for the CrowdVM public API (/api/v1, which
// mirrors Boat's v1 API), the CLI auth endpoints and sandbox tunnels.
//
//	c := client.New("http://10.0.0.5:8420", "cvm_...")
//	sb, err := c.CreateSandbox(ctx, &client.CreateSandboxRequest{Type: "small"}, "")
//
// Every API failure is returned as *Error (code, status, retryable). Requests
// that fail with 429/502/503/504 are retried with backoff, honoring
// retryAfterSeconds / Retry-After. Non-idempotent POSTs are only retried on
// those statuses (the server refused them before acting), or on network
// errors when they carry an Idempotency-Key.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/compat"
)

// Version is sent in the User-Agent. The CLI sets it at build time.
var Version = "dev"

// Client talks to one CrowdVM server. The zero value is not usable; use New.
type Client struct {
	// BaseURL is the server root, e.g. "http://10.0.0.5:8420" (no /api/v1).
	BaseURL string
	// APIKey is sent as "Authorization: Bearer <key>" when set.
	APIKey string
	// Org, when set, is sent as X-CrowdVM-Org on every request and as the
	// "org" query parameter on endpoints that take one.
	Org string
	// HTTP is the underlying client. It must not have a global Timeout,
	// because streams (exec, attach, tunnels) are long-lived; use contexts.
	HTTP      *http.Client
	UserAgent string
	// MaxRetries is the number of retries after the first attempt (default 4).
	MaxRetries int
	// MaxRetryWait caps a single wait between retries (default 60s).
	MaxRetryWait time.Duration
	// sleep is replaced in tests.
	sleep func(context.Context, time.Duration) error
}

// New returns a client for base, which may be the server root
// ("http://host:8420") or the API base ("http://host:8420/api/v1").
func New(base, apiKey string) *Client {
	return &Client{
		BaseURL:      NormalizeBase(base),
		APIKey:       apiKey,
		HTTP:         &http.Client{},
		UserAgent:    "crowdvm-cli/" + Version,
		MaxRetries:   4,
		MaxRetryWait: 60 * time.Second,
	}
}

// NormalizeBase strips trailing slashes and a trailing /api/v1 and adds
// http:// when no scheme is given.
func NormalizeBase(base string) string {
	base = strings.TrimSpace(base)
	if base != "" && !strings.Contains(base, "://") {
		base = "http://" + base
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/api/v1")
	return strings.TrimRight(base, "/")
}

// APIBase returns BaseURL + "/api/v1".
func (c *Client) APIBase() string { return c.BaseURL + "/api/v1" }

// Error is an API error (the {"ok":false,...} envelope), or a synthesized one
// for non-JSON failures.
type Error struct {
	Status     int            `json:"status"`
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	RequestID  string         `json:"requestId,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	RetryAfter time.Duration  `json:"-"`
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
	}
	return e.Code + ": " + msg
}

// Retryable reports whether repeating the same request later can succeed.
func (e *Error) Retryable() bool {
	if b, ok := e.Details["retryable"].(bool); ok {
		return b
	}
	switch e.Status {
	case 429, 502, 503, 504:
		return true
	}
	return e.Code == "sandbox_starting" || e.Code == "sandbox_restoring"
}

// AsError returns the *Error in err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsCode reports whether err is an API error with one of the given codes.
func IsCode(err error, codes ...string) bool {
	e, ok := AsError(err)
	if !ok {
		return false
	}
	for _, c := range codes {
		if e.Code == c {
			return true
		}
	}
	return false
}

// IsStatus reports whether err is an API error with the given HTTP status.
func IsStatus(err error, status int) bool {
	e, ok := AsError(err)
	return ok && e.Status == status
}

// parseError decodes an error envelope from a non-2xx response body.
func parseError(resp *http.Response, body []byte) *Error {
	var env struct {
		Status    int    `json:"status"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId"`
		Err       *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Status  int            `json:"status"`
			Details map[string]any `json:"details"`
		} `json:"error"`
		RetryAfterSeconds *float64 `json:"retryAfterSeconds"`
	}
	e := &Error{Status: resp.StatusCode}
	if json.Unmarshal(body, &env) == nil {
		e.Code, e.Message, e.RequestID = env.Code, env.Message, env.RequestID
		if env.Err != nil {
			if e.Code == "" {
				e.Code = env.Err.Code
			}
			if e.Message == "" {
				e.Message = env.Err.Message
			}
			e.Details = env.Err.Details
		}
		if env.RetryAfterSeconds != nil {
			e.RetryAfter = secs(*env.RetryAfterSeconds)
		}
	} else if s := strings.TrimSpace(string(body)); s != "" && len(s) < 500 {
		e.Message = s
	}
	if e.Code == "" {
		e.Code = codeForStatus(resp.StatusCode)
	}
	if e.RequestID == "" {
		e.RequestID = resp.Header.Get("X-Request-Id")
	}
	if v, ok := e.Details["retryAfterSeconds"].(float64); ok && e.RetryAfter == 0 {
		e.RetryAfter = secs(v)
	}
	if e.RetryAfter == 0 {
		if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil {
			e.RetryAfter = secs(s)
		}
	}
	return e
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func codeForStatus(s int) string {
	switch s {
	case 400:
		return "bad_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 503:
		return "unavailable"
	}
	return "http_" + strconv.Itoa(s)
}

// Request describes one API call for Client.Do.
type Request struct {
	Method string
	// Path is relative to the API base ("/sandboxes"), or to the server root
	// when RootPath is set.
	Path   string
	Query  url.Values
	Body   any // JSON-encoded unless it is an io.Reader or []byte
	Header http.Header
	// IdempotencyKey makes the POST safe to retry after network errors.
	IdempotencyKey string
	// RootPath means Path is relative to the server root, not /api/v1.
	RootPath bool
	// NoRetry disables retries.
	NoRetry bool
}

// Do performs req and decodes a 2xx JSON body into out (if non-nil).
func (c *Client) Do(ctx context.Context, req *Request, out any) error {
	resp, err := c.send(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s %s: %w", req.Method, req.Path, err)
	}
	return nil
}

// Raw performs req and returns the 2xx response; the caller closes Body.
// Use it for byte streams and NDJSON.
func (c *Client) Raw(ctx context.Context, req *Request) (*http.Response, error) {
	return c.send(ctx, req)
}

// send runs the request with retries and returns a 2xx response or an error.
func (c *Client) send(ctx context.Context, req *Request) (*http.Response, error) {
	var body []byte
	var stream io.Reader
	ctype := ""
	switch b := req.Body.(type) {
	case nil:
	case []byte:
		body, ctype = b, "application/octet-stream"
	case io.Reader:
		stream, ctype = b, "application/octet-stream"
	default:
		var err error
		if body, err = json.Marshal(b); err != nil {
			return nil, err
		}
		ctype = "application/json"
	}
	u := c.url(req)
	idempotent := req.Method == "" || req.Method == http.MethodGet || req.Method == http.MethodHead ||
		req.Method == http.MethodPut || req.Method == http.MethodDelete || req.IdempotencyKey != ""
	retries := c.MaxRetries
	if req.NoRetry || stream != nil {
		retries = 0
	}
	for attempt := 0; ; attempt++ {
		var rd io.Reader = stream
		if body != nil {
			rd = bytes.NewReader(body)
		}
		hr, err := http.NewRequestWithContext(ctx, orGET(req.Method), u, rd)
		if err != nil {
			return nil, err
		}
		c.setHeaders(hr.Header)
		if ctype != "" {
			hr.Header.Set("Content-Type", ctype)
		}
		if req.IdempotencyKey != "" {
			hr.Header.Set("Idempotency-Key", req.IdempotencyKey)
		}
		for k, vs := range req.Header {
			hr.Header[k] = vs
		}
		resp, err := c.HTTP.Do(hr)
		if err == nil {
			noteClientStatus(resp)
		}
		var wait time.Duration
		if err != nil {
			if ctx.Err() != nil || !idempotent || attempt >= retries {
				return nil, err
			}
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		} else {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			apiErr := parseError(resp, b)
			// no_ready_machine answers failFast: the caller asked not to wait.
			retryableStatus := resp.StatusCode == 429 || (resp.StatusCode == 503 && apiErr.Code != "no_ready_machine") ||
				(idempotent && (resp.StatusCode == 502 || resp.StatusCode == 504))
			if !retryableStatus || attempt >= retries {
				return nil, apiErr
			}
			wait = apiErr.RetryAfter
		}
		if wait <= 0 {
			wait = backoff(attempt)
		}
		if c.MaxRetryWait > 0 && wait > c.MaxRetryWait {
			wait = c.MaxRetryWait
		}
		if err := c.doSleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func orGET(m string) string {
	if m == "" {
		return http.MethodGet
	}
	return m
}

func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond * time.Duration(math.Pow(2, float64(attempt)))
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d/2 + time.Duration(mrand.Int64N(int64(d/2)+1))
}

func (c *Client) doSleep(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) setHeaders(h http.Header) {
	if c.APIKey != "" {
		h.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Org != "" {
		h.Set("X-CrowdVM-Org", c.Org)
	}
	if c.UserAgent != "" {
		h.Set("User-Agent", c.UserAgent)
	}
	h.Set("Accept", "application/json, application/x-ndjson")
	h.Set(compat.Header, strconv.Itoa(compat.APIGen))
}

func (c *Client) url(req *Request) string {
	base := c.APIBase()
	if req.RootPath {
		base = c.BaseURL
	}
	u := base + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	return u
}

// NewIdempotencyKey returns a random key for Idempotency-Key headers.
func NewIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// orgQuery returns the "org" query for endpoints that take one.
func (c *Client) orgQuery(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if c.Org != "" && q.Get("org") == "" {
		q.Set("org", c.Org)
	}
	return q
}

func p(parts ...string) string {
	var sb strings.Builder
	for _, s := range parts {
		sb.WriteByte('/')
		sb.WriteString(url.PathEscape(s))
	}
	return sb.String()
}

func get(path string, q url.Values) *Request { return &Request{Method: "GET", Path: path, Query: q} }
func post(path string, body any) *Request    { return &Request{Method: "POST", Path: path, Body: body} }
func put(path string, body any) *Request     { return &Request{Method: "PUT", Path: path, Body: body} }
func patch(path string, body any) *Request   { return &Request{Method: "PATCH", Path: path, Body: body} }
func del(path string, q url.Values) *Request {
	return &Request{Method: "DELETE", Path: path, Query: q}
}

// Object is an untyped JSON object, used for payloads with open shapes.
type Object = map[string]any

// call is a typed shorthand for Do.
func call[T any](ctx context.Context, c *Client, req *Request) (*T, error) {
	out := new(T)
	if err := c.Do(ctx, req, out); err != nil {
		return nil, err
	}
	return out, nil
}
