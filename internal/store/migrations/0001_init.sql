-- Initial schema. Source records (sessions, events, turns), derived claims,
-- task checkpoints and the disposable FTS5 projections share one file.

CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

INSERT INTO meta (key, value) VALUES ('generation', '0');

CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  enrolled INTEGER NOT NULL DEFAULT 1,
  collect INTEGER NOT NULL DEFAULT 0,
  recall INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  withdrawn_at TEXT
) STRICT;

-- Absolute resolved paths are mappings, not identities.
CREATE TABLE roots (
  path TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  worktree_id TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('primary', 'worktree')),
  added_at TEXT NOT NULL
) STRICT;

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  worktree_id TEXT,
  title TEXT NOT NULL,
  state TEXT NOT NULL CHECK (
    state IN ('active', 'blocked', 'completed', 'cancelled', 'reopened')
  ),
  revision INTEGER NOT NULL,
  progress_ref TEXT NOT NULL DEFAULT '{}',
  origin TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  harness TEXT NOT NULL,
  native_id TEXT NOT NULL,
  parent_id TEXT REFERENCES sessions (id) ON DELETE CASCADE,
  project_id TEXT REFERENCES projects (id) ON DELETE CASCADE,
  worktree_id TEXT,
  task_id TEXT REFERENCES tasks (id) ON DELETE SET NULL,
  agent_type TEXT NOT NULL DEFAULT '',
  harness_version TEXT NOT NULL DEFAULT '',
  boundaries TEXT NOT NULL DEFAULT '{}',
  imported INTEGER NOT NULL DEFAULT 0,
  first_seen TEXT NOT NULL,
  last_seen TEXT NOT NULL,
  UNIQUE (harness, native_id)
) STRICT;

CREATE TABLE cursors (
  session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  device INTEGER NOT NULL DEFAULT 0,
  inode INTEGER NOT NULL DEFAULT 0,
  offset INTEGER NOT NULL DEFAULT 0,
  line_no INTEGER NOT NULL DEFAULT 0,
  malformed INTEGER NOT NULL DEFAULT 0,
  oversized INTEGER NOT NULL DEFAULT 0,
  rotations INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (session_id, path)
) STRICT;

CREATE TABLE events (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  native_id TEXT NOT NULL,
  id_basis TEXT NOT NULL CHECK (id_basis IN ('native', 'fallback')),
  project_id TEXT REFERENCES projects (id) ON DELETE CASCADE,
  worktree_id TEXT,
  turn_id TEXT,
  kind TEXT NOT NULL,
  seq INTEGER,
  event_time TEXT,
  ingested_at TEXT NOT NULL,
  source TEXT NOT NULL,
  source_digest TEXT NOT NULL,
  adapter_version TEXT NOT NULL,
  flags TEXT NOT NULL DEFAULT '{}',
  tool_name TEXT NOT NULL DEFAULT '',
  call_id TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  invalid TEXT NOT NULL DEFAULT '',
  UNIQUE (session_id, native_id)
) STRICT;

CREATE INDEX events_turn ON events (turn_id);

CREATE TABLE turns (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  project_id TEXT REFERENCES projects (id) ON DELETE CASCADE,
  worktree_id TEXT,
  first_event TEXT NOT NULL,
  last_event TEXT NOT NULL,
  prev_id TEXT,
  next_id TEXT,
  event_time TEXT,
  text TEXT NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  invalid TEXT NOT NULL DEFAULT '',
  digest TEXT NOT NULL
) STRICT;

CREATE INDEX turns_scope ON turns (project_id, session_id);

CREATE VIRTUAL TABLE turns_fts USING fts5 (
  text,
  content = 'turns',
  content_rowid = 'rowid',
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER turns_ai AFTER INSERT ON turns BEGIN
  INSERT INTO turns_fts (rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER turns_ad AFTER DELETE ON turns BEGIN
  INSERT INTO turns_fts (turns_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;

CREATE TRIGGER turns_au AFTER UPDATE OF text ON turns BEGIN
  INSERT INTO turns_fts (turns_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
  INSERT INTO turns_fts (rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TABLE claims (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  worktree_id TEXT,
  task_id TEXT REFERENCES tasks (id) ON DELETE SET NULL,
  kind TEXT NOT NULL CHECK (kind IN ('observation', 'decision', 'summary', 'note')),
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  rationale TEXT NOT NULL DEFAULT '',
  alternatives TEXT NOT NULL DEFAULT '[]',
  asserted TEXT NOT NULL CHECK (asserted IN ('observed', 'inferred')),
  status TEXT NOT NULL CHECK (
    status IN ('observed', 'inferred', 'disputed', 'superseded', 'invalidated', 'unsupported')
  ),
  status_reason TEXT NOT NULL DEFAULT '',
  origin TEXT NOT NULL,
  lineage TEXT NOT NULL,
  transform TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  event_time TEXT,
  validated_at TEXT,
  expires_at TEXT,
  superseded_by TEXT REFERENCES claims (id) ON DELETE SET NULL
) STRICT;

CREATE INDEX claims_scope ON claims (project_id, status);

CREATE VIRTUAL TABLE claims_fts USING fts5 (
  title,
  body,
  rationale,
  content = 'claims',
  content_rowid = 'rowid',
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER claims_ai AFTER INSERT ON claims BEGIN
  INSERT INTO claims_fts (rowid, title, body, rationale)
  VALUES (new.rowid, new.title, new.body, new.rationale);
END;

CREATE TRIGGER claims_ad AFTER DELETE ON claims BEGIN
  INSERT INTO claims_fts (claims_fts, rowid, title, body, rationale)
  VALUES ('delete', old.rowid, old.title, old.body, old.rationale);
END;

-- Support references and applicability dependencies of a claim.
CREATE TABLE refs (
  claim_id TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK (role IN ('support', 'depends')),
  kind TEXT NOT NULL,
  ref TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '{}',
  root TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'ok' CHECK (state IN ('ok', 'lost')),
  lost_reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (claim_id, role, kind, ref)
) STRICT;

CREATE INDEX refs_target ON refs (kind, ref);

CREATE TABLE links (
  from_id TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
  to_id TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
  relation TEXT NOT NULL CHECK (relation IN ('supersedes', 'contradicts', 'derived_from')),
  PRIMARY KEY (from_id, to_id, relation)
) STRICT;

CREATE INDEX links_to ON links (to_id, relation);

CREATE TABLE checkpoints (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  revision INTEGER NOT NULL,
  accepted INTEGER NOT NULL,
  body TEXT NOT NULL,
  origin TEXT NOT NULL,
  session_id TEXT REFERENCES sessions (id) ON DELETE SET NULL,
  created_at TEXT NOT NULL
) STRICT;

CREATE INDEX checkpoints_task ON checkpoints (task_id, accepted, revision);

CREATE TABLE transitions (
  task_id TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  revision INTEGER NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  reason TEXT NOT NULL,
  origin TEXT NOT NULL,
  at TEXT NOT NULL,
  PRIMARY KEY (task_id, revision)
) STRICT;

CREATE TABLE shares (
  id TEXT PRIMARY KEY,
  claim_id TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
  to_project TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  origin TEXT NOT NULL,
  created_at TEXT NOT NULL,
  revoked_at TEXT
) STRICT;

-- Deletion markers that ingestion replay must respect.
CREATE TABLE tombstones (
  key TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  created_at TEXT NOT NULL
) STRICT;

CREATE TABLE environments (
  epoch TEXT PRIMARY KEY,
  descriptor TEXT NOT NULL,
  first_seen TEXT NOT NULL
) STRICT;
