package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Join registers the node with the server (POST /node/v1/register) and saves
// the node id + token into cfg.
func Join(ctx context.Context, cfg *Config, server, joinToken string, info proto.NodeInfo) error {
	server = strings.TrimRight(server, "/")
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := doReq(ctx, http.DefaultClient, "POST", server+"/node/v1/register",
		jsonBody(proto.RegisterRequest{JoinToken: joinToken, Info: info}), nil)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	var rr proto.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return fmt.Errorf("register: bad response: %w", err)
	}
	if rr.NodeID == "" || rr.NodeToken == "" {
		return fmt.Errorf("register: server returned no node id/token")
	}
	cfg.Server, cfg.NodeID, cfg.NodeToken = server, rr.NodeID, rr.NodeToken
	return cfg.Save()
}
