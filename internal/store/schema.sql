-- Schema version 1. Used for both the project database
-- (.agent-shiftboard/state.db) and the user database
-- (~/.config/agent-shiftboard/state.db). The user database only holds
-- user-scope rules and shared resources.

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  title          TEXT NOT NULL,
  status         TEXT NOT NULL,
  priority       INTEGER NOT NULL DEFAULT 0,
  worktree       TEXT NOT NULL DEFAULT '',
  branch         TEXT NOT NULL DEFAULT '',
  brief_revision INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_deps (
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  depends_on INTEGER NOT NULL REFERENCES tasks(id),
  PRIMARY KEY (task_id, depends_on)
);

CREATE TABLE IF NOT EXISTS briefs (
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  revision   INTEGER NOT NULL,
  fields     TEXT NOT NULL,          -- JSON of lead-owned fields
  updated_by TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (task_id, revision)
);

CREATE TABLE IF NOT EXISTS rules (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  scope         TEXT NOT NULL,
  task_id       INTEGER,
  text          TEXT NOT NULL,
  source        TEXT NOT NULL,
  confirmation  TEXT NOT NULL,
  status        TEXT NOT NULL,
  valid_from    TEXT NOT NULL,
  superseded_by TEXT NOT NULL DEFAULT '',
  created_by    TEXT NOT NULL,
  created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pending (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id      INTEGER NOT NULL REFERENCES tasks(id),
  kind         TEXT NOT NULL,
  action       TEXT NOT NULL,
  target       TEXT NOT NULL DEFAULT '',
  requester    TEXT NOT NULL,
  status       TEXT NOT NULL,
  expires_at   TEXT NOT NULL DEFAULT '',
  resolved_via TEXT NOT NULL DEFAULT '',
  quote        TEXT NOT NULL DEFAULT '',
  answer       TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  resolved_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS delegations (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  scope      TEXT NOT NULL,
  exclusions TEXT NOT NULL DEFAULT '',
  granted_by TEXT NOT NULL,
  granted_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  status     TEXT NOT NULL,
  created_by TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS delegation_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  delegation_id INTEGER NOT NULL REFERENCES delegations(id),
  decision      TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  at            TEXT NOT NULL,
  needs_review  INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS assignments (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id        INTEGER NOT NULL REFERENCES tasks(id),
  worker_session TEXT NOT NULL DEFAULT '',
  provider       TEXT NOT NULL DEFAULT '',
  scope          TEXT NOT NULL,
  expected       TEXT NOT NULL DEFAULT '',
  required_tools TEXT NOT NULL DEFAULT '',
  constraints    TEXT NOT NULL DEFAULT '',
  worktree       TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL,
  result         TEXT NOT NULL DEFAULT '',
  evidence       TEXT NOT NULL DEFAULT '',
  reason         TEXT NOT NULL DEFAULT '',
  created_by     TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS side_effects (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id       INTEGER NOT NULL REFERENCES tasks(id),
  session_id    TEXT NOT NULL,
  action        TEXT NOT NULL,
  target        TEXT NOT NULL,
  status        TEXT NOT NULL,
  approval_id   INTEGER,
  evidence      TEXT NOT NULL DEFAULT '',
  rollback_ref  TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  started_at    TEXT NOT NULL DEFAULT '',
  finished_at   TEXT NOT NULL DEFAULT '',
  reconciled_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS milestones (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  kind        TEXT NOT NULL,
  target      TEXT NOT NULL,
  status      TEXT NOT NULL,
  evidence    TEXT NOT NULL DEFAULT '',
  observed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id                TEXT PRIMARY KEY,   -- AgentShiftboard session ID (UUID)
  task_id           INTEGER NOT NULL REFERENCES tasks(id),
  provider          TEXT NOT NULL,
  role              TEXT NOT NULL,      -- lead | worker
  native_session_id TEXT NOT NULL DEFAULT '',
  pane_id           TEXT NOT NULL DEFAULT '',
  pid               INTEGER NOT NULL DEFAULT 0,
  permission_mode   TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL,
  reason            TEXT NOT NULL DEFAULT '',
  status_source     TEXT NOT NULL,
  observed_at       TEXT NOT NULL,
  unknown_since     TEXT NOT NULL DEFAULT '',
  turns_since_brief INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS leases (
  task_id         INTEGER PRIMARY KEY REFERENCES tasks(id),
  state           TEXT NOT NULL,
  holder_session  TEXT NOT NULL DEFAULT '',
  pending_session TEXT NOT NULL DEFAULT '',
  note            TEXT NOT NULL DEFAULT '',
  updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS owned_resources (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id    INTEGER REFERENCES tasks(id),
  session_id TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL,             -- worktree | tempdir | port | process
  value      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS config_values (
  name        TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  updated_by  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS secret_refs (
  name       TEXT PRIMARY KEY,
  location   TEXT NOT NULL,
  keys       TEXT NOT NULL DEFAULT '',
  purpose    TEXT NOT NULL DEFAULT '',
  handling   TEXT NOT NULL,
  updated_by TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tools (
  name          TEXT PRIMARY KEY,
  description   TEXT NOT NULL,
  command       TEXT NOT NULL,
  usage         TEXT NOT NULL DEFAULT '',
  effect_class  TEXT NOT NULL,
  targets       TEXT NOT NULL DEFAULT '',
  requires      TEXT NOT NULL DEFAULT '{}',  -- JSON {config:[],secrets:[],executables:[]}
  locks         TEXT NOT NULL DEFAULT '',
  created_by    TEXT NOT NULL,
  verified_at   TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  superseded_by TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS locks (
  name           TEXT PRIMARY KEY,
  holder_session TEXT NOT NULL,
  task_id        INTEGER,
  status         TEXT NOT NULL,
  acquired_at    TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER,
  at      TEXT NOT NULL,
  actor   TEXT NOT NULL,
  kind    TEXT NOT NULL,
  detail  TEXT NOT NULL DEFAULT ''
);
