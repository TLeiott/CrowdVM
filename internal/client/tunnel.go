package client

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Tunnel opens a raw TCP connection to 127.0.0.1:port inside the sandbox,
// carried over a binary websocket (GET /api/v1/sandboxes/{id}/tunnel?port=N).
// The returned conn stays open after ctx is done; close it to end the tunnel.
// Transient failures (sandbox starting, 429/503) are retried.
func (c *Client) Tunnel(ctx context.Context, id string, port int) (net.Conn, error) {
	u := c.APIBase() + p("sandboxes", id, "tunnel") + "?" + url.Values{"port": {strconv.Itoa(port)}}.Encode()
	u = "ws" + strings.TrimPrefix(u, "http") // http→ws, https→wss
	h := http.Header{}
	c.setHeaders(h)
	h.Del("Accept")
	for attempt := 0; ; attempt++ {
		ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.HTTP, HTTPHeader: h})
		if err == nil {
			ws.SetReadLimit(-1)
			// NetConn's context governs the conn lifetime, not the dial.
			return websocket.NetConn(context.Background(), ws, websocket.MessageBinary), nil
		}
		var apiErr *Error
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			var b []byte
			if resp.Body != nil {
				b, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
			}
			apiErr = parseError(resp, b)
		}
		if apiErr == nil || !apiErr.Retryable() || attempt >= c.MaxRetries || ctx.Err() != nil {
			if apiErr != nil {
				return nil, apiErr
			}
			return nil, err
		}
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = backoff(attempt) + time.Second
		}
		if c.MaxRetryWait > 0 && wait > c.MaxRetryWait {
			wait = c.MaxRetryWait
		}
		if err := c.doSleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// Pipe copies local and remote both ways until the remote side closes (or
// fails), then closes remote. EOF on local's reader does not end the pipe:
// the remote side may still be answering.
func Pipe(local io.ReadWriter, remote net.Conn) error {
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(remote, local)
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		_ = err // keep reading until the remote side ends
	}()
	go func() {
		_, err := io.Copy(local, remote)
		done <- err
	}()
	err := <-done
	remote.Close()
	if err != nil && isClosedErr(err) {
		return nil
	}
	return err
}

func isClosedErr(err error) bool {
	if isEOF(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "use of closed") || strings.Contains(s, "StatusNormalClosure") ||
		strings.Contains(s, "status = StatusNormalClosure") || strings.Contains(s, "failed to read frame header: EOF")
}
