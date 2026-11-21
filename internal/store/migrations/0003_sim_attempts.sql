CREATE TABLE sim_attempts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  problem TEXT NOT NULL,
  attempt TEXT NOT NULL,
  cause TEXT NOT NULL,
  intervention TEXT NOT NULL,
  prediction TEXT NOT NULL,
  context TEXT NOT NULL,
  proposed_at TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT '',
  evidence TEXT NOT NULL DEFAULT '',
  evidence_sha256 TEXT NOT NULL DEFAULT '',
  evidence_worktree TEXT NOT NULL DEFAULT '',
  observed_at TEXT NOT NULL DEFAULT '',
  UNIQUE (project_id, problem, attempt)
) STRICT;

CREATE INDEX sim_history ON sim_attempts (project_id, problem, id);
