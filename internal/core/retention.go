package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
	"github.com/timfewi/ichnora/internal/store"
)

// RetentionResult previews or reports an explicit deletion.
type RetentionResult struct {
	Selector   string         `json:"selector"`
	Target     string         `json:"target"`
	Counts     map[string]int `json:"counts"`
	Dependants []string       `json:"dependent_claims_losing_support"`
	Confirm    string         `json:"confirm,omitempty"`
	Applied    bool           `json:"applied"`
	Erasure    string         `json:"erasure"`
}

const erasureNote = "Rows, FTS entries and WAL frames are removed and freed pages zeroed (secure_delete). Copy-on-write filesystems, SSD remapping, filesystem snapshots and existing backups may still hold the data; physical erasure is not guaranteed. Tombstones stop replays from re-importing deleted sessions."

type selection struct {
	claims, sessions []string
	tombstones       []string
	tasks            []string
	project          string
	// owner is the project whose records change.
	owner string
}

// token binds a retention confirmation to the selection and the current
// policy and owner-project generations.
func (c *Core) retentionToken(ctx context.Context, q querier, req contract.AdminRequest, owner string) (string, error) {
	gen, err := store.Generation(ctx, q)
	if err != nil {
		return "", err
	}
	return c.sign("retention", req.Selector, req.Target, itoa(int(gen)), itoa(int(store.ProjectGeneration(ctx, q, owner))))[:32], nil
}

func (c *Core) selectFor(ctx context.Context, q querier, selector, target string) (selection, error) {
	var sel selection
	collect := func(query string, args ...any) ([]string, error) {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	var err error
	switch selector {
	case "claim":
		var id string
		if err := q.QueryRowContext(ctx, `SELECT id, project_id FROM claims WHERE id = ?`, target).Scan(&id, &sel.owner); err != nil {
			return sel, contract.Errf(contract.CodeNotFound, "unknown claim")
		}
		sel.claims = []string{id}
	case "session":
		// The session and its child sessions.
		if sel.sessions, err = collect(`WITH RECURSIVE tree(id) AS (SELECT id FROM sessions WHERE id = ?
			UNION SELECT s.id FROM sessions s JOIN tree ON s.parent_id = tree.id) SELECT id FROM tree`, target); err != nil {
			return sel, err
		}
		if len(sel.sessions) == 0 {
			return sel, contract.Errf(contract.CodeNotFound, "unknown session")
		}
		var owner sql.NullString
		_ = q.QueryRowContext(ctx, `SELECT project_id FROM sessions WHERE id = ?`, target).Scan(&owner)
		sel.owner = owner.String
	case "project":
		if sel.project, err = c.projectByRef(ctx, q, target); err != nil {
			return sel, err
		}
		sel.owner = sel.project
		if sel.sessions, err = collect(`SELECT id FROM sessions WHERE project_id = ?`, sel.project); err != nil {
			return sel, err
		}
		if sel.claims, err = collect(`SELECT id FROM claims WHERE project_id = ?`, sel.project); err != nil {
			return sel, err
		}
		if sel.tasks, err = collect(`SELECT id FROM tasks WHERE project_id = ?`, sel.project); err != nil {
			return sel, err
		}
	default:
		return sel, contract.Errf(contract.CodeInvalid, "selector must be claim, session or project")
	}
	for _, id := range sel.sessions {
		var harness, native string
		if err := q.QueryRowContext(ctx, `SELECT harness, native_id FROM sessions WHERE id = ?`, id).Scan(&harness, &native); err != nil {
			return sel, err
		}
		sel.tombstones = append(sel.tombstones, tombstoneKey(harness, native))
	}
	return sel, nil
}

func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anys(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func (c *Core) countSelection(ctx context.Context, q querier, sel selection) (map[string]int, []string, error) {
	counts := map[string]int{"claims": len(sel.claims), "sessions": len(sel.sessions), "tasks": len(sel.tasks)}
	var dependants []string
	if len(sel.sessions) > 0 {
		args := anys(sel.sessions)
		for name, query := range map[string]string{
			"events": `SELECT count(*) FROM events WHERE session_id IN (` + inList(len(args)) + `)`,
			"turns":  `SELECT count(*) FROM turns WHERE session_id IN (` + inList(len(args)) + `)`,
		} {
			var n int
			if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
				return nil, nil, err
			}
			counts[name] = n
		}
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT r.claim_id FROM refs r WHERE (r.kind = 'event' AND r.ref IN
			(SELECT id FROM events WHERE session_id IN (`+inList(len(args))+`))) OR (r.kind = 'turn' AND r.ref IN
			(SELECT id FROM turns WHERE session_id IN (`+inList(len(args))+`)))`, append(args, args...)...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, nil, err
			}
			dependants = append(dependants, id)
		}
		rows.Close()
	}
	if len(sel.claims) > 0 {
		args := anys(sel.claims)
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT claim_id FROM refs WHERE kind = 'claim' AND ref IN (`+inList(len(args))+`)
			AND claim_id NOT IN (`+inList(len(args))+`)`, append(args, args...)...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, nil, err
			}
			dependants = append(dependants, id)
		}
		rows.Close()
	}
	return counts, dependants, nil
}

// RetentionPreview is the dry run; its confirmation token is bound to the
// current generation, so any change requires a new preview.
func (c *Core) RetentionPreview(ctx context.Context, caller Caller, req contract.AdminRequest) (RetentionResult, error) {
	if err := c.admin(caller); err != nil {
		return RetentionResult{}, err
	}
	db := c.store.Read()
	sel, err := c.selectFor(ctx, db, req.Selector, req.Target)
	if err != nil {
		return RetentionResult{}, err
	}
	counts, deps, err := c.countSelection(ctx, db, sel)
	if err != nil {
		return RetentionResult{}, err
	}
	token, err := c.retentionToken(ctx, db, req, sel.owner)
	if err != nil {
		return RetentionResult{}, err
	}
	return RetentionResult{Selector: req.Selector, Target: req.Target, Counts: counts, Dependants: nonNil(deps),
		Confirm: token, Erasure: erasureNote}, nil
}

// RetentionApply deletes the selection, writes tombstones, marks dependent
// support as lost, recomputes statuses and truncates the WAL.
func (c *Core) RetentionApply(ctx context.Context, caller Caller, req contract.AdminRequest) (RetentionResult, error) {
	if err := c.admin(caller); err != nil {
		return RetentionResult{}, err
	}
	out := RetentionResult{Selector: req.Selector, Target: req.Target, Erasure: erasureNote}
	err := c.write(ctx, func(tx *sql.Tx) error {
		sel, err := c.selectFor(ctx, tx, req.Selector, req.Target)
		if err != nil {
			return err
		}
		token, err := c.retentionToken(ctx, tx, req, sel.owner)
		if err != nil {
			return err
		}
		if req.Confirm == "" || req.Confirm != token {
			return contract.Errf(contract.CodeConflict, "confirmation does not match the current state; preview again")
		}
		counts, deps, err := c.countSelection(ctx, tx, sel)
		if err != nil {
			return err
		}
		out.Counts, out.Dependants = counts, nonNil(deps)
		for _, key := range sel.tombstones {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tombstones (key, reason, created_at) VALUES (?, ?, ?)`,
				key, "explicit "+req.Selector+" deletion", c.now()); err != nil {
				return err
			}
		}
		for _, sid := range sel.sessions {
			for _, kind := range []string{"event", "turn"} {
				table := kind + "s"
				rows, err := tx.QueryContext(ctx, `SELECT id FROM `+table+` WHERE session_id = ?`, sid)
				if err != nil {
					return err
				}
				var ids []string
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					ids = append(ids, id)
				}
				rows.Close()
				for _, id := range ids {
					if _, err := loseSupport(ctx, tx, kind, id, "source deleted"); err != nil {
						return err
					}
				}
			}
			// Turns first so their FTS rows are removed by trigger.
			if _, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE session_id = ?`, sid); err != nil {
				return err
			}
		}
		for _, sid := range sel.sessions {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sid); err != nil {
				return err
			}
		}
		if sel.project != "" {
			// Mixed sessions of other projects may hold events attributed here.
			if _, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE project_id = ?`, sel.project); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id = ?`, sel.project); err != nil {
				return err
			}
		}
		for _, id := range sel.claims {
			if _, err := loseSupport(ctx, tx, "claim", id, "source claim deleted"); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE claims SET superseded_by = NULL WHERE superseded_by = ?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE id = ?`, id); err != nil {
				return err
			}
		}
		for _, id := range sel.tasks {
			if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
				return err
			}
		}
		var remaining []string
		deleted := map[string]bool{}
		for _, id := range sel.claims {
			deleted[id] = true
		}
		for _, d := range deps {
			if !deleted[d] {
				remaining = append(remaining, d)
			}
		}
		if _, err := c.recompute(ctx, tx, remaining); err != nil {
			return err
		}
		if sel.owner != "" {
			return store.BumpProject(ctx, tx, sel.owner)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	out.Applied = true
	if err := c.store.Checkpoint(ctx); err != nil {
		c.log.Warn("wal checkpoint after deletion failed", "error_type", errorType(err))
	}
	return out, nil
}

// ImportSession describes one explicitly selected historical transcript.
type ImportSession struct {
	Path      string `json:"path"`
	Session   string `json:"native_session"`
	Project   string `json:"project,omitempty"`
	Lines     int    `json:"lines_previewed"`
	Ingested  int    `json:"events_ingested,omitempty"`
	Decision  string `json:"decision"`
	Truncated bool   `json:"preview_truncated,omitempty"`
}

// ImportResult previews or applies a historical import.
type ImportResult struct {
	Harness  string          `json:"harness"`
	Sessions []ImportSession `json:"sessions"`
	Confirm  string          `json:"confirm,omitempty"`
	Applied  bool            `json:"applied"`
}

// importTarget inspects the start of a transcript to find its session and
// working directory, without storing anything.
func (c *Core) importTarget(ctx context.Context, harness, path string) (ImportSession, Scope, string, error) {
	out := ImportSession{Path: path}
	resolved, err := c.pol.Transcript(harness, path)
	if err != nil {
		out.Decision = "denied: " + reasonOf(err)
		return out, Scope{}, "", nil
	}
	adapter := ingest.ForHarness(harness)
	lines, _, err := ingest.Increment(ingest.Cursor{Path: resolved}, 1<<20, int(c.cfg.Limits.LineBytes))
	if err != nil {
		out.Decision = "denied: unreadable"
		return out, Scope{}, "", nil
	}
	var st ingest.State
	cwd := ""
	for i, l := range lines {
		if i >= 200 {
			out.Truncated = true
			break
		}
		out.Lines++
		evs, _ := adapter.Parse(l.Data, &st)
		for _, e := range evs {
			if cwd == "" && e.Cwd != "" {
				cwd = e.Cwd
			}
		}
	}
	out.Session = st.Session
	if out.Session == "" {
		out.Decision = "skipped: no session identity found"
		return out, Scope{}, resolved, nil
	}
	s, ok, err := c.resolvePath(ctx, c.store.Read(), cwd)
	if err != nil {
		return out, Scope{}, "", err
	}
	if !ok {
		out.Decision = "skipped: working directory is not inside an enrolled project"
		return out, Scope{}, resolved, nil
	}
	var dead int
	_ = c.store.Read().QueryRowContext(ctx, `SELECT count(*) FROM tombstones WHERE key = ?`, tombstoneKey(harness, out.Session)).Scan(&dead)
	if dead > 0 {
		out.Decision = "skipped: session was deleted (tombstone)"
		return out, Scope{}, resolved, nil
	}
	out.Project, out.Decision = s.Project, "import"
	return out, s, resolved, nil
}

func reasonOf(err error) string {
	var pe *contract.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	if r := strings.TrimPrefix(err.Error(), "policy denied: "); r != "" {
		return r
	}
	return "denied"
}

// Import previews or applies an explicit historical import of selected files.
func (c *Core) Import(ctx context.Context, caller Caller, req contract.AdminRequest, apply bool) (ImportResult, error) {
	if err := c.admin(caller); err != nil {
		return ImportResult{}, err
	}
	adapter := ingest.ForHarness(req.Harness)
	if adapter == nil || len(req.Paths) == 0 || len(req.Paths) > 16 {
		return ImportResult{}, contract.Errf(contract.CodeInvalid, "select a harness and one to sixteen transcript paths")
	}
	out := ImportResult{Harness: req.Harness, Sessions: []ImportSession{}}
	type target struct {
		scope Scope
		path  string
		idx   int
	}
	var targets []target
	for _, p := range req.Paths {
		sess, s, resolved, err := c.importTarget(ctx, req.Harness, p)
		if err != nil {
			return out, err
		}
		if sess.Decision == "import" {
			targets = append(targets, target{s, resolved, len(out.Sessions)})
		}
		out.Sessions = append(out.Sessions, sess)
	}
	token := c.sign("import", req.Harness, mustJSON(out.Sessions))[:32]
	if !apply {
		out.Confirm = token
		return out, nil
	}
	if req.Confirm != token {
		return out, contract.Errf(contract.CodeConflict, "confirmation does not match the previewed selection; preview again")
	}
	for _, t := range targets {
		sess := &out.Sessions[t.idx]
		var sessionID string
		if err := c.write(ctx, func(tx *sql.Tx) error {
			var err error
			sessionID, err = c.ensureSession(ctx, tx, req.Harness, sess.Session, "", t.scope.ProjectID, t.scope.Worktree, "", "", true)
			return err
		}); err != nil {
			return out, err
		}
		if sessionID == "" {
			continue
		}
		// Bounded increments until the file is consumed.
		for i := 0; i < 256; i++ {
			n, err := c.ingestFile(ctx, req.Harness, adapter, t.scope, sessionID, t.path)
			if err != nil {
				return out, err
			}
			sess.Ingested += n
			var offset int64
			_ = c.store.Read().QueryRowContext(ctx, `SELECT offset FROM cursors WHERE session_id = ? AND path = ?`, sessionID, t.path).Scan(&offset)
			if st, err := osStat(t.path); err != nil || offset >= st {
				break
			}
		}
	}
	out.Applied = true
	return out, nil
}

// ReconcileResult reports cursors advanced after missed callbacks.
type ReconcileResult struct {
	Cursors  int `json:"cursors_checked"`
	Ingested int `json:"events_ingested"`
	Skipped  int `json:"skipped"`
}

// Reconcile continues known transcripts of collecting projects from their
// cursors. It never discovers new files.
func (c *Core) Reconcile(ctx context.Context, caller Caller) (ReconcileResult, error) {
	var out ReconcileResult
	if err := c.admin(caller); err != nil {
		return out, err
	}
	if c.ingestionPaused() {
		return out, contract.Errf(contract.CodeStorageFull, "ingestion is paused at the storage limit")
	}
	rows, err := c.store.Read().QueryContext(ctx, `SELECT k.session_id, k.path, x.harness, x.project_id FROM cursors k
		JOIN sessions x ON x.id = k.session_id JOIN projects p ON p.id = x.project_id WHERE p.enrolled = 1 AND p.collect = 1
		ORDER BY k.updated_at LIMIT 64`)
	if err != nil {
		return out, err
	}
	type cur struct{ session, path, harness, project string }
	var list []cur
	for rows.Next() {
		var k cur
		if err := rows.Scan(&k.session, &k.path, &k.harness, &k.project); err != nil {
			rows.Close()
			return out, err
		}
		list = append(list, k)
	}
	rows.Close()
	for _, k := range list {
		out.Cursors++
		var s Scope
		if err := c.store.Read().QueryRowContext(ctx, `SELECT p.id, p.name, r.worktree_id, r.path FROM projects p JOIN roots r ON r.project_id = p.id
			WHERE p.id = ? AND r.kind = 'primary'`, k.project).Scan(&s.ProjectID, &s.Project, &s.Worktree, &s.Root); err != nil {
			out.Skipped++
			continue
		}
		n, err := c.ingestFile(ctx, k.harness, ingest.ForHarness(k.harness), s, k.session, k.path)
		if err != nil {
			out.Skipped++
			continue
		}
		out.Ingested += n
	}
	return out, nil
}
