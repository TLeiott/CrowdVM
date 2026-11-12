package client

import (
	"net/http"
	"sync/atomic"

	"github.com/crowdvm/crowdvm/internal/compat"
)

var clientStatus atomic.Value // string: the server's last X-Crowdvm-Client-Status

func noteClientStatus(resp *http.Response) {
	if v := resp.Header.Get(compat.ClientStatusHeader); v != "" {
		clientStatus.Store(v)
	}
}

// ClientStatus is what a server said about this client's generation in
// this process ("outdated; latest=0.3.0"), "" when it said nothing.
func ClientStatus() string {
	v, _ := clientStatus.Load().(string)
	return v
}
