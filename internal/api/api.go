// Package api holds the public HTTP API (/api/v1) wire types shared by the
// server, the guest agent (which serves the sandbox-scoped endpoints), the Go
// client and the CLI. The API mirrors Boat's v1 API (reference: .ref/boat-v1.yaml)
// with CrowdVM naming (CROWDVM_API_KEY, keys prefixed "cvm_").
package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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

// SizeSpec is one machine shape: a catalog entry or a custom shape.
type SizeSpec struct {
	Name       string  `json:"name"`
	VCPU       int     `json:"vcpu"`
	MemoryMiB  int     `json:"memoryMiB"`
	DiskGiB    int     `json:"diskGiB"`
	Multiplier float64 `json:"multiplier"` // credit rate relative to "default"
}

// Credits per hour for each vCPU and GiB of RAM. Every size, named or
// custom, costs vcpu*PerVCPU + GiB*PerGiB, so default (4 vCPU, 8 GiB) is 1.
const (
	PerVCPU = 0.125
	PerGiB  = 0.0625
)

// Custom shape bounds ("<vcpu>c<GiB>g", e.g. 8c10g).
const (
	MinVCPU, MaxVCPU     = 1, 64
	MinMemGiB, MaxMemGiB = 2, 256
)

// Sizes is the named catalog, same shapes as Boat.
var Sizes = []SizeSpec{
	shape("small", 2, 4, 51),
	shape("default", 4, 8, 51),
	shape("large", 8, 16, 111),
	shape("xlarge", 16, 32, 251),
}

// InvalidTypeMsg is the error for a type SizeByName rejects.
var InvalidTypeMsg = fmt.Sprintf("type must be small, default, large, xlarge or a custom shape like 8c10g (%d-%d vCPU, %d-%d GiB).", MinVCPU, MaxVCPU, MinMemGiB, MaxMemGiB)

func shape(name string, vcpu, gib, disk int) SizeSpec {
	return SizeSpec{name, vcpu, gib * 1024, disk, float64(vcpu)*PerVCPU + float64(gib)*PerGiB}
}

var customRe = regexp.MustCompile(`^([0-9]{1,3})c([0-9]{1,4})g$`)

// SizeByName resolves a catalog name or a custom "<vcpu>c<GiB>g" shape.
// A custom shape equal to a catalog entry resolves to that entry, and a
// custom one gets the disk of the smallest named size that covers it.
func SizeByName(name string) (SizeSpec, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "medium" {
		name = "default"
	}
	for _, s := range Sizes {
		if s.Name == name {
			return s, true
		}
	}
	m := customRe.FindStringSubmatch(name)
	if m == nil {
		return SizeSpec{}, false
	}
	vcpu, _ := strconv.Atoi(m[1])
	gib, _ := strconv.Atoi(m[2])
	if vcpu < MinVCPU || vcpu > MaxVCPU || gib < MinMemGiB || gib > MaxMemGiB {
		return SizeSpec{}, false
	}
	disk := Sizes[len(Sizes)-1].DiskGiB
	for _, s := range Sizes {
		if s.VCPU == vcpu && s.MemoryMiB == gib*1024 {
			return s, true
		}
		if s.VCPU >= vcpu && s.MemoryMiB >= gib*1024 {
			disk = min(disk, s.DiskGiB)
		}
	}
	return shape(fmt.Sprintf("%dc%dg", vcpu, gib), vcpu, gib, disk), true
}

// Within reports whether s needs no more vCPU and RAM than limit.
func (s SizeSpec) Within(limit SizeSpec) bool {
	return s.VCPU <= limit.VCPU && s.MemoryMiB <= limit.MemoryMiB
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
