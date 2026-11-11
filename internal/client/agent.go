package client

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// --- commands ---

// Exec runs a command and waits for it (sync mode).
func (c *Client) Exec(ctx context.Context, id string, req CommandRequest) (*CommandResult, error) {
	req.Stream, req.Detached = false, false
	r := post(p("sandboxes", id, "commands"), req)
	return call[CommandResult](ctx, c, r)
}

// ExecStream runs a command and streams its output frames.
func (c *Client) ExecStream(ctx context.Context, id string, req CommandRequest) (*Stream[CommandFrame], error) {
	req.Stream, req.Detached = true, false
	return openStream[CommandFrame](ctx, c, post(p("sandboxes", id, "commands"), req))
}

// ExecDetached starts a background command and returns its process id.
func (c *Client) ExecDetached(ctx context.Context, id string, req CommandRequest) (*CommandStarted, error) {
	req.Stream, req.Detached = false, true
	return call[CommandStarted](ctx, c, post(p("sandboxes", id, "commands"), req))
}

// CommandStatus reads a detached command's status and log tails.
func (c *Client) CommandStatus(ctx context.Context, id string, pid int, o *CommandStatusOptions) (*CommandStatus, error) {
	q := url.Values{}
	if o != nil {
		setInt(q, "tailBytes", o.TailBytes)
		setInt64(q, "stdoutOffset", o.StdoutOffset)
		setInt64(q, "stderrOffset", o.StderrOffset)
	}
	return call[CommandStatus](ctx, c, get(p("sandboxes", id, "commands", strconv.Itoa(pid)), q))
}

// StopCommand kills a detached command and its children.
func (c *Client) StopCommand(ctx context.Context, id string, pid int) (*CommandStopResult, error) {
	return call[CommandStopResult](ctx, c, del(p("sandboxes", id, "commands", strconv.Itoa(pid)), nil))
}

// AttachCommand streams a detached command's output from the given offsets.
func (c *Client) AttachCommand(ctx context.Context, id string, pid int, stdoutOff, stderrOff int64) (*Stream[AttachFrame], error) {
	q := url.Values{}
	setInt64(q, "stdoutOffset", stdoutOff)
	setInt64(q, "stderrOffset", stderrOff)
	return openStream[AttachFrame](ctx, c, get(p("sandboxes", id, "commands", strconv.Itoa(pid), "stream"), q))
}

// CommandHistory lists commands, newest first. mode is a comma list.
func (c *Client) CommandHistory(ctx context.Context, id, mode, query string, limit int, cursor string) (*CommandHistory, error) {
	q := url.Values{}
	setStr(q, "mode", mode)
	setStr(q, "q", query)
	setInt(q, "limit", limit)
	setStr(q, "cursor", cursor)
	return call[CommandHistory](ctx, c, get(p("sandboxes", id, "command-history"), q))
}

func (c *Client) GetCommand(ctx context.Context, id, commandID string) (*CommandRecord, error) {
	r, err := call[struct {
		Command CommandRecord `json:"command"`
	}](ctx, c, get(p("sandboxes", id, "command-history", commandID), nil))
	if err != nil {
		return nil, err
	}
	return &r.Command, nil
}

// CommandLogs reads stored output. stream is "stdout" or "stderr".
func (c *Client) CommandLogs(ctx context.Context, id, commandID, stream string, offset int64, limit int) (*CommandLogs, error) {
	q := url.Values{}
	setStr(q, "stream", stream)
	setInt64(q, "offset", offset)
	setInt(q, "limit", limit)
	return call[CommandLogs](ctx, c, get(p("sandboxes", id, "command-history", commandID, "logs"), q))
}

// Bytes decodes Data according to Encoding.
func (l *CommandLogs) Bytes() []byte {
	if l.Encoding == "base64" {
		b, _ := base64.StdEncoding.DecodeString(l.Data)
		return b
	}
	return []byte(l.Data)
}

func (c *Client) Processes(ctx context.Context, id string) (*ProcessList, error) {
	return call[ProcessList](ctx, c, get(p("sandboxes", id, "processes"), nil))
}

// --- files ---

// ReadFile returns a file's bytes (JSON endpoint, base64 transport).
func (c *Client) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	r, err := call[FileContent](ctx, c, get(p("sandboxes", id, "files"), url.Values{"path": {path}, "encoding": {"base64"}}))
	if err != nil {
		return nil, err
	}
	if r.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(r.Content)
	}
	return []byte(r.Content), nil
}

// WriteFile writes data to path (JSON endpoint, base64 transport).
func (c *Client) WriteFile(ctx context.Context, id, path string, data []byte) error {
	return c.Do(ctx, put(p("sandboxes", id, "files"), FileSpec{
		Path: path, Content: base64.StdEncoding.EncodeToString(data), Encoding: "base64",
	}), nil)
}

func (c *Client) DeleteFile(ctx context.Context, id, path string, recursive bool) error {
	q := url.Values{"path": {path}}
	if recursive {
		q.Set("recursive", "true")
	}
	return c.Do(ctx, del(p("sandboxes", id, "files"), q), nil)
}

func (c *Client) ListFiles(ctx context.Context, id, path string) (*FileList, error) {
	return call[FileList](ctx, c, get(p("sandboxes", id, "files", "list"), url.Values{"path": {path}}))
}

// StatFile returns one entry; a missing path is a not_found error.
func (c *Client) StatFile(ctx context.Context, id, path string) (*FileEntry, error) {
	return call[FileEntry](ctx, c, get(p("sandboxes", id, "files", "info"), url.Values{"path": {path}}))
}

func (c *Client) SearchFiles(ctx context.Context, id, path, pattern string) (*FileSearchResult, error) {
	return call[FileSearchResult](ctx, c, get(p("sandboxes", id, "files", "search"), url.Values{"path": {path}, "pattern": {pattern}}))
}

func (c *Client) FindInFiles(ctx context.Context, id, path, pattern string, regex bool) (*FileFindResult, error) {
	q := url.Values{"path": {path}, "pattern": {pattern}}
	if regex {
		q.Set("regex", "true")
	}
	return call[FileFindResult](ctx, c, get(p("sandboxes", id, "files", "find"), q))
}

func (c *Client) CreateFolder(ctx context.Context, id, path, mode string) error {
	return c.Do(ctx, post(p("sandboxes", id, "files", "folder"), Object{"path": path, "mode": mode}), nil)
}

func (c *Client) MoveFile(ctx context.Context, id, src, dst string, overwrite bool) error {
	return c.Do(ctx, post(p("sandboxes", id, "files", "move"), Object{"source": src, "destination": dst, "overwrite": overwrite}), nil)
}

func (c *Client) SetFilePermissions(ctx context.Context, id, path, mode string) error {
	return c.Do(ctx, post(p("sandboxes", id, "files", "permissions"), Object{"path": path, "mode": mode}), nil)
}

func (c *Client) ReplaceInFiles(ctx context.Context, id string, paths []string, pattern, newValue string) (*FileReplaceResult, error) {
	return call[FileReplaceResult](ctx, c, post(p("sandboxes", id, "files", "replace"), Object{"paths": paths, "pattern": pattern, "newValue": newValue}))
}

// DownloadURL returns a short-lived signed GET URL for path.
func (c *Client) DownloadURL(ctx context.Context, id, path string, ttlSeconds int) (*FileURL, error) {
	return call[FileURL](ctx, c, post(p("sandboxes", id, "files", "download-url"), fileURLReq(path, ttlSeconds)))
}

// UploadURL returns a short-lived signed PUT URL for path.
func (c *Client) UploadURL(ctx context.Context, id, path string, ttlSeconds int) (*FileURL, error) {
	return call[FileURL](ctx, c, post(p("sandboxes", id, "files", "upload-url"), fileURLReq(path, ttlSeconds)))
}

func fileURLReq(path string, ttl int) Object {
	o := Object{"path": path}
	if ttl > 0 {
		o["ttlSeconds"] = ttl
	}
	return o
}

// Artifact streams one file (up to 50 MiB) as raw bytes; the caller closes it.
func (c *Client) Artifact(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.send(ctx, get(p("sandboxes", id, "artifacts"), url.Values{"path": {path}}))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Upload writes r to path in the sandbox using a signed upload URL (no size
// limit). When the server has no signed URLs it falls back to WriteFile.
func (c *Client) Upload(ctx context.Context, id, path string, r io.Reader) error {
	u, err := c.UploadURL(ctx, id, path, 0)
	if err != nil {
		if !IsStatus(err, 404) && !IsStatus(err, 501) && !IsCode(err, "not_implemented") {
			return err
		}
		data, rerr := io.ReadAll(r)
		if rerr != nil {
			return rerr
		}
		return c.WriteFile(ctx, id, path, data)
	}
	hr, err := http.NewRequestWithContext(ctx, orDefault(u.Method, "PUT"), c.resolve(u.URL), r)
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return parseError(resp, b)
	}
	return nil
}

// Download streams the file at path, via a signed URL when available, else
// the artifacts endpoint. The caller closes the reader.
func (c *Client) Download(ctx context.Context, id, path string) (io.ReadCloser, error) {
	u, err := c.DownloadURL(ctx, id, path, 0)
	if err != nil {
		if !IsStatus(err, 404) && !IsStatus(err, 501) && !IsCode(err, "not_implemented") || IsCode(err, "file_not_found") {
			return nil, err
		}
		return c.Artifact(ctx, id, path)
	}
	hr, err := http.NewRequestWithContext(ctx, orDefault(u.Method, "GET"), c.resolve(u.URL), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, parseError(resp, b)
	}
	return resp.Body, nil
}

// resolve makes a server-relative URL absolute.
func (c *Client) resolve(u string) string {
	if strings.Contains(u, "://") {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return c.BaseURL + u
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// --- integrated agents ---

func (c *Client) Prompt(ctx context.Context, id string, req *PromptRequest) (*PromptResult, error) {
	return call[PromptResult](ctx, c, post(p("sandboxes", id, "prompt"), req))
}

func (c *Client) PromptRunStatus(ctx context.Context, id, promptID string) (*PromptRun, error) {
	r, err := call[struct {
		PromptRun PromptRun `json:"promptRun"`
	}](ctx, c, get(p("sandboxes", id, "prompts", promptID), nil))
	if err != nil {
		return nil, err
	}
	return &r.PromptRun, nil
}

func (c *Client) Events(ctx context.Context, id string, o *EventsOptions) (*EventList, error) {
	q := url.Values{}
	if o != nil {
		setInt(q, "limit", o.Limit)
		setStr(q, "cursor", o.Cursor)
		setStr(q, "sort", o.Sort)
		setStr(q, "type", o.Type)
		for _, cv := range o.Conversations {
			q.Add("conversation", cv)
		}
	}
	return call[EventList](ctx, c, get(p("sandboxes", id, "events"), q))
}

func (c *Client) Conversations(ctx context.Context, id string) ([]Conversation, error) {
	r, err := call[struct {
		Conversations []Conversation `json:"conversations"`
	}](ctx, c, get(p("sandboxes", id, "conversations"), nil))
	if err != nil {
		return nil, err
	}
	return r.Conversations, nil
}

// Steer adds a message to a running turn. conversation may be empty.
func (c *Client) Steer(ctx context.Context, id, message, conversation string) (*SteerResult, error) {
	body := Object{"message": message}
	if conversation != "" {
		body["conversation"] = conversation
	}
	return call[SteerResult](ctx, c, post(p("sandboxes", id, "steer"), body))
}

// Interrupt stops agent work: all conversations, or just one.
func (c *Client) Interrupt(ctx context.Context, id, conversation string) (*SandboxAction, error) {
	r := post(p("sandboxes", id, "interrupt"), nil)
	if conversation != "" {
		r.Query = url.Values{"conversation": {conversation}}
	}
	return call[SandboxAction](ctx, c, r)
}

// ExitCode returns the exit code of a result, or -1 when unknown.
func ExitCode(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// FrameError converts an error frame to *Error.
func FrameError(code, msg string, retryable bool) error {
	if code == "" {
		code = "stream_error"
	}
	e := &Error{Code: code, Message: msg}
	if retryable {
		e.Details = map[string]any{"retryable": true}
	}
	return e
}
