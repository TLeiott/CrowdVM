package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Sandbox is the server's record of one sandbox.
type Sandbox struct {
	ID, Name, OwnerID, OrgID, State, Error, Size string
	TTLSeconds                                   *int
	ArchiveAfter                                 *time.Time
	CreatedAt, UpdatedAt                         time.Time
	StartedAt, StoppedAt                         *time.Time
	NodeID, ImageID, HeadSnapshotID              string
	Labels, Env                                  map[string]string
	Nested                                       bool
	Placement                                    Placement
	SetupScript                                  string
	Files                                        []proto.File
	FirstBootDone                                bool
	SetupStatus, SetupError                      string
	Visibility                                   string // "", "shared", "private"
	PrivateByCreator, HoldsCreatorLogins         bool
	WipePending, NoEnv                           bool
	SnapshotsEnabled, Monitoring                 bool
	Environment, EnvironmentID                   string
	EnvironmentVersion                           *int
	CreatedByKey, GuestToken                     string
	SourceKind, SourceID, BootKind               string
	AgentState                                   string // "", "idle", "running"
	Health, HealthReason                         string
	DegradedSince                                *time.Time
	Hydrated                                     bool
	HydratedAt                                   *time.Time
	LastSnapshotAt, SnapshotCompletedAt          *time.Time
	LastSnapshotStatus                           string
	LastRecoverAt, LastRebootAt                  *time.Time
	LastRebootReason, StopID, Subdomain          string
	Disk                                         string // "luks" | "plain" | "" (guest's report at boot)
	Direct                                       map[string]string
	Extra                                        map[string]any // feature data (JSON column "extra")
	DeletedAt                                    *time.Time
}

// Wallet is the id of the wallet the sandbox bills.
func (sb *Sandbox) Wallet() string {
	if sb.OrgID != "" {
		return sb.OrgID
	}
	return sb.OwnerID
}

// Active reports a state with a machine (or one being made).
func (sb *Sandbox) Active() bool { return slices.Contains(activeStates, sb.State) }

// Usable reports a state that accepts commands.
func (sb *Sandbox) Usable() bool {
	return sb.State == api.StateReady || sb.State == api.StateIdle || sb.State == api.StateRunning
}

func (sb *Sandbox) starting() bool {
	return sb.State == api.StateProvisioning || sb.State == api.StateProvisioned || sb.State == api.StateCloning
}

const sbCols = `id,name,owner_id,COALESCE(org_id,''),state,COALESCE(error,''),size,ttl_seconds,archive_after,created_at,updated_at,started_at,stopped_at,
COALESCE(node_id,''),image_id,COALESCE(head_snapshot_id,''),labels,env,nested,placement,setup_script,files,first_boot_done,
COALESCE(setup_status,''),COALESCE(setup_error,''),COALESCE(visibility,''),private_by_creator,holds_creator_logins,wipe_pending,no_env,
snapshots_enabled,monitoring,COALESCE(environment,''),COALESCE(environment_id,''),environment_version,COALESCE(created_by_key,''),guest_token,
source_kind,source_id,boot_kind,agent_state,health,COALESCE(health_reason,''),degraded_since,hydrated,hydrated_at,
last_snapshot_at,snapshot_completed_at,COALESCE(last_snapshot_status,''),last_recover_at,last_reboot_at,COALESCE(last_reboot_reason,''),
COALESCE(stop_id,''),COALESCE(subdomain,''),direct,extra,deleted_at,disk`

func scanSandbox(sc interface{ Scan(...any) error }) (*Sandbox, error) {
	sb := &Sandbox{}
	var ttl, envVer sql.NullInt64
	var archive, started, stopped, degraded, hydratedAt, lastSnap, snapDone, lastRec, lastBoot, deleted sql.NullInt64
	var created, updated int64
	var labels, env, placement, files, direct, extra string
	err := sc.Scan(&sb.ID, &sb.Name, &sb.OwnerID, &sb.OrgID, &sb.State, &sb.Error, &sb.Size, &ttl, &archive, &created, &updated, &started, &stopped,
		&sb.NodeID, &sb.ImageID, &sb.HeadSnapshotID, &labels, &env, &sb.Nested, &placement, &sb.SetupScript, &files, &sb.FirstBootDone,
		&sb.SetupStatus, &sb.SetupError, &sb.Visibility, &sb.PrivateByCreator, &sb.HoldsCreatorLogins, &sb.WipePending, &sb.NoEnv,
		&sb.SnapshotsEnabled, &sb.Monitoring, &sb.Environment, &sb.EnvironmentID, &envVer, &sb.CreatedByKey, &sb.GuestToken,
		&sb.SourceKind, &sb.SourceID, &sb.BootKind, &sb.AgentState, &sb.Health, &sb.HealthReason, &degraded, &sb.Hydrated, &hydratedAt,
		&lastSnap, &snapDone, &sb.LastSnapshotStatus, &lastRec, &lastBoot, &sb.LastRebootReason,
		&sb.StopID, &sb.Subdomain, &direct, &extra, &deleted, &sb.Disk)
	if err != nil {
		return nil, err
	}
	if ttl.Valid {
		v := int(ttl.Int64)
		sb.TTLSeconds = &v
	}
	if envVer.Valid {
		v := int(envVer.Int64)
		sb.EnvironmentVersion = &v
	}
	sb.CreatedAt, sb.UpdatedAt = store.Time(created), store.Time(updated)
	sb.ArchiveAfter, sb.StartedAt, sb.StoppedAt = store.NullTime(archive), store.NullTime(started), store.NullTime(stopped)
	sb.DegradedSince, sb.HydratedAt = store.NullTime(degraded), store.NullTime(hydratedAt)
	sb.LastSnapshotAt, sb.SnapshotCompletedAt = store.NullTime(lastSnap), store.NullTime(snapDone)
	sb.LastRecoverAt, sb.LastRebootAt, sb.DeletedAt = store.NullTime(lastRec), store.NullTime(lastBoot), store.NullTime(deleted)
	sb.Labels, sb.Env, sb.Direct, sb.Extra = map[string]string{}, map[string]string{}, map[string]string{}, map[string]any{}
	_ = json.Unmarshal([]byte(labels), &sb.Labels)
	_ = json.Unmarshal([]byte(env), &sb.Env)
	_ = json.Unmarshal([]byte(placement), &sb.Placement)
	_ = json.Unmarshal([]byte(files), &sb.Files)
	_ = json.Unmarshal([]byte(direct), &sb.Direct)
	_ = json.Unmarshal([]byte(extra), &sb.Extra)
	return sb, nil
}

// SandboxByID loads a sandbox (including deleted ones; check DeletedAt).
func (s *Server) SandboxByID(id string) (*Sandbox, error) {
	return scanSandbox(s.DB.QueryRow(`SELECT `+sbCols+` FROM sandboxes WHERE id=?`, id))
}

// SetSandboxExtra stores feature data under key in the sandbox's extra JSON.
func (s *Server) SetSandboxExtra(id, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE sandboxes SET extra=json_set(extra, '$.'||?, json(?)) WHERE id=?`, key, string(b), id)
	return err
}

// ---------------------------------------------------------------------------
// Access
// ---------------------------------------------------------------------------

// Access levels.
const (
	AccessOwner = "owner"
	AccessUse   = "use"
	AccessView  = "view"
)

// accessOf returns the viewer's access ("" = none).
func (s *Server) accessOf(p *Principal, sb *Sandbox) string {
	if sb.OwnerID == p.User.ID {
		return AccessOwner
	}
	if sb.OrgID == "" || s.memberRole(sb.OrgID, p.User.ID) == "" {
		return ""
	}
	if sb.Visibility == "shared" && !sb.HoldsCreatorLogins {
		return AccessUse
	}
	return AccessView
}

// viewActions are allowed on a teammate's private sandbox.
var viewActions = []string{"sandbox.read", "sandbox.stop"}

// ownerOnly marks operations that stay with the creator.
type ownerOnly bool

// Sandbox loads {sandboxId} and checks access for action. When owner is true
// the operation is creator-only (fork, delete, settings, share).
func (c *Ctx) Sandbox(action string, owner ownerOnly) (*Sandbox, error) {
	return c.SandboxByID(c.Param("sandboxId"), action, owner)
}

// SandboxByID is Sandbox for an explicit id.
func (c *Ctx) SandboxByID(id, action string, owner ownerOnly) (*Sandbox, error) {
	sb, err := c.S.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil {
		return nil, errNotFound("Sandbox")
	}
	if err := c.S.authorizeSandbox(c.P, sb, action, bool(owner)); err != nil {
		return nil, err
	}
	return sb, nil
}

func (s *Server) authorizeSandbox(p *Principal, sb *Sandbox, action string, owner bool) error {
	if p.Key != nil && p.Key.OrgID != "" && sb.OrgID != p.Key.OrgID {
		return errNotFound("Sandbox")
	}
	acc := s.accessOf(p, sb)
	if action == "sandbox.delete" {
		u, err := s.UserByID(sb.OwnerID)
		if err != nil && !store.IsNoRows(err) {
			return err
		}
		// A service account's sandboxes are the organization's: its owners
		// and admins delete them also when the account's key expired
		// (bugs-r5-opus 4).
		orphan := store.IsNoRows(err) || u.Disabled || (sb.OrgID != "" && s.memberRole(sb.OrgID, sb.OwnerID) == "") ||
			(sb.OrgID != "" && u.Kind == "service")
		role := s.memberRole(sb.OrgID, p.User.ID)
		if orphan && (p.IsAdmin() || role == "owner" || role == "admin") {
			acc = AccessOwner
		}
	}
	switch acc {
	case "":
		return errNotFound("Sandbox")
	case AccessView:
		if owner {
			return Err(403, "owner_only", "Only the sandbox's creator can do this.")
		}
		if !slices.Contains(viewActions, action) {
			return Err(403, "sandbox_private", "This sandbox is private to its creator.")
		}
		if action == "sandbox.stop" && sb.PrivateByCreator {
			if r := s.memberRole(sb.OrgID, p.User.ID); r != "owner" && r != "admin" {
				return Err(403, "sandbox_private", "Its creator made this sandbox private; only they or an org admin can stop it.")
			}
		}
	case AccessUse:
		if owner {
			return Err(403, "owner_only", "Only the sandbox's creator can do this.")
		}
	}
	if p.Key != nil && !p.Key.AllowsSandbox(sb, action) {
		if action == "sandbox.delete" && p.Key.hasAction("sandbox.delete-own") {
			return Err(403, "api_key_action_forbidden", "This key can only delete the sandboxes it created.")
		}
		return Err(403, "api_key_sandbox_forbidden", "The API key is not allowed on this sandbox.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

// SandboxJSON renders the Boat Sandbox object for viewer p (nil = owner view).
func (s *Server) SandboxJSON(sb *Sandbox, p *Principal) map[string]any {
	spec, _ := api.SizeByName(sb.Size)
	disk := diskSpec(sb, spec).DiskGiB // the disk it has (bugs-r17-opus 9)
	access := AccessOwner
	if p != nil {
		access = s.accessOf(p, sb)
	}
	var team any
	if sb.OrgID != "" {
		if o, err := s.OrgByID(sb.OrgID); err == nil {
			team = map[string]any{"id": o.ID, "name": o.Name}
		}
	}
	var createdBy any
	if u, err := s.UserByID(sb.OwnerID); err == nil {
		createdBy = u.DisplayName
	}
	var ttl, archive any
	if sb.TTLSeconds != nil {
		ttl = *sb.TTLSeconds
	}
	if sb.ArchiveAfter != nil {
		archive = ts(*sb.ArchiveAfter)
	}
	var vis any
	if sb.OrgID != "" && sb.Visibility != "" {
		vis = sb.Visibility
	}
	var ip, sshEP any
	if sb.Usable() && sb.NodeID != "" {
		if n, err := s.NodeByID(sb.NodeID); err == nil && n.Info.DirectHost != "" {
			ip = n.Info.DirectHost
		}
		if v := sb.Direct["ssh"]; v != "" {
			sshEP = v
		}
	}
	var lastReboot any
	if sb.LastRebootAt != nil {
		lastReboot = map[string]any{"at": ts(*sb.LastRebootAt), "reason": firstNonEmpty(sb.LastRebootReason, "unknown")}
	}
	var setupStatus, setupErr any
	if sb.SetupStatus != "" {
		setupStatus = sb.SetupStatus
	}
	if sb.SetupError != "" {
		setupErr = sb.SetupError
	}
	var stop any
	if sb.StopID != "" {
		if so, err := s.stopByID(sb.StopID); err == nil {
			stop = s.stopJSON(so, true)
		}
	}
	var stopped any
	if sb.State == api.StateArchived && sb.StoppedAt != nil {
		stopped = ts(*sb.StoppedAt)
	}
	var degraded, hydratedAt, snapDone, lastSnap any
	if sb.DegradedSince != nil {
		degraded = ts(*sb.DegradedSince)
	}
	if sb.HydratedAt != nil {
		hydratedAt = ts(*sb.HydratedAt)
	}
	// SNAP-002: completed = the head snapshot is stored on the server;
	// verified = its stored copy matches the node's checksum.
	stored, verified := s.headSnapshotState(sb) // snapshotAvailable: a stopped snapshots-off sandbox kept no disk
	var snapVerified any
	if stored != nil {
		snapDone = ts(*stored)
	}
	if verified != nil {
		snapVerified = ts(*verified)
	}
	if sb.LastSnapshotAt != nil {
		lastSnap = ts(*sb.LastSnapshotAt)
	}
	var envVer any
	if sb.EnvironmentVersion != nil {
		envVer = *sb.EnvironmentVersion
	}
	labels := sb.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	m := map[string]any{
		"id": sb.ID, "name": sb.Name, "state": sb.State, "error": nz(sb.Error), "dataExpiredAt": nil, "stoppedAt": stopped,
		"labels": labels, "health": firstNonEmpty(sb.Health, "ok"), "healthReason": nz(sb.HealthReason), "degradedSince": degraded,
		"hydrated": sb.Hydrated, "hydratedAt": hydratedAt, "hydration": nil,
		"type": spec.Name, "vcpu": spec.VCPU, "memoryGB": spec.MemoryMiB / 1024, "billingMultiplier": spec.Multiplier,
		"machineProvider": "baremetal", "url": nil, "ip": ip, "sshEndpoint": sshEP,
		"createdAt": ts(sb.CreatedAt), "updatedAt": ts(sb.UpdatedAt), "archiveAfter": archive, "ttlSeconds": ttl,
		"desktopAvailable": sb.Usable(), "desktopUrl": nil, "snapshots": sb.SnapshotsEnabled, "monitoring": sb.Monitoring,
		"snapshotAvailable": stored != nil && (sb.SnapshotsEnabled || sb.State != api.StateArchived), "snapshotCompletedAt": snapDone, "snapshotVerifiedAt": snapVerified,
		"team": team, "createdBy": createdBy, "createdById": sb.OwnerID, "access": access,
		"holdsCreatorLogins": sb.HoldsCreatorLogins, "visibility": vis, "wipePendingUntilRestart": sb.WipePending,
		"subdomain": nz(sb.Subdomain), "lastSnapshotAttemptAt": lastSnap, "lastSnapshotStatus": nz(sb.LastSnapshotStatus),
		"lastReboot": lastReboot, "setupStatus": setupStatus, "setupError": setupErr,
		"environment": nz(sb.Environment), "environmentVersion": envVer, "stop": stop,
		// CrowdVM extensions
		"nodeId": nz(sb.NodeID), "node": nz(sb.NodeID), "imageId": nz(sb.ImageID), "placement": sb.Placement, "agentState": nz(sb.AgentState),
		"noEnv": sb.NoEnv, "nested": sb.Nested, "storage": map[string]any{"diskGB": disk, "encrypted": sb.Disk == "luks"},
	}
	if v, ok := sb.Extra["snapshotDataExpiredAt"].(float64); ok && v > 0 {
		m["dataExpiredAt"] = tsMs(int64(v))
	}
	// Billing paused: stop_refused (a stop waits for a save that works) or
	// not_answering (the guest does not answer); null = metered as usual.
	m["billingPaused"] = nz(billingPause(sb))
	if sb.starting() && isSecuring(sb) {
		m["securing"] = true // removing a previous owner's credentials before ready
	}
	waitingJSON(sb, m)
	hydrationJSON(sb, m)
	for _, h := range s.Hooks.SandboxJSON {
		h(sb, m)
	}
	return m
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var (
	labelKeyRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	reservedEnv = []string{"CROWDVM_API_KEY", "CROWDVM_API_URL", "CROWDVM_SANDBOX_ID", "ASCII_TOKEN", "ASCII_API_URL", "AGENT_ID",
		"PRODUCT_MODE", "ENVIRONMENT_ID", "BOAT_ID", "SERVICE_PREVIEW_TOKEN", "BOAT_CLI_TOKEN"}
)

const maxTTL = 2592000

// validateTTL checks ttlSeconds and caps values above 30 days at 30 days
// (RUN-001, as Boat does).
func validateTTL(o *OptInt) error {
	if o.Set && !o.Null && o.Value < 1 {
		return Err(400, "invalid_ttl", "ttlSeconds must be at least 1, or null for no auto-stop.")
	}
	if o.Set && !o.Null && o.Value > maxTTL {
		o.Value = maxTTL
	}
	return nil
}

func ttlPtr(o OptInt, def *int) *int {
	if !o.Set {
		return def
	}
	if o.Null {
		return nil
	}
	v := o.Value
	return &v
}

func validateLabels(l map[string]string) error {
	if len(l) > 20 {
		return Err(400, "invalid_labels", "At most 20 labels.")
	}
	for k, v := range l {
		if !labelKeyRe.MatchString(k) || len(v) > 256 {
			return Err(400, "invalid_labels", "Label keys match [A-Za-z0-9][A-Za-z0-9._/-]{0,62} and values have at most 256 bytes: "+k)
		}
	}
	return nil
}

func validateEnv(e map[string]string) error {
	if len(e) > 100 {
		return Err(400, "invalid_env", "At most 100 environment variables.")
	}
	total := 0
	for k, v := range e {
		if !envKeyRe.MatchString(k) {
			return Err(400, "invalid_env", "Invalid environment variable name "+k+".")
		}
		if slices.Contains(reservedEnv, k) {
			return Err(400, "invalid_env", k+" is reserved.")
		}
		total += len(k) + len(v)
	}
	if total > 64<<10 {
		return Err(400, "invalid_env", "Environment variables exceed 64KB.")
	}
	return nil
}

type fileReq struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func validateFiles(fs []fileReq) ([]proto.File, error) {
	if len(fs) > 32 {
		return nil, Err(400, "invalid_files", "At most 32 files.")
	}
	var out []proto.File
	seen := map[string]bool{}
	total := 0
	for _, f := range fs {
		p := f.Path
		if p == "" {
			return nil, Err(400, "invalid_files", "Each file needs a path.")
		}
		if !strings.HasPrefix(p, "/") {
			p = path.Join(proto.GuestHome, p)
		}
		p = path.Clean(p)
		if !(strings.HasPrefix(p, proto.GuestHome+"/") || strings.HasPrefix(p, "/tmp/")) {
			return nil, Err(400, "invalid_files", "File paths must be under /home/user or /tmp: "+f.Path)
		}
		if seen[p] {
			return nil, Err(400, "invalid_files", "Duplicate file path "+f.Path)
		}
		seen[p] = true
		var b []byte
		switch f.Encoding {
		case "", "utf8", "utf-8":
			b = []byte(f.Content)
		case "base64":
			var err error
			if b, err = base64.StdEncoding.DecodeString(f.Content); err != nil {
				return nil, Err(400, "invalid_files", "Invalid base64 in "+f.Path)
			}
		default:
			return nil, Err(400, "invalid_files", "encoding must be utf8 or base64.")
		}
		total += len(b)
		out = append(out, proto.File{Path: p, Content: b})
	}
	if total > 256<<10 {
		return nil, Err(400, "invalid_files", "Files exceed 256KB in total. Upload bigger files with files/upload-url.")
	}
	return out, nil
}

func defaultName(t time.Time) string { return "Sandbox " + t.UTC().Format("2006-01-02 15:04") }

func validName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" {
		return "", Err(400, "invalid_name", "name must not be empty.")
	}
	if len(n) > 120 {
		n = n[:120]
		for !utf8.ValidString(n) { // cut on a rune boundary
			n = n[:len(n)-1]
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// List / get / update
// ---------------------------------------------------------------------------

func (s *Server) handleListSandboxes(c *Ctx) error {
	pg, err := ParsePage(c.R)
	if err != nil {
		return err
	}
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	q := `SELECT ` + sbCols + ` FROM sandboxes WHERE deleted_at IS NULL`
	var args []any
	switch {
	case c.P.Key != nil && c.P.Key.OrgID != "":
		q += ` AND org_id=?`
		args = append(args, c.P.Key.OrgID)
	case org != nil:
		q += ` AND (owner_id=? OR org_id=?)`
		args = append(args, c.P.User.ID, org.ID)
	default:
		q += ` AND owner_id=?`
		args = append(args, c.P.User.ID)
	}
	if v := c.Query("state"); v != "" {
		var ph []string
		for _, st := range strings.Split(v, ",") {
			st = strings.TrimSpace(st)
			if st == "stopped" {
				st = api.StateArchived
			}
			ph = append(ph, "?")
			args = append(args, st)
		}
		q += ` AND state IN (` + strings.Join(ph, ",") + `)`
	}
	if v := c.Query("namePrefix"); v != "" {
		q += ` AND substr(name,1,?)=?`
		args = append(args, utf8.RuneCountInString(v), v)
	}
	if v := c.Query("stoppedBefore"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return Err(400, "invalid_stopped_before", "stoppedBefore must be an ISO 8601 time.")
		}
		q += ` AND state='archived' AND stopped_at IS NOT NULL AND stopped_at<?`
		args = append(args, t.UnixMilli())
	}
	switch c.Query("dataExpired") {
	case "true":
		q += ` AND json_extract(extra,'$.snapshotDataExpired')=1`
	case "false":
		q += ` AND COALESCE(json_extract(extra,'$.snapshotDataExpired'),0)=0`
	}
	for _, l := range c.R.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return Err(400, "invalid_label", "label filters look like key=value.")
		}
		q += ` AND json_extract(labels, '$.' || json_quote(?)) = ?`
		args = append(args, k, v)
	}
	where, pargs, tail := pg.SQL("created_at", "id")
	if c.P.Key != nil {
		// The key's sandbox filter runs in Go: read on until the page is
		// full instead of cutting it at the SQL LIMIT (opus-4 B2).
		tail, _, _ = strings.Cut(tail, " LIMIT ")
	}
	rows, err := s.DB.Query(q+where+tail, append(args, pargs...)...)
	if err != nil {
		return err
	}
	var list []*Sandbox
	for len(list) <= pg.Limit && rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if c.P.Key != nil && !c.P.Key.AllowsSandbox(sb, "sandbox.read") {
			continue
		}
		list = append(list, sb)
	}
	rows.Close()
	list, info := PageResult(pg, list, func(sb *Sandbox) (int64, string) { return store.T(sb.CreatedAt), sb.ID })
	include := c.Query("include")
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]map[string]any, 0, len(list))
	for _, sb := range list {
		m := s.SandboxJSON(sb, c.P)
		if strings.Contains(include, "cost") {
			secs, _ := s.usageSeconds(sb.ID, month.UnixMilli(), now.UnixMilli())
			m["cost"] = map[string]any{"since": ts(month), "seconds": int(secs), "dollars": round6(secs / float64(s.Cfg.SecondsPerDollar)), "credits": round6(secs / 3600)}
		}
		out = append(out, m)
	}
	resp := map[string]any{"sandboxes": out, "pageInfo": pageJSON(info)}
	if strings.Contains(include, "deletedCost") && (c.P.Key == nil || !c.P.Key.Restricted()) {
		var secs float64
		rows, err := s.DB.Query(`SELECT r.multiplier,r.started_at,COALESCE(r.ended_at,?) FROM sandbox_runs r JOIN sandboxes s ON s.id=r.sandbox_id WHERE s.deleted_at IS NOT NULL AND s.owner_id=? AND COALESCE(r.ended_at,?)>?`,
			now.UnixMilli(), c.P.User.ID, now.UnixMilli(), month.UnixMilli())
		if err == nil {
			for rows.Next() {
				var m float64
				var st, en int64
				if rows.Scan(&m, &st, &en) == nil {
					secs += float64(en-max(st, month.UnixMilli())) / 1000 * m
				}
			}
			rows.Close()
		}
		resp["deletedCost"] = map[string]any{"since": ts(month), "seconds": int(secs), "dollars": round6(secs / float64(s.Cfg.SecondsPerDollar))}
	}
	return c.OK(200, "sandbox.list", resp)
}

func (s *Server) handleGetSandbox(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.read", false)
	if err != nil {
		return err
	}
	return c.OK(200, "sandbox.info", map[string]any{"sandbox": s.SandboxJSON(sb, c.P)})
}

func (s *Server) handleUpdateSandbox(c *Ctx) error {
	var req struct {
		Name       *string `json:"name"`
		TTLSeconds OptInt  `json:"ttlSeconds"`
		Subdomain  *string `json:"subdomain"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := c.Sandbox("sandbox.update", true)
	if err != nil {
		return err
	}
	if req.Name == nil && !req.TTLSeconds.Set && req.Subdomain == nil {
		return Err(400, "no_changes", "Pass name, ttlSeconds or subdomain.")
	}
	sets := map[string]any{}
	if req.Name != nil {
		n, err := validName(*req.Name)
		if err != nil {
			return err
		}
		sets["name"] = n
	}
	if req.TTLSeconds.Set {
		if err := validateTTL(&req.TTLSeconds); err != nil {
			return err
		}
		if req.TTLSeconds.Null {
			sets["ttl_seconds"], sets["archive_after"] = nil, nil
		} else {
			sets["ttl_seconds"] = req.TTLSeconds.Value
			if sb.Active() {
				sets["archive_after"] = store.Now() + int64(req.TTLSeconds.Value)*1000
			}
		}
	}
	if req.Subdomain != nil {
		if !sb.Usable() && sb.State != api.StateArchived {
			return ErrD(409, "sandbox_busy", "The sandbox is "+sb.State+"; change its subdomain while it runs or is stopped.", map[string]any{"retryable": true, "state": sb.State})
		}
		sd := *req.Subdomain
		if len(sd) < 3 || len(sd) > 40 || !subdomainRe.MatchString(sd) || strings.Contains(sd, "--") || strings.HasSuffix(sd, "-desktop") || regexp.MustCompile(`-\d+$`).MatchString(sd) {
			return Err(400, "invalid_subdomain", "subdomain must be 3-40 lowercase letters, digits and single hyphens, not ending in -desktop or -<number>.")
		}
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE subdomain=? AND id<>? AND deleted_at IS NULL`, sd, sb.ID).Scan(&n)
		if n > 0 {
			return Err(409, "subdomain_taken", "That subdomain is taken.")
		}
		sets["subdomain"] = sd
	}
	if err := s.updateSandbox(sb.ID, sets); err != nil {
		return err
	}
	sb, _ = s.SandboxByID(sb.ID)
	s.Bus.Publish(Event{Type: EvSandboxUpdated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, State: sb.State})
	return c.OK(200, "sandbox.info", map[string]any{"sandbox": s.SandboxJSON(sb, c.P)})
}

// updateSandbox sets columns (keys are trusted column names).
func (s *Server) updateSandbox(id string, sets map[string]any) error {
	if len(sets) == 0 {
		return nil
	}
	var parts []string
	var args []any
	for k, v := range sets {
		parts = append(parts, k+"=?")
		args = append(args, v)
	}
	parts = append(parts, "updated_at=?")
	args = append(args, store.Now(), id)
	_, err := s.DB.Exec(`UPDATE sandboxes SET `+strings.Join(parts, ",")+` WHERE id=?`, args...)
	return err
}

func (s *Server) handleSandboxUsage(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.read", false)
	if err != nil {
		return err
	}
	since, until := sb.CreatedAt, time.Now().UTC()
	if v := c.Query("since"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return Err(400, "invalid_since", "since must be ISO 8601 or unix seconds.")
		}
		since = t
	}
	if v := c.Query("until"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return Err(400, "invalid_until", "until must be ISO 8601 or unix seconds.")
		}
		if t.Before(until) {
			until = t
		}
	}
	secs, running := s.usageSeconds(sb.ID, since.UnixMilli(), until.UnixMilli())
	spec, _ := api.SizeByName(sb.Size)
	return c.OK(200, "sandbox.usage", map[string]any{
		"sandboxId": sb.ID, "sandboxType": spec.Name, "billingMultiplier": spec.Multiplier,
		"since": ts(since), "until": ts(until), "seconds": int(secs), "dollars": round6(secs / float64(s.Cfg.SecondsPerDollar)),
		"secondsPerDollar": s.Cfg.SecondsPerDollar, "running": running, "credits": round6(secs / 3600),
	})
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateSandboxRequest is POST /sandboxes (Boat fields + "placement").
type CreateSandboxRequest struct {
	Type        string            `json:"type"`
	TTLSeconds  OptInt            `json:"ttlSeconds"`
	Labels      map[string]string `json:"labels"`
	Env         map[string]string `json:"env"`
	Environment string            `json:"environment"`
	NoEnv       bool              `json:"noEnv"`
	Snapshots   OptBool           `json:"snapshots"`
	Monitoring  OptBool           `json:"monitoring"`
	FailFast    bool              `json:"failFast"`
	Count       int               `json:"count"`
	SetupScript *string           `json:"setupScript"`
	Name        string            `json:"name"`
	Visibility  string            `json:"visibility"`
	HostedPorts []json.RawMessage `json:"hostedPorts"`
	Files       []fileReq         `json:"files"`
	Org         string            `json:"org"`
	TeamID      string            `json:"teamId"`
	From        string            `json:"from"`
	Nested      bool              `json:"nested"`
	Placement   Placement         `json:"placement"`
}

func (s *Server) handleCreateSandbox(c *Ctx) error {
	raw, err := readBody(c)
	if err != nil {
		return err
	}
	var req CreateSandboxRequest
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return Err(400, "invalid_json", "Request body must be valid JSON: "+err.Error())
		}
	}
	orgRef := req.Org
	if orgRef == "" {
		orgRef = req.TeamID
	}
	org, err := c.Scope(orgRef)
	if err != nil {
		return err
	}
	u := c.P.User
	// Validation.
	if err := validateTTL(&req.TTLSeconds); err != nil {
		return err
	}
	if err := validateLabels(req.Labels); err != nil {
		return err
	}
	if err := validateEnv(req.Env); err != nil {
		return err
	}
	if err := req.Placement.validate(); err != nil {
		return err
	}
	files, err := validateFiles(req.Files)
	if err != nil {
		return err
	}
	if req.SetupScript != nil && len(*req.SetupScript) > 65536 {
		return Err(400, "invalid_setup_script", "setupScript can be at most 64KB.")
	}
	if req.Type != "" {
		if _, ok := api.SizeByName(req.Type); !ok {
			return Err(400, "invalid_type", "type must be small, default, large or xlarge.")
		}
	}
	if req.Visibility != "" && req.Visibility != "shared" && req.Visibility != "private" {
		return Err(400, "invalid_visibility", "visibility must be shared or private.")
	}
	if req.Visibility == "shared" && org == nil {
		return Err(400, "not_org_sandbox", "A personal sandbox is always private.")
	}
	if req.Count == 0 {
		req.Count = 1
	}
	if req.Count < 1 || req.Count > 1000 {
		return Err(400, "invalid_count", "count must be 1 to 1000.")
	}
	if req.Count > 1 && req.From == "" {
		return Err(400, "count_requires_from", "count needs from (a saved snapshot).")
	}
	if req.Count > 1 && req.FailFast {
		return Err(400, "count_with_fail_fast", "count and failFast cannot be combined.")
	}
	if len(req.HostedPorts) > 0 && req.Count > 1 {
		return Err(400, "count_with_hosted_ports", "count and hostedPorts cannot be combined.")
	}
	if len(req.HostedPorts) > 0 && req.FailFast {
		return Err(400, "fail_fast_with_hosted_ports", "failFast and hostedPorts cannot be combined.")
	}
	if len(req.HostedPorts) > 50 {
		return Err(400, "invalid_hosted_ports", "At most 50 hosted ports.")
	}
	if err := checkHostedPorts(req.HostedPorts); err != nil {
		return err
	}
	if req.Name != "" {
		if req.Name, err = validName(req.Name); err != nil {
			return err
		}
	}

	// Source: saved snapshot or share code.
	var src *sourceSnap
	if req.From != "" {
		if c.P.Key != nil && !c.P.Key.Allows("snapshot.read") {
			return Err(403, "api_key_action_forbidden", "Creating from a saved snapshot needs snapshot.read.")
		}
		src, err = s.resolveFrom(u, req.From, org)
		if err != nil {
			return err
		}
		// A restricted key deploys only saved snapshots of sandboxes it may
		// reach (a share code is its own grant).
		if src.Kind == "saved" && c.P.Key != nil && c.P.Key.Restricted() {
			if sb, err := s.SandboxByID(src.SourceSandboxID); err != nil || !c.P.Key.AllowsSandbox(sb, "snapshot.read") {
				return Err(404, "snapshot_not_found", "No saved snapshot or share code "+req.From+".")
			}
		}
	}
	size := firstNonEmpty(req.Type, "default")
	if req.Type == "" && src != nil && src.Size != "" {
		size = src.Size
	}
	spec, _ := api.SizeByName(size)
	env := req.Env
	noEnv := req.NoEnv
	if src != nil && env == nil && !src.Foreign {
		env = src.Env
	}
	if src != nil && src.NoEnv {
		noEnv = true
	}
	renv, err := s.resolveEnv(c, EnvRequest{User: u, Org: org, Name: req.Environment, NoEnv: noEnv, Op: "create"})
	if err != nil {
		return err
	}

	// Idempotency.
	idemKey := strings.TrimSpace(c.R.Header.Get("Idempotency-Key"))
	if len(idemKey) > 255 {
		return Err(400, "invalid_idempotency_key", "Idempotency-Key can be at most 255 characters.")
	}
	if idemKey != "" {
		done, err := s.idemBegin(c, idemKey, "POST /sandboxes", raw)
		if done || err != nil {
			return err
		}
	}
	fail := func(err error) error {
		if idemKey != "" {
			s.idemRelease(u.ID, idemKey)
		}
		return err
	}

	// Limits.
	unlock := s.lockLimits()
	defer unlock()
	if err := s.checkCredits(u, org); err != nil {
		return fail(err)
	}
	if err := s.checkConcurrent(u, org, req.Count); err != nil {
		return fail(err)
	}
	wallet := walletOf(u, org)
	if err := s.checkStarts(wallet, req.Count); err != nil {
		return fail(err)
	}

	visibility := ""
	privByCreator := false
	if org != nil {
		visibility = "shared"
		if req.Visibility == "private" || (req.Visibility == "" && org.NewSandboxesPrivate) {
			visibility, privByCreator = "private", true
		}
		if renv.HoldsLogins && visibility == "shared" {
			visibility = "private"
		}
	}
	ttl := 3600
	ttlp := ttlPtr(req.TTLSeconds, &ttl)
	now := time.Now().UTC()
	name := req.Name
	if name == "" {
		if src != nil {
			name = src.Name
		} else {
			name = defaultName(now)
		}
	}
	setup := ""
	if req.SetupScript != nil {
		setup = *req.SetupScript
	}
	image := s.defaultImage()
	parent := ""
	var chain []string
	if src != nil {
		parent = src.SnapshotID
		chain = s.snapshotChain(parent)
		if sn, err := s.SnapshotByID(parent); err == nil && sn.ImageID != "" {
			image = sn.ImageID
		}
	}
	snapsOn := !req.Snapshots.Set || req.Snapshots.Value
	monOn := !req.Monitoring.Set || req.Monitoring.Value
	pol := &SandboxPolicy{Op: "create", Owner: u, SnapshotsReq: req.Snapshots, MonitorReq: req.Monitoring, Snapshots: snapsOn, Monitoring: monOn}
	for _, fn := range s.Hooks.NewSandbox {
		if err := fn(c, pol); err != nil {
			return fail(err) // bugs-r11-sonnet 5
		}
	}
	snapsOn, monOn = pol.Snapshots, pol.Monitoring

	if req.Nested {
		req.Placement.Nested = true // the top-level field places like placement.nested
	}
	var created []*Sandbox
	waiting := map[string]bool{}
	for i := 0; i < req.Count; i++ {
		sb := &Sandbox{
			ID: api.NewID(api.PrefixSandbox, 8), Name: name, OwnerID: u.ID, OrgID: orgID(org), State: api.StateProvisioning,
			Size: spec.Name, TTLSeconds: ttlp, CreatedAt: now, UpdatedAt: now, ImageID: image, Labels: req.Labels, Env: env,
			Nested: req.Nested || req.Placement.Nested, Placement: req.Placement, SetupScript: setup, Files: files,
			Visibility: visibility, PrivateByCreator: privByCreator, HoldsCreatorLogins: renv.HoldsLogins, NoEnv: noEnv,
			SnapshotsEnabled: snapsOn, Monitoring: monOn, Environment: renv.Name, EnvironmentID: renv.ID, EnvironmentVersion: renv.Version,
			CreatedByKey: keyID(c.P), BootKind: string(proto.BootCreate),
		}
		if req.Count > 1 {
			sb.Name = fmt.Sprintf("%s %d", name, i+1)
		}
		if src != nil {
			sb.SourceKind, sb.SourceID = src.Kind, src.ID
		}
		if setup != "" || len(files) > 0 {
			sb.SetupStatus = "pending"
		}
		if ttlp != nil {
			t := now.Add(time.Duration(*ttlp) * time.Second)
			sb.ArchiveAfter = &t
		}
		if err := s.insertSandbox(sb); err != nil {
			s.rollbackCreated(created)
			return fail(err)
		}
		if src != nil && src.Foreign {
			s.addPendingScrub(sb.ID, s.foreignDeployScrub(src.OwnerID, src.SourceSandboxID))
		}
		sreq := schedReq{Size: spec, Placement: sb.Placement, Chain: chain, ImageID: image, Owners: s.ownersFor(sb.OwnerID, sb.OrgID), Encrypted: sb.Disk == "luks"}
		if _, err := s.placeSandbox(sb.ID, api.StateProvisioning, sreq); err != nil {
			if req.FailFast || !s.couldEverPlace(sreq) {
				_, _ = s.DB.Exec(`DELETE FROM sandboxes WHERE id=?`, sb.ID)
				s.rollbackCreated(created)
				if req.FailFast {
					err = ErrD(503, "no_ready_machine", "No machine of this type is ready right now.", map[string]any{"retryAfterSeconds": 5})
				} else {
					err = s.neverFitsErr(sreq, err)
				}
				return fail(err)
			}
			waiting[sb.ID] = true // SIZE-011: wait for capacity
		}
		created = append(created, sb)
	}
	for range created {
		s.recordStart(wallet, u.ID)
	}
	unlock()
	for _, sb := range created {
		if src != nil && src.ShareID != "" {
			_, _ = s.DB.Exec(`UPDATE snapshot_shares SET use_count=use_count+1, last_used_at=? WHERE id=?`, store.Now(), src.ShareID)
		}
		full, _ := s.SandboxByID(sb.ID)
		s.Bus.Publish(Event{Type: EvSandboxCreated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, NodeID: full.NodeID, State: sb.State})
		if waiting[sb.ID] {
			s.queueBoot(sb.ID, pendingBoot{Kind: proto.BootCreate, FirstBoot: true, Parent: parent, Reason: "capacity"})
			continue
		}
		s.startBoot(full, bootOpts{Kind: proto.BootCreate, FirstBoot: true, Parent: parent, Env: renv, FailFast: req.FailFast})
	}
	if idemKey != "" {
		s.idemFinish(u.ID, idemKey, created[0].ID)
	}
	first, _ := s.SandboxByID(created[0].ID)
	resp := map[string]any{"status": "provisioning", "ttlSeconds": ttlOut(first.TTLSeconds), "sandbox": s.SandboxJSON(first, c.P)}
	if req.Count > 1 {
		var all []map[string]any
		for _, sb := range created {
			full, _ := s.SandboxByID(sb.ID)
			all = append(all, s.SandboxJSON(full, c.P))
		}
		resp["sandboxes"] = all
	}
	if len(req.HostedPorts) > 0 {
		resp["hostedPorts"] = s.hostPorts(c, first, req.HostedPorts)
	}
	if idemKey != "" {
		s.idemResponse(u.ID, idemKey, resp)
	}
	return c.OK(202, "sandbox.created", resp)
}

func ttlOut(t *int) any {
	if t == nil {
		return nil
	}
	return *t
}

func orgID(o *Org) string {
	if o == nil {
		return ""
	}
	return o.ID
}

func (s *Server) rollbackCreated(list []*Sandbox) {
	for _, sb := range list {
		_, _ = s.DB.Exec(`DELETE FROM sandboxes WHERE id=?`, sb.ID)
	}
}

func readBody(c *Ctx) ([]byte, error) {
	b := make([]byte, 0, 1024)
	buf := make([]byte, 32<<10)
	for {
		n, err := c.R.Body.Read(buf)
		b = append(b, buf[:n]...)
		if len(b) > 8<<20 {
			return nil, Err(413, "body_too_large", "Request body is larger than 8MB.")
		}
		if err != nil {
			break
		}
	}
	return b, nil
}

func (s *Server) insertSandbox(sb *Sandbox) error {
	var org, ttl, archive, envVer any
	if sb.OrgID != "" {
		org = sb.OrgID
	}
	if sb.TTLSeconds != nil {
		ttl = *sb.TTLSeconds
	}
	if sb.ArchiveAfter != nil {
		archive = sb.ArchiveAfter.UnixMilli()
	}
	if sb.EnvironmentVersion != nil {
		envVer = *sb.EnvironmentVersion
	}
	labels := sb.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	env := sb.Env
	if env == nil {
		env = map[string]string{}
	}
	files := sb.Files
	if files == nil {
		files = []proto.File{}
	}
	_, err := s.DB.Exec(`INSERT INTO sandboxes(id,name,owner_id,org_id,state,size,ttl_seconds,archive_after,created_at,updated_at,image_id,labels,env,nested,placement,
		setup_script,files,setup_status,visibility,private_by_creator,holds_creator_logins,no_env,snapshots_enabled,monitoring,environment,environment_id,environment_version,
		created_by_key,source_kind,source_id,boot_kind,hydrated) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
		sb.ID, sb.Name, sb.OwnerID, org, sb.State, sb.Size, ttl, archive, store.T(sb.CreatedAt), store.T(sb.UpdatedAt), sb.ImageID,
		mustJSON(labels), mustJSON(env), boolInt(sb.Nested), mustJSON(sb.Placement), sb.SetupScript, mustJSON(files), nz(sb.SetupStatus),
		nz(sb.Visibility), boolInt(sb.PrivateByCreator), boolInt(sb.HoldsCreatorLogins), boolInt(sb.NoEnv), boolInt(sb.SnapshotsEnabled),
		boolInt(sb.Monitoring), nz(sb.Environment), nz(sb.EnvironmentID), envVer, nz(sb.CreatedByKey), sb.SourceKind, sb.SourceID, sb.BootKind)
	return err
}

// hostPorts delegates create-time hostedPorts to the HostPorts hook.
func (s *Server) hostPorts(c *Ctx, sb *Sandbox, ports []json.RawMessage) []map[string]any {
	if s.Hooks.HostPorts != nil {
		return s.Hooks.HostPorts(c, sb, ports)
	}
	out := []map[string]any{}
	for _, p := range ports {
		var v struct {
			Port int `json:"port"`
		}
		_ = json.Unmarshal(p, &v)
		out = append(out, map[string]any{"port": v.Port, "error": "not_supported", "message": "Public URLs are not enabled on this server."})
	}
	return out
}
