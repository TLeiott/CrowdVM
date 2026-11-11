package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// mountWeb serves Cfg.WebFS at / with SPA fallback to index.html.
func (s *Server) mountWeb() {
	if s.Cfg.WebFS == nil {
		// Minimal approval page; the dashboard serves its own /device.
		s.router.HandleFunc("GET /device", s.handleDevicePage)
		s.router.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("CrowdVM server. API: " + s.Cfg.PublicURL + "/api/v1\n"))
		})
		return
	}
	root := webRoot(s.Cfg.WebFS)
	files := http.FileServerFS(root)
	s.router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The dashboard (with the device-login approval page) is never shown
		// in another site's frame (bugs-r8-sonnet 2); a desktop URL is meant
		// for one (Boat: "Put the URL in an iframe").
		if !strings.HasPrefix(r.URL.Path, "/desktop/") {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(root, p); err != nil || st.IsDir() {
			if path.Ext(p) != "" && !strings.HasSuffix(p, ".html") {
				http.NotFound(w, r)
				return
			}
			b, err := fs.ReadFile(root, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(b)
			return
		}
		if p == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// webRoot finds the directory holding index.html (root, or one level down
// such as dist/).
func webRoot(f fs.FS) fs.FS {
	if _, err := fs.Stat(f, "index.html"); err == nil {
		return f
	}
	ents, _ := fs.ReadDir(f, ".")
	for _, e := range ents {
		if e.IsDir() {
			if _, err := fs.Stat(f, e.Name()+"/index.html"); err == nil {
				sub, _ := fs.Sub(f, e.Name())
				return sub
			}
		}
	}
	return f
}
