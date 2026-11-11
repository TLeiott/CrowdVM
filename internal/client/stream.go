package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Stream reads newline-delimited JSON values of type T from a response.
type Stream[T any] struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

func newStream[T any](resp *http.Response) *Stream[T] {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	return &Stream[T]{body: resp.Body, sc: sc}
}

// Recv returns the next value, or io.EOF at the end of the stream.
func (s *Stream[T]) Recv() (T, error) {
	var v T
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if len(line) == 0 || (len(line) == 1 && line[0] == '\r') {
			continue
		}
		err := json.Unmarshal(line, &v)
		return v, err
	}
	if err := s.sc.Err(); err != nil {
		return v, err
	}
	return v, io.EOF
}

// Close releases the connection.
func (s *Stream[T]) Close() error { return s.body.Close() }

// openStream sends req and returns an NDJSON stream. A JSON (non-NDJSON)
// 2xx body is still readable: it is one line of a stream.
func openStream[T any](ctx context.Context, c *Client, req *Request) (*Stream[T], error) {
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	return newStream[T](resp), nil
}

// isEOF reports a clean or abrupt end of a stream.
func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
