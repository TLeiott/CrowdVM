package server

import "github.com/crowdvm/crowdvm/internal/server/store"

// Core schema. Times are unix milliseconds. Features add their own tables
// with store.Register("<feature>", store.Migration{...}).
func init() {
	store.Register("core", store.Migration{Version: 1, SQL: `
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email TEXT UNIQUE COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL DEFAULT '',
  is_admin INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT 'user',
  connection_method TEXT NOT NULL DEFAULT 'password',
  claimed_by TEXT,
  disabled INTEGER NOT NULL DEFAULT 0,
  active_org TEXT,
  active_wallet_chosen INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE email_codes (
  email TEXT PRIMARY KEY COLLATE NOCASE,
  code_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE invite_codes (
  id TEXT PRIMARY KEY,
  code_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_by TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  max_uses INTEGER NOT NULL DEFAULT 1,
  uses INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER
);

CREATE TABLE device_codes (
  id TEXT PRIMARY KEY,
  user_code TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  client_name TEXT NOT NULL DEFAULT '',
  email TEXT,
  agent_user_id TEXT,
  status TEXT NOT NULL,
  user_id TEXT,
  token TEXT,
  token_type TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  interval_s INTEGER NOT NULL,
  last_poll_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE api_keys (
  id TEXT PRIMARY KEY,
  secret_hash TEXT NOT NULL UNIQUE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  org_id TEXT,
  name TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  last_four TEXT NOT NULL,
  actions TEXT NOT NULL,
  sandbox_ids TEXT NOT NULL,
  environment_ids TEXT NOT NULL,
  preset TEXT NOT NULL DEFAULT '',
  sandbox_id TEXT,
  created_by_key TEXT,
  lane TEXT NOT NULL DEFAULT 'scoped-v1',
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  last_used_at INTEGER,
  revoked_at INTEGER
);
CREATE INDEX api_keys_user ON api_keys(user_id);

CREATE TABLE api_key_usage (
  key_id TEXT NOT NULL,
  day TEXT NOT NULL,
  requests INTEGER NOT NULL,
  PRIMARY KEY (key_id, day)
);

CREATE TABLE orgs (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  new_sandboxes_private INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE org_members (
  org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role TEXT NOT NULL,
  joined_at INTEGER NOT NULL,
  concurrent_cap INTEGER,
  usage_cap_credits REAL,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX org_members_user ON org_members(user_id);

CREATE TABLE org_invites (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  email TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'member',
  code_hash TEXT NOT NULL UNIQUE,
  invited_by TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  accepted_by TEXT,
  accepted_at INTEGER,
  revoked_at INTEGER
);

CREATE TABLE wallets (
  id TEXT PRIMARY KEY,
  balance REAL NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE TABLE credit_ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  wallet_id TEXT NOT NULL,
  delta REAL NOT NULL,
  kind TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  sandbox_id TEXT,
  node_id TEXT,
  actor_id TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX credit_ledger_wallet ON credit_ledger(wallet_id, created_at);

CREATE TABLE join_tokens (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  created_by TEXT,
  owner_id TEXT,
  labels TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  max_uses INTEGER NOT NULL DEFAULT 0,
  uses INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER
);

CREATE TABLE nodes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  owner_id TEXT,
  join_token_id TEXT,
  info TEXT NOT NULL DEFAULT '{}',
  usage TEXT NOT NULL DEFAULT '{}',
  labels TEXT NOT NULL DEFAULT '{}',
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER,
  connected_at INTEGER,
  remote_addr TEXT NOT NULL DEFAULT ''
);

CREATE TABLE images (
  id TEXT PRIMARY KEY,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  uploaded_by TEXT,
  created_at INTEGER NOT NULL
);

CREATE TABLE sandboxes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  org_id TEXT,
  state TEXT NOT NULL,
  error TEXT,
  size TEXT NOT NULL,
  ttl_seconds INTEGER,
  archive_after INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  started_at INTEGER,
  stopped_at INTEGER,
  node_id TEXT,
  image_id TEXT NOT NULL DEFAULT '',
  head_snapshot_id TEXT,
  labels TEXT NOT NULL DEFAULT '{}',
  env TEXT NOT NULL DEFAULT '{}',
  nested INTEGER NOT NULL DEFAULT 0,
  placement TEXT NOT NULL DEFAULT '{}',
  setup_script TEXT NOT NULL DEFAULT '',
  files TEXT NOT NULL DEFAULT '[]',
  first_boot_done INTEGER NOT NULL DEFAULT 0,
  setup_status TEXT,
  setup_error TEXT,
  visibility TEXT,
  private_by_creator INTEGER NOT NULL DEFAULT 0,
  holds_creator_logins INTEGER NOT NULL DEFAULT 0,
  wipe_pending INTEGER NOT NULL DEFAULT 0,
  no_env INTEGER NOT NULL DEFAULT 0,
  snapshots_enabled INTEGER NOT NULL DEFAULT 1,
  monitoring INTEGER NOT NULL DEFAULT 1,
  environment TEXT,
  environment_id TEXT,
  environment_version INTEGER,
  created_by_key TEXT,
  guest_token TEXT NOT NULL DEFAULT '',
  source_kind TEXT NOT NULL DEFAULT '',
  source_id TEXT NOT NULL DEFAULT '',
  boot_kind TEXT NOT NULL DEFAULT '',
  agent_state TEXT NOT NULL DEFAULT '',
  health TEXT NOT NULL DEFAULT 'ok',
  health_reason TEXT,
  degraded_since INTEGER,
  hydrated INTEGER NOT NULL DEFAULT 1,
  hydrated_at INTEGER,
  last_snapshot_at INTEGER,
  last_snapshot_status TEXT,
  snapshot_completed_at INTEGER,
  last_recover_at INTEGER,
  last_reboot_at INTEGER,
  last_reboot_reason TEXT,
  stop_id TEXT,
  direct TEXT NOT NULL DEFAULT '{}',
  subdomain TEXT,
  extra TEXT NOT NULL DEFAULT '{}',
  deleted_at INTEGER
);
CREATE INDEX sandboxes_owner ON sandboxes(owner_id, created_at);
CREATE INDEX sandboxes_org ON sandboxes(org_id, created_at);
CREATE INDEX sandboxes_node ON sandboxes(node_id);

CREATE TABLE sandbox_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sandbox_id TEXT NOT NULL,
  wallet_id TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  node_id TEXT,
  size TEXT NOT NULL,
  multiplier REAL NOT NULL,
  started_at INTEGER NOT NULL,
  ended_at INTEGER,
  billed_until INTEGER NOT NULL
);
CREATE INDEX sandbox_runs_sb ON sandbox_runs(sandbox_id);
CREATE INDEX sandbox_runs_open ON sandbox_runs(ended_at);

CREATE TABLE stops (
  id TEXT PRIMARY KEY,
  sandbox_id TEXT NOT NULL,
  status TEXT NOT NULL,
  requested_at INTEGER NOT NULL,
  last_attempt_at INTEGER,
  error TEXT,
  ended_at INTEGER,
  forced INTEGER NOT NULL DEFAULT 0,
  snapshot_id TEXT,
  attempts INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE snapshots (
  id TEXT PRIMARY KEY,
  sandbox_id TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  parent_id TEXT,
  image_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0,
  chain_id TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  node_id TEXT,
  created_at INTEGER NOT NULL,
  uploaded_at INTEGER,
  deleted_at INTEGER,
  purged_at INTEGER
);
CREATE INDEX snapshots_sb ON snapshots(sandbox_id, created_at);
CREATE INDEX snapshots_parent ON snapshots(parent_id);

CREATE TABLE snapshot_locations (
  snapshot_id TEXT NOT NULL,
  node_id TEXT NOT NULL,
  PRIMARY KEY (snapshot_id, node_id)
);

CREATE TABLE saved_snapshots (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  org_id TEXT,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  error TEXT,
  source_sandbox_id TEXT NOT NULL,
  snapshot_id TEXT,
  pending_snapshot_id TEXT,
  size_name TEXT NOT NULL DEFAULT 'default',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  env TEXT NOT NULL DEFAULT '{}',
  no_env INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (owner_id, name)
);

CREATE TABLE snapshot_shares (
  id TEXT PRIMARY KEY,
  saved_id TEXT NOT NULL REFERENCES saved_snapshots(id) ON DELETE CASCADE,
  code_hash TEXT NOT NULL UNIQUE,
  code_prefix TEXT NOT NULL,
  status TEXT NOT NULL,
  use_count INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER,
  revoked_at INTEGER,
  created_by TEXT,
  created_at INTEGER NOT NULL
);

CREATE TABLE deletion_ops (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  target_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  status TEXT NOT NULL,
  stage TEXT NOT NULL,
  attempt_count INTEGER NOT NULL DEFAULT 0,
  requested_at INTEGER NOT NULL,
  completed_at INTEGER
);

CREATE TABLE idempotency (
  owner_id TEXT NOT NULL,
  key TEXT NOT NULL,
  body_hash TEXT NOT NULL,
  sandbox_id TEXT,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (owner_id, key)
);

CREATE TABLE starts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  wallet_id TEXT NOT NULL,
  at INTEGER NOT NULL
);
CREATE INDEX starts_wallet ON starts(wallet_id, at);
`})
}
