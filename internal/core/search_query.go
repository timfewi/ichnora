package core

import (
	"context"
	"strings"

	"github.com/timfewi/ichnora/internal/contract"
)

// candidateBatch bounds each database fetch, not the set eligible for retrieval.
// Every scoped match is visited before view filtering, ranking and pagination.
const candidateBatch = 500

// A broad request fails explicitly rather than returning a silently incomplete
// ranked set. Callers can narrow task, kind, session, terms or time range.
const maxSearchCandidates = 10000

var errSearchTooBroad = contract.Errf(contract.CodeInvalid, "search exceeds 10000 matching records; narrow the query or filters")

func walkClaims(ctx context.Context, q querier, s Scope, req contract.SearchRequest, match, since, until string, visit func(claimRow) error) error {
	var where []string
	var args []any
	from := `claims c`
	if match != "" {
		from = `claims_fts JOIN claims c ON c.rowid = claims_fts.rowid`
		where = append(where, `claims_fts MATCH ?`)
		args = append(args, match)
	}
	where = append(where, `(c.project_id = ? OR c.id IN (SELECT claim_id FROM shares WHERE to_project = ? AND revoked_at IS NULL))`)
	args = append(args, s.ProjectID, s.ProjectID)
	if len(req.Kinds) > 0 {
		var kinds []string
		for _, kind := range req.Kinds {
			if kind != "source" {
				kinds = append(kinds, "?")
				args = append(args, kind)
			}
		}
		if len(kinds) == 0 {
			return nil
		}
		where = append(where, `c.kind IN (`+strings.Join(kinds, ",")+`)`)
	}
	if req.TaskID != "" {
		where = append(where, `c.task_id = ?`)
		args = append(args, req.TaskID)
	}
	if req.SessionID != "" {
		where = append(where, `json_extract(c.origin, '$.session') = ?`)
		args = append(args, req.SessionID)
	}
	if since != "" {
		where = append(where, `coalesce(c.event_time, c.created_at) >= ?`)
		args = append(args, since)
	}
	if until != "" {
		where = append(where, `coalesce(c.event_time, c.created_at) <= ?`)
		args = append(args, until)
	}
	query := `SELECT ` + claimColumns + ` FROM ` + from + ` WHERE ` + strings.Join(where, " AND ") + ` AND c.rowid > ? ORDER BY c.rowid LIMIT ?`
	var after int64
	visited := 0
	for {
		batchArgs := append(append([]any(nil), args...), after, candidateBatch)
		rows, err := q.QueryContext(ctx, query, batchArgs...)
		if err != nil {
			return err
		}
		var batch []claimRow
		for rows.Next() {
			cl, err := scanClaim(rows)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, cl)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Release the cursor before evaluating references on the same snapshot.
		for _, cl := range batch {
			visited++
			if visited > maxSearchCandidates {
				return errSearchTooBroad
			}
			if err := visit(cl); err != nil {
				return err
			}
			after = cl.RowID
		}
		if len(batch) < candidateBatch {
			return nil
		}
	}
}

func turnCandidates(ctx context.Context, q querier, s Scope, req contract.SearchRequest, match, since, until string, terms []string) ([]ranked, error) {
	where := []string{`turns_fts MATCH ?`, `t.project_id = ?`, `t.invalid = ''`}
	args := []any{match, s.ProjectID}
	if req.TaskID != "" {
		where = append(where, `x.task_id = ?`)
		args = append(args, req.TaskID)
	}
	if req.SessionID != "" {
		where = append(where, `t.session_id = ?`)
		args = append(args, req.SessionID)
	}
	if since != "" {
		where = append(where, `t.event_time >= ?`)
		args = append(args, since)
	}
	if until != "" {
		where = append(where, `t.event_time <= ?`)
		args = append(args, until)
	}
	query := `SELECT t.rowid, t.id, t.session_id, t.text, t.truncated, coalesce(t.event_time, ''), t.digest,
		coalesce(t.prev_id, ''), coalesce(t.next_id, ''), x.harness FROM turns_fts JOIN turns t ON t.rowid = turns_fts.rowid
		JOIN sessions x ON x.id = t.session_id WHERE ` + strings.Join(where, " AND ") + ` AND t.rowid > ? ORDER BY t.rowid LIMIT ?`
	var out []ranked
	var after int64
	for {
		batchArgs := append(append([]any(nil), args...), after, candidateBatch)
		rows, err := q.QueryContext(ctx, query, batchArgs...)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var id, session, text, at, digest, prev, next, harness string
			var truncated bool
			if err := rows.Scan(&after, &id, &session, &text, &truncated, &at, &digest, &prev, &next, &harness); err != nil {
				rows.Close()
				return nil, err
			}
			count++
			if len(out) >= maxSearchCandidates {
				rows.Close()
				return nil, errSearchTooBroad
			}
			n, _ := coverage(terms, text)
			pv, trunc := preview(text)
			item := contract.Item{ID: id, Group: "source", Kind: "turn", Preview: pv, Truncated: trunc || truncated,
				Status: "observed", Applicability: "historical", Reasons: []string{"terms matched: " + itoa(n) + "/" + itoa(len(terms)),
					"historical session record; not validated as current"},
				Provenance: contract.Provenance{Origin: "ingest:" + harness + " " + session, CreatedAt: at, EventTime: at,
					Support: 1, SourceDigest: digest}}
			for _, nb := range []string{prev, next} {
				if nb != "" {
					item.Neighbors = append(item.Neighbors, nb)
				}
			}
			out = append(out, ranked{item: item, group: "source", score: 4 * float64(n) / float64(max(len(terms), 1)), created: at})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if count < candidateBatch {
			return out, nil
		}
	}
}
