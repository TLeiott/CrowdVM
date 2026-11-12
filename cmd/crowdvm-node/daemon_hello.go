package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/crowdvm/crowdvm/internal/node"
)

// verifiedToken returns the control token only when the listener on the UI
// port proved it holds that token (bugs-r12-opus 2): a random nonce goes out,
// and the answer must be HMAC(token, "crowdvm-node-hello:"+nonce). Until then
// nothing secret leaves this process, so another local process on the port
// learns nothing. "" means no verified daemon.
func verifiedToken(ctx context.Context, cfg *node.Config) string {
	token, err := node.ReadUIToken(cfg.DataDir)
	if err != nil {
		return ""
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", uiURL(cfg)+"/api/hello?nonce="+url.QueryEscape(nonce), nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var out struct {
		Proof string `json:"proof"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out) != nil {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(out.Proof), []byte(node.HelloProof(token, nonce))) != 1 {
		return ""
	}
	return token
}
