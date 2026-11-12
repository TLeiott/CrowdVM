package client

import (
	"encoding/json"
	"strconv"
)

// PageInfo is returned by cursor-paginated lists.
type PageInfo struct {
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
	Limit      int    `json:"limit,omitempty"`
}

// TTL is a sandbox lifetime in requests: nil leaves the server default,
// NoAutoStop() sends an explicit null (no automatic stop).
type TTL struct {
	Seconds int
	Off     bool
}

func TTLSeconds(n int) *TTL { return &TTL{Seconds: n} }
func NoAutoStop() *TTL      { return &TTL{Off: true} }

func (t TTL) MarshalJSON() ([]byte, error) {
	if t.Off {
		return []byte("null"), nil
	}
	return []byte(strconv.Itoa(t.Seconds)), nil
}

func (t *TTL) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = TTL{Off: true}
		return nil
	}
	return json.Unmarshal(b, &t.Seconds)
}

// Sandbox is one machine.
type Sandbox struct {
	DirectEndpoints     map[string]string `json:"directEndpoints,omitempty"`
	Storage             Object            `json:"storage,omitempty"`
	ImageID             string            `json:"imageId,omitempty"`
	NodeID              string            `json:"nodeId,omitempty"`
	Placement           Object            `json:"placement,omitempty"`
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	State               string            `json:"state"`
	Error               string            `json:"error,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Health              string            `json:"health,omitempty"`
	NoEnv               bool              `json:"noEnv"`
	HoldsCreatorLogins  bool              `json:"holdsCreatorLogins"`
	HealthReason        string            `json:"healthReason,omitempty"`
	Hydrated            bool              `json:"hydrated,omitempty"`
	Type                string            `json:"type,omitempty"`
	VCPU                int               `json:"vcpu,omitempty"`
	MemoryGB            int               `json:"memoryGB,omitempty"`
	BillingMultiplier   float64           `json:"billingMultiplier,omitempty"`
	URL                 string            `json:"url,omitempty"`
	IP                  string            `json:"ip,omitempty"`
	SSHEndpoint         string            `json:"sshEndpoint,omitempty"`
	CreatedAt           string            `json:"createdAt,omitempty"`
	UpdatedAt           string            `json:"updatedAt,omitempty"`
	StoppedAt           string            `json:"stoppedAt,omitempty"`
	ArchiveAfter        string            `json:"archiveAfter,omitempty"`
	TTLSeconds          *int              `json:"ttlSeconds,omitempty"`
	DesktopAvailable    bool              `json:"desktopAvailable"`
	DesktopURL          string            `json:"desktopUrl,omitempty"`
	Snapshots           *bool             `json:"snapshots,omitempty"`
	Monitoring          *bool             `json:"monitoring,omitempty"`
	SnapshotAvailable   bool              `json:"snapshotAvailable"`
	SnapshotCompletedAt string            `json:"snapshotCompletedAt,omitempty"`
	CreatedBy           string            `json:"createdBy,omitempty"`
	Access              string            `json:"access,omitempty"`
	Visibility          string            `json:"visibility,omitempty"`
	Cost                *Cost             `json:"cost,omitempty"`
	Subdomain           string            `json:"subdomain,omitempty"`
	LastSnapshotStatus  string            `json:"lastSnapshotStatus,omitempty"`
	LastReboot          Object            `json:"lastReboot,omitempty"`
	SetupStatus         string            `json:"setupStatus,omitempty"`
	SetupError          string            `json:"setupError,omitempty"`
	Environment         string            `json:"environment,omitempty"`
	EnvironmentVersion  *int              `json:"environmentVersion,omitempty"`
	Team                Object            `json:"team,omitempty"`
	Stop                *StopOperation    `json:"stop,omitempty"`
	// Node is a CrowdVM extension: the node the sandbox runs on.
	Node string `json:"node,omitempty"`
	// WaitingForSnapshot (CrowdVM): the snapshot a starting sandbox waits
	// to be uploaded from the node that holds it.
	WaitingForSnapshot string `json:"waitingForSnapshot,omitempty"`
	// WaitingForCapacity (CrowdVM): a starting sandbox queued until a node has
	// room; StatusMessage explains the wait.
	WaitingForCapacity bool   `json:"waitingForCapacity,omitempty"`
	WaitingSince       string `json:"waitingSince,omitempty"`
	WaitDeadline       string `json:"waitDeadline,omitempty"`
	StatusMessage      string `json:"statusMessage,omitempty"`
}

type Cost struct {
	Since   string  `json:"since,omitempty"`
	Seconds int64   `json:"seconds"`
	Dollars float64 `json:"dollars"`
}

type StopOperation struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	RequestedAt   string `json:"requestedAt,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	Error         string `json:"error,omitempty"`
	EndedAt       string `json:"endedAt,omitempty"`
	Forced        bool   `json:"forced,omitempty"`
}

type HostPortRequest struct {
	Port      int    `json:"port"`
	Public    bool   `json:"public,omitempty"`
	Localhost bool   `json:"localhost,omitempty"`
	Title     string `json:"title,omitempty"`
}

type HostedPort struct {
	Port        int    `json:"port"`
	URL         string `json:"url"`
	IsProtected bool   `json:"isProtected"`
	Access      string `json:"access,omitempty"`
	Localhost   bool   `json:"localhost,omitempty"`
	Error       string `json:"error,omitempty"`
	Message     string `json:"message,omitempty"`
}

type FileSpec struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"`
}

type CreateSandboxRequest struct {
	Type        string            `json:"type,omitempty"`
	TTLSeconds  *TTL              `json:"ttlSeconds,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Environment string            `json:"environment,omitempty"`
	NoEnv       bool              `json:"noEnv,omitempty"`
	Snapshots   *bool             `json:"snapshots,omitempty"`
	Monitoring  *bool             `json:"monitoring,omitempty"`
	FailFast    bool              `json:"failFast,omitempty"`
	Count       int               `json:"count,omitempty"`
	SetupScript string            `json:"setupScript,omitempty"`
	Name        string            `json:"name,omitempty"`
	Visibility  string            `json:"visibility,omitempty"`
	HostedPorts []HostPortRequest `json:"hostedPorts,omitempty"`
	Files       []FileSpec        `json:"files,omitempty"`
	Org         string            `json:"org,omitempty"`
	From        string            `json:"from,omitempty"`
	// Placement is the CrowdVM extension: hard requirements on the node.
	Placement *Placement `json:"placement,omitempty"`
}

// Placement (CrowdVM extension of create, resume and fork) pins a sandbox to
// nodes with a GPU, nested virtualization, labels, a minimum tier or minimum
// benchmark levels, or to one node.
type Placement struct {
	GPU        bool              `json:"gpu,omitempty"`
	Nested     bool              `json:"nested,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	MinTier    string            `json:"minTier,omitempty"`
	Categories map[string]string `json:"categories,omitempty"`
	Node       string            `json:"node,omitempty"`
}

type CreateSandboxResponse struct {
	Status      string       `json:"status"`
	TTLSeconds  *int         `json:"ttlSeconds,omitempty"`
	Sandbox     *Sandbox     `json:"sandbox,omitempty"`
	Sandboxes   []Sandbox    `json:"sandboxes,omitempty"`
	HostedPorts []HostedPort `json:"hostedPorts,omitempty"`
}

type ResumeRequest struct {
	FailFast    bool              `json:"failFast,omitempty"`
	Type        string            `json:"type,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Environment string            `json:"environment,omitempty"`
	NoEnv       bool              `json:"noEnv,omitempty"`
	TTLSeconds  *TTL              `json:"ttlSeconds,omitempty"`
	Placement   *Placement        `json:"placement,omitempty"`
}

type ForkRequest struct {
	Labels      map[string]string `json:"labels,omitempty"`
	FailFast    bool              `json:"failFast,omitempty"`
	Monitoring  *bool             `json:"monitoring,omitempty"`
	Snapshots   *bool             `json:"snapshots,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Type        string            `json:"type,omitempty"`
	TTLSeconds  *TTL              `json:"ttlSeconds,omitempty"`
	Environment string            `json:"environment,omitempty"`
	NoEnv       bool              `json:"noEnv,omitempty"`
	Name        string            `json:"name,omitempty"`
	Placement   *Placement        `json:"placement,omitempty"`
}

type UpdateSandboxRequest struct {
	Name       string `json:"name,omitempty"`
	TTLSeconds *TTL   `json:"ttlSeconds,omitempty"`
	Subdomain  string `json:"subdomain,omitempty"`
}

// SandboxAction is the response of stop/resume/fork/share/recover/interrupt.
type SandboxAction struct {
	Type           string   `json:"type,omitempty"`
	ID             string   `json:"id"`
	Status         string   `json:"status,omitempty"`
	Sandbox        *Sandbox `json:"sandbox,omitempty"`
	ConversationID string   `json:"conversationId,omitempty"`
	// Operation is set by stop (operation.id).
	Operation *StopOperation `json:"operation,omitempty"`
	Stop      *StopOperation `json:"stop,omitempty"`
	// Share/unshare: the change applies after a stop + resume.
	RestartRequired bool   `json:"restartRequired,omitempty"`
	Message         string `json:"message,omitempty"`
}

type ListSandboxesOptions struct {
	Limit      int
	Cursor     string
	Sort       string
	State      string
	NamePrefix string
	Label      []string
	Include    string
}

type SandboxList struct {
	Sandboxes []Sandbox `json:"sandboxes"`
	PageInfo  PageInfo  `json:"pageInfo"`
}

// --- commands ---

type CommandRequest struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	Detached       bool   `json:"detached,omitempty"`
	Stream         bool   `json:"stream,omitempty"`
	// KillAfterSeconds (detached): the sandbox kills the command after this
	// long (CrowdVM extension; the CLI's backstop for its own --timeout).
	KillAfterSeconds int `json:"killAfterSeconds,omitempty"`
}

type CommandResult struct {
	CommandID       string `json:"commandId,omitempty"`
	ExitCode        *int   `json:"exitCode"`
	Signal          string `json:"signal,omitempty"`
	OOMKilled       bool   `json:"oomKilled,omitempty"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool   `json:"stderrTruncated,omitempty"`
	TimedOut        bool   `json:"timedOut,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	StartedAt       string `json:"startedAt,omitempty"`
	FinishedAt      string `json:"finishedAt,omitempty"`
}

// CommandFrame is one NDJSON frame of a streamed command.
type CommandFrame struct {
	Signal    string `json:"signal,omitempty"`
	Type      string `json:"type"` // started, stdout, stderr, exit, error
	CommandID string `json:"commandId,omitempty"`
	Data      string `json:"data,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Success   bool   `json:"success,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

type CommandStarted struct {
	CommandID  string `json:"commandId,omitempty"`
	ProcessID  int    `json:"processId"`
	PID        int    `json:"pid,omitempty"`
	Command    string `json:"command,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	LogPath    string `json:"logPath,omitempty"`
	ErrLogPath string `json:"errLogPath,omitempty"`
}

type CommandStatus struct {
	ProcessID        int    `json:"processId"`
	PID              int    `json:"pid,omitempty"`
	Status           string `json:"status"` // running, exited, lost
	Known            bool   `json:"known"`
	Running          bool   `json:"running"`
	ExitCode         *int   `json:"exitCode"`
	Signal           string `json:"signal,omitempty"`
	OOMKilled        bool   `json:"oomKilled,omitempty"`
	Command          string `json:"command,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	StartedAt        string `json:"startedAt,omitempty"`
	FinishedAt       string `json:"finishedAt,omitempty"`
	Stdout           string `json:"stdout"`
	Stderr           string `json:"stderr"`
	StdoutTruncated  bool   `json:"stdoutTruncated,omitempty"`
	StderrTruncated  bool   `json:"stderrTruncated,omitempty"`
	LogPath          string `json:"logPath,omitempty"`
	ErrLogPath       string `json:"errLogPath,omitempty"`
	StdoutNextOffset int64  `json:"stdoutNextOffset,omitempty"`
	StderrNextOffset int64  `json:"stderrNextOffset,omitempty"`
	More             bool   `json:"more,omitempty"`
}

type CommandStatusOptions struct {
	TailBytes    int
	StdoutOffset int64
	StderrOffset int64
}

// AttachFrame is one NDJSON frame of GET /commands/{pid}/stream.
type AttachFrame struct {
	Type         string `json:"type"` // attached, stdout, stderr, heartbeat, exit, reconnect, error
	ProcessID    int    `json:"processId,omitempty"`
	Data         string `json:"data,omitempty"`
	NextOffset   int64  `json:"nextOffset,omitempty"`
	StdoutOffset int64  `json:"stdoutOffset,omitempty"`
	StderrOffset int64  `json:"stderrOffset,omitempty"`
	Status       string `json:"status,omitempty"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Signal       string `json:"signal,omitempty"`
	Error        string `json:"error,omitempty"`
	Message      string `json:"message,omitempty"`
	// OutputTruncated (exit): the command wrote more than the sandbox keeps
	// per stream; the rest was dropped.
	OutputTruncated bool `json:"outputTruncated,omitempty"`
}

type CommandStopResult struct {
	ProcessID int            `json:"processId"`
	Running   bool           `json:"running"`
	Signaled  bool           `json:"signaled"`
	Command   *CommandRecord `json:"command,omitempty"`
}

type CommandRecord struct {
	CommandID       string `json:"commandId"`
	SandboxID       string `json:"sandboxId"`
	Command         string `json:"command"`
	Cwd             string `json:"cwd,omitempty"`
	Mode            string `json:"mode"`
	ProcessID       *int   `json:"processId,omitempty"`
	Status          string `json:"status"`
	ExitCode        *int   `json:"exitCode"`
	Signal          string `json:"signal,omitempty"`
	StdoutBytes     int64  `json:"stdoutBytes"`
	StderrBytes     int64  `json:"stderrBytes"`
	OutputTruncated bool   `json:"outputTruncated"`
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt,omitempty"`
}

type CommandHistory struct {
	Commands   []CommandRecord `json:"commands"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

type CommandLogs struct {
	CommandID  string `json:"commandId"`
	Stream     string `json:"stream"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	Size       int64  `json:"size"`
	Encoding   string `json:"encoding"`
	Data       string `json:"data"`
	Done       bool   `json:"done"`
}

type Process struct {
	PID        int     `json:"pid"`
	ParentPID  int     `json:"parentPid,omitempty"`
	User       string  `json:"user,omitempty"`
	CPUPercent float64 `json:"cpuPercent"`
	MemoryMB   float64 `json:"memoryMB"`
	StartedAt  string  `json:"startedAt,omitempty"`
	Command    string  `json:"command"`
	CommandID  string  `json:"commandId,omitempty"`
}

type ProcessList struct {
	Live      bool      `json:"live"`
	SampledAt string    `json:"sampledAt,omitempty"`
	Total     int       `json:"total"`
	Processes []Process `json:"processes"`
}

// --- files ---

type FileEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	IsDir      bool   `json:"isDir"`
	IsSymlink  bool   `json:"isSymlink"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	ModifiedAt string `json:"modifiedAt"`
}

type FileContent struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
	Content  string `json:"content"`
}

type FileList struct {
	Path      string      `json:"path"`
	Files     []FileEntry `json:"files"`
	Truncated bool        `json:"truncated"`
}

type FileSearchResult struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
	Errors    []string `json:"errors,omitempty"`
}

type FileMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

type FileFindResult struct {
	Matches   []FileMatch `json:"matches"`
	Truncated bool        `json:"truncated"`
	Errors    []string    `json:"errors,omitempty"`
}

type FileReplaceResult struct {
	Results []struct {
		File         string `json:"file"`
		Success      bool   `json:"success"`
		Replacements int    `json:"replacements"`
		Error        string `json:"error,omitempty"`
	} `json:"results"`
}

type FileURL struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// --- access ---

type HostPortResult struct {
	Port        int    `json:"port"`
	URL         string `json:"url"`
	IsProtected bool   `json:"isProtected"`
	Access      string `json:"access"`
	SandboxID   string `json:"sandboxId,omitempty"`
	Localhost   bool   `json:"localhost"`
	Title       string `json:"title,omitempty"`
	// CrowdVM extensions
	HostURL        string        `json:"hostUrl,omitempty"`
	PathURL        string        `json:"pathUrl,omitempty"`
	Hidden         bool          `json:"hidden,omitempty"`
	Firewall       *HostFirewall `json:"firewall,omitempty"`
	DirectEndpoint string        `json:"directEndpoint,omitempty"`
}

type HostFirewall struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

type DesktopRequest struct {
	// bugs-r17-opus 8: a pointer, so false ("make it private") is sent too.
	PublicAccess *bool `json:"publicAccess,omitempty"`
}

type DesktopResult struct {
	DesktopURL   string `json:"desktopUrl"`
	PublicAccess bool   `json:"publicAccess,omitempty"` // bugs-r17-opus 8
	IP           string `json:"ip,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Provisioning bool   `json:"provisioning,omitempty"`
	Message      string `json:"message,omitempty"`
}

type SSHKeyResult struct {
	MachineIP   string `json:"machineIp,omitempty"`
	SSHUser     string `json:"sshUser"`
	SSHEndpoint string `json:"sshEndpoint,omitempty"`
	HostKey     string `json:"hostKey,omitempty"`
}

// --- agents ---

type PromptRequest struct {
	Provider        string `json:"provider,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	Fast            *bool  `json:"fast,omitempty"`
	New             bool   `json:"new,omitempty"`
	ConversationID  string `json:"conversationId,omitempty"`
	Prompt          string `json:"prompt"`
	// Attachments: files already in the sandbox (~/attachments/...); harnesses
	// with native attachments (images for vision models) get them directly.
	Attachments []string `json:"attachments,omitempty"`
	// Credentials: env vars for this turn's harness process only (e.g.
	// CLAUDE_CODE_OAUTH_TOKEN, CHATGPT_ACCESS_TOKEN + CHATGPT_ACCOUNT_ID,
	// ANTHROPIC_API_KEY). The guest keeps them in memory for the turn; they
	// win over the sandbox's own credentials. Set by `crowdvm prompt --local-login`.
	Credentials map[string]string `json:"credentials,omitempty"`
}

type PromptRun struct {
	ID             string `json:"id"`
	PromptID       string `json:"promptId"`
	SandboxID      string `json:"sandboxId"`
	Status         string `json:"status"`
	Done           bool   `json:"done"`
	CreatedAt      string `json:"createdAt,omitempty"`
	Model          string `json:"model,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
}

type PromptResult struct {
	ID              string     `json:"id"`
	PromptID        string     `json:"promptId"`
	ConversationID  string     `json:"conversationId,omitempty"`
	PromptRun       *PromptRun `json:"promptRun,omitempty"`
	Status          string     `json:"status"`
	Provider        string     `json:"provider,omitempty"`
	Model           string     `json:"model,omitempty"`
	ReasoningEffort string     `json:"reasoningEffort,omitempty"`
}

type Event struct {
	ID             string `json:"id,omitempty"`
	Type           string `json:"type"`
	Timestamp      int64  `json:"timestamp,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	Data           Object `json:"data,omitempty"`
}

type EventsOptions struct {
	Limit         int
	Cursor        string
	Sort          string // asc or desc
	Type          string
	Conversations []string
}

type EventList struct {
	Events   []Event  `json:"events"`
	PageInfo PageInfo `json:"pageInfo"`
}

type Conversation struct {
	ID                string `json:"id"`
	CreatedAt         string `json:"createdAt,omitempty"`
	LastPromptAt      string `json:"lastPromptAt,omitempty"`
	Prompts           int    `json:"prompts"`
	Running           bool   `json:"running"`
	LastHarness       string `json:"lastHarness,omitempty"`
	LastModel         string `json:"lastModel,omitempty"`
	LastPromptPreview string `json:"lastPromptPreview,omitempty"`
	Current           bool   `json:"current"`
}

type SteerResult struct {
	ID             string `json:"id,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	PromptID       string `json:"promptId,omitempty"`
	Native         bool   `json:"native"`
	Mode           string `json:"mode,omitempty"`
	Status         string `json:"status,omitempty"`
}

// --- snapshots ---

type Snapshot struct {
	ID               string `json:"id"`
	SandboxID        string `json:"sandboxId"`
	Status           string `json:"status"`
	Kind             string `json:"kind,omitempty"`
	Generation       *int   `json:"generation,omitempty"`
	ChainID          string `json:"chainId,omitempty"`
	CreatedAt        string `json:"createdAt"`
	CompletedAt      string `json:"completedAt,omitempty"`
	SizeBytes        *int64 `json:"sizeBytes,omitempty"`
	FileCount        *int64 `json:"fileCount,omitempty"`
	ContentSizeBytes *int64 `json:"contentSizeBytes,omitempty"`
	Origin           string `json:"origin,omitempty"`
}

type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
	PageInfo  PageInfo   `json:"pageInfo"`
}

type SavedSnapshot struct {
	Name            string `json:"name"`
	Org             string `json:"org,omitempty"` // organization of a team image
	Status          string `json:"status"`        // saving, ready, failed
	Error           string `json:"error,omitempty"`
	SourceSandboxID string `json:"sourceSandboxId"`
	SnapshotID      string `json:"snapshotId,omitempty"`
	Type            string `json:"type,omitempty"`
	SizeBytes       int64  `json:"sizeBytes,omitempty"`
	CreatedAt       string `json:"createdAt"`
}

type SavedSnapshotList struct {
	Snapshots []SavedSnapshot `json:"snapshots"`
	Allowance Object          `json:"allowance,omitempty"`
}

type SnapshotShare struct {
	ID         string `json:"id"`
	CodePrefix string `json:"codePrefix"`
	Status     string `json:"status"`
	UseCount   int    `json:"useCount"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
	RevokedAt  string `json:"revokedAt,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

type SnapshotShareCreated struct {
	Name      string        `json:"name"`
	ShareCode string        `json:"shareCode"`
	Share     SnapshotShare `json:"share"`
}

type SnapshotTreeEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type SnapshotTree struct {
	SnapshotID     string              `json:"snapshotId"`
	SandboxID      string              `json:"sandboxId"`
	Generation     int                 `json:"generation"`
	TreeAvailable  bool                `json:"treeAvailable"`
	Truncated      bool                `json:"truncated"`
	FileCount      int64               `json:"fileCount"`
	TotalSizeBytes int64               `json:"totalSizeBytes"`
	Entries        []SnapshotTreeEntry `json:"entries"`
	Reason         string              `json:"reason,omitempty"`
}

type DeletionOperation struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	TargetID     string `json:"targetId"`
	Reason       string `json:"reason"`
	Status       string `json:"status"` // pending, processing, blocked, completed
	Stage        string `json:"stage,omitempty"`
	ExpectedBy   string `json:"expectedBy,omitempty"`
	AttemptCount int    `json:"attemptCount"`
	RequestedAt  string `json:"requestedAt"`
	CompletedAt  string `json:"completedAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
}

// --- account ---

type User struct {
	ID                string   `json:"id"`
	Login             string   `json:"login"`
	Email             string   `json:"email,omitempty"`
	DisplayName       string   `json:"displayName,omitempty"`
	ConnectionMethod  string   `json:"connectionMethod,omitempty"`
	ConnectionMethods []string `json:"connectionMethods,omitempty"`
	AccountPlan       string   `json:"accountPlan,omitempty"`
	ServiceAccount    bool     `json:"serviceAccount,omitempty"`
	AccountStatus     string   `json:"accountStatus,omitempty"`
	ZeroDataRetention bool     `json:"zeroDataRetention,omitempty"`
	Admin             bool     `json:"admin,omitempty"`
}

type Org struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Type               string `json:"type"`
	Role               string `json:"role"`
	MemberCount        int    `json:"memberCount"`
	SubscriptionStatus string `json:"subscriptionStatus,omitempty"`
	Active             bool   `json:"active"`
	// NewSandboxesPrivate is the org's default visibility for new sandboxes (orgs only).
	NewSandboxesPrivate bool `json:"newSandboxesPrivate,omitempty"`
}

type OrgList struct {
	ActiveWalletChosen bool  `json:"activeWalletChosen"`
	Orgs               []Org `json:"orgs"`
}

type SandboxUsage struct {
	SandboxID         string  `json:"sandboxId"`
	SandboxType       string  `json:"sandboxType"`
	BillingMultiplier float64 `json:"billingMultiplier"`
	Since             string  `json:"since"`
	Until             string  `json:"until"`
	Seconds           int64   `json:"seconds"`
	Dollars           float64 `json:"dollars"`
	SecondsPerDollar  int64   `json:"secondsPerDollar,omitempty"`
	Running           bool    `json:"running"`
	// Credits is a CrowdVM extension (credits spent in the window).
	Credits *float64 `json:"credits,omitempty"`
}

type DataRetention struct {
	Enabled       bool   `json:"enabled"`
	EnabledAt     string `json:"enabledAt,omitempty"`
	SnapshotsOff  bool   `json:"snapshotsOff"`
	MonitoringOff bool   `json:"monitoringOff"`
}

// DataRetentionConfirmation is the phrase the API requires to turn zero data retention on.
const DataRetentionConfirmation = "delete archived sandbox data"

type DataRetentionUpdate struct {
	Enabled       *bool  `json:"enabled,omitempty"`
	SnapshotsOff  *bool  `json:"snapshotsOff,omitempty"`
	MonitoringOff *bool  `json:"monitoringOff,omitempty"`
	Confirmation  string `json:"confirmation,omitempty"`
}

type APIKey struct {
	Preset         string `json:"preset,omitempty"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	KeyPrefix      string `json:"keyPrefix"`
	KeyLastFour    string `json:"keyLastFour"`
	SandboxID      string `json:"sandboxId,omitempty"`
	Org            string `json:"org,omitempty"` // organization of an org key
	CreatedAt      string `json:"createdAt"`
	LastUsedAt     string `json:"lastUsedAt,omitempty"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	Expired        bool   `json:"expired,omitempty"`
	CredentialLane string `json:"credentialLane,omitempty"`
	Usage          struct {
		Requests   int `json:"requests"`
		WindowDays int `json:"windowDays"`
	} `json:"usage"`
	Resources struct {
		Total     int `json:"total"`
		Sandboxes int `json:"sandboxes"`
		Agents    int `json:"agents"`
	} `json:"resources"`
	Scope *struct {
		ExpiresAt    string   `json:"expiresAt,omitempty"`
		Actions      []string `json:"actions,omitempty"`
		Sandboxes    any      `json:"sandboxes,omitempty"`
		Environments any      `json:"environments,omitempty"`
	} `json:"scope,omitempty"`
	CreatedResources []Object `json:"createdResources,omitempty"`
}

type APIKeyList struct {
	APIKeys []APIKey `json:"apiKeys"`
	Catalog Object   `json:"catalog,omitempty"`
}

type CreateAPIKeyRequest struct {
	Name           string   `json:"name,omitempty"`
	TTL            string   `json:"ttl,omitempty"`
	Preset         string   `json:"preset,omitempty"`
	Actions        []string `json:"actions,omitempty"`
	SandboxIDs     []string `json:"sandboxIds,omitempty"`
	EnvironmentIDs []string `json:"environmentIds,omitempty"`
}

type APIKeySecret struct {
	APIKey APIKey `json:"apiKey"`
	Secret string `json:"secret"`
}

type Webhook struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
}

type WebhookRequest struct {
	Name   string   `json:"name,omitempty"`
	URL    string   `json:"url,omitempty"`
	Events []string `json:"events,omitempty"`
}

type WebhookResult struct {
	Webhook Webhook `json:"webhook"`
	Secret  string  `json:"secret,omitempty"`
}

type Telemetry struct {
	Endpoint     string   `json:"endpoint"`
	Headers      []string `json:"headers"`
	LastExportAt string   `json:"lastExportAt,omitempty"`
	LastError    Object   `json:"lastError,omitempty"`
	CreatedAt    string   `json:"createdAt"`
}

type TelemetryResult struct {
	Telemetry *Telemetry `json:"telemetry"`
	Test      Object     `json:"test,omitempty"`
}

// --- environments ---

type SecretFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

type ResourceAccess struct {
	Restricted bool     `json:"restricted"`
	Members    []string `json:"members,omitempty"`
}

type Environment struct {
	Owner                  Object          `json:"owner,omitempty"`
	ID                     string          `json:"id"`
	Name                   string          `json:"name"`
	IsDefault              bool            `json:"isDefault"`
	LatestVersionID        string          `json:"latestVersionId,omitempty"`
	LatestVersionNumber    int             `json:"latestVersionNumber,omitempty"`
	SafeForThirdParties    bool            `json:"safeForThirdParties"`
	PassGithub             bool            `json:"passGithub"`
	PassSecrets            bool            `json:"passSecrets"`
	PassSandboxCredentials bool            `json:"passSandboxCredentials"`
	PassAgentsCredentials  bool            `json:"passAgentsCredentials"`
	CredentialProfileID    string          `json:"credentialProfileId,omitempty"`
	ProxyAICredentials     bool            `json:"proxyAiCredentials,omitempty"`
	Access                 *ResourceAccess `json:"access,omitempty"`
	EnvContents            string          `json:"envContents"`
	SecretFiles            []SecretFile    `json:"secretFiles"`
	SelectedRepositories   []Object        `json:"selectedRepositories,omitempty"`
	Effective              Object          `json:"effective,omitempty"` // what a sandbox actually gets
	Warning                string          `json:"warning,omitempty"`
	Versions               []struct {
		ID            string `json:"id"`
		VersionNumber int    `json:"versionNumber"`
		SandboxCount  int    `json:"sandboxCount"`
		CreatedAt     string `json:"createdAt"`
	} `json:"versions"`
}

type UpdateEnvironmentRequest struct {
	Name                   string          `json:"name,omitempty"`
	IsDefault              *bool           `json:"isDefault,omitempty"`
	SafeForThirdParties    *bool           `json:"safeForThirdParties,omitempty"`
	PassGithub             *bool           `json:"passGithub,omitempty"`
	PassSecrets            *bool           `json:"passSecrets,omitempty"`
	PassSandboxCredentials *bool           `json:"passSandboxCredentials,omitempty"`
	PassAgentsCredentials  *bool           `json:"passAgentsCredentials,omitempty"`
	CredentialProfileID    *string         `json:"credentialProfileId,omitempty"`
	ProxyAICredentials     *bool           `json:"proxyAiCredentials,omitempty"`
	Access                 *ResourceAccess `json:"access,omitempty"`
	EnvContents            *string         `json:"envContents,omitempty"`
	SecretFiles            []SecretFile    `json:"secretFiles,omitempty"`
}

type EnvironmentChange struct {
	Success       bool   `json:"success"`
	VersionID     string `json:"versionId,omitempty"`
	VersionNumber int    `json:"versionNumber,omitempty"`
}

type EnvironmentUpgrade struct {
	Success         bool   `json:"success"`
	Upgraded        int    `json:"upgraded"`
	UpgradedLive    int    `json:"upgradedLive"`
	UpgradedStopped int    `json:"upgradedStopped"`
	Failed          int    `json:"failed"`
	LatestVersionID string `json:"latestVersionId,omitempty"`
}

// --- AI credentials ---

type CredentialProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	Owner     struct {
		ID             string `json:"id"`
		Kind           string `json:"kind"`
		Name           string `json:"name"`
		AIProxyEnabled bool   `json:"aiProxyEnabled"`
	} `json:"owner"`
	CanManage  bool            `json:"canManage"`
	Connected  []string        `json:"connected"`
	Accounts   Object          `json:"accounts,omitempty"`
	CustomAPIs []string        `json:"customApis"`
	CreatedAt  string          `json:"createdAt"`
	UpdatedAt  string          `json:"updatedAt"`
	Access     *ResourceAccess `json:"access,omitempty"`
}

type CreateCredentialProfileRequest struct {
	Name     string `json:"name"`
	Org      string `json:"org,omitempty"`
	CopyFrom string `json:"copyFrom,omitempty"`
}

type UpdateCredentialProfileRequest struct {
	Name      string          `json:"name,omitempty"`
	IsDefault *bool           `json:"isDefault,omitempty"`
	Access    *ResourceAccess `json:"access,omitempty"`
}

type CredentialAccount struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Active  bool   `json:"active"`
	AddedAt string `json:"addedAt,omitempty"`
}

type CredentialAccounts struct {
	ProfileID  string              `json:"profileId"`
	Credential string              `json:"credential"`
	Accounts   []CredentialAccount `json:"accounts"`
}

type AIProxy struct {
	AccountID string `json:"accountId,omitempty"`
	Org       bool   `json:"org"`
	Enabled   bool   `json:"enabled"`
	EnabledAt string `json:"enabledAt,omitempty"`
	Pricing   Object `json:"pricing,omitempty"`
	ProxyURL  string `json:"proxyUrl,omitempty"`
}
