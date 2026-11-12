package client

import "context"

// DownloadFile is one binary of GET /downloads.
type DownloadFile struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"` // "" from servers before checksums
	URL    string `json:"url"`
}

// DownloadList is GET /downloads: the binaries the server offers and the
// release they come from (Version "" when the server has no manifest).
type DownloadList struct {
	Version   string         `json:"version"`
	Downloads []DownloadFile `json:"downloads"`
}

// Downloads lists the binaries the server offers (no sign-in needed).
func (c *Client) Downloads(ctx context.Context) (*DownloadList, error) {
	return call[DownloadList](ctx, c, &Request{Method: "GET", Path: "/downloads", RootPath: true})
}

// Find returns the entry for goos/arch/name.
func (l *DownloadList) Find(goos, arch, name string) (DownloadFile, bool) {
	for _, f := range l.Downloads {
		if f.OS == goos && f.Arch == arch && f.Name == name {
			return f, true
		}
	}
	return DownloadFile{}, false
}
