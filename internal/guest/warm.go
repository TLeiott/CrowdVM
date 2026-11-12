package guest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Warm pool VMs (SIZE-012): a node boots VMs from its default image with
// GuestConfig{Boot: warm} before anyone asks for them. Such a guest only
// answers GET /v1/health and POST /v1/internal/adopt. Adopt saves the
// sandbox's config as <data dir>/adopted.json and returns it, and the agent
// then runs for that sandbox like a fresh create. Later agent restarts and
// reboots (the config drive still says warm) use adopted.json.

func adoptedPath(dataDir string) string {
	if dataDir == "" {
		dataDir = defaultDataDir
	}
	return filepath.Join(dataDir, "adopted.json")
}

// LoadAdopted returns the config a warm VM was adopted with, if any.
func LoadAdopted(dataDir string) (proto.GuestConfig, bool) {
	var cfg proto.GuestConfig
	b, err := os.ReadFile(adoptedPath(dataDir))
	if err != nil || json.Unmarshal(b, &cfg) != nil || cfg.Boot == proto.BootWarm || cfg.SandboxID == "" {
		return cfg, false
	}
	return cfg, true
}

// ClearAdopted removes adopted.json. It holds the adopted sandbox's whole
// config (API key, guest token, env secrets) and is only needed while the
// VM still boots from its warm config drive: any other boot (resume, fork,
// a deploy of a saved or shared snapshot) runs from its own config, and the
// file must not travel on in the disk (bugs-r13-opus 1).
func ClearAdopted(dataDir string) error {
	p := adoptedPath(dataDir)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(p + ".tmp")
	return nil
}

// RunWarm serves a warm VM until it is adopted and returns the adopted
// config (saved for restarts). listen overrides cfg.ListenAddr.
func RunWarm(ctx context.Context, cfg proto.GuestConfig, dataDir string, logger *log.Logger) (proto.GuestConfig, error) {
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":" + strconv.Itoa(proto.GuestPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return cfg, err
	}
	started := time.Now()
	adopted := make(chan proto.GuestConfig, 1)
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		ok(w, "guest.health", map[string]any{"version": Version, "boot": proto.BootWarm, "warm": true,
			"uptimeSeconds": int(time.Since(started).Seconds()), "reposCloning": false})
	})
	m.HandleFunc("POST /v1/internal/adopt", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if cfg.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		var nc proto.GuestConfig
		if !decodeJSON(w, r, &nc) {
			return
		}
		if nc.SandboxID == "" || nc.Boot == proto.BootWarm || nc.Boot == proto.BootInspect {
			writeErr(w, http.StatusBadRequest, "invalid_config", "a sandbox config (create) is required")
			return
		}
		if nc.ListenAddr == "" {
			nc.ListenAddr = cfg.ListenAddr
		}
		b, _ := json.Marshal(nc)
		p := adoptedPath(dataDir)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p+".tmp", b, 0o600); err != nil || os.Rename(p+".tmp", p) != nil {
			writeErr(w, http.StatusInternalServerError, "adopt_failed", "cannot save the config")
			return
		}
		ok(w, "guest.adopted", map[string]any{"sandboxId": nc.SandboxID})
		select {
		case adopted <- nc:
		default:
		}
	})
	srv := &http.Server{Handler: m, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	if logger != nil {
		logger.Printf("warm pool VM: waiting for adoption on %s", ln.Addr())
	}
	select {
	case nc := <-adopted:
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(sctx) // the reply is sent; the agent listens again for the sandbox
		cancel()
		return nc, nil
	case <-ctx.Done():
		srv.Close()
		return cfg, errors.New("stopped before adoption")
	}
}
