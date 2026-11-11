package server

// Install scripts and binary downloads (Boat parity INST-001, -002, -004..007).
//
//   GET /install, /install.sh   scripts/install.sh with the server URL baked in
//   GET /install.ps1            scripts/install.ps1, likewise
//   GET /downloads                              JSON list of the binaries present
//   GET /downloads/{os}/{arch}/{name}           binaries from Config.DownloadsDir
//       (crowdvm-linux-amd64 = the static in-sandbox CLI, the node's
//       --cli-binary default; Windows nodes fetch it from windows/amd64)
//   GET /api/crowdvm/cli/download?platform=linux-x64   static CLI (Boat's
//       /api/boat/cli/download; also /api/boat/cli/download)
//
// The scripts are embedded copies of scripts/ (a test keeps them in sync).
// `crowdvm-server downloads import <dist dir>` fills the downloads dir from
// scripts/dist.sh output (ImportDownloads).

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/crowdvm/crowdvm/internal/api"
)

//go:embed feat_install_files/install.sh feat_install_files/install.ps1
var installFiles embed.FS

var (
	installOS    = []string{"linux", "windows", "darwin"}
	installArch  = []string{"amd64", "arm64"}
	installNames = []string{"crowdvm", "crowdvm-node", "crowdvm-guest", "crowdvm-server", "crowdvm-linux-amd64"}
	linuxOnly    = []string{"crowdvm-guest", "crowdvm-linux-amd64"} // never .exe
)

func init() {
	RegisterFeature("install", func(s *Server) error {
		r := s.Router()
		r.HandleFunc("GET /install", s.serveInstallScript("install.sh", "text/x-shellscript; charset=utf-8"))
		r.HandleFunc("GET /install.sh", s.serveInstallScript("install.sh", "text/x-shellscript; charset=utf-8"))
		r.HandleFunc("GET /install.ps1", s.serveInstallScript("install.ps1", "text/plain; charset=utf-8"))
		r.HandleFunc("GET /downloads", s.serveDownloadList)
		r.HandleFunc("GET /downloads/{$}", s.serveDownloadList)
		r.HandleFunc("GET /downloads/{os}/{arch}/{name}", s.serveDownload)
		r.HandleFunc("GET /api/crowdvm/cli/download", s.serveCLIDownload)
		r.HandleFunc("GET /api/boat/cli/download", s.serveCLIDownload)
		return nil
	})
}

// DownloadsDir is where /downloads serves binaries from.
func (s *Server) DownloadsDir() string {
	if s.Cfg.DownloadsDir != "" {
		return s.Cfg.DownloadsDir
	}
	return filepath.Join(s.Cfg.DataDir, "downloads")
}

func (s *Server) serveInstallScript(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := installFiles.ReadFile("feat_install_files/" + name)
		if err != nil {
			http.Error(w, "installer missing", http.StatusInternalServerError)
			return
		}
		body := strings.ReplaceAll(string(b), "__CROWDVM_SERVER__", strings.TrimRight(s.Cfg.PublicURL, "/"))
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, body)
	}
}

// downloadPath validates os/arch/name and returns the file path.
func (s *Server) downloadPath(goos, arch, name string) (string, bool) {
	base := strings.TrimSuffix(name, ".exe")
	if !slices.Contains(installOS, goos) || !slices.Contains(installArch, arch) || !slices.Contains(installNames, base) {
		return "", false
	}
	// Windows binaries end in .exe (the guest is always a Linux binary).
	if strings.HasSuffix(name, ".exe") && (goos != "windows" || slices.Contains(linuxOnly, base)) {
		return "", false
	}
	return filepath.Join(s.DownloadsDir(), goos, arch, name), true
}

func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	goos, arch, name := r.PathValue("os"), r.PathValue("arch"), r.PathValue("name")
	p, ok := s.downloadPath(goos, arch, name)
	if !ok {
		http.Error(w, "unknown download "+goos+"/"+arch+"/"+name, http.StatusNotFound)
		return
	}
	s.sendBinary(w, r, p, name)
}

// serveDownloadList lists the binaries this server can serve.
func (s *Server) serveDownloadList(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(s.Cfg.PublicURL, "/")
	out := []map[string]any{}
	for _, goos := range installOS {
		for _, arch := range installArch {
			for _, n := range installNames {
				for _, name := range []string{n, n + ".exe"} {
					p, ok := s.downloadPath(goos, arch, name)
					if !ok {
						continue
					}
					if st, err := os.Stat(p); err == nil && !st.IsDir() {
						out = append(out, map[string]any{"os": goos, "arch": arch, "name": name, "size": st.Size(),
							"modifiedAt": ts(st.ModTime()), "url": base + "/downloads/" + goos + "/" + arch + "/" + name})
					}
				}
			}
		}
	}
	api.WriteOK(w, 200, "download.list", map[string]any{"downloads": out})
}

func (s *Server) sendBinary(w http.ResponseWriter, r *http.Request, p, name string) {
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, name+" is not available on this server. An admin can add binaries with: crowdvm-server downloads import <dist dir>", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// serveCLIDownload maps Boat's platform names (linux-x64, linux-arm64,
// windows-x64, darwin-arm64, ...) to the crowdvm CLI binary.
func (s *Server) serveCLIDownload(w http.ResponseWriter, r *http.Request) {
	plat := r.URL.Query().Get("platform")
	goos, arch, _ := strings.Cut(plat, "-")
	switch arch {
	case "x64", "x86_64", "amd64":
		arch = "amd64"
	case "arm64", "aarch64":
		arch = "arm64"
	}
	if goos == "macos" {
		goos = "darwin"
	}
	name := "crowdvm"
	if goos == "windows" {
		name += ".exe"
	}
	p, ok := s.downloadPath(goos, arch, name)
	if !ok {
		http.Error(w, "platform must be one of linux-x64, linux-arm64, windows-x64, windows-arm64, darwin-x64, darwin-arm64", http.StatusBadRequest)
		return
	}
	s.sendBinary(w, r, p, name)
}

var distDirRe = regexp.MustCompile(`_(linux|windows|darwin)_(amd64|arm64)$`)

// ImportDownloads copies binaries from a scripts/dist.sh output directory
// (dist/crowdvm_<version>_<os>_<arch>/...) or from an <os>/<arch>/ tree into
// dst. It returns the files written (relative to dst).
func ImportDownloads(distDir, dst string) ([]string, error) {
	ents, err := os.ReadDir(distDir)
	if err != nil {
		return nil, err
	}
	var done []string
	copyDir := func(src, goos, arch string) error {
		for _, n := range installNames {
			for _, name := range []string{n, n + ".exe"} {
				in := filepath.Join(src, name)
				if st, err := os.Stat(in); err != nil || st.IsDir() {
					continue
				}
				rel := filepath.Join(goos, arch, name)
				if err := copyFileAtomic(in, filepath.Join(dst, rel)); err != nil {
					return err
				}
				done = append(done, rel)
			}
		}
		return nil
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if m := distDirRe.FindStringSubmatch(e.Name()); m != nil {
			if err := copyDir(filepath.Join(distDir, e.Name()), m[1], m[2]); err != nil {
				return done, err
			}
			continue
		}
		if slices.Contains(installOS, e.Name()) {
			for _, a := range installArch {
				if err := copyDir(filepath.Join(distDir, e.Name(), a), e.Name(), a); err != nil {
					return done, err
				}
			}
		}
	}
	if len(done) == 0 {
		return nil, errors.New("no binaries found (expected crowdvm_<version>_<os>_<arch>/ directories from scripts/dist.sh)")
	}
	return done, nil
}

func copyFileAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
