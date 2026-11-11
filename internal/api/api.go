// Package api holds the public HTTP API (/api/v1) wire types shared by the
// server, the guest agent (which serves the sandbox-scoped endpoints), the Go
// client and the CLI. The API mirrors Boat's v1 API (reference: .ref/boat-v1.yaml)
// with CrowdVM naming (CROWDVM_API_KEY, keys prefixed "cvm_").
package api

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
)

// Error is the error envelope:
//
//	{"ok":false,"type":"sandbox.error","status":409,"code":"...","message":"...",
//	 "error":{"code":"...","message":"...","status":409,"details":{...}},"requestId":"req_..."}
type Error struct {
	OK        bool        `json:"ok"`
	Type      string      `json:"type"`
	Status    int         `json:"status"`
	Code      string      `json:"code"`
	Message   string      `json:"message"`
	Err       ErrorDetail `json:"error"`
	RequestID string      `json:"requestId,omitempty"`
}

type ErrorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Status  int            `json:"status"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, msg string, details map[string]any) *Error {
	return &Error{Type: "sandbox.error", Status: status, Code: code, Message: msg,
		Err: ErrorDetail{Code: code, Message: msg, Status: status, Details: details}}
}

// WriteError writes the error envelope.
func WriteError(w http.ResponseWriter, e *Error) {
	if e.RequestID == "" {
		e.RequestID = w.Header().Get("X-Request-Id")
	}
	WriteJSON(w, e.Status, e)
}

// WriteOK writes {"ok":true,"type":typ, ...fields}. fields may be a map or a
// struct (its JSON object fields are merged).
func WriteOK(w http.ResponseWriter, status int, typ string, fields any) {
	m := map[string]any{}
	if fields != nil {
		b, _ := json.Marshal(fields)
		_ = json.Unmarshal(b, &m)
	}
	m["ok"] = true
	m["type"] = typ
	WriteJSON(w, status, m)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// PageInfo is returned by every cursor-paginated list.
type PageInfo struct {
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
	Limit      int    `json:"limit"`
}

// SizeSpec is one entry of the machine size catalog.
type SizeSpec struct {
	Name       string  `json:"name"`
	VCPU       int     `json:"vcpu"`
	MemoryMiB  int     `json:"memoryMiB"`
	DiskGiB    int     `json:"diskGiB"`
	Multiplier float64 `json:"multiplier"` // credit rate relative to "default"
}

// Sizes is the size catalog, same shapes as Boat.
var Sizes = []SizeSpec{
	{"small", 2, 4096, 51, 0.5},
	{"default", 4, 8192, 51, 1},
	{"large", 8, 16384, 111, 2},
	{"xlarge", 16, 32768, 251, 50.0 / 9}, // Boat: 50/9 ≈ 5.556
}

func SizeByName(name string) (SizeSpec, bool) {
	if name == "" || name == "medium" {
		name = "default"
	}
	for _, s := range Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return SizeSpec{}, false
}

// Sandbox API states (the CLI shows "stopped" for "archived").
const (
	StateProvisioning = "provisioning"
	StateProvisioned  = "provisioned"
	StateCloning      = "cloning"
	StateReady        = "ready"
	StateIdle         = "idle"
	StateRunning      = "running"
	StateArchiving    = "archiving"
	StateArchived     = "archived"
	StateError        = "error"
	// StateCancelled: a create or fork aborted before its first boot (no
	// node had room in time); there is no disk. Resume retries the create.
	StateCancelled = "cancelled"
)

// ID prefixes.
const (
	PrefixSandbox  = "bx_"
	PrefixSnapshot = "snap_"
	PrefixAPIKey   = "cvm_"
	PrefixNode     = "node_"
	PrefixShare    = "bsh_" // Boat's share-code prefix
	// PrefixShareLegacy: codes minted before the switch to bsh_; still accepted.
	PrefixShareLegacy = "csh_"
	PrefixRequest     = "req_"
)

// idAlphabet is Boat's id alphabet (boat-v1.yaml: ^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$).
const idAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"

// NewID returns prefix + n random characters from idAlphabet (unbiased).
func NewID(prefix string, n int) string {
	var sb strings.Builder
	sb.WriteString(prefix)
	limit := byte(256 / len(idAlphabet) * len(idAlphabet))
	var b [64]byte
	for sb.Len() < len(prefix)+n {
		_, _ = rand.Read(b[:])
		for _, c := range b {
			if c < limit && sb.Len() < len(prefix)+n {
				sb.WriteByte(idAlphabet[int(c)%len(idAlphabet)])
			}
		}
	}
	return sb.String()
}
