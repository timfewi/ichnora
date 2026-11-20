package core

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/store"
)

var simKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)

func (c *Core) Sim(ctx context.Context, caller Caller, req contract.SimRequest) (any, int, error) {
	if !simKey.MatchString(req.Problem) {
		return nil, 0, contract.Errf(contract.CodeInvalid, "problem must be a stable identifier of up to 80 characters")
	}
	if req.Action == "history" {
		if !caller.Has(contract.RoleReader) {
			return nil, 0, errForbidden
		}
		if req.Attempt != "" || req.SimProposal != (contract.SimProposal{}) || req.Outcome != "" || req.Evidence != "" {
			return nil, 0, contract.Errf(contract.CodeInvalid, "history only accepts a problem and paging options")
		}
		return c.simHistory(ctx, caller, req)
	}
	if !caller.Has(contract.RoleCapture) {
		return nil, 0, errForbidden
	}
	if !simKey.MatchString(req.Attempt) || req.Offset != 0 || req.Limit != 0 || req.MaxBytes != 0 {
		return nil, 0, contract.Errf(contract.CodeInvalid, "writes require a stable attempt identifier and no paging options")
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return nil, 0, err
	}
	var evidenceSHA string
	switch req.Action {
	case "propose":
		if req.Outcome != "" || req.Evidence != "" {
			return nil, 0, contract.Errf(contract.CodeInvalid, "record the prediction before submitting the observation")
		}
		for _, field := range []struct{ name, value string }{
			{"cause", req.Cause}, {"change", req.Change}, {"prediction", req.Prediction}, {"context", req.Context},
		} {
			if err := boundedText(field.name, field.value, 1200, true); err != nil {
				return nil, 0, err
			}
		}
	case "observe":
		if req.SimProposal != (contract.SimProposal{}) {
			return nil, 0, contract.Errf(contract.CodeInvalid, "observations cannot modify the proposal")
		}
		if err := boundedText("outcome", req.Outcome, 1200, true); err != nil {
			return nil, 0, err
		}
		ref, err := c.prepareRef(s, "support", contract.Ref{Kind: "file", Path: req.Evidence})
		if err != nil {
			return nil, 0, err
		}
		evidenceSHA = ref.detail.SHA256
	default:
		return nil, 0, contract.Errf(contract.CodeInvalid, "action must be propose, observe or history")
	}
	err = c.write(ctx, func(tx *sql.Tx) error {
		current, ok, err := c.resolvePath(ctx, tx, caller.Cwd)
		if err != nil {
			return err
		}
		if !ok || current.ProjectID != s.ProjectID || current.Worktree != s.Worktree {
			return errNoScope
		}
		var before contract.SimAttempt
		query := `SELECT cause, intervention, prediction, context, outcome, evidence, evidence_sha256, evidence_worktree FROM sim_attempts
			WHERE project_id = ? AND problem = ? AND attempt = ?`
		err = tx.QueryRowContext(ctx, query, s.ProjectID, req.Problem, req.Attempt).Scan(&before.Cause, &before.Change,
			&before.Prediction, &before.Context, &before.Outcome, &before.Evidence, &before.EvidenceSHA256, &before.EvidenceWorktree)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		switch {
		case req.Action == "propose" && !missing:
			if before.SimProposal == req.SimProposal {
				return nil
			}
			return contract.Errf(contract.CodeConflict, "attempt already has a different proposal")
		case req.Action == "observe" && missing:
			return contract.Errf(contract.CodeNotFound, "propose this attempt before recording an outcome")
		case req.Action == "observe" && before.Outcome != "":
			if before.Outcome == req.Outcome && before.Evidence == req.Evidence && before.EvidenceSHA256 == evidenceSHA && before.EvidenceWorktree == s.Worktree {
				return nil
			}
			return contract.Errf(contract.CodeConflict, "attempt already has a different observation")
		}
		if err := c.checkCapacity(); err != nil {
			return err
		}
		if req.Action == "propose" {
			_, err = tx.ExecContext(ctx, `INSERT INTO sim_attempts (project_id, problem, attempt, cause, intervention, prediction, context, proposed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, s.ProjectID, req.Problem, req.Attempt, req.Cause, req.Change, req.Prediction, req.Context, c.now())
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE sim_attempts SET outcome = ?, evidence = ?, evidence_sha256 = ?, evidence_worktree = ?, observed_at = ?
				WHERE project_id = ? AND problem = ? AND attempt = ?`, req.Outcome, req.Evidence, evidenceSHA, s.Worktree, c.now(), s.ProjectID, req.Problem, req.Attempt)
		}
		if err != nil {
			return err
		}
		return store.BumpProject(ctx, tx, s.ProjectID)
	})
	return map[string]string{"problem": req.Problem, "attempt": req.Attempt}, 0, err
}

func (c *Core) simHistory(ctx context.Context, caller Caller, req contract.SimRequest) (any, int, error) {
	budget, err := contract.Budget(req.MaxBytes)
	if err != nil {
		return nil, 0, err
	}
	if req.Offset < 0 || req.Limit < 0 || req.Limit > 50 {
		return nil, 0, contract.Errf(contract.CodeInvalid, "offset must be nonnegative and limit at most 50")
	}
	if req.Limit == 0 {
		req.Limit = 10
	}
	tx, err := c.store.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	s, ok, err := c.resolvePath(ctx, tx, caller.Cwd)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, errNoScope
	}
	out := &contract.SimHistory{Problem: req.Problem, Offset: req.Offset, Attempts: []contract.SimAttempt{}}
	rows, err := tx.QueryContext(ctx, `SELECT attempt, cause, intervention, prediction, context, proposed_at, outcome, evidence,
		evidence_sha256, evidence_worktree, observed_at FROM sim_attempts WHERE project_id = ? AND problem = ? ORDER BY id LIMIT ? OFFSET ?`,
		s.ProjectID, req.Problem, req.Limit+1, req.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Attempts) == req.Limit {
			out.NextOffset = req.Offset + req.Limit
			break
		}
		var a contract.SimAttempt
		if err := rows.Scan(&a.Attempt, &a.Cause, &a.Change, &a.Prediction, &a.Context, &a.ProposedAt, &a.Outcome,
			&a.Evidence, &a.EvidenceSHA256, &a.EvidenceWorktree, &a.ObservedAt); err != nil {
			return nil, 0, err
		}
		out.Attempts = append(out.Attempts, a)
	}
	return out, budget, rows.Err()
}
