package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/bench"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Version of the node app (set with -ldflags "-X github.com/crowdvm/crowdvm/internal/node.Version=...").
var Version = "dev"

// Config is the persisted node configuration (<data dir>/config.json).
type Config struct {
	Server    string `json:"server,omitempty"` // e.g. http://192.168.1.10:8420
	NodeID    string `json:"nodeId,omitempty"`
	NodeToken string `json:"nodeToken,omitempty"`
	Name      string `json:"name,omitempty"` // default: hostname
	Driver    string `json:"driver,omitempty"`

	// Shared capacity. Zero = default: half the logical CPUs, half the RAM,
	// 90% of the disk space the node can use in the data dir (free + what
	// it already stores), max sandboxes = max(1, cpus/2).
	CPUs         int `json:"cpus,omitempty"`
	MemoryMiB    int `json:"memoryMiB,omitempty"`
	DiskGiB      int `json:"diskGiB,omitempty"` // real disk shared (see disk.go)
	MaxSandboxes int `json:"maxSandboxes,omitempty"`
	// DiskOvercommit: sandbox disk sizes may add up to DiskGiB × this
	// (overlays are thin; default 4). DiskReserveGiB: free space always
	// kept on the host filesystem (default 10).
	DiskOvercommit float64 `json:"diskOvercommit,omitempty"`
	DiskReserveGiB *int    `json:"diskReserveGiB,omitempty"`

	DirectHost  string `json:"directHost,omitempty"`  // LAN/Tailscale IP for direct ports
	GuestBinary string `json:"guestBinary,omitempty"` // default: crowdvm-guest next to the executable
	// CLIBinary: linux/amd64 `crowdvm` CLI put on the config drive for the
	// in-sandbox CLI (default: crowdvm-linux-amd64, else crowdvm on
	// linux/amd64 hosts, next to the executable; "off" disables).
	CLIBinary string `json:"cliBinary,omitempty"`
	// NoNested hides virtualization extensions from sandboxes that did not
	// ask for nested virtualization (default: exposed when the host can).
	NoNested bool `json:"noNested,omitempty"`
	// NoLazyChain: pull a sandbox's whole snapshot chain before it boots
	// (default: boot at once and read missing layers from the server on
	// demand while they download, when the driver can; LIFE-013).
	NoLazyChain bool `json:"noLazyChain,omitempty"`
	// WarmPool: VMs kept booted from the default image and size (the last
	// plain create's) so creates land on a running machine (SIZE-012).
	// They count against the shared capacity; default 0 (off).
	WarmPool int               `json:"warmPool,omitempty"`
	UIAddr   string            `json:"uiAddr,omitempty"` // default 127.0.0.1:8421; "off" disables
	Labels   map[string]string `json:"labels,omitempty"`
	Paused   bool              `json:"paused,omitempty"` // do not accept new sandboxes
	// SnapshotInterval overrides the server-provided periodic live snapshot
	// interval (Go duration, "0" = use server/default 60s, "off" = disabled).
	SnapshotInterval string `json:"snapshotInterval,omitempty"`
	// Net: guest networking and egress policy (qemu driver).
	Net NetConfig `json:"net,omitzero"`
	// GPU: GPUs lent to sandboxes (gpu.go).
	GPU GPUConfig `json:"gpu,omitzero"`

	DataDir string `json:"-"`
}

// NetConfig is the owner's guest network setting. By default guests reach
// the internet and the CrowdVM server, but not this machine, its LANs or
// its tailnets.
type NetConfig struct {
	Mode     string   `json:"mode,omitempty"`     // "netstack" (default) | "slirp" (insecure fallback)
	AllowLAN bool     `json:"allowLan,omitempty"` // open RFC 1918, CGNAT/Tailscale and IPv6 ULA ranges
	Allow    []string `json:"allow,omitempty"`    // extra reachable CIDRs (owner override)
	Deny     []string `json:"deny,omitempty"`     // extra blocked CIDRs (owner override)
}

// GPUConfig: Passthrough lists the PCI addresses of GPUs given to one VM at
// a time through VFIO (checked when the node starts). VirGL ("" off, "on",
// or a /dev/dri/renderD* path) gives every VM an OpenGL virtio-gpu rendered
// on this machine's GPU.
type GPUConfig struct {
	Passthrough []string `json:"passthrough,omitempty"`
	VirGL       string   `json:"virgl,omitempty"`
}

// ServerAddr returns the server's host:port (port defaulted from the scheme).
func (c Config) ServerAddr() string {
	u, err := url.Parse(c.Server)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func parseCIDRList(val string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ' ' }) {
		if _, err := netip.ParsePrefix(f); err != nil {
			if _, err := netip.ParseAddr(f); err != nil {
				return nil, fmt.Errorf("invalid CIDR or address %q", f)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// DefaultDataDir: $CROWDVM_NODE_DATA_DIR, else ~/.local/share/crowdvm/node
// ($XDG_DATA_HOME honored) on Unix and %LOCALAPPDATA%\CrowdVM\node on Windows.
func DefaultDataDir() string {
	if d := os.Getenv("CROWDVM_NODE_DATA_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "CrowdVM", "node")
		}
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "crowdvm", "node")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "crowdvm", "node")
}

func configPath(dataDir string) string { return filepath.Join(dataDir, "config.json") }

// LoadConfig reads <dataDir>/config.json (a missing file yields defaults).
func LoadConfig(dataDir string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(configPath(dataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", configPath(dataDir), err)
		}
	}
	c.DataDir = dataDir
	return c, nil
}

// Save writes the config atomically with mode 0600 (it holds the node token).
func (c *Config) Save() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := configPath(c.DataDir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(c.DataDir))
}

func (c Config) NodeName() string {
	if c.Name != "" {
		return c.Name
	}
	h, _ := os.Hostname()
	return h
}

func (c Config) UIListen() string {
	if c.UIAddr == "" {
		return fmt.Sprintf("127.0.0.1:%d", proto.DefaultNodeUIPort)
	}
	return c.UIAddr
}

// GuestBinaryPath resolves the crowdvm-guest binary shipped on the config drive.
func (c Config) GuestBinaryPath() string {
	if c.GuestBinary != "" {
		return c.GuestBinary
	}
	exe, err := os.Executable()
	if err != nil {
		return "crowdvm-guest"
	}
	return filepath.Join(filepath.Dir(exe), "crowdvm-guest")
}

// CLIBinaryPath resolves the in-sandbox CLI binary ("" = none available).
func (c Config) CLIBinaryPath() string {
	switch c.CLIBinary {
	case "off", "none":
		return ""
	case "":
	default:
		return c.CLIBinary
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	names := []string{"crowdvm-linux-amd64"}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		names = append(names, "crowdvm")
	}
	for _, n := range names {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && st.Mode().IsRegular() {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

// Overcommit is the disk overcommit factor (default 4).
func (c Config) Overcommit() float64 {
	if c.DiskOvercommit > 0 {
		return c.DiskOvercommit
	}
	return defaultDiskOvercommit
}

// DiskReserve is the free space (GiB) always kept on the host (default 10).
func (c Config) DiskReserve() int {
	if c.DiskReserveGiB != nil {
		return *c.DiskReserveGiB
	}
	return defaultDiskReserveGiB
}

// Capacity returns the effective shared capacity (defaults filled in).
// DiskGiB is the real disk shared; the node advertises more (overcommit,
// see Node.capacity).
func (c Config) Capacity() proto.Capacity {
	cp := proto.Capacity{VCPU: c.CPUs, MemoryMiB: c.MemoryMiB, DiskGiB: c.DiskGiB, MaxSandboxes: c.MaxSandboxes}
	if cp.VCPU <= 0 {
		cp.VCPU = max(1, runtime.NumCPU()/2)
	}
	if cp.MemoryMiB <= 0 {
		total, _ := bench.MemInfo()
		cp.MemoryMiB = max(1024, int(total>>20)/2)
	}
	if cp.DiskGiB <= 0 {
		free, _ := bench.DataDirFree(c.DataDir)
		cp.DiskGiB = max(1, int(free*9/10>>30))
	}
	if cp.MaxSandboxes <= 0 {
		cp.MaxSandboxes = max(1, cp.VCPU/2)
	}
	return cp
}

// SnapshotEvery returns the local override (ok=false: use the server value).
func (c Config) SnapshotEvery() (d time.Duration, ok bool) {
	switch strings.TrimSpace(c.SnapshotInterval) {
	case "", "0":
		return 0, false
	case "off", "disabled", "-1":
		return 0, true
	}
	d, err := time.ParseDuration(c.SnapshotInterval)
	return d, err == nil
}

// ConfigKeys lists the keys accepted by Get/Set (CLI `config get/set`).
var ConfigKeys = []string{"server", "name", "driver", "cpus", "memory", "disk", "disk-overcommit", "disk-reserve",
	"max-sandboxes", "direct-host", "guest-binary", "cli-binary", "nested", "lazy-chain", "warm-pool", "ui-addr", "paused", "snapshot-interval", "node-id",
	"net", "net.allow-lan", "net.allow", "net.deny", "gpu.passthrough", "gpu.virgl"}

// Get returns a config value by CLI key.
func (c Config) Get(key string) (string, error) {
	switch key {
	case "server":
		return c.Server, nil
	case "name":
		return c.NodeName(), nil
	case "driver":
		return c.Driver, nil
	case "cpus":
		return strconv.Itoa(c.Capacity().VCPU), nil
	case "memory":
		return fmt.Sprintf("%dMiB", c.Capacity().MemoryMiB), nil
	case "disk":
		return fmt.Sprintf("%dGiB", c.Capacity().DiskGiB), nil
	case "disk-overcommit":
		return strconv.FormatFloat(c.Overcommit(), 'g', -1, 64), nil
	case "disk-reserve":
		return fmt.Sprintf("%dGiB", c.DiskReserve()), nil
	case "max-sandboxes":
		return strconv.Itoa(c.Capacity().MaxSandboxes), nil
	case "direct-host":
		return c.DirectHost, nil
	case "guest-binary":
		return c.GuestBinaryPath(), nil
	case "cli-binary":
		if p := c.CLIBinaryPath(); p != "" {
			return p, nil
		}
		return "off", nil
	case "nested":
		return strconv.FormatBool(!c.NoNested), nil
	case "lazy-chain":
		return strconv.FormatBool(!c.NoLazyChain), nil
	case "warm-pool":
		return strconv.Itoa(c.WarmPool), nil
	case "ui-addr":
		return c.UIListen(), nil
	case "paused":
		return strconv.FormatBool(c.Paused), nil
	case "snapshot-interval":
		return c.SnapshotInterval, nil
	case "node-id":
		return c.NodeID, nil
	case "net":
		if c.Net.Mode == "" {
			return "netstack", nil
		}
		return c.Net.Mode, nil
	case "net.allow-lan":
		return strconv.FormatBool(c.Net.AllowLAN), nil
	case "net.allow":
		return strings.Join(c.Net.Allow, ","), nil
	case "net.deny":
		return strings.Join(c.Net.Deny, ","), nil
	case "gpu.passthrough":
		return strings.Join(c.GPU.Passthrough, ","), nil
	case "gpu.virgl":
		if c.GPU.VirGL == "" {
			return "off", nil
		}
		return c.GPU.VirGL, nil
	}
	return "", fmt.Errorf("unknown key %q (keys: %s)", key, strings.Join(ConfigKeys, ", "))
}

// Set updates a config value by CLI key ("" or "0" resets capacity keys to the default).
func (c *Config) Set(key, val string) error {
	var err error
	switch key {
	case "server":
		u, e := url.Parse(val)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("server must be an http or https URL")
		}
	case "cpus":
		n, e := atoiOrZero(val)
		if e != nil || n < 0 || n > runtime.NumCPU() {
			return fmt.Errorf("cpus must be 0..%d", runtime.NumCPU())
		}
	case "memory", "disk", "disk-reserve":
		unit := int64(1 << 30)
		if key == "memory" {
			unit = 1 << 20
		}
		n, e := ParseSize(val, unit)
		if e != nil {
			return e
		}
		if n == 0 && val != "" && val != "0" {
			return fmt.Errorf("%s must be at least one allocation unit", key)
		}
		if key == "memory" {
			total, _ := bench.MemInfo()
			if total > 0 && int64(n) > int64(total>>20) {
				return fmt.Errorf("memory exceeds host RAM")
			}
		}
	case "max-sandboxes":
		n, e := atoiOrZero(val)
		if e != nil || n < 0 {
			return fmt.Errorf("max-sandboxes must be non-negative")
		}
	case "direct-host":
		if val != "" {
			if _, e := netip.ParseAddr(val); e != nil {
				return fmt.Errorf("direct-host must be an IP address")
			}
		}
	case "ui-addr":
		if val != "" && val != "off" {
			_, port, e := net.SplitHostPort(val)
			n, ne := strconv.Atoi(port)
			if e != nil || ne != nil || n < 1 || n > 65535 {
				return fmt.Errorf("ui-addr must be host:port with port 1..65535")
			}
		}
	case "snapshot-interval":
		if val != "" && val != "0" && val != "off" && val != "disabled" && val != "-1" {
			d, e := time.ParseDuration(val)
			if e != nil || d < 10*time.Second {
				return fmt.Errorf("snapshot-interval must be at least 10s, 0 or off")
			}
		}
	}
	switch key {
	case "server":
		c.Server = strings.TrimRight(val, "/")
	case "name":
		c.Name = val
	case "driver":
		c.Driver = val
	case "cpus":
		c.CPUs, err = atoiOrZero(val)
	case "memory":
		c.MemoryMiB, err = ParseSize(val, 1<<20)
	case "disk":
		c.DiskGiB, err = ParseSize(val, 1<<30)
	case "disk-overcommit":
		c.DiskOvercommit = 0
		if val != "" {
			if c.DiskOvercommit, err = strconv.ParseFloat(val, 64); err == nil && (c.DiskOvercommit < 1 || math.IsNaN(c.DiskOvercommit) || math.IsInf(c.DiskOvercommit, 0)) {
				err = fmt.Errorf("disk-overcommit must be >= 1")
			}
		}
	case "disk-reserve":
		c.DiskReserveGiB = nil
		if val != "" {
			var g int
			if g, err = ParseSize(val, 1<<30); err == nil {
				c.DiskReserveGiB = &g
			}
		}
	case "max-sandboxes":
		c.MaxSandboxes, err = atoiOrZero(val)
	case "direct-host":
		c.DirectHost = val
	case "guest-binary":
		c.GuestBinary = val
	case "cli-binary":
		c.CLIBinary = val
	case "nested":
		var on bool
		on, err = strconv.ParseBool(val)
		c.NoNested = !on
	case "lazy-chain":
		var on bool
		on, err = strconv.ParseBool(val)
		c.NoLazyChain = !on
	case "warm-pool":
		c.WarmPool, err = strconv.Atoi(val)
		if err == nil && (c.WarmPool < 0 || c.WarmPool > 16) {
			err = fmt.Errorf("warm-pool must be 0..16")
		}
	case "ui-addr":
		c.UIAddr = val
	case "paused":
		c.Paused, err = strconv.ParseBool(val)
	case "snapshot-interval":
		c.SnapshotInterval = val
		if _, ok := c.SnapshotEvery(); !ok && val != "" && val != "0" {
			err = fmt.Errorf("invalid duration %q", val)
		}
	case "net":
		switch val {
		case "", "netstack", "slirp":
			c.Net.Mode = val
		default:
			err = fmt.Errorf("net: want netstack or slirp, got %q", val)
		}
	case "net.allow-lan":
		c.Net.AllowLAN, err = strconv.ParseBool(val)
	case "net.allow":
		c.Net.Allow, err = parseCIDRList(val)
	case "net.deny":
		c.Net.Deny, err = parseCIDRList(val)
	case "gpu.passthrough":
		c.GPU.Passthrough, err = parsePCIList(val)
	case "gpu.virgl":
		switch v := strings.TrimSpace(val); {
		case v == "" || v == "off" || v == "false":
			c.GPU.VirGL = ""
		case v == "on" || v == "true" || strings.HasPrefix(v, "/dev/dri/"):
			c.GPU.VirGL = v
		default:
			err = fmt.Errorf("gpu.virgl: want on, off or a /dev/dri/renderD* path, got %q", val)
		}
	default:
		_, err = c.Get(key)
		if err == nil {
			err = fmt.Errorf("%s is read-only", key)
		}
	}
	return err
}

func atoiOrZero(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// ParseSize parses "16G", "16GiB", "8192M", "512MiB", "1T" or a bare number
// (already in unit) and returns it in multiples of unit (1<<20 or 1<<30).
func ParseSize(s string, unit int64) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := unit
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = 1<<10, s[:n-1]
		case 'M':
			mult, s = 1<<20, s[:n-1]
		case 'G':
			mult, s = 1<<30, s[:n-1]
		case 'T':
			mult, s = 1<<40, s[:n-1]
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) || f*float64(mult)/float64(unit) >= float64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int(f * float64(mult) / float64(unit)), nil
}
