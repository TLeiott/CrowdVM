package client

import (
	"context"
	"time"
)

// DeviceStart is the response of POST /auth/device/start (RFC 8628 style).
type DeviceStart struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURLComplete string `json:"verificationUrlComplete,omitempty"`
	ExpiresIn               int    `json:"expiresIn,omitempty"` // seconds
	ExpiresAt               string `json:"expiresAt,omitempty"`
	Interval                int    `json:"interval,omitempty"` // poll seconds
}

// LoginResult is returned by a successful device poll or password login.
type LoginResult struct {
	APIKey string `json:"apiKey"`
	User   *User  `json:"user,omitempty"`
}

// Device-flow poll error codes (as *Error codes).
const (
	CodeAuthorizationPending = "authorization_pending"
	CodeSlowDown             = "slow_down"
	CodeExpiredToken         = "expired_token"
	CodeAccessDenied         = "access_denied"
)

// StartDeviceLogin starts a browser login for the CLI.
func (c *Client) StartDeviceLogin(ctx context.Context, clientName string) (*DeviceStart, error) {
	var raw map[string]any
	r := &Request{Method: "POST", Path: "/auth/device/start", RootPath: true, Body: Object{"clientName": clientName}}
	if err := c.Do(ctx, r, &raw); err != nil {
		return nil, err
	}
	d := &DeviceStart{
		DeviceCode:              str(raw, "deviceCode", "device_code"),
		UserCode:                str(raw, "userCode", "user_code"),
		VerificationURL:         str(raw, "verificationUrl", "verificationUri", "verification_uri", "verification_url"),
		VerificationURLComplete: str(raw, "verificationUrlComplete", "verificationUriComplete", "verification_uri_complete"),
		ExpiresAt:               str(raw, "expiresAt"),
		ExpiresIn:               num(raw, "expiresIn", "expires_in"),
		Interval:                num(raw, "interval"),
	}
	if d.VerificationURL != "" {
		d.VerificationURL = c.resolve(d.VerificationURL)
	}
	if d.VerificationURLComplete != "" {
		d.VerificationURLComplete = c.resolve(d.VerificationURLComplete)
	}
	if d.ExpiresAt == "" && d.ExpiresIn > 0 {
		d.ExpiresAt = time.Now().Add(time.Duration(d.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return d, nil
}

// PollDeviceLogin polls once. While the user has not approved yet it
// returns an *Error with code authorization_pending (or slow_down).
func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (*LoginResult, error) {
	var raw map[string]any
	r := &Request{Method: "POST", Path: "/auth/device/poll", RootPath: true, NoRetry: true, Body: Object{"deviceCode": deviceCode}}
	if err := c.Do(ctx, r, &raw); err != nil {
		return nil, err
	}
	key := str(raw, "apiKey", "api_key", "token", "accessToken", "access_token")
	if key == "" {
		switch status := str(raw, "status"); status {
		case "", "pending", CodeAuthorizationPending:
			return nil, &Error{Status: 428, Code: CodeAuthorizationPending, Message: "waiting for approval in the browser"}
		case CodeSlowDown:
			return nil, &Error{Status: 429, Code: CodeSlowDown, Message: "polling too fast"}
		case "denied", CodeAccessDenied:
			return nil, &Error{Status: 403, Code: CodeAccessDenied, Message: "the login was denied in the browser"}
		case "expired", CodeExpiredToken:
			return nil, &Error{Status: 410, Code: CodeExpiredToken, Message: "the login link expired; start again"}
		default:
			return nil, &Error{Status: 400, Code: status, Message: firstStr(str(raw, "message"), "login failed: "+status)}
		}
	}
	return &LoginResult{APIKey: key, User: userFrom(raw)}, nil
}

// WaitDeviceLogin polls until the login is approved, denied or expired.
func (c *Client) WaitDeviceLogin(ctx context.Context, d *DeviceStart) (*LoginResult, error) {
	interval := time.Duration(d.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		res, err := c.PollDeviceLogin(ctx, d.DeviceCode)
		if err == nil {
			return res, nil
		}
		switch {
		case IsCode(err, CodeSlowDown):
			interval += time.Second
		case IsCode(err, CodeAuthorizationPending):
		default:
			if e, ok := AsError(err); !ok || !e.Retryable() {
				return nil, err
			}
		}
		if err := c.doSleep(ctx, interval); err != nil {
			return nil, err
		}
	}
}

// Login exchanges a username and password for an API key.
func (c *Client) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	var raw map[string]any
	r := &Request{Method: "POST", Path: "/auth/login", RootPath: true, NoRetry: true,
		Body: Object{"username": username, "password": password, "client": "cli"}}
	if err := c.Do(ctx, r, &raw); err != nil {
		return nil, err
	}
	key := str(raw, "apiKey", "api_key", "token")
	if key == "" {
		return nil, &Error{Code: "login_failed", Message: "the server returned no API key"}
	}
	return &LoginResult{APIKey: key, User: userFrom(raw)}, nil
}

func userFrom(raw map[string]any) *User {
	m, ok := raw["user"].(map[string]any)
	if !ok {
		return nil
	}
	return &User{ID: str(m, "id"), Login: str(m, "login", "username"), Email: str(m, "email"), DisplayName: str(m, "displayName")}
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func num(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok {
			return int(f)
		}
	}
	return 0
}

func firstStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
