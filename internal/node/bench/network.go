package bench

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Network measures latency (median of small requests), download and upload
// throughput against the server's bench endpoints. Each transfer runs for at
// most d. hostHint (or the URL host) classifies the path.
func Network(ctx context.Context, c *http.Client, serverURL string, hdr http.Header, hostHint string, d time.Duration) (proto.NetworkBench, error) {
	serverURL = strings.TrimRight(serverURL, "/")
	if hostHint == "" {
		if u, err := url.Parse(serverURL); err == nil {
			hostHint = u.Host
		}
	}
	r := proto.NetworkBench{Via: Classify(hostHint)}

	do := func(ctx context.Context, method, u string, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return nil, fmt.Errorf("%s %s: %s", method, u, resp.Status)
		}
		return resp, nil
	}

	// Latency: 1 warm-up + 7 samples.
	var samples []float64
	for i := range 8 {
		t := time.Now()
		resp, err := do(ctx, "GET", serverURL+"/node/v1/bench/download?bytes=1", nil)
		if err != nil {
			return r, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if i > 0 {
			samples = append(samples, float64(time.Since(t).Microseconds())/1000)
		}
	}
	sort.Float64s(samples)
	r.LatencyMs = samples[len(samples)/2]

	// Download: ask for plenty, stop reading after d.
	dctx, cancel := context.WithTimeout(ctx, d+5*time.Second)
	defer cancel()
	resp, err := do(dctx, "GET", fmt.Sprintf("%s/node/v1/bench/download?bytes=%d", serverURL, int64(4)<<30), nil)
	if err != nil {
		return r, err
	}
	start := time.Now()
	n, _ := copyFor(resp.Body, d)
	r.DownMbps = float64(n) * 8 / time.Since(start).Seconds() / 1e6
	resp.Body.Close()

	// Upload: stream for d.
	uctx, cancel2 := context.WithTimeout(ctx, d+10*time.Second)
	defer cancel2()
	src := &timedReader{deadline: time.Now().Add(d), max: int64(4) << 30}
	start = time.Now()
	resp, err = do(uctx, "POST", serverURL+"/node/v1/bench/upload", src)
	if err != nil {
		return r, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r.UpMbps = float64(src.n) * 8 / time.Since(start).Seconds() / 1e6
	return r, nil
}

func copyFor(r io.Reader, d time.Duration) (int64, error) {
	buf := make([]byte, 256<<10)
	deadline := time.Now().Add(d)
	var n int64
	for time.Now().Before(deadline) {
		m, err := r.Read(buf)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

type timedReader struct {
	deadline time.Time
	max, n   int64
}

var zeros = make([]byte, 256<<10)

func (t *timedReader) Read(p []byte) (int, error) {
	if t.n >= t.max || time.Now().After(t.deadline) {
		return 0, io.EOF
	}
	m := copy(p, zeros)
	t.n += int64(m)
	return m, nil
}

var tailscaleV4 = mustCIDR("100.64.0.0/10")
var tailscaleV6 = mustCIDR("fd7a:115c:a1e0::/48")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// Classify returns "loopback", "tailscale", "lan" or "other" for a server
// host[:port]. Names are resolved; a *.ts.net name or a route over a
// tailscale interface also counts as tailscale.
func Classify(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "other"
	}
	if strings.HasSuffix(strings.ToLower(host), ".ts.net") {
		return "tailscale"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return "other"
		}
		ip = ips[0]
	}
	switch {
	case ip.IsLoopback():
		return "loopback"
	case tailscaleV4.Contains(ip), tailscaleV6.Contains(ip):
		return "tailscale"
	case ip.IsPrivate(), ip.IsLinkLocalUnicast():
		return "lan"
	}
	if strings.HasPrefix(strings.ToLower(ifaceName(ip)), "tailscale") {
		return "tailscale"
	}
	return "other"
}

// ifaceName returns the name of the local interface used to reach ip.
func ifaceName(ip net.IP) string {
	c, err := net.Dial("udp", net.JoinHostPort(ip.String(), "9"))
	if err != nil {
		return ""
	}
	local := c.LocalAddr().(*net.UDPAddr).IP
	c.Close()
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
				return i.Name
			}
		}
	}
	return ""
}

// InternetTargets are public anycast endpoints the internet check dials.
var InternetTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}

// Internet checks the node's own outbound internet (its sandboxes use it
// for packages and AI APIs; the server test above only covers the LAN):
// three rounds of TCP connects to targets in parallel, best time wins.
func Internet(ctx context.Context, targets []string, timeout time.Duration) (string, float64) {
	best := -1.0
	for range 3 {
		ch := make(chan float64, len(targets))
		for _, t := range targets {
			go func(addr string) {
				start := time.Now()
				var d net.Dialer
				cctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				c, err := d.DialContext(cctx, "tcp", addr)
				if err != nil {
					ch <- -1
					return
				}
				c.Close()
				ch <- float64(time.Since(start).Microseconds()) / 1000
			}(t)
		}
		for range targets {
			if ms := <-ch; ms >= 0 && (best < 0 || ms < best) {
				best = ms
			}
		}
	}
	if best < 0 {
		return "unreachable", 0
	}
	return "reachable", best
}
