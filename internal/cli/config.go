package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"time"
)

// Config is ~/.config/crowdvm/config.json (%APPDATA%\crowdvm on Windows).
type Config struct {
	// Profile is the profile used when neither --profile nor CROWDVM_PROFILE is set.
	Profile  string              `json:"profile,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
}

// Profile is one server + credentials, plus small CLI state.
type Profile struct {
	Server string `json:"server,omitempty"`
	APIKey string `json:"apiKey,omitempty"`
	User   string `json:"user,omitempty"`
	// Current is the last sandbox created or forked (the "current" alias).
	Current string `json:"current,omitempty"`
	// Conversations maps sandbox id → current conversation id.
	Conversations map[string]string `json:"conversations,omitempty"`
	// PendingLogin is an unfinished device login, reused by `login --json`.
	PendingLogin *PendingLogin `json:"pendingLogin,omitempty"`
	// Purpose is the onboarding answer: "own" or "others" (third parties).
	Purpose string `json:"purpose,omitempty"`
	// Shells holds per-shell state (current sandbox, conversations), keyed by
	// CROWDVM_SHELL_ID, which the shell function from `crowdvm shell-init` sets.
	Shells map[string]*ShellState `json:"shells,omitempty"`
}

// ShellState is the "current" sandbox and conversations of one shell.
type ShellState struct {
	Current       string            `json:"current,omitempty"`
	Conversations map[string]string `json:"conversations,omitempty"`
	Seen          int64             `json:"seen"` // Unix seconds of the last write
}

type PendingLogin struct {
	Server          string `json:"server"`
	DeviceCode      string `json:"deviceCode"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	Interval        int    `json:"interval,omitempty"`
}

// DefaultConfigDir returns the config directory.
func DefaultConfigDir(getenv func(string) string) string {
	if d := getenv("CROWDVM_CONFIG_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if d, err := os.UserConfigDir(); err == nil {
			return filepath.Join(d, "crowdvm")
		}
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "crowdvm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "crowdvm")
}

func loadConfig(dir string) (*Config, error) {
	cfg := &Config{Profiles: map[string]*Profile{}}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	return cfg, nil
}

// saveConfig replaces config.json with cfg (under the config lock).
func saveConfig(dir string, cfg *Config) error {
	return updateConfig(dir, func(cur *Config) error { *cur = *cfg; return nil })
}

// updateConfig re-reads config.json under a file lock, lets fn change it and
// writes it back atomically. An unreadable config is an error, never replaced.
func updateConfig(dir string, fn func(cur *Config) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockConfig(dir)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := loadConfig(dir)
	if err != nil {
		return fmt.Errorf("config.json unreadable, not overwriting it: %w", err)
	}
	// Apply fn's changes onto the file's own JSON, so fields this version does
	// not know (written by a newer crowdvm) survive the save.
	raw := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &raw)
		if raw == nil {
			raw = map[string]any{}
		}
	}
	base := toMap(cur)
	if err := fn(cur); err != nil {
		return err
	}
	merge3(raw, base, toMap(cur))
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "config.json.*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, "config.json"))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// lockConfig takes config.json's lock file (created exclusively, so it works
// on every OS) and returns the unlock func. A lock left by a killed process
// is taken over after 30 seconds. The lock file holds a token; unlock removes
// the file only while it still holds this process's token (not a new owner's).
func lockConfig(dir string) (func(), error) { return lockFile(filepath.Join(dir, "config.lock")) }

// lockFile takes the lock file path the way lockConfig does.
func lockFile(path string) (func(), error) {
	tok := make([]byte, 8)
	_, _ = rand.Read(tok)
	token := fmt.Sprintf("%d-%x", os.Getpid(), tok)
	owned := func(want string) bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) == want
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = f.WriteString(token)
			f.Close()
			if err != nil {
				os.Remove(path)
				return nil, err
			}
			return func() {
				if owned(token) {
					os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 30*time.Second {
			// Remove the stale lock only if it is still the one we saw.
			if b, rerr := os.ReadFile(path); rerr == nil && owned(string(b)) {
				os.Remove(path)
			}
		} else if time.Now().After(deadline) {
			return nil, fmt.Errorf("locked by another crowdvm (%s)", path)
		}
	}
}

// toMap and fromMap convert a Config to and from its generic JSON form.
func toMap(cfg *Config) map[string]any {
	var m map[string]any
	b, _ := json.Marshal(cfg)
	_ = json.Unmarshal(b, &m)
	return m
}

func fromMap(m map[string]any, cfg *Config) {
	*cfg = Config{}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, cfg)
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
}

// merge3 applies what mine changed since base onto cur: fields mine did not
// touch keep the value cur has (e.g. a logout done meanwhile by another process).
func merge3(cur, base, mine map[string]any) {
	for k, mv := range mine {
		bv, inBase := base[k]
		if inBase && reflect.DeepEqual(bv, mv) {
			continue
		}
		cm, curIsMap := cur[k].(map[string]any)
		mm, mineIsMap := mv.(map[string]any)
		if curIsMap && mineIsMap {
			bm, _ := bv.(map[string]any)
			merge3(cm, bm, mm)
			continue
		}
		cur[k] = mv
	}
	for k := range base {
		if _, ok := mine[k]; !ok {
			delete(cur, k)
		}
	}
}
