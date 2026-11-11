package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Secrets of environments and credential profiles are sealed at rest with
// AES-256-GCM. The key lives next to the database in env-secrets.key (0600),
// so a copy of crowdvm.db alone reveals nothing.

var envAEADs sync.Map // data dir -> cipher.AEAD

func (s *Server) envAEAD() (cipher.AEAD, error) {
	if v, ok := envAEADs.Load(s.Cfg.DataDir); ok {
		return v.(cipher.AEAD), nil
	}
	p := filepath.Join(s.Cfg.DataDir, "env-secrets.key")
	key, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		f, ferr := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if ferr == nil {
			_, ferr = f.Write(key)
			if cerr := f.Close(); ferr == nil {
				ferr = cerr
			}
		}
		if ferr != nil { // lost a race or cannot write: use what is there
			if key, err = os.ReadFile(p); err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s: want a 32-byte key", p)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v, _ := envAEADs.LoadOrStore(s.Cfg.DataDir, a)
	return v.(cipher.AEAD), nil
}

// envSeal encrypts plain into "v1:<base64(nonce|ciphertext)>".
func (s *Server) envSeal(plain []byte) (string, error) {
	a, err := s.envAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	_, _ = rand.Read(nonce)
	return "v1:" + base64.StdEncoding.EncodeToString(a.Seal(nonce, nonce, plain, nil)), nil
}

// envOpen reverses envSeal.
func (s *Server) envOpen(sealed string) ([]byte, error) {
	if len(sealed) < 3 || sealed[:3] != "v1:" {
		return nil, errors.New("unsealed value")
	}
	a, err := s.envAEAD()
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(sealed[3:])
	if err != nil || len(b) < a.NonceSize() {
		return nil, errors.New("corrupt sealed value")
	}
	return a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], nil)
}

// envUUID returns a random RFC 4122 v4 UUID (Boat's environment/profile ids).
func envUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
