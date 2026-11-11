package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

var b64 = base64.URLEncoding

// randToken returns n random base32 (Crockford-ish, lowercase) characters.
func randToken(n int) string {
	const alpha = "0123456789abcdefghjkmnpqrstvwxyz"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return string(b)
}

// randDigits returns n random decimal digits.
func randDigits(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		sb.WriteByte(byte('0' + d.Int64()))
	}
	return sb.String()
}

// randHex returns n random bytes as hex.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hashToken is the at-rest form of secrets (keys, sessions, codes).
func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// setting returns a persistent server setting, creating it with gen() once.
func (s *Server) setting(key string, gen func() string) string {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == nil {
		return v
	}
	v = gen()
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES(?,?)`, key, v)
	_ = s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

// sign returns an HMAC-SHA256 (hex) of msg with the server secret.
func (s *Server) sign(msg string) string {
	key := s.setting("signing_key", func() string { return randHex(32) })
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxy {
		// The trusted proxy appends the address it saw: the rightmost entry.
		// Anything left of it came from the client and can be forged
		// (bugs-r7-sonnet 1, 2).
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			parts := strings.Split(vals[len(vals)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TimeFormat is the API timestamp format: UTC, fixed millisecond precision
// (sortable as strings), e.g. 2026-09-01T00:00:00.000Z.
const TimeFormat = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(TimeFormat)
}

func tsMs(ms int64) any {
	if ms == 0 {
		return nil
	}
	return store.Time(ms).Format(TimeFormat)
}

func tsNull(v sql.NullInt64) any {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	return tsMs(v.Int64)
}

func nullStr(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// parseTime accepts RFC 3339 or unix epoch seconds.
func parseTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, true
	}
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return time.Time{}, false
		}
		n = n*10 + int64(c-'0')
	}
	return time.Unix(n, 0).UTC(), true
}

// OptInt is a JSON field that distinguishes absent, null and a number.
// The legacy string "infinite" is read as null.
type OptInt struct {
	Set   bool // present in the JSON
	Null  bool // explicit null
	Value int
}

func (o *OptInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `"infinite"` {
		o.Null = true
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	o.Value = int(f)
	if float64(o.Value) != f {
		return &json.UnmarshalTypeError{Value: s, Type: nil}
	}
	return nil
}

// OptBool distinguishes absent from false.
type OptBool struct {
	Set   bool
	Value bool
}

func (o *OptBool) UnmarshalJSON(b []byte) error {
	o.Set = true
	if strings.TrimSpace(string(b)) == "null" {
		o.Set = false
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// isSafe reports whether the method does not change state.
func isSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
