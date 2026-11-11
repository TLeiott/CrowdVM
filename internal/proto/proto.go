// Package proto defines the internal wire contracts between CrowdVM components:
//
//	server  <-- yamux over websocket -->  node  <-- HTTP (loopback forward) -->  guest agent (inside the VM)
//
// The public HTTP API (/api/v1) types live in internal/api. This package is the
// contract every component team codes against; change it only together with all
// users (server, node, guest).
package proto

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// GuestPort is the TCP port the guest agent listens on inside the VM.
	GuestPort = 7070
	// GuestUser / GuestHome: the unprivileged sandbox user (passwordless sudo).
	GuestUser = "user"
	GuestHome = "/home/user"
	// TunnelUpgrade is the HTTP Upgrade protocol token for raw TCP tunnels
	// (guest /v1/tunnel, node /v1/vms/{id}/tunnel).
	TunnelUpgrade = "crowdvm-tunnel"
	// ConfigDriveLabel is the volume label of the per-boot config drive that holds
	// config.json and the crowdvm-guest binary.
	ConfigDriveLabel = "CROWDVM"
	// DefaultServerPort is the default listen port of crowdvm-server.
	DefaultServerPort = 8420
	// DefaultNodeUIPort is the default (localhost) port of the node status UI.
	DefaultNodeUIPort = 8421
)

// ---------------------------------------------------------------------------
// Guest config (node -> guest, via config drive config.json)
// ---------------------------------------------------------------------------

// BootKind tells the guest agent why it is booting.
type BootKind string

const (
	BootCreate  BootKind = "create"  // brand new sandbox from a base image or saved snapshot
	BootResume  BootKind = "resume"  // same sandbox, from its latest snapshot
	BootFork    BootKind = "fork"    // new sandbox, copy of another sandbox's snapshot
	BootRecover BootKind = "recover" // restart of a hung machine, disk untouched
	BootInspect BootKind = "inspect" // throwaway VM used to read files out of a snapshot
	// BootWarm: a node's pre-booted pool VM (SIZE-012). The guest serves
	// only GET /v1/health and POST /v1/internal/adopt (body: the sandbox's
	// GuestConfig, bearer = the warm token); it then saves that config and
	// boots as that sandbox (Boot create), also after later restarts.
	BootWarm BootKind = "warm"
)

type GuestConfig struct {
	SandboxID  string   `json:"sandboxId"`
	Token      string   `json:"token"`      // bearer token required on every guest API call
	ListenAddr string   `json:"listenAddr"` // default ":7070"
	Hostname   string   `json:"hostname"`
	Boot       BootKind `json:"boot"`
	// FirstBoot is true when one-time work (Files, SetupScript) must run:
	// on create, not on resume/fork/recover.
	FirstBoot bool `json:"firstBoot"`

	Env               map[string]string `json:"env,omitempty"`         // exported for commands, ssh, agents
	SecretFiles       []File            `json:"secretFiles,omitempty"` // environment secret files, written every boot
	Files             []File            `json:"files,omitempty"`       // one-time files (--file), FirstBoot only
	SetupScript       string            `json:"setupScript,omitempty"` // one-time, as GuestUser, in background after ready
	Repos             []Repo            `json:"repos,omitempty"`       // cloned on first boot; repo setup runs every boot
	SSHAuthorizedKeys []string          `json:"sshAuthorizedKeys,omitempty"`
	RegistryLogins    []RegistryLogin   `json:"registryLogins,omitempty"` // docker login on every boot
	AIProxy           *AIProxy          `json:"aiProxy,omitempty"`
	Desktop           DesktopConfig     `json:"desktop"`

	// Scrub: credentials to delete from the disk at this boot, before the
	// guest writes SecretFiles/Files and before it serves its API (see
	// GuestScrub). nil = nothing to scrub.
	Scrub *GuestScrub `json:"scrub,omitempty"`
	// NoMonitoring: monitoring is off for this sandbox: keep no command
	// history, output or process samples and emit no command.*/process.*
	// events (Boat "monitoring: false"). Data recorded before is deleted.
	NoMonitoring bool `json:"noMonitoring,omitempty"`
	// Playbook: paths the guest reads first when it starts from a snapshot
	// (LIFE-018): a saved snapshot's learned startup playbook. Merged ahead
	// of the disk's own ~/.crowdvm/playbook.json. Optional.
	Playbook []string `json:"playbook,omitempty"`
	// ZeroDataRetention: the owner's account has zero data retention on.
	// Implies NoMonitoring (the server sets both); in addition the guest
	// keeps no prompt transcripts/events on disk after a turn ends.
	ZeroDataRetention bool `json:"zeroDataRetention,omitempty"`
}

// GuestScrub lists what the guest deletes synchronously in Boot, before
// :7070 serves (ENV-018 --no-env, SNAP-023 deploy of another account's
// saved snapshot, ENV-007 secrets withheld by a newer environment version).
// Paths are relative to GuestHome unless absolute. Paths that the same
// GuestConfig writes (SecretFiles, Files) are never listed. The guest
// records the outcome under its data dir and answers
// GET /v1/internal/scrub -> {"id","done","error"} (404 = scrub unsupported:
// the server then scrubs through the commands API before marking ready).
type GuestScrub struct {
	ID              string   `json:"id"`
	Files           []string `json:"files,omitempty"`           // rm -f
	Dirs            []string `json:"dirs,omitempty"`            // rm -rf (repository clones)
	SSHKeys         bool     `json:"sshKeys,omitempty"`         // ~/.ssh/id_* (private + .pub)
	AuthorizedKeys  bool     `json:"authorizedKeys,omitempty"`  // empty ~/.ssh/authorized_keys
	GitHubLogout    bool     `json:"githubLogout,omitempty"`    // gh auth logout for every host
	RegistryLogouts []string `json:"registryLogouts,omitempty"` // docker logout <registry> (root and user)
	History         bool     `json:"history,omitempty"`         // the command history and detached logs (with their output)
}

type File struct {
	Path    string `json:"path"` // absolute, or relative to GuestHome
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode,omitempty"` // default 0600 for secrets, 0644 otherwise
}

type Repo struct {
	URL         string `json:"url"`
	Branch      string `json:"branch,omitempty"`
	Path        string `json:"path,omitempty"` // default GuestHome/<repo name>
	SetupScript string `json:"setupScript,omitempty"`
	Token       string `json:"token,omitempty"` // optional git credential (https)
	// SetupBlocking: integrated-agent prompts wait until this repo's setup
	// script finished (commands and SSH do not).
	SetupBlocking bool `json:"setupBlocking,omitempty"`
}

type RegistryLogin struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// AIProxy: when set, harness CLIs are pointed at the server's AI credentials
// proxy instead of receiving raw provider keys.
type AIProxy struct {
	BaseURL string `json:"baseUrl"` // e.g. http://10.0.2.2:8420/ai-proxy  (as reachable from the guest)
	Token   string `json:"token"`   // per-sandbox proxy token
}

type DesktopConfig struct {
	Width  int `json:"width,omitempty"`  // default 1920
	Height int `json:"height,omitempty"` // default 1080
}

// ---------------------------------------------------------------------------
// Node <-> server
// ---------------------------------------------------------------------------

// Size is a concrete machine shape (see internal/api.Sizes for the catalog).
type Size struct {
	Name      string `json:"name"`
	VCPU      int    `json:"vcpu"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
}

// RegisterRequest: POST {server}/node/v1/register (plain HTTP, before the tunnel exists).
type RegisterRequest struct {
	JoinToken string   `json:"joinToken"`
	Info      NodeInfo `json:"info"`
}

type RegisterResponse struct {
	NodeID    string `json:"nodeId"`
	NodeToken string `json:"nodeToken"` // used as Bearer on GET /node/v1/connect (websocket)
}

type NodeInfo struct {
	NodeID   string            `json:"nodeId,omitempty"`
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	OS       string            `json:"os"`     // runtime.GOOS
	Arch     string            `json:"arch"`   // runtime.GOARCH
	Driver   string            `json:"driver"` // "qemu" | "process" (tests only)
	Labels   map[string]string `json:"labels,omitempty"`
	Bench    Benchmark         `json:"benchmark"`
	Capacity Capacity          `json:"capacity"` // what the owner shares
	// DirectHost: LAN/Tailscale address on which the node publishes direct
	// per-sandbox ports (ssh etc.), "" when disabled.
	DirectHost string   `json:"directHost,omitempty"`
	Images     []string `json:"images,omitempty"` // base image ids cached locally
	// Paused: the owner stopped new sandboxes on this node (running ones stay).
	Paused bool `json:"paused,omitempty"`
	// PauseReason says why Paused is set ("owner", "schedule", "hold"), so
	// the dashboard can show it. Display only; the scheduler reads Paused.
	PauseReason string `json:"pauseReason,omitempty"`
	// LazyChain: the node boots a VM before its snapshot chain is local
	// (LIFE-013): missing layers are read on demand from the server while
	// the node downloads them (VMStatus.Hydrating until done). The server
	// need not pull the chain (POST /v1/snapshots/{id}/fetch) before
	// POST /v1/vms; the node needs only the base image.
	LazyChain bool `json:"lazyChain,omitempty"`
	// GPUs: the GPUs this node passes through to sandboxes (VFIO; node
	// config gpu.passthrough, validated when the node starts). Only these
	// make a node eligible for placement gpu, and only while one is free
	// (len(GPUs) > Usage.GPUs): a VM with VMSpec.GPU gets one for itself.
	// Bench.GPUs lists every detected GPU, passed through or not.
	GPUs []PassthroughGPU `json:"gpus,omitempty"`
	// VirGL: every VM gets a virtio-gpu with OpenGL rendered on this node's
	// GPU (virgl + egl-headless; node config gpu.virgl): /dev/dri in the
	// guest for 3D desktops and EGL/OpenGL apps. No CUDA or compute, and
	// not what placement gpu asks for.
	VirGL bool `json:"virgl,omitempty"`
}

// PassthroughGPU is a GPU a node hands to one VM at a time through VFIO,
// together with the other functions of its IOMMU group (e.g. its HDMI
// audio).
type PassthroughGPU struct {
	Address    string   `json:"address"` // PCI address, e.g. "0000:01:00.0"
	Vendor     string   `json:"vendor"`  // "nvidia" | "amd" | "intel" | PCI vendor id
	Model      string   `json:"model,omitempty"`
	IOMMUGroup int      `json:"iommuGroup"`
	Functions  []string `json:"functions"`       // every device given to the VM, the GPU first
	InUse      string   `json:"inUse,omitempty"` // sandbox using it
}

type Benchmark struct {
	RanAt          time.Time    `json:"ranAt"`
	DurationMs     int64        `json:"durationMs"`
	CPU            CPUBench     `json:"cpu"`
	Memory         MemoryBench  `json:"memory"`
	Disk           DiskBench    `json:"disk"`
	Network        NetworkBench `json:"network"`
	GPUs           []GPUInfo    `json:"gpus"`
	Virtualization VirtInfo     `json:"virtualization"`
	// Categories: one tier per dimension: "cpu","memory","disk","network","gpu" ->
	// "low" | "medium" | "high" | "extreme" | "none".
	Categories map[string]string `json:"categories"`
	Score      int               `json:"score"` // overall 0..1000
	Tier       string            `json:"tier"`  // "bronze" | "silver" | "gold" | "platinum"
}

type CPUBench struct {
	Model       string  `json:"model"`
	Cores       int     `json:"cores"`
	Threads     int     `json:"threads"`
	SingleScore float64 `json:"singleScore"` // ops/s single thread (higher better)
	MultiScore  float64 `json:"multiScore"`
}

type MemoryBench struct {
	TotalMiB      int     `json:"totalMiB"`
	AvailableMiB  int     `json:"availableMiB"`
	BandwidthMBps float64 `json:"bandwidthMBps"`
}

type DiskBench struct {
	Path      string  `json:"path"`
	FreeGiB   float64 `json:"freeGiB"`
	WriteMBps float64 `json:"writeMBps"`
	ReadMBps  float64 `json:"readMBps"`
}

type NetworkBench struct {
	LatencyMs float64 `json:"latencyMs"` // to the server
	DownMbps  float64 `json:"downMbps"`  // server -> node
	UpMbps    float64 `json:"upMbps"`    // node -> server
	Via       string  `json:"via"`       // "lan" | "tailscale" | "loopback" | "other"
	// Internet: the node's own outbound internet, which its sandboxes use
	// for packages and AI APIs: "reachable" | "unreachable" ("" = not
	// measured). InternetLatencyMs: best TCP connect time to public anycast
	// resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9 on 443).
	Internet          string  `json:"internet,omitempty"`
	InternetLatencyMs float64 `json:"internetLatencyMs,omitempty"`
}

type GPUInfo struct {
	Vendor  string `json:"vendor"`
	Model   string `json:"model"`
	VRAMMiB int    `json:"vramMiB"`
	Driver  string `json:"driver,omitempty"`
	Compute string `json:"compute,omitempty"` // e.g. CUDA capability "8.6"
}

type VirtInfo struct {
	Accel  string `json:"accel"` // "kvm" | "whpx" | "hvf" | "tcg" | "none"
	Nested bool   `json:"nested"`
	QEMU   string `json:"qemu,omitempty"` // qemu version
}

type Capacity struct {
	VCPU         int `json:"vcpu"`
	MemoryMiB    int `json:"memoryMiB"`
	DiskGiB      int `json:"diskGiB"`
	MaxSandboxes int `json:"maxSandboxes"`
}

type Usage struct {
	VCPU      int `json:"vcpu"`
	MemoryMiB int `json:"memoryMiB"`
	DiskGiB   int `json:"diskGiB"`
	VMs       int `json:"vms"`
	GPUs      int `json:"gpus,omitempty"` // passthrough GPUs in use (NodeInfo.GPUs)
}

// VM states as reported by the node.
const (
	VMStarting = "starting" // qemu launched, guest agent not yet healthy
	VMRunning  = "running"  // guest agent healthy
	VMStopping = "stopping"
	VMStopped  = "stopped"
	VMError    = "error"
)

// VMSpec: POST /v1/vms  (server -> node). The node replies 202 with VMStatus and
// boots asynchronously; progress arrives as vm.state events.
type VMSpec struct {
	SandboxID string      `json:"sandboxId"`
	Size      Size        `json:"size"`
	ImageID   string      `json:"imageId"`  // base image
	ParentID  string      `json:"parentId"` // snapshot to start from ("" = clean base image)
	Guest     GuestConfig `json:"guest"`
	Nested    bool        `json:"nested"`        // expose virtualization extensions
	Net       *NetPolicy  `json:"net,omitempty"` // optional per-sandbox egress rules
	// NoSnapshots: the sandbox has snapshots off (Boat snapshots:false): the
	// node takes no periodic live snapshots of it and uploads nothing; the
	// server stops it with DELETE /v1/vms/{id} (the disk is erased).
	NoSnapshots bool `json:"noSnapshots,omitempty"`
	// DirectPorts: guest ports the node publishes on its DirectHost from
	// boot on (URL-012, see DirectPortsRequest). Ignored without DirectHost.
	DirectPorts []DirectPort `json:"directPorts,omitempty"`
	// GPU: the sandbox gets a GPU of its own (placement gpu): the node
	// passes one of its free NodeInfo.GPUs through, or answers 503
	// out_of_capacity when none is free.
	GPU bool `json:"gpu,omitempty"`
	// GPUDevices is set by the node, never by the server: the PCI
	// functions (one GPU's IOMMU group) the VM gets through VFIO.
	GPUDevices []string `json:"gpuDevices,omitempty"`
}

// DirectPort is a guest port the node publishes on its LAN/Tailscale
// DirectHost (URL-012, Boat "raw TCP/UDP via the sandbox IP"). Traffic
// reaches the guest's NIC from the gateway (10.0.2.2), so the guest
// firewall applies: the port must be allowed (ufw allow <port>).
// The process driver has no NIC: TCP goes through the agent tunnel, UDP
// is not available.
type DirectPort struct {
	Port  int    `json:"port"`            // guest port 1..65535
	Proto string `json:"proto,omitempty"` // "tcp" (default) | "udp"
}

// DirectPortsRequest: PUT /v1/vms/{id}/direct-ports (server -> node)
// replaces the set of published DirectPorts (empty = only ssh). The node
// answers 200 VMStatus whose Direct map has "tcp/<port>" / "udp/<port>"
// keys -> "host:port" (the same port number when it was free on the
// node, else a random one), or 409 direct_disabled without DirectHost.
// The set is kept across restarts of the VM on that node (not across
// stop/resume: the server re-sends VMSpec.DirectPorts).
type DirectPortsRequest struct {
	Ports []DirectPort `json:"ports"`
}

// NetPolicy is a per-sandbox egress firewall (CIDRs or bare addresses). The
// node applies it on top of its own policy, so it can only narrow what the
// node allows; the CrowdVM server stays reachable. Enforced by the qemu
// driver's netstack networking (not by slirp or the process driver).
type NetPolicy struct {
	Allow []string `json:"allow,omitempty"` // if set, only these destinations are reachable
	Deny  []string `json:"deny,omitempty"`
}

type VMStatus struct {
	SandboxID  string    `json:"sandboxId"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	GuestReady bool      `json:"guestReady"`
	// Direct: LAN-reachable endpoints published by the node, e.g.
	// {"ssh":"192.168.1.5:40122","tcp/3000":"192.168.1.5:3000","udp/5353":"192.168.1.5:41000"}.
	Direct map[string]string `json:"direct,omitempty"`
	// GuestVersion: the guest agent's version as its GET /v1/health reports
	// it ("" until the node probed it). CMD-012: the server compares it to
	// the oldest guest it supports and answers 409 agent_outdated.
	GuestVersion string `json:"guestVersion,omitempty"`
	// Hydrating: the VM booted before its whole snapshot chain was on the
	// node (lazy chain fetch, LIFE-013): missing layers are read on demand
	// from the server while the node downloads them. A vm.state with
	// Hydrating false (and State running) follows when the chain is local.
	Hydrating bool `json:"hydrating,omitempty"`
	// GPU: PCI address of the passthrough GPU the VM has (VMSpec.GPU).
	GPU string `json:"gpu,omitempty"`
}

// StopRequest: POST /v1/vms/{id}/stop. Synchronous. The guest cleans unsaved
// paths, the VM powers off, and the disk delta becomes immutable snapshot
// SnapshotID. The node then uploads it (snapshot.uploaded event).
type StopRequest struct {
	SnapshotID string `json:"snapshotId"`
	Force      bool   `json:"force,omitempty"` // skip graceful shutdown
}

// SnapshotRequest: POST /v1/vms/{id}/snapshot. Live, crash-consistent snapshot
// of a running VM (guest fs synced first). Used for periodic saves and fork.
type SnapshotRequest struct {
	SnapshotID string `json:"snapshotId"`
}

type SnapshotRef struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId,omitempty"` // previous snapshot in the chain
	ImageID   string    `json:"imageId"`            // base image at the bottom of the chain
	SizeBytes int64     `json:"sizeBytes"`
	CreatedAt time.Time `json:"createdAt"`
	// SHA256 (hex) of the snapshot file. Set by the node on upload (also sent
	// as header X-Crowdvm-Sha256 on PUT /node/v1/blobs/...); the server returns
	// it in GET /node/v1/snapshots/{id} and as X-Crowdvm-Sha256 on blob GETs so
	// downloads can be verified. Optional.
	SHA256 string `json:"sha256,omitempty"`
	// Supersedes: snapshots this one replaces in the sandbox's chain. To keep
	// chains short at a 60 s interval, the node merges runs of older layers
	// into the next one (tiered compaction): this layer then holds their data
	// and its ParentID skips them. The server may delete superseded periodic
	// snapshots that nothing else needs (saved snapshots, forks, heads).
	Supersedes []string `json:"supersedes,omitempty"`
	// SandboxID: the sandbox the node took the snapshot of (set by the node
	// on snapshots it takes; empty on layers pulled from the server). The
	// server uses it to refuse and erase uploads of a deleted sandbox.
	SandboxID string `json:"sandboxId,omitempty"`
}

// A node reports each snapshot it takes on its own (periodic) with
// POST /node/v1/snapshots (body: SnapshotRef) right after taking it and
// before the upload starts (again when the upload starts, in case the
// first report failed). The server records it at once, with its parent and
// the node as its location, as not stored yet; the upload then only marks
// it stored. Until then nothing that builds on the layer (a stop, save or
// fork) sees a hole in its chain.

// NodeSnapshot is one entry of the node's GET /v1/snapshots: a snapshot in
// its local store, or one it is still uploading.
type NodeSnapshot struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandboxId,omitempty"` // "" when unknown (pulled layers)
	ParentID  string `json:"parentId,omitempty"`
	// Uploading: the node has not finished uploading it to the server (also
	// while it retries); the server must not purge it before.
	Uploading bool `json:"uploading,omitempty"`
	// From the layer's record (a stop result the server adopts after a 404).
	ImageID    string     `json:"imageId,omitempty"`
	SizeBytes  int64      `json:"sizeBytes,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	Supersedes []string   `json:"supersedes,omitempty"`
}

// PrepareStopRequest is the optional JSON body of the guest's
// POST /v1/internal/prepare-stop (node -> guest; an empty body means {}).
// Boat stop order: pre-stop hook (~/.crowdvm/pre-stop, else ~/.boat/pre-stop;
// as GuestUser, cwd GuestHome, 60 s cap, failures ignored), stop running
// Docker containers (30 s each), then delete unsaved paths, fstrim, sync.
// The node then powers the VM off WITHOUT an OS shutdown (QMP stop + quit):
// other programs get no signal.
type PrepareStopRequest struct {
	// Force: forced stop: skip the pre-stop hook and the Docker stop; still
	// clean, sync and (with Freeze) freeze. The node allows it ~15 s.
	Force bool `json:"force,omitempty"`
	// Freeze: after the sync, freeze the writable guest block filesystems
	// (FIFREEZE) and answer only then. The node terminates the VM right
	// after the reply; the guest auto-thaws after FreezeTimeoutSeconds
	// (default 60) as a safety net if it is still running.
	Freeze               bool `json:"freeze,omitempty"`
	FreezeTimeoutSeconds int  `json:"freezeTimeoutSeconds,omitempty"`
}

// FreezeRequest is the optional body of guest POST /v1/internal/freeze
// (live snapshots: sync + freeze, QMP snapshot, POST /v1/internal/thaw).
// The guest auto-thaws after TimeoutSeconds (default 60).
type FreezeRequest struct {
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// Event: node -> server, POST /node/v1/events (batch: []Event).
type Event struct {
	Time      time.Time       `json:"time"`
	SandboxID string          `json:"sandboxId,omitempty"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// Event types emitted by the node.
const (
	EvVMState          = "vm.state"          // Data: VMStatus
	EvSnapshotUploaded = "snapshot.uploaded" // Data: SnapshotRef
	EvSnapshotFailed   = "snapshot.failed"   // Data: {"snapshotId","error"}
	EvGuest            = "guest.event"       // Data: raw guest event (see GuestEvent)
	// EvTransfer: image/snapshot download or upload progress (throttled ~1/s),
	// Data: TransferProgress. SandboxID set when the transfer is for a VM start.
	EvTransfer = "node.transfer"
)

// TransferProgress is the Data of EvTransfer events.
type TransferProgress struct {
	Kind      string `json:"kind"`      // "image" | "snapshot"
	ID        string `json:"id"`        // image or snapshot id
	Direction string `json:"direction"` // "download" | "upload"
	Bytes     int64  `json:"bytes"`
	Total     int64  `json:"total"` // -1 if unknown
	Done      bool   `json:"done,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Heartbeat: node -> server every 10s, POST /node/v1/heartbeat.
type Heartbeat struct {
	Usage Usage      `json:"usage"`
	VMs   []VMStatus `json:"vms"`
	Info  *NodeInfo  `json:"info,omitempty"` // sent when changed (e.g. new benchmark)
}

// HeartbeatResponse is the optional JSON body of the heartbeat reply: settings
// the server pushes to the node. Missing/zero fields keep node defaults.
type HeartbeatResponse struct {
	// SnapshotIntervalSec: periodic live snapshot interval for running VMs
	// (0 = node default 60 s, < 0 = disabled).
	SnapshotIntervalSec int `json:"snapshotIntervalSec,omitempty"`
	// Drained: the node's owner or an admin drained this node (no new
	// sandboxes), shown in the node's status (bugs-r6-opus 7).
	Drained bool `json:"drained,omitempty"`
	// EarnedCredits: credits this node earned its owner in total, and in the
	// last 24 hours (shown on the node's local page).
	EarnedCredits float64 `json:"earnedCredits,omitempty"`
	Earned24h     float64 `json:"earned24h,omitempty"`
	// Evacuation: the owner or an admin is reclaiming this node: at Deadline
	// its sandboxes are stopped and resumed on other nodes (node-ui design).
	Evacuation *Evacuation `json:"evacuation,omitempty"`
}

// Evacuation of a node: sandboxes still on it at Deadline are stopped (their
// disks saved) and resumed elsewhere; --no-snapshots sandboxes are erased.
type Evacuation struct {
	StartedAt time.Time `json:"startedAt"`
	Deadline  time.Time `json:"deadline"`
}

// GuestEvent is one line of the guest's GET /v1/internal/events/stream NDJSON.
// The node wraps each into an Event{Type: EvGuest}.
type GuestEvent struct {
	Time time.Time       `json:"time"`
	Type string          `json:"type"` // e.g. "setup.status", "prompt.event", "command.exited"
	Data json.RawMessage `json:"data,omitempty"`
}

// prompt.event guest events (agent events, AGT-013) are persisted by the
// server from the event stream. Command history, output and process
// samples are pulled by the server from the guest instead:
// GET /v1/internal/history/export + POST /v1/internal/history/ack.

// ---------------------------------------------------------------------------
// Guest live config, history export (guest-core; additive)
// ---------------------------------------------------------------------------

// ApplyConfig is the body of guest POST /v1/internal/apply-config: hot-apply a
// new environment version to a running sandbox (ENV-007). A nil field keeps
// the current value; a set field (also an empty one) replaces it:
//   - Env: rewrites /etc/profile.d/crowdvm-env.sh and the ~/.bashrc block; new
//     commands, shells and integrated-agent turns get it.
//   - SecretFiles: written (default 0600); files the guest wrote for an
//     earlier config and that are no longer listed are deleted.
//   - Repos: repositories not on disk are cloned and set up in the
//     background (setup-status / repo.setup events), SetupBlocking honoured.
//   - RegistryLogins: docker login for each; registries no longer listed get
//     docker logout.
//   - AIProxy / ClearAIProxy: the AI proxy variables of commands and agents.
//   - SSHAuthorizedKeys: added to ~/.ssh/authorized_keys (never removed).
//
// Scrub, when set, runs first (same semantics as GuestConfig.Scrub).
// Answer: guest.config_applied {filesWritten, filesRemoved, registriesLoggedIn,
// registriesLoggedOut, reposStarted, envKeys}.
type ApplyConfig struct {
	Env map[string]string `json:"env"`
	// EnvSet / EnvUnset patch the current env instead of replacing it (applied
	// after Env), so a caller need not re-send values it cannot read back
	// (e.g. CROWDVM_API_KEY).
	EnvSet            map[string]string `json:"envSet,omitempty"`
	EnvUnset          []string          `json:"envUnset,omitempty"`
	SecretFiles       []File            `json:"secretFiles"`
	Repos             []Repo            `json:"repos"`
	RegistryLogins    []RegistryLogin   `json:"registryLogins"`
	AIProxy           *AIProxy          `json:"aiProxy,omitempty"`
	ClearAIProxy      bool              `json:"clearAIProxy,omitempty"`
	SSHAuthorizedKeys []string          `json:"sshAuthorizedKeys"`
	Scrub             *GuestScrub       `json:"scrub,omitempty"`
}

// CommandRecord is Boat's CommandRecord (command history), shared by the
// guest (which records it) and the server (which keeps a copy readable while
// the sandbox is stopped).
type CommandRecord struct {
	CommandID       string     `json:"commandId"`
	SandboxID       string     `json:"sandboxId"`
	Command         string     `json:"command"`
	Cwd             *string    `json:"cwd"`
	Mode            string     `json:"mode"` // sync | stream | detached | process
	ProcessID       *int       `json:"processId"`
	Status          string     `json:"status"` // running | exited | timed_out | failed | killed | lost
	ExitCode        *int       `json:"exitCode"`
	Signal          *string    `json:"signal"`
	StdoutBytes     int64      `json:"stdoutBytes"`
	StderrBytes     int64      `json:"stderrBytes"`
	OutputTruncated bool       `json:"outputTruncated"`
	OOMKilled       bool       `json:"oomKilled,omitempty"`
	StartedAt       time.Time  `json:"startedAt"`
	FinishedAt      *time.Time `json:"finishedAt"`
}

// HistoryExport answers guest GET /v1/internal/history/export?flush=0|1&maxBytes=N
// (type "history.export"). The server polls it (~2 s) and acks each batch
// with POST /v1/internal/history/ack {"batch"}; unacked data comes again in
// the next export, so storing must be idempotent: records by CommandID
// (upsert), chunks by (CommandID, Stream, Offset), samples by Time.
// Output cadence (Boat): mode stream every 2 s, detached and process every
// 30 s, sync when it ends (first 8 MiB per stream); flush=1 returns all
// pending output (the server calls it before a non-forced stop). Empty with
// GuestConfig.NoMonitoring. More = data is left: export again.
type HistoryExport struct {
	Batch   string          `json:"batch"` // "" when there is nothing to ack
	Records []CommandRecord `json:"records"`
	Chunks  []HistoryChunk  `json:"chunks"`
	Samples []GuestSample   `json:"samples"`
	More    bool            `json:"more"`
	// Now: the guest's clock when it built the export, so the server can
	// place record times on its own clock (history privacy periods).
	Now time.Time `json:"now,omitempty"`
}

// HistoryChunk is a piece of a command's output (Data is base64 in JSON).
type HistoryChunk struct {
	CommandID string `json:"commandId"`
	Stream    string `json:"stream"` // stdout | stderr
	Offset    int64  `json:"offset"`
	Data      []byte `json:"data"`
}

// GuestSample is taken every 30 s while monitoring is on (CMD-025/027):
// whole-VM CPU/RAM/disk and the 12 busiest processes. The server keeps 7 d
// and answers /processes of a stopped sandbox from the last one (live:false).
type GuestSample struct {
	Time             time.Time     `json:"time"`
	CPUUtilization   float64       `json:"cpuUtilization"` // 0..1, all vCPUs
	MemoryUsedBytes  int64         `json:"memoryUsedBytes"`
	MemoryTotalBytes int64         `json:"memoryTotalBytes"`
	DiskUsedBytes    int64         `json:"diskUsedBytes"` // root filesystem
	DiskTotalBytes   int64         `json:"diskTotalBytes"`
	IOPressure       float64       `json:"ioPressure"` // /proc/pressure/io some avg60, 0..1
	Processes        []ProcessInfo `json:"processes"`
}

// ProcessInfo is one row of Boat's /processes listing.
type ProcessInfo struct {
	Pid        int       `json:"pid"`
	ParentPid  int       `json:"parentPid"`
	User       string    `json:"user"`
	CPUPercent float64   `json:"cpuPercent"`
	MemoryMB   float64   `json:"memoryMB"`
	StartedAt  time.Time `json:"startedAt"`
	Command    string    `json:"command"`
	CommandID  *string   `json:"commandId"`
}
