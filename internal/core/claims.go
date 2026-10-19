package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
	"github.com/timfewi/ichnora/internal/store"
)

var claimKinds = map[string]bool{"observation": true, "decision": true, "summary": true, "note": true}

// Sticky statuses are set by explicit actions and survive recomputation.
var sticky = map[string]bool{"superseded": true, "invalidated": true, "disputed": true}

func active(status string) bool { return status == "observed" || status == "inferred" }

type preparedRef struct {
	role, kind, ref, root string
	detail                contract.Ref
	state, lost           string
}

type preparedClaim struct {
	in      contract.ClaimInput
	refs    []preparedRef
	expires string
	evtTime string
}

func boundedText(field, s string, limit int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return contract.Errf(contract.CodeInvalid, "%s is required", field)
	}
	if len(s) > limit {
		return contract.Errf(contract.CodeInvalid, "%s exceeds %d bytes", field, limit)
	}
	return nil
}

func parseTime(field, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", contract.Errf(contract.CodeInvalid, "%s must be RFC 3339", field)
	}
	return t.UTC().Format(contract.TimeFormat), nil
}

// prepare validates a claim and resolves file digests outside any transaction.
func (c *Core) prepare(ctx context.Context, s Scope, in contract.ClaimInput) (preparedClaim, error) {
	p := preparedClaim{in: in}
	if !claimKinds[in.Kind] {
		return p, contract.Errf(contract.CodeInvalid, "kind must be observation, decision, summary or note")
	}
	if err := boundedText("title", in.Title, 200, true); err != nil {
		return p, err
	}
	if err := boundedText("body", in.Body, 8192, false); err != nil {
		return p, err
	}
	if err := boundedText("rationale", in.Rationale, 4096, false); err != nil {
		return p, err
	}
	if len(in.Alternatives) > 10 || len(in.Support) > 20 || len(in.Depends) > 20 ||
		len(in.Supersedes) > 10 || len(in.Contradicts) > 10 || len(in.DerivedFrom) > 20 {
		return p, contract.Errf(contract.CodeInvalid, "too many alternatives or references")
	}
	for _, a := range in.Alternatives {
		if err := boundedText("alternative", a, 500, false); err != nil {
			return p, err
		}
	}
	switch in.Status {
	case "":
		p.in.Status = "observed"
		if in.Kind == "summary" {
			p.in.Status = "inferred"
		}
	case "observed", "inferred":
	default:
		return p, contract.Errf(contract.CodeInvalid, "captured status must be observed or inferred")
	}
	if in.Kind == "summary" && p.in.Transform == "" {
		p.in.Transform = "agent-summary/1"
	}
	if len(p.in.Transform) > 64 {
		return p, contract.Errf(contract.CodeInvalid, "transform exceeds 64 bytes")
	}
	var err error
	if p.expires, err = parseTime("expires_at", in.ExpiresAt); err != nil {
		return p, err
	}
	if p.evtTime, err = parseTime("event_time", in.EventTime); err != nil {
		return p, err
	}
	for _, r := range in.Support {
		pr, err := c.prepareRef(s, "support", r)
		if err != nil {
			return p, err
		}
		p.refs = append(p.refs, pr)
	}
	for _, r := range in.Depends {
		pr, err := c.prepareRef(s, "depends", r)
		if err != nil {
			return p, err
		}
		p.refs = append(p.refs, pr)
	}
	for _, id := range in.DerivedFrom {
		p.refs = append(p.refs, preparedRef{role: "support", kind: "claim", ref: id, detail: contract.Ref{Kind: "claim", Ref: id}, state: "ok"})
	}
	return p, nil
}

func (c *Core) prepareRef(s Scope, role string, r contract.Ref) (preparedRef, error) {
	pr := preparedRef{role: role, kind: r.Kind, detail: r, state: "ok"}
	for _, f := range []string{r.Ref, r.Path, r.Locator, r.Value, r.Card, r.Section, r.Symbol, r.Snapshot} {
		if len(f) > 512 {
			return pr, contract.Errf(contract.CodeInvalid, "reference field exceeds 512 bytes")
		}
	}
	switch {
	case role == "support" && (r.Kind == "event" || r.Kind == "turn" || r.Kind == "claim" || r.Kind == "checkpoint"):
		if r.Ref == "" {
			return pr, contract.Errf(contract.CodeInvalid, "%s reference needs ref", r.Kind)
		}
		pr.ref = r.Ref
	case r.Kind == "file" || r.Kind == "ast":
		abs, err := c.pol.Source(s.Root, r.Path)
		if err != nil {
			return pr, err
		}
		cur, err := digestFile(abs)
		if err != nil {
			return pr, contract.Errf(contract.CodeInvalid, "file cannot be digested")
		}
		if r.SHA256 == "" {
			pr.detail.SHA256 = cur
		} else if r.SHA256 != cur && role == "depends" {
			return pr, contract.Errf(contract.CodeInvalid, "dependency digest differs from the current file; capture the observed digest")
		}
		pr.ref = r.Path
		if r.Kind == "ast" && r.Symbol != "" {
			pr.ref = r.Path + "#" + r.Symbol
		}
		pr.root = r.Kind + ":" + r.Path + "@" + pr.detail.SHA256
	case role == "depends" && r.Kind == "revision":
		v := r.Value
		if v == "" || v == "current" {
			head, err := gitHead(s.Root)
			if err != nil {
				return pr, contract.Errf(contract.CodeInvalid, "current revision is unavailable")
			}
			v = head
		}
		pr.detail.Value, pr.ref = v, v
	case role == "depends" && r.Kind == "environment":
		epoch := r.Epoch
		if epoch == "" || epoch == "current" {
			env, _ := c.environment()
			if env == nil {
				return pr, contract.Errf(contract.CodeInvalid, "no host environment descriptor; the dependency would be unknown")
			}
			epoch = env.ActivationEpoch
		}
		pr.detail.Epoch, pr.ref = epoch, epoch
	case r.Kind == "tabula":
		if r.Card == "" || r.Section == "" || r.Fingerprint == "" {
			return pr, contract.Errf(contract.CodeInvalid, "tabula references need card, section and fingerprint")
		}
		pr.ref = r.Card + "#" + r.Section
		pr.root = "tabula:" + pr.ref + "@" + r.Fingerprint
	case role == "support" && (r.Kind == "check" || r.Kind == "external"):
		key := r.Ref + r.Locator
		if key == "" {
			return pr, contract.Errf(contract.CodeInvalid, "%s reference needs ref or locator", r.Kind)
		}
		pr.ref = key
		pr.root = r.Kind + ":" + key
	default:
		return pr, contract.Errf(contract.CodeInvalid, "unsupported %s reference kind", role)
	}
	return pr, nil
}

// inScopeClaim reports whether a claim is visible to the scope: owned by the
// project or shared into it by an unrevoked grant.
func inScopeClaim(ctx context.Context, q querier, s Scope, id string) (owner string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT project_id FROM claims WHERE id = ? AND (project_id = ?
		OR id IN (SELECT claim_id FROM shares WHERE to_project = ? AND revoked_at IS NULL))`, id, s.ProjectID, s.ProjectID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return owner, err == nil, err
}

var errRef = contract.Errf(contract.CodeOutOfScope, "a referenced record does not exist in the caller's scope")

// bindRef checks a source reference inside the transaction and derives its
// independence root and state.
func bindRef(ctx context.Context, tx *sql.Tx, s Scope, pr *preparedRef) error {
	var project sql.NullString
	var flags, invalid string
	switch pr.kind {
	case "event":
		if err := tx.QueryRowContext(ctx, `SELECT project_id, flags, invalid FROM events WHERE id = ?`, pr.ref).Scan(&project, &flags, &invalid); err != nil {
			return errRef
		}
		var f ingest.Flags
		_ = json.Unmarshal([]byte(flags), &f)
		if f.Derived {
			pr.state, pr.lost = "lost", "derived or injected context cannot corroborate"
		}
		pr.root = "event:" + pr.ref
	case "turn":
		if err := tx.QueryRowContext(ctx, `SELECT project_id, invalid FROM turns WHERE id = ?`, pr.ref).Scan(&project, &invalid); err != nil {
			return errRef
		}
		pr.root = "turn:" + pr.ref
	case "checkpoint":
		if err := tx.QueryRowContext(ctx, `SELECT t.project_id FROM checkpoints k JOIN tasks t ON t.id = k.task_id WHERE k.id = ?`, pr.ref).Scan(&project); err != nil {
			return errRef
		}
		pr.root = "checkpoint:" + pr.ref
	case "claim":
		if _, ok, err := inScopeClaim(ctx, tx, s, pr.ref); err != nil {
			return err
		} else if !ok {
			return errRef
		}
		return nil
	default:
		return nil
	}
	if !project.Valid || project.String != s.ProjectID {
		return errRef
	}
	if invalid != "" {
		pr.state, pr.lost = "lost", "source invalidated: "+invalid
	}
	return nil
}

// Capture records claims with provenance, references and supersession links.
func (c *Core) Capture(ctx context.Context, caller Caller, req contract.CaptureRequest) (contract.CaptureResult, error) {
	out := contract.CaptureResult{IDs: []string{}}
	if !caller.Has(contract.RoleCapture) {
		return out, errForbidden
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return out, err
	}
	if len(req.Claims) == 0 || len(req.Claims) > 20 {
		return out, contract.Errf(contract.CodeInvalid, "capture between 1 and 20 claims")
	}
	if err := c.checkCapacity(); err != nil {
		return out, err
	}
	prepared := make([]preparedClaim, len(req.Claims))
	for i, in := range req.Claims {
		if prepared[i], err = c.prepare(ctx, s, in); err != nil {
			return out, err
		}
	}
	err = c.write(ctx, func(tx *sql.Tx) error {
		session := c.hintedSession(ctx, tx, s, req.Session)
		origin := mustJSON(map[string]string{"role": "capture", "session": session, "via": "service"})
		var touched []string
		for _, p := range prepared {
			id := newID("clm_")
			if p.in.TaskID != "" {
				var project string
				if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, p.in.TaskID).Scan(&project); err != nil || project != s.ProjectID {
					return errRef
				}
			}
			alts := mustJSON(nonNil(p.in.Alternatives))
			if _, err := tx.ExecContext(ctx, `INSERT INTO claims (id, project_id, worktree_id, task_id, kind, title, body, rationale,
				alternatives, asserted, status, origin, lineage, transform, created_at, event_time, expires_at)
				VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))`,
				id, s.ProjectID, s.Worktree, p.in.TaskID, p.in.Kind, p.in.Title, p.in.Body, p.in.Rationale, alts,
				p.in.Status, p.in.Status, origin, "self:"+id, p.in.Transform, c.now(), p.evtTime, p.expires); err != nil {
				return err
			}
			for i := range p.refs {
				pr := &p.refs[i]
				if err := bindRef(ctx, tx, s, pr); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO refs (claim_id, role, kind, ref, detail, root, state, lost_reason)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, pr.role, pr.kind, pr.ref, mustJSON(pr.detail), pr.root, pr.state, pr.lost); err != nil {
					return err
				}
				if pr.kind == "claim" && pr.role == "support" {
					if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO links (from_id, to_id, relation) VALUES (?, ?, 'derived_from')`, id, pr.ref); err != nil {
						return err
					}
				}
			}
			for _, old := range p.in.Supersedes {
				var project, status string
				if err := tx.QueryRowContext(ctx, `SELECT project_id, status FROM claims WHERE id = ?`, old).Scan(&project, &status); err != nil || project != s.ProjectID {
					return errRef
				}
				if _, err := tx.ExecContext(ctx, `UPDATE claims SET status = 'superseded', superseded_by = ?, status_reason = ? WHERE id = ?`,
					id, "superseded by "+id, old); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO links (from_id, to_id, relation) VALUES (?, ?, 'supersedes')`, id, old); err != nil {
					return err
				}
				touched = append(touched, old)
			}
			for _, other := range p.in.Contradicts {
				if _, ok, err := inScopeClaim(ctx, tx, s, other); err != nil {
					return err
				} else if !ok {
					return errRef
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO links (from_id, to_id, relation) VALUES (?, ?, 'contradicts')`, id, other); err != nil {
					return err
				}
			}
			out.IDs = append(out.IDs, id)
			touched = append(touched, id)
		}
		if _, err := c.recompute(ctx, tx, touched); err != nil {
			return err
		}
		return store.BumpProject(ctx, tx, s.ProjectID)
	})
	if err != nil {
		out.IDs = []string{}
		return out, err
	}
	out.Durable = true
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// hintedSession accepts a session hint only for a known session attributed
// to the caller's project; otherwise it is ignored.
func (c *Core) hintedSession(ctx context.Context, q querier, s Scope, hint *contract.SessionHint) string {
	if hint == nil || hint.NativeID == "" {
		return ""
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM sessions WHERE harness = ? AND native_id = ? AND project_id = ?`,
		hint.Harness, hint.NativeID, s.ProjectID).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// supportRoots returns the independent support roots of a claim. Summaries
// inherit their sources' roots, so repetition never adds corroboration.
func supportRoots(ctx context.Context, q querier, id string, depth int, memo map[string][]string) ([]string, error) {
	if r, ok := memo[id]; ok {
		return r, nil
	}
	if depth > 8 {
		return nil, nil
	}
	var kind, status string
	if err := q.QueryRowContext(ctx, `SELECT kind, status FROM claims WHERE id = ?`, id).Scan(&kind, &status); err != nil {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT kind, ref, root, state FROM refs WHERE claim_id = ? AND role = 'support'`, id)
	if err != nil {
		return nil, err
	}
	type ref struct{ kind, ref, root, state string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.kind, &r.ref, &r.root, &r.state); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	set := map[string]bool{}
	if kind == "decision" {
		// A recorded decision is its own observation of having been decided.
		set["self:"+id] = true
	}
	for _, r := range refs {
		if r.state != "ok" {
			continue
		}
		if r.kind == "claim" {
			sub, err := supportRoots(ctx, q, r.ref, depth+1, memo)
			if err != nil {
				return nil, err
			}
			for _, s := range sub {
				set[s] = true
			}
			continue
		}
		if r.root != "" {
			set[r.root] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	memo[id] = out
	return out, nil
}

func lineageOf(id string, roots []string) string {
	if len(roots) == 0 {
		return "self:" + id
	}
	h := sha256.Sum256([]byte(strings.Join(roots, "\n")))
	return hex.EncodeToString(h[:16])
}

// recompute refreshes status and lineage of the given claims and propagates
// support changes to claims derived from them.
func (c *Core) recompute(ctx context.Context, tx *sql.Tx, start []string) ([]string, error) {
	queue := append([]string(nil), start...)
	seen := map[string]int{}
	var changed []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id]++; seen[id] > 4 {
			continue
		}
		var asserted, status, lineage string
		if err := tx.QueryRowContext(ctx, `SELECT asserted, status, lineage FROM claims WHERE id = ?`, id).Scan(&asserted, &status, &lineage); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		roots, err := supportRoots(ctx, tx, id, 0, map[string][]string{})
		if err != nil {
			return nil, err
		}
		next := status
		if !sticky[status] {
			next = asserted
			if len(roots) == 0 {
				next = "unsupported"
			}
		}
		nextLineage := lineageOf(id, roots)
		if next != status || nextLineage != lineage {
			reason := ""
			if next == "unsupported" {
				reason = "no remaining independent support"
			}
			if sticky[next] {
				if _, err := tx.ExecContext(ctx, `UPDATE claims SET lineage = ? WHERE id = ?`, nextLineage, id); err != nil {
					return nil, err
				}
			} else if _, err := tx.ExecContext(ctx, `UPDATE claims SET status = ?, status_reason = ?, lineage = ? WHERE id = ?`, next, reason, nextLineage, id); err != nil {
				return nil, err
			}
			changed = append(changed, id)
		}
		ok := active(next)
		lost := ""
		if !ok {
			lost = "source claim is " + next
		}
		if _, err := tx.ExecContext(ctx, `UPDATE refs SET state = ?, lost_reason = ? WHERE role = 'support' AND kind = 'claim' AND ref = ?`,
			map[bool]string{true: "ok", false: "lost"}[ok], lost, id); err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT claim_id FROM refs WHERE role = 'support' AND kind = 'claim' AND ref = ?`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return nil, err
			}
			queue = append(queue, d)
		}
		rows.Close()
	}
	return changed, nil
}

// Update disputes or invalidates a claim, or invalidates a source record,
// and recomputes everything that depended on it.
func (c *Core) Update(ctx context.Context, caller Caller, req contract.UpdateRequest) (contract.UpdateResult, error) {
	out := contract.UpdateResult{ID: req.ID, Recomputed: []string{}}
	if !caller.Has(contract.RoleCapture) {
		return out, errForbidden
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return out, err
	}
	if req.Status != "disputed" && req.Status != "invalidated" {
		return out, contract.Errf(contract.CodeInvalid, "status must be disputed or invalidated")
	}
	if err := boundedText("reason", req.Reason, 500, true); err != nil {
		return out, err
	}
	err = c.write(ctx, func(tx *sql.Tx) error {
		var affected []string
		switch {
		case strings.HasPrefix(req.ID, "clm_"):
			var project string
			if err := tx.QueryRowContext(ctx, `SELECT project_id FROM claims WHERE id = ?`, req.ID).Scan(&project); err != nil || project != s.ProjectID {
				return errRef
			}
			if _, err := tx.ExecContext(ctx, `UPDATE claims SET status = ?, status_reason = ? WHERE id = ?`, req.Status, req.Reason, req.ID); err != nil {
				return err
			}
			affected = []string{req.ID}
			out.Status = req.Status
		case strings.HasPrefix(req.ID, "evt_") || strings.HasPrefix(req.ID, "trn_"):
			if req.Status != "invalidated" {
				return contract.Errf(contract.CodeInvalid, "source records can only be invalidated")
			}
			table, kind := "events", "event"
			if strings.HasPrefix(req.ID, "trn_") {
				table, kind = "turns", "turn"
			}
			res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET invalid = ? WHERE id = ? AND project_id = ?`, req.Reason, req.ID, s.ProjectID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errRef
			}
			var err2 error
			if affected, err2 = loseSupport(ctx, tx, kind, req.ID, "source invalidated: "+req.Reason); err2 != nil {
				return err2
			}
			out.Status = "invalidated"
		default:
			return contract.Errf(contract.CodeInvalid, "id must name a claim, event or turn")
		}
		changed, err := c.recompute(ctx, tx, affected)
		if err != nil {
			return err
		}
		out.Recomputed = append(out.Recomputed, changed...)
		return store.BumpProject(ctx, tx, s.ProjectID)
	})
	return out, err
}

// loseSupport marks every reference to a source as lost and returns the claims affected.
func loseSupport(ctx context.Context, tx *sql.Tx, kind, ref, reason string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT claim_id FROM refs WHERE kind = ? AND ref = ?`, kind, ref)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	_, err = tx.ExecContext(ctx, `UPDATE refs SET state = 'lost', lost_reason = ? WHERE kind = ? AND ref = ?`, reason, kind, ref)
	return ids, err
}
