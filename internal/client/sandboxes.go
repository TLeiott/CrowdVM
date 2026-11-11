package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// CreateSandbox creates one sandbox (or req.Count sandboxes with req.From).
// idemKey makes the call safe to retry; an empty key gets a random one.
func (c *Client) CreateSandbox(ctx context.Context, req *CreateSandboxRequest, idemKey string) (*CreateSandboxResponse, error) {
	if idemKey == "" {
		idemKey = NewIdempotencyKey()
	}
	r := post("/sandboxes", req)
	r.IdempotencyKey = idemKey
	r.Query = c.orgQuery(nil)
	return call[CreateSandboxResponse](ctx, c, r)
}

// ListSandboxes lists one page of sandboxes.
func (c *Client) ListSandboxes(ctx context.Context, o *ListSandboxesOptions) (*SandboxList, error) {
	q := c.orgQuery(nil)
	if o != nil {
		setInt(q, "limit", o.Limit)
		setStr(q, "cursor", o.Cursor)
		setStr(q, "sort", o.Sort)
		setStr(q, "state", o.State)
		setStr(q, "namePrefix", o.NamePrefix)
		setStr(q, "include", o.Include)
		for _, l := range o.Label {
			q.Add("label", l)
		}
	}
	return call[SandboxList](ctx, c, get("/sandboxes", q))
}

// ListAllSandboxes follows the cursor through every page.
func (c *Client) ListAllSandboxes(ctx context.Context, o ListSandboxesOptions) ([]Sandbox, error) {
	var all []Sandbox
	if o.Limit == 0 {
		o.Limit = 100
	}
	for {
		page, err := c.ListSandboxes(ctx, &o)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Sandboxes...)
		if !page.PageInfo.HasMore || page.PageInfo.NextCursor == "" || page.PageInfo.NextCursor == o.Cursor {
			return all, nil
		}
		o.Cursor = page.PageInfo.NextCursor
	}
}

func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	r, err := call[struct {
		Sandbox Sandbox `json:"sandbox"`
	}](ctx, c, get(p("sandboxes", id), nil))
	if err != nil {
		return nil, err
	}
	return &r.Sandbox, nil
}

// UpdateSandbox renames a sandbox or sets its auto-stop timer.
func (c *Client) UpdateSandbox(ctx context.Context, id string, req *UpdateSandboxRequest) (*Sandbox, error) {
	r, err := call[struct {
		Sandbox Sandbox `json:"sandbox"`
	}](ctx, c, patch(p("sandboxes", id), req))
	if err != nil {
		return nil, err
	}
	return &r.Sandbox, nil
}

// DeleteSandbox permanently deletes a sandbox and its snapshots.
func (c *Client) DeleteSandbox(ctx context.Context, id string) (*DeletionOperation, error) {
	r := del(p("sandboxes", id), nil)
	r.Header = confirmHeader(id)
	return opResult(ctx, c, r)
}

func confirmHeader(id string) http.Header {
	h := http.Header{}
	h.Set("X-CrowdVM-Confirm-Delete", id)
	h.Set("X-Ascii-Confirm-Delete", id)
	return h
}

func opResult(ctx context.Context, c *Client, r *Request) (*DeletionOperation, error) {
	out, err := call[struct {
		Operation DeletionOperation `json:"operation"`
	}](ctx, c, r)
	if err != nil {
		return nil, err
	}
	return &out.Operation, nil
}

func (c *Client) StopSandbox(ctx context.Context, id string, force bool) (*SandboxAction, error) {
	return call[SandboxAction](ctx, c, post(p("sandboxes", id, "stop"), Object{"force": force}))
}

// GetStop reads one stop operation.
func (c *Client) GetStop(ctx context.Context, id, stopID string) (*StopOperation, error) {
	r, err := call[struct {
		Stop StopOperation `json:"stop"`
	}](ctx, c, get(p("sandboxes", id, "stops", stopID), nil))
	if err != nil {
		return nil, err
	}
	return &r.Stop, nil
}

func (c *Client) ResumeSandbox(ctx context.Context, id string, req *ResumeRequest) (*SandboxAction, error) {
	if req == nil {
		req = &ResumeRequest{}
	}
	return call[SandboxAction](ctx, c, post(p("sandboxes", id, "resume"), req))
}

// ForkSandbox forks id; the response ID is the new sandbox.
func (c *Client) ForkSandbox(ctx context.Context, id string, req *ForkRequest, idemKey string) (*SandboxAction, error) {
	if req == nil {
		req = &ForkRequest{}
	}
	if idemKey == "" {
		idemKey = NewIdempotencyKey()
	}
	r := post(p("sandboxes", id, "fork"), req)
	r.IdempotencyKey = idemKey
	return call[SandboxAction](ctx, c, r)
}

func (c *Client) ShareSandbox(ctx context.Context, id string) (*SandboxAction, error) {
	return call[SandboxAction](ctx, c, post(p("sandboxes", id, "share"), nil))
}

func (c *Client) UnshareSandbox(ctx context.Context, id string) (*SandboxAction, error) {
	return call[SandboxAction](ctx, c, post(p("sandboxes", id, "unshare"), nil))
}

func (c *Client) RecoverSandbox(ctx context.Context, id string) (*SandboxAction, error) {
	return call[SandboxAction](ctx, c, post(p("sandboxes", id, "recover"), nil))
}

// HostPort exposes a sandbox port on a URL.
func (c *Client) HostPort(ctx context.Context, id string, req *HostPortRequest) (*HostPortResult, error) {
	return call[HostPortResult](ctx, c, post(p("sandboxes", id, "host"), req))
}

// Desktop returns the desktop (VNC) URL. vnc selects noVNC.
func (c *Client) Desktop(ctx context.Context, id string, req *DesktopRequest, vnc bool) (*DesktopResult, error) {
	if req == nil {
		req = &DesktopRequest{}
	}
	r := post(p("sandboxes", id, "desktop"), req)
	if vnc {
		r.Query = url.Values{"vnc": {"1"}}
	}
	return call[DesktopResult](ctx, c, r)
}

// AddSSHKey authorizes an OpenSSH public key for user "user".
func (c *Client) AddSSHKey(ctx context.Context, id, publicKey string) (*SSHKeyResult, error) {
	return call[SSHKeyResult](ctx, c, post(p("sandboxes", id, "sshkey"), Object{"key": publicKey}))
}

// SandboxUsage returns machine time and cost; since/until are ISO 8601 or Unix seconds.
func (c *Client) SandboxUsage(ctx context.Context, id, since, until string) (*SandboxUsage, error) {
	q := url.Values{}
	setStr(q, "since", since)
	setStr(q, "until", until)
	return call[SandboxUsage](ctx, c, get(p("sandboxes", id, "usage"), q))
}

func (c *Client) SandboxAIUsage(ctx context.Context, id string, days int) (Object, error) {
	q := url.Values{}
	setInt(q, "days", days)
	return obj(ctx, c, get(p("sandboxes", id, "ai-usage"), q))
}

func (c *Client) SetSandboxAIAccess(ctx context.Context, id string, blocked bool) (Object, error) {
	return obj(ctx, c, put(p("sandboxes", id, "ai-access"), Object{"blocked": blocked}))
}

// --- helpers ---

func setStr(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func setInt(q url.Values, k string, v int) {
	if v != 0 {
		q.Set(k, strconv.Itoa(v))
	}
}

func setInt64(q url.Values, k string, v int64) {
	if v != 0 {
		q.Set(k, strconv.FormatInt(v, 10))
	}
}

func obj(ctx context.Context, c *Client, r *Request) (Object, error) {
	out := Object{}
	if err := c.Do(ctx, r, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DirectPort is a guest port published raw on the node's direct host (--direct-host).
type DirectPort struct {
	Port     int    `json:"port"`
	Proto    string `json:"proto,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Source   string `json:"source,omitempty"`
}

// DirectPorts is GET/PUT /sandboxes/{id}/direct-ports.
type DirectPorts struct {
	SandboxID  string       `json:"sandboxId"`
	DirectHost string       `json:"directHost,omitempty"`
	Ports      []DirectPort `json:"ports"`
}

func (c *Client) DirectPorts(ctx context.Context, id string) (*DirectPorts, error) {
	return call[DirectPorts](ctx, c, get(p("sandboxes", id, "direct-ports"), nil))
}

// SetDirectPorts replaces the ports asked for (public hosted ports stay published).
func (c *Client) SetDirectPorts(ctx context.Context, id string, ports []DirectPort) (*DirectPorts, error) {
	if ports == nil {
		ports = []DirectPort{}
	}
	return call[DirectPorts](ctx, c, put(p("sandboxes", id, "direct-ports"), Object{"ports": ports}))
}
