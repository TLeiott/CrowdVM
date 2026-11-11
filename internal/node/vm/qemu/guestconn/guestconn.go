// Package guestconn is the node's minimal client for the guest agent's
// internal endpoints (health, prepare-stop, sync, tunnel). The qemu and
// process drivers share it.
package guestconn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
)

// Guest addresses one guest agent.
type Guest struct {
	Addr  string // host:port
	Token string
}

var client = &http.Client{Transport: &http.Transport{
	DisableKeepAlives: true, // hostfwd conns are cheap; avoid stale idle conns across reboots
}}

func (g Guest) do(ctx context.Context, method, path string, body any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+g.Addr+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("guest %s %s: %s: %s", method, path, resp.Status, b)
	}
	return nil
}

// Healthy reports whether GET /v1/health answers 2xx.
func (g Guest) Healthy(ctx context.Context) bool {
	return g.HealthyWithin(ctx, 2*time.Second)
}

// HealthyWithin is Healthy with a custom timeout.
func (g Guest) HealthyWithin(ctx context.Context, timeout time.Duration) bool {
	return g.do(ctx, http.MethodGet, "/v1/health", nil, timeout) == nil
}

// WaitHealthy polls /v1/health until it answers, ctx ends, or stop closes.
// Attempts time out quickly: while the guest NIC is down, QEMU's hostfwd
// accepts the connection but never answers.
func (g Guest) WaitHealthy(ctx context.Context, stop <-chan struct{}) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if g.do(ctx, http.MethodGet, "/v1/health", nil, 500*time.Millisecond) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stop:
			return fmt.Errorf("stopped while waiting for guest")
		case <-t.C:
		}
	}
}

// Post calls an internal endpoint, e.g. "/v1/internal/sync". body (if not
// nil) is sent as JSON.
func (g Guest) Post(ctx context.Context, path string, body any, timeout time.Duration) error {
	return g.do(ctx, http.MethodPost, path, body, timeout)
}

// Dial opens a raw TCP tunnel to 127.0.0.1:port inside the guest.
func (g Guest) Dial(ctx context.Context, port int) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", g.Addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	h := http.Header{"Authorization": {"Bearer " + g.Token}}
	tc, err := mux.DialUpgrade(c, g.Addr, "/v1/tunnel?port="+strconv.Itoa(port), h)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return tc, nil
}
