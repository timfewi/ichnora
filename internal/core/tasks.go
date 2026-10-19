package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/store"
)

// Explicit task transitions; everything else is a conflict.
var transitions = map[string]map[string]bool{
	"active":    {"blocked": true, "completed": true, "cancelled": true},
	"blocked":   {"active": true, "completed": true, "cancelled": true},
	"reopened":  {"active": true, "blocked": true, "completed": true, "cancelled": true},
	"completed": {"reopened": true},
	"cancelled": {"reopened": true},
}

func closed(state string) bool { return state == "completed" || state == "cancelled" }

type progressRef struct {
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

func (c *Core) loadTask(ctx context.Context, q querier, s Scope, id string) (contract.Task, bool, error) {
	var t contract.Task
	var project, wt, ref string
	err := q.QueryRowContext(ctx, `SELECT id, project_id, title, state, revision, coalesce(worktree_id, ''), updated_at, progress_ref
		FROM tasks WHERE id = ?`, id).Scan(&t.ID, &project, &t.Title, &t.State, &t.Revision, &wt, &t.UpdatedAt, &ref)
	if errors.Is(err, sql.ErrNoRows) || err == nil && project != s.ProjectID {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	t.Worktree = wt
	var p progressRef
	_ = json.Unmarshal([]byte(ref), &p)
	t.ProgressRef = p.Path
	return t, true, nil
}

func latestCheckpoint(ctx context.Context, q querier, task string) (*contract.Checkpoint, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT body FROM checkpoints WHERE task_id = ? AND accepted = 1 ORDER BY revision DESC LIMIT 1`, task).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return &contract.Checkpoint{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cp contract.Checkpoint
	return &cp, json.Unmarshal([]byte(body), &cp)
}

func validateCheckpoint(cp *contract.Checkpoint) error {
	lists := [][]string{cp.Constraints, cp.Completed, cp.Remaining, cp.Blockers}
	for _, l := range lists {
		if len(l) > 50 {
			return contract.Errf(contract.CodeInvalid, "checkpoint lists hold at most 50 entries")
		}
		for _, e := range l {
			if err := boundedText("checkpoint entry", e, 1024, true); err != nil {
				return err
			}
		}
	}
	if err := boundedText("objective", cp.Objective, 4096, false); err != nil {
		return err
	}
	if err := boundedText("next_action", cp.NextAction, 1024, false); err != nil {
		return err
	}
	if len(cp.Decisions) > 50 || len(cp.Verification) > 20 {
		return contract.Errf(contract.CodeInvalid, "too many decisions or verification entries")
	}
	for _, v := range cp.Verification {
		if err := boundedText("check", v.Check, 200, true); err != nil {
			return err
		}
		switch v.Outcome {
		case "passed", "failed", "skipped":
		default:
			return contract.Errf(contract.CodeInvalid, "verification outcome must be passed, failed or skipped")
		}
		if len(v.Inputs.Files) > 50 {
			return contract.Errf(contract.CodeInvalid, "verification inputs hold at most 50 files")
		}
	}
	return nil
}

// merge applies a partial checkpoint: present lists and non-empty strings replace.
func merge(prev, next *contract.Checkpoint) contract.Checkpoint {
	out := *prev
	if next.Objective != "" {
		out.Objective = next.Objective
	}
	if next.NextAction != "" {
		out.NextAction = next.NextAction
	}
	for _, f := range []struct {
		dst *[]string
		src []string
	}{
		{&out.Constraints, next.Constraints}, {&out.Completed, next.Completed}, {&out.Remaining, next.Remaining},
		{&out.Blockers, next.Blockers}, {&out.Decisions, next.Decisions},
	} {
		if f.src != nil {
			*f.dst = f.src
		}
	}
	if next.Verification != nil {
		out.Verification = next.Verification
	}
	return out
}

// sessionInfo resolves the caller's session hint to a known session and
// whether it is a child session.
func (c *Core) sessionInfo(ctx context.Context, q querier, s Scope, hint *contract.SessionHint) (id string, child bool) {
	id = c.hintedSession(ctx, q, s, hint)
	if id == "" {
		return "", false
	}
	var parent sql.NullString
	_ = q.QueryRowContext(ctx, `SELECT parent_id FROM sessions WHERE id = ?`, id).Scan(&parent)
	return id, parent.Valid
}

// Task creates, checkpoints, transitions or lists tasks.
func (c *Core) Task(ctx context.Context, caller Caller, req contract.TaskRequest) (contract.TaskResult, error) {
	var out contract.TaskResult
	need := contract.RoleCapture
	if req.Action == "list" {
		need = contract.RoleReader
	}
	if !caller.Has(need) {
		return out, errForbidden
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return out, err
	}
	switch req.Action {
	case "list":
		return c.listTasks(ctx, s)
	case "create", "checkpoint", "transition":
	default:
		return out, contract.Errf(contract.CodeInvalid, "action must be create, checkpoint, transition or list")
	}
	if err := c.checkCapacity(); err != nil {
		return out, err
	}
	if req.Checkpoint != nil {
		if err := validateCheckpoint(req.Checkpoint); err != nil {
			return out, err
		}
	}
	var progress progressRef
	if req.Action == "create" {
		if err := boundedText("title", req.Title, 200, true); err != nil {
			return out, err
		}
		if req.ProgressRef != "" {
			abs, err := c.pol.Source(s.Root, req.ProgressRef)
			if err != nil {
				return out, err
			}
			d, err := digestFile(abs)
			if err != nil {
				return out, contract.Errf(contract.CodeInvalid, "progress file cannot be digested")
			}
			progress = progressRef{Path: req.ProgressRef, SHA256: d}
		}
	}
	err = c.write(ctx, func(tx *sql.Tx) error {
		session, child := c.sessionInfo(ctx, tx, s, req.Session)
		origin := mustJSON(map[string]string{"role": "capture", "session": session})
		if req.Checkpoint != nil {
			for _, d := range req.Checkpoint.Decisions {
				if _, ok, err := inScopeClaim(ctx, tx, s, d); err != nil {
					return err
				} else if !ok {
					return errRef
				}
			}
		}
		switch req.Action {
		case "create":
			id := newID("tsk_")
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, project_id, worktree_id, title, state, revision, progress_ref, origin,
				created_at, updated_at) VALUES (?, ?, ?, ?, 'active', 1, ?, ?, ?, ?)`, id, s.ProjectID, s.Worktree, req.Title,
				mustJSON(progress), origin, c.now(), c.now()); err != nil {
				return err
			}
			cp := contract.Checkpoint{}
			if req.Checkpoint != nil {
				cp = merge(&cp, req.Checkpoint)
			}
			out.CheckpointID = newID("ckp_")
			if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints (id, task_id, revision, accepted, body, origin, session_id, created_at)
				VALUES (?, ?, 1, 1, ?, ?, NULLIF(?, ''), ?)`, out.CheckpointID, id, mustJSON(cp), origin, session, c.now()); err != nil {
				return err
			}
			if session != "" && !child {
				if _, err := tx.ExecContext(ctx, `UPDATE sessions SET task_id = ? WHERE id = ?`, id, session); err != nil {
					return err
				}
			}
			req.TaskID = id
		case "checkpoint":
			t, ok, err := c.loadTask(ctx, tx, s, req.TaskID)
			if err != nil {
				return err
			}
			if !ok {
				return errRef
			}
			if req.Checkpoint == nil {
				return contract.Errf(contract.CodeInvalid, "checkpoint fields are required")
			}
			if child {
				// A child conclusion stays attributed and never replaces the parent's checkpoint.
				accepted := false
				out.Accepted = &accepted
				out.CheckpointID = newID("ckp_")
				_, err := tx.ExecContext(ctx, `INSERT INTO checkpoints (id, task_id, revision, accepted, body, origin, session_id, created_at)
					VALUES (?, ?, ?, 0, ?, ?, ?, ?)`, out.CheckpointID, t.ID, t.Revision, mustJSON(req.Checkpoint), origin, session, c.now())
				if err != nil {
					return err
				}
				break
			}
			if req.ExpectedRevision != t.Revision {
				return &contract.Error{Code: contract.CodeConflict, Message: "task revision changed; reload and merge before writing", CurrentRevision: t.Revision}
			}
			if closed(t.State) {
				return &contract.Error{Code: contract.CodeConflict, Message: "task is closed; reopen it explicitly before checkpointing", CurrentRevision: t.Revision}
			}
			prev, err := latestCheckpoint(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			next := merge(prev, req.Checkpoint)
			accepted := true
			out.Accepted = &accepted
			out.CheckpointID = newID("ckp_")
			if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints (id, task_id, revision, accepted, body, origin, session_id, created_at)
				VALUES (?, ?, ?, 1, ?, ?, NULLIF(?, ''), ?)`, out.CheckpointID, t.ID, t.Revision+1, mustJSON(next), origin, session, c.now()); err != nil {
				return err
			}
			if err := bumpTask(ctx, tx, t.ID, t.Revision, t.State, c.now()); err != nil {
				return err
			}
			if session != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE sessions SET task_id = ? WHERE id = ?`, t.ID, session); err != nil {
					return err
				}
			}
		case "transition":
			t, ok, err := c.loadTask(ctx, tx, s, req.TaskID)
			if err != nil {
				return err
			}
			if !ok {
				return errRef
			}
			if err := boundedText("reason", req.Reason, 500, true); err != nil {
				return err
			}
			if child {
				return contract.Errf(contract.CodeForbidden, "child sessions cannot change the parent's task state")
			}
			if req.ExpectedRevision != t.Revision {
				return &contract.Error{Code: contract.CodeConflict, Message: "task revision changed; reload before transitioning", CurrentRevision: t.Revision}
			}
			if !transitions[t.State][req.To] {
				return &contract.Error{Code: contract.CodeConflict, Message: "transition not allowed from the current state", CurrentRevision: t.Revision}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO transitions (task_id, revision, from_state, to_state, reason, origin, at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, t.ID, t.Revision+1, t.State, req.To, req.Reason, origin, c.now()); err != nil {
				return err
			}
			if err := bumpTask(ctx, tx, t.ID, t.Revision, req.To, c.now()); err != nil {
				return err
			}
		}
		return store.BumpProject(ctx, tx, s.ProjectID)
	})
	if err != nil {
		return contract.TaskResult{}, err
	}
	t, _, err := c.loadTask(ctx, c.store.Read(), s, req.TaskID)
	if err != nil {
		return out, err
	}
	out.Task = &t
	out.Durable = true
	return out, nil
}

func bumpTask(ctx context.Context, tx *sql.Tx, id string, revision int64, state, now string) error {
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET revision = revision + 1, state = ?, updated_at = ? WHERE id = ? AND revision = ?`,
		state, now, id, revision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &contract.Error{Code: contract.CodeConflict, Message: "task revision changed concurrently"}
	}
	return nil
}

func (c *Core) listTasks(ctx context.Context, s Scope) (contract.TaskResult, error) {
	rows, err := c.store.Read().QueryContext(ctx, `SELECT id FROM tasks WHERE project_id = ? ORDER BY
		CASE state WHEN 'active' THEN 0 WHEN 'reopened' THEN 0 WHEN 'blocked' THEN 1 ELSE 2 END, updated_at DESC LIMIT 50`, s.ProjectID)
	if err != nil {
		return contract.TaskResult{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return contract.TaskResult{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := contract.TaskResult{Tasks: []contract.Task{}, Durable: true}
	for _, id := range ids {
		t, _, err := c.loadTask(ctx, c.store.Read(), s, id)
		if err != nil {
			return out, err
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out, nil
}

// Resume builds the working context of one explicitly selected task afresh
// from its accepted checkpoint and currently applicable evidence. It never
// concatenates earlier summaries and never picks a task on its own.
func (c *Core) Resume(ctx context.Context, caller Caller, req contract.ResumeRequest) (*contract.ResumeResult, int, error) {
	if !caller.Has(contract.RoleReader) {
		return nil, 0, errForbidden
	}
	budget, err := contract.Budget(req.MaxBytes)
	if err != nil {
		return nil, 0, err
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return nil, 0, err
	}
	res, err := c.buildResume(ctx, s, req.TaskID, budget)
	if err != nil {
		return nil, 0, err
	}
	if caller.Has(contract.RoleCapture) {
		// Remember the explicit selection so compaction rebuilds this task.
		db := c.store.Read()
		if session, child := c.sessionInfo(ctx, db, s, req.Session); session != "" && !child {
			_ = c.write(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE sessions SET task_id = ? WHERE id = ?`, req.TaskID, session)
				return err
			})
		}
	}
	return res, budget, nil
}

func (c *Core) buildResume(ctx context.Context, s Scope, taskID string, budget int) (*contract.ResumeResult, error) {
	db := c.store.Read()
	t, ok, err := c.loadTask(ctx, db, s, taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, contract.Errf(contract.CodeNotFound, "no such task in the caller's scope")
	}
	cp, err := latestCheckpoint(ctx, db, t.ID)
	if err != nil {
		return nil, err
	}
	ev, err := c.evaluator(ctx)
	if err != nil {
		return nil, err
	}
	res := &contract.ResumeResult{Scope: s.info(), Task: t, NextAction: cp.NextAction, Lines: []contract.Line{},
		Totals: map[string]int{}, Complete: true, Notice: Notice, Budget: contract.BudgetInfo{MaxBytes: budget, Unit: contract.BudgetUnit}}
	res.Authority = "No project progress file is referenced; this checkpoint is the task record."
	if t.ProgressRef != "" {
		var ref string
		_ = db.QueryRowContext(ctx, `SELECT progress_ref FROM tasks WHERE id = ?`, t.ID).Scan(&ref)
		var p progressRef
		_ = json.Unmarshal([]byte(ref), &p)
		f := ev.file(ev.roots[t.Worktree], p.Path)
		state := "unchanged since the task referenced it"
		switch {
		case f.reason != "":
			state = "unavailable"
		case f.digest != p.SHA256:
			state = "changed since the task referenced it; read it before acting"
		}
		res.Authority = "Project progress file " + p.Path + " is authoritative (" + state + "); Ichnora references it."
	}
	if closed(t.State) {
		res.NextAction = ""
		res.Lines = append(res.Lines, contract.Line{Section: "state", Text: "Task is " + t.State + "; it is not active work. Reopen it explicitly to continue."})
	}
	add := func(section, text, ref, state string, reasons []string) {
		text, _ = contract.Truncate(text, 600)
		res.Lines = append(res.Lines, contract.Line{Section: section, Text: text, Ref: ref, State: state, Reasons: reasons})
		res.Totals[section]++
	}
	// Decisions: claims linked to the task plus those named by the checkpoint.
	ids := map[string]bool{}
	for _, d := range cp.Decisions {
		ids[d] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM claims WHERE task_id = ? AND project_id = ? LIMIT 200`, t.ID, s.ProjectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
	}
	rows.Close()
	type decided struct {
		group string
		line  contract.Line
		at    string
	}
	var claims []decided
	for id := range ids {
		if _, ok, err := inScopeClaim(ctx, db, s, id); err != nil || !ok {
			continue
		}
		cl, err := scanClaim(db.QueryRowContext(ctx, `SELECT `+claimColumns+` FROM claims c WHERE c.id = ?`, id))
		if err != nil {
			return nil, err
		}
		refs, err := loadRefs(ctx, db, id)
		if err != nil {
			return nil, err
		}
		v := ev.claim(cl.Kind, cl.Worktree, cl.Expires, depsOf(refs))
		conflicts, err := conflictsOf(ctx, db, s, id)
		if err != nil {
			return nil, err
		}
		group := classify(cl.Status, v.state, len(conflicts))
		text := cl.Title
		if cl.Body != "" {
			text += ": " + cl.Body
		}
		section := map[string]string{"conflict": "conflict", "working": "decision", "lead": "lead", "history": "history"}[group]
		if group == "working" && cl.Kind != "decision" {
			section = "fact"
		}
		reasons := append([]string{"status " + cl.Status, "applicability " + v.state}, firstN(v.reasons, 2)...)
		if cl.SupersededBy != "" {
			reasons = append(reasons, "replaced by "+cl.SupersededBy)
		}
		if len(conflicts) > 0 {
			reasons = append(reasons, "conflicts with "+strings.Join(conflicts, ", "))
		}
		text, _ = contract.Truncate(text, 600)
		claims = append(claims, decided{group, contract.Line{Section: section, Text: text, Ref: id, State: v.state, Reasons: reasons}, cl.Created})
	}
	sort.Slice(claims, func(i, j int) bool {
		if groupOrder[claims[i].group] != groupOrder[claims[j].group] {
			return groupOrder[claims[i].group] < groupOrder[claims[j].group]
		}
		return claims[i].at > claims[j].at
	})
	emitClaims := func(group string) {
		for _, d := range claims {
			if d.group == group {
				res.Lines = append(res.Lines, d.line)
				res.Totals[d.line.Section]++
			}
		}
	}
	emitClaims("conflict")
	if cp.Objective != "" {
		add("objective", cp.Objective, "", "", nil)
	}
	open := !closed(t.State)
	for _, b := range cp.Blockers {
		if open {
			add("blocker", b, "", "", nil)
		}
	}
	for _, r := range cp.Remaining {
		if open {
			add("remaining", r, "", "", nil)
		} else {
			res.Totals["closed_remaining_omitted"]++
		}
	}
	emitClaims("working")
	var stale []contract.Verification
	for _, v := range cp.Verification {
		verdict := ev.inputs(t.Worktree, v.Inputs)
		if verdict.state == ApCurrent {
			add("check", v.Check+" "+v.Outcome, "", "applicable", firstN(verdict.reasons, 3))
		} else {
			stale = append(stale, v)
			res.Totals["checks_not_applicable"]++
		}
	}
	for _, cons := range cp.Constraints {
		add("constraint", cons, "", "", nil)
	}
	emitClaims("lead")
	for _, done := range cp.Completed {
		add("completed", done, "", "", nil)
	}
	var children []struct{ id, body, session string }
	crow, err := db.QueryContext(ctx, `SELECT id, body, coalesce(session_id, '') FROM checkpoints WHERE task_id = ? AND accepted = 0
		ORDER BY created_at DESC LIMIT 10`, t.ID)
	if err != nil {
		return nil, err
	}
	for crow.Next() {
		var ch struct{ id, body, session string }
		if err := crow.Scan(&ch.id, &ch.body, &ch.session); err != nil {
			crow.Close()
			return nil, err
		}
		children = append(children, ch)
	}
	crow.Close()
	for _, ch := range children {
		var body contract.Checkpoint
		_ = json.Unmarshal([]byte(ch.body), &body)
		summary := body.NextAction
		if summary == "" && len(body.Completed) > 0 {
			summary = strings.Join(body.Completed, "; ")
		}
		add("child_conclusion", "Unaccepted child conclusion from "+ch.session+": "+summary, ch.id, "unaccepted", nil)
	}
	emitClaims("history")
	for _, v := range stale {
		add("history", "Check "+v.Check+" "+v.Outcome+" ran against different inputs; not applicable now", "", "stale", nil)
	}
	return res, nil
}
