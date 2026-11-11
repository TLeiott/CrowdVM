// Command fakeguest is a tiny stand-in for crowdvm-guest used by the VM
// driver tests: health, tunnel, prepare-stop/sync, a Boat-shaped subset of
// /v1/files, and SSH authorized keys for user.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func main() {
	cfgPath := flag.String("config", "/run/crowdvm/cfg/config.json", "")
	home := flag.String("home", proto.GuestHome, "")
	unpriv := flag.Bool("unprivileged", false, "")
	flag.Parse()
	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg proto.GuestConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatal(err)
	}
	if len(cfg.SSHAuthorizedKeys) > 0 && !*unpriv {
		dir := filepath.Join(*home, ".ssh")
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(strings.Join(cfg.SSHAuthorizedKeys, "\n")+"\n"), 0o600)
		if u, err := user.Lookup(proto.GuestUser); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			os.Chown(dir, uid, gid)
			os.Chown(filepath.Join(dir, "authorized_keys"), uid, gid)
		}
	}
	reply := func(w http.ResponseWriter, v map[string]any) {
		v["ok"] = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	path := func(p string) string {
		return filepath.Join(*home, strings.TrimPrefix(filepath.Clean("/"+p), proto.GuestHome))
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"sandboxId": cfg.SandboxID, "boot": cfg.Boot, "fake": true})
	})
	m.HandleFunc("GET /v1/tunnel", func(w http.ResponseWriter, r *http.Request) {
		port, _ := strconv.Atoi(r.URL.Query().Get("port"))
		up, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := mux.HijackUpgrade(w)
		if err != nil {
			up.Close()
			return
		}
		mux.Join(c, up)
	})
	m.HandleFunc("POST /v1/internal/prepare-stop", func(w http.ResponseWriter, r *http.Request) {
		exec.Command("sync").Run()
		if !*unpriv {
			exec.Command("fstrim", "-a").Run()
		}
		reply(w, map[string]any{})
	})
	m.HandleFunc("POST /v1/internal/sync", func(w http.ResponseWriter, r *http.Request) {
		exec.Command("sync").Run()
		reply(w, map[string]any{})
	})
	m.HandleFunc("GET /v1/files", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(path(r.URL.Query().Get("path")))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		reply(w, map[string]any{"content": string(b)})
	})
	m.HandleFunc("PUT /v1/files", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Path, Content string }
		json.NewDecoder(r.Body).Decode(&req)
		p := path(req.Path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(req.Content), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply(w, map[string]any{})
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" && r.Header.Get("Authorization") != "Bearer "+cfg.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		m.ServeHTTP(w, r)
	})
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":7070"
	}
	log.Printf("fakeguest %s listening on %s (boot=%s)", cfg.SandboxID, addr, cfg.Boot)
	log.Fatal(http.ListenAndServe(addr, h))
}
