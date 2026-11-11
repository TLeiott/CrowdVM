package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Blob kinds.
const (
	BlobImage    = "image"
	BlobSnapshot = "snapshot"
)

// ErrBlobNotFound is returned for missing blobs.
var ErrBlobNotFound = errors.New("blob not found")

// ErrChecksum is returned when an upload does not match its declared sha256.
var ErrChecksum = errors.New("sha256 mismatch")

var blobIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Blobs stores immutable files under Dir/<kind>/<id> with a sidecar
// <id>.sha256 holding the hex digest.
type Blobs struct{ Dir string }

// BlobInfo describes a stored blob.
type BlobInfo struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Path   string `json:"-"`
}

// ValidKind reports whether kind is a blob kind.
func ValidKind(kind string) bool { return kind == BlobImage || kind == BlobSnapshot }

func (b *Blobs) path(kind, id string) (string, error) {
	if !ValidKind(kind) || !blobIDRe.MatchString(id) || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("invalid blob %s/%s", kind, id)
	}
	return filepath.Join(b.Dir, kind, id), nil
}

// Put streams r into the store. When wantSHA is non-empty the upload is
// rejected (ErrChecksum) unless it matches. Existing blobs are replaced.
func (b *Blobs) Put(kind, id string, r io.Reader, wantSHA string) (BlobInfo, error) {
	p, err := b.path(kind, id)
	if err != nil {
		return BlobInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return BlobInfo{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return BlobInfo{}, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return BlobInfo{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(wantSHA, sum) {
		return BlobInfo{}, fmt.Errorf("%w: got %s want %s", ErrChecksum, sum, wantSHA)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return BlobInfo{}, err
	}
	if err := os.WriteFile(p+".sha256", []byte(sum), 0o600); err != nil {
		return BlobInfo{}, err
	}
	return BlobInfo{Kind: kind, ID: id, Size: n, SHA256: sum, Path: p}, nil
}

// Stat returns the blob's info or ErrBlobNotFound.
func (b *Blobs) Stat(kind, id string) (BlobInfo, error) {
	p, err := b.path(kind, id)
	if err != nil {
		return BlobInfo{}, ErrBlobNotFound
	}
	fi, err := os.Stat(p)
	if err != nil {
		return BlobInfo{}, ErrBlobNotFound
	}
	sum, _ := os.ReadFile(p + ".sha256")
	return BlobInfo{Kind: kind, ID: id, Size: fi.Size(), SHA256: strings.TrimSpace(string(sum)), Path: p}, nil
}

// Delete removes a blob (no error when missing).
func (b *Blobs) Delete(kind, id string) error {
	p, err := b.path(kind, id)
	if err != nil {
		return err
	}
	_ = os.Remove(p + ".sha256")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List returns the blobs of a kind.
func (b *Blobs) List(kind string) ([]BlobInfo, error) {
	ents, err := os.ReadDir(filepath.Join(b.Dir, kind))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BlobInfo
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".sha256") {
			continue
		}
		if bi, err := b.Stat(kind, n); err == nil {
			out = append(out, bi)
		}
	}
	return out, nil
}

// CleanTemp removes partial uploads (".upload-*") older than maxAge: files a
// server crash left behind mid-upload. 0 removes all (at start no upload runs).
func (b *Blobs) CleanTemp(maxAge time.Duration) int {
	n := 0
	for _, kind := range []string{BlobImage, BlobSnapshot} {
		p, err := b.path(kind, "x")
		if err != nil {
			continue
		}
		ents, _ := os.ReadDir(filepath.Dir(p))
		for _, e := range ents {
			if !strings.HasPrefix(e.Name(), ".upload-") {
				continue
			}
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) >= maxAge {
				if os.Remove(filepath.Join(filepath.Dir(p), e.Name())) == nil {
					n++
				}
			}
		}
	}
	return n
}
