package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/store"
)

// Groups in priority order: conflicts must survive small budgets first.
var groupOrder = map[string]int{"conflict": 0, "working": 1, "lead": 2, "source": 3, "history": 4}

// candidateLimit bounds the authorized candidate set before ranking.
const candidateLimit = 500

type claimRow struct {
	ID, Project, Worktree, Task, Kind, Title, Body, Rationale, Status, Reason, Origin, Lineage, Transform, Created string
	EventTime, Validated, Expires, SupersededBy                                                                    string
}

const claimColumns = `c.id, c.project_id, coalesce(c.worktree_id, ''), coalesce(c.task_id, ''), c.kind, c.title, c.body,
	c.rationale, c.status, c.status_reason, c.origin, c.lineage, c.transform, c.created_at, coalesce(c.event_time, ''),
	coalesce(c.validated_at, ''), coalesce(c.expires_at, ''), coalesce(c.superseded_by, '')`

func scanClaim(rows interface{ Scan(...any) error }) (claimRow, error) {
	var r claimRow
	err := rows.Scan(&r.ID, &r.Project, &r.Worktree, &r.Task, &r.Kind, &r.Title, &r.Body, &r.Rationale, &r.Status,
		&r.Reason, &r.Origin, &r.Lineage, &r.Transform, &r.Created, &r.EventTime, &r.Validated, &r.Expires, &r.SupersededBy)
	return r, err
}

type refRow struct {
	Role, Kind, Ref, Root, State, Lost string
	Detail                             contract.Ref
}

func loadRefs(ctx context.Context, q querier, id string) ([]refRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT role, kind, ref, detail, root, state, lost_reason FROM refs WHERE claim_id = ? ORDER BY role, kind, ref`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []refRow
	for rows.Next() {
		var r refRow
		var detail string
		if err := rows.Scan(&r.Role, &r.Kind, &r.Ref, &detail, &r.Root, &r.State, &r.Lost); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(detail), &r.Detail)
		out = append(out, r)
	}
	return out, rows.Err()
}

func depsOf(refs []refRow) []contract.Ref {
	var deps []contract.Ref
	for _, r := range refs {
		if r.Role == "depends" {
			deps = append(deps, r.Detail)
		}
	}
	return deps
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// ftsQuery quotes each term so user input cannot inject FTS5 syntax.
func ftsQuery(q string) ([]string, string) {
	seen := map[string]bool{}
	var terms []string
	for _, w := range wordRE.FindAllString(strings.ToLower(q), 32) {
		if !seen[w] && utf8.RuneCountInString(w) <= 64 {
			seen[w] = true
			terms = append(terms, w)
		}
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	return terms, strings.Join(quoted, " OR ")
}

type ranked struct {
	item    contract.Item
	group   string
	score   float64
	created string
	lineage string
	unknown bool
}

func coverage(terms []string, text string) (int, bool) {
	lower := strings.ToLower(text)
	n := 0
	for _, t := range terms {
		if strings.Contains(lower, t) {
			n++
		}
	}
	return n, n > 0
}

func preview(s string) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	return contract.Truncate(s, contract.PreviewBytes)
}

func originLabel(origin string) string {
	var o map[string]string
	if json.Unmarshal([]byte(origin), &o) != nil {
		return "unknown"
	}
	label := o["role"]
	if o["harness"] != "" {
		label += ":" + o["harness"]
	}
	if o["session"] != "" {
		label += " " + o["session"]
	}
	return label
}

// Search runs scoped, deterministic lexical retrieval. Scope is applied in
// SQL before ranking, previews, counts and pagination; FTS5 is used only as a
// membership filter, so ranking never uses statistics from other projects.
func (c *Core) Search(ctx context.Context, caller Caller, req contract.SearchRequest) (*contract.SearchResult, int, error) {
	if !caller.Has(contract.RoleReader) {
		return nil, 0, errForbidden
	}
	budget, err := contract.Budget(req.MaxBytes)
	if err != nil {
		return nil, 0, err
	}
	switch req.View {
	case "":
		req.View = "working"
	case "working", "history", "all":
	default:
		return nil, 0, contract.Errf(contract.CodeInvalid, "view must be working, history or all")
	}
	if req.Limit == 0 {
		req.Limit = 10
	}
	if req.Limit < 1 || req.Limit > 50 || len(req.Query) > 512 || len(req.Files) > 20 || len(req.Kinds) > 4 {
		return nil, 0, contract.Errf(contract.CodeInvalid, "limit must be 1-50; query, files and kinds are bounded")
	}
	for _, k := range req.Kinds {
		if !claimKinds[k] && k != "source" {
			return nil, 0, contract.Errf(contract.CodeInvalid, "unknown kind filter")
		}
	}
	since, err := parseTime("since", req.Since)
	if err != nil {
		return nil, 0, err
	}
	until, err := parseTime("until", req.Until)
	if err != nil {
		return nil, 0, err
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return nil, 0, err
	}
	db := c.store.Read()
	gen, err := c.scopeGeneration(ctx, db, s)
	if err != nil {
		return nil, 0, err
	}
	scopeKey := c.sign("scope", s.ProjectID, s.Worktree)[:16]
	norm := req
	norm.Cursor, norm.MaxBytes, norm.Session = "", 0, nil
	queryKey := c.sign("query", mustJSON(norm))[:16]
	offset := 0
	if req.Cursor != "" {
		if offset, err = c.decodeCursor(req.Cursor, gen, scopeKey, queryKey); err != nil {
			return nil, 0, err
		}
	}
	terms, match := ftsQuery(req.Query)
	ev, err := c.evaluator(ctx)
	if err != nil {
		return nil, 0, err
	}
	res := &contract.SearchResult{Scope: s.info(), View: req.View, Items: []contract.Item{}, Complete: true,
		Notice: Notice, Budget: contract.BudgetInfo{MaxBytes: budget, Unit: contract.BudgetUnit}}

	wantKind := func(k string) bool {
		if len(req.Kinds) == 0 {
			return true
		}
		for _, w := range req.Kinds {
			if w == k {
				return true
			}
		}
		return false
	}
	var all []ranked
	claims, err := c.claimCandidates(ctx, db, s, req, match, since, until)
	if err != nil {
		return nil, 0, err
	}
	for _, cl := range claims {
		if !wantKind(cl.Kind) {
			continue
		}
		r, err := c.rankClaim(ctx, db, ev, s, cl, req, terms)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, r)
	}
	if req.Query != "" && wantKind("source") && req.View != "history" {
		turns, err := c.turnCandidates(ctx, db, s, req, match, since, until, terms)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, turns...)
	}

	// Separate by group, then collapse duplicate lineages and filter the view.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if groupOrder[a.group] != groupOrder[b.group] {
			return groupOrder[a.group] < groupOrder[b.group]
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.created != b.created {
			return a.created > b.created
		}
		return a.item.ID < b.item.ID
	})
	seenLineage := map[string]int{}
	var visible []ranked
	for _, r := range all {
		if r.lineage != "" && !strings.HasPrefix(r.lineage, "self:") {
			if idx, dup := seenLineage[r.group+r.lineage]; dup {
				res.Omitted.Duplicates++
				visible[idx].item.Reasons = appendOnce(visible[idx].item.Reasons, "duplicate lineage collapsed")
				continue
			}
		}
		switch r.group {
		case "history":
			if r.item.Status == "superseded" || r.item.Status == "invalidated" {
				if req.View == "working" {
					res.Omitted.Replaced++
					continue
				}
			} else if req.View == "working" {
				res.Omitted.Stale++
				continue
			}
		case "working", "lead", "source":
			if req.View == "history" {
				continue
			}
		}
		if r.lineage != "" {
			seenLineage[r.group+r.lineage] = len(visible)
		}
		visible = append(visible, r)
	}
	for _, r := range visible {
		switch r.group {
		case "conflict":
			res.Totals.Conflicts++
		case "working":
			res.Totals.Working++
		case "lead":
			res.Totals.Leads++
		case "source":
			res.Totals.Sources++
		case "history":
			res.Totals.History++
		}
		if r.unknown {
			res.Totals.Unknown++
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events e JOIN sessions x ON x.id = e.session_id
		WHERE x.project_id = ? AND e.project_id IS NULL`, s.ProjectID).Scan(&res.Omitted.Unattributed); err != nil {
		return nil, 0, err
	}
	if offset > len(visible) {
		return nil, 0, errCursor
	}
	end := offset + req.Limit
	if end > len(visible) {
		end = len(visible)
	}
	for _, r := range visible[offset:end] {
		res.Items = append(res.Items, r.item)
	}
	res.Omitted.Page = len(visible) - (end - offset)
	res.Start = offset
	res.Cursor = func(next int) string { return c.encodeCursor(next, gen, scopeKey, queryKey) }
	if end < len(visible) {
		res.Complete = false
		res.NextCursor = res.Cursor(end)
	}
	return res, budget, nil
}

func appendOnce(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

func (c *Core) claimCandidates(ctx context.Context, db *sql.DB, s Scope, req contract.SearchRequest, match, since, until string) ([]claimRow, error) {
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
	rows, err := db.QueryContext(ctx, `SELECT `+claimColumns+` FROM `+from+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY c.created_at DESC, c.id LIMIT ?`, append(args, candidateLimit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimRow
	for rows.Next() {
		r, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// conflictsOf lists unresolved contradictions with other active, visible claims.
func conflictsOf(ctx context.Context, q querier, s Scope, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT o.id FROM links l JOIN claims o ON o.id = CASE WHEN l.from_id = ? THEN l.to_id ELSE l.from_id END
		WHERE l.relation = 'contradicts' AND (l.from_id = ? OR l.to_id = ?) AND o.status IN ('observed', 'inferred', 'disputed')
		AND (o.project_id = ? OR o.id IN (SELECT claim_id FROM shares WHERE to_project = ? AND revoked_at IS NULL))
		ORDER BY o.id`, id, id, id, s.ProjectID, s.ProjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// classify places a claim into a retrieval group.
func classify(status, applicability string, conflicts int) string {
	switch {
	case status == "disputed" || (conflicts > 0 && active(status)):
		return "conflict"
	case status == "superseded" || status == "invalidated":
		return "history"
	case applicability == ApStale || applicability == ApExpired:
		return "history"
	case active(status) && (applicability == ApCurrent || applicability == ApDurable):
		return "working"
	}
	return "lead"
}

func (c *Core) rankClaim(ctx context.Context, q querier, ev *evaluator, s Scope, cl claimRow, req contract.SearchRequest, terms []string) (ranked, error) {
	refs, err := loadRefs(ctx, q, cl.ID)
	if err != nil {
		return ranked{}, err
	}
	v := ev.claim(cl.Kind, cl.Worktree, cl.Expires, depsOf(refs))
	conflicts, err := conflictsOf(ctx, q, s, cl.ID)
	if err != nil {
		return ranked{}, err
	}
	group := classify(cl.Status, v.state, len(conflicts))
	roots, err := supportRoots(ctx, q, cl.ID, 0, map[string][]string{})
	if err != nil {
		return ranked{}, err
	}
	lost := 0
	for _, r := range refs {
		if r.Role == "support" && r.State == "lost" {
			lost++
		}
	}
	text := cl.Title + " " + cl.Body
	pv, trunc := preview(cl.Body)
	if pv == "" {
		pv, trunc = preview(cl.Title)
	}
	item := contract.Item{ID: cl.ID, Group: group, Kind: cl.Kind, Title: cl.Title, Preview: pv, Truncated: trunc,
		Status: cl.Status, Applicability: v.state, Reasons: []string{}, TaskID: cl.Task, ReplacedBy: cl.SupersededBy,
		ConflictsWith: conflicts,
		Provenance: contract.Provenance{Origin: originLabel(cl.Origin), CreatedAt: cl.Created, EventTime: cl.EventTime,
			ValidatedAt: cl.Validated, Support: len(roots), LostSupport: lost, Derivation: cl.Transform}}
	if cl.Project != s.ProjectID {
		item.Provenance.SharedFrom = "shared claim from another project"
	}
	score := 0.0
	if req.TaskID != "" && cl.Task == req.TaskID {
		score += 8
		item.Reasons = append(item.Reasons, "task match")
	}
	for _, f := range req.Files {
		for _, r := range refs {
			if (r.Kind == "file" || r.Kind == "ast") && r.Detail.Path == f {
				score += 6
				item.Reasons = appendOnce(item.Reasons, "file match: "+f)
			}
		}
	}
	if n, ok := coverage(terms, text); ok {
		score += 4 * float64(n) / float64(len(terms))
		item.Reasons = append(item.Reasons, "terms matched: "+itoa(n)+"/"+itoa(len(terms)))
		if hit, _ := coverage(terms, cl.Title); hit > 0 {
			score++
		}
	}
	switch v.state {
	case ApCurrent:
		score += 2
	case ApDurable:
		score += 1.5
	}
	item.Reasons = append(item.Reasons, "applicability "+v.state)
	item.Reasons = append(item.Reasons, firstN(v.reasons, 3)...)
	if t, err := time.Parse(contract.TimeFormat, cl.Created); err == nil {
		days := c.Now().Sub(t).Hours() / 24
		score += 1 / (1 + math.Max(days, 0)/30)
	}
	return ranked{item: item, group: group, score: score, created: cl.Created, lineage: cl.Lineage,
		unknown: v.state == ApUnknown || v.state == ApUnchecked}, nil
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (c *Core) turnCandidates(ctx context.Context, db *sql.DB, s Scope, req contract.SearchRequest, match, since, until string, terms []string) ([]ranked, error) {
	where := []string{`turns_fts MATCH ?`, `t.project_id = ?`, `t.invalid = ''`}
	args := []any{match, s.ProjectID}
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
	rows, err := db.QueryContext(ctx, `SELECT t.id, t.session_id, t.text, t.truncated, coalesce(t.event_time, ''), t.digest,
		coalesce(t.prev_id, ''), coalesce(t.next_id, ''), x.harness FROM turns_fts JOIN turns t ON t.rowid = turns_fts.rowid
		JOIN sessions x ON x.id = t.session_id WHERE `+strings.Join(where, " AND ")+` ORDER BY t.rowid DESC LIMIT ?`,
		append(args, candidateLimit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ranked
	for rows.Next() {
		var id, session, text, at, digest, prev, next, harness string
		var truncated bool
		if err := rows.Scan(&id, &session, &text, &truncated, &at, &digest, &prev, &next, &harness); err != nil {
			return nil, err
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
	return out, rows.Err()
}

// Read returns full records for up to 20 IDs. Each ID is re-checked against
// the caller's current scope; foreign and unknown IDs are indistinguishable.
func (c *Core) Read(ctx context.Context, caller Caller, req contract.ReadRequest) (*contract.ReadResult, int, error) {
	if !caller.Has(contract.RoleReader) {
		return nil, 0, errForbidden
	}
	budget, err := contract.Budget(req.MaxBytes)
	if err != nil {
		return nil, 0, err
	}
	if len(req.IDs) == 0 || len(req.IDs) > 20 {
		return nil, 0, contract.Errf(contract.CodeInvalid, "read between 1 and 20 ids")
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return nil, 0, err
	}
	ev, err := c.evaluator(ctx)
	if err != nil {
		return nil, 0, err
	}
	db := c.store.Read()
	res := &contract.ReadResult{Records: []contract.Record{}, Missing: []string{}, Complete: true, Notice: Notice,
		Budget: contract.BudgetInfo{MaxBytes: budget, Unit: contract.BudgetUnit}}
	for _, id := range req.IDs {
		rec, ok, err := c.readOne(ctx, db, ev, s, id)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			res.Missing = append(res.Missing, id)
			continue
		}
		res.Records = append(res.Records, rec)
	}
	return res, budget, nil
}

func (c *Core) readOne(ctx context.Context, db *sql.DB, ev *evaluator, s Scope, id string) (contract.Record, bool, error) {
	if len(id) > 64 {
		return contract.Record{}, false, nil
	}
	switch {
	case strings.HasPrefix(id, "clm_"):
		if _, ok, err := inScopeClaim(ctx, db, s, id); err != nil || !ok {
			return contract.Record{}, false, err
		}
		cl, err := scanClaim(db.QueryRowContext(ctx, `SELECT `+claimColumns+` FROM claims c WHERE c.id = ?`, id))
		if err != nil {
			return contract.Record{}, false, err
		}
		refs, err := loadRefs(ctx, db, id)
		if err != nil {
			return contract.Record{}, false, err
		}
		v := ev.claim(cl.Kind, cl.Worktree, cl.Expires, depsOf(refs))
		conflicts, err := conflictsOf(ctx, db, s, id)
		if err != nil {
			return contract.Record{}, false, err
		}
		roots, err := supportRoots(ctx, db, id, 0, map[string][]string{})
		if err != nil {
			return contract.Record{}, false, err
		}
		type refOut struct {
			Role   string       `json:"role"`
			Kind   string       `json:"kind"`
			Ref    string       `json:"ref"`
			State  string       `json:"state"`
			Lost   string       `json:"lost_reason,omitempty"`
			Detail contract.Ref `json:"detail"`
		}
		var outRefs []refOut
		for _, r := range refs {
			outRefs = append(outRefs, refOut{r.Role, r.Kind, r.Ref, r.State, r.Lost, r.Detail})
		}
		var alts []string
		_ = json.Unmarshal([]byte(c.claimAlternatives(ctx, db, id)), &alts)
		data := map[string]any{"kind": cl.Kind, "title": cl.Title, "body": cl.Body, "rationale": cl.Rationale,
			"alternatives": alts, "task_id": cl.Task, "status_reason": cl.Reason, "replaced_by": cl.SupersededBy,
			"conflicts_with": conflicts, "refs": outRefs, "independent_support": len(roots), "derivation": cl.Transform,
			"provenance": map[string]string{"origin": originLabel(cl.Origin), "created_at": cl.Created, "event_time": cl.EventTime,
				"validated_at": cl.Validated, "expires_at": cl.Expires}}
		if cl.Project != s.ProjectID {
			data["shared_from"] = "another project"
		}
		return contract.Record{ID: id, Type: "claim", Data: raw(data), Status: cl.Status, Applicability: v.state,
			Reasons: firstN(v.reasons, 8)}, true, nil
	case strings.HasPrefix(id, "tsk_"):
		t, ok, err := c.loadTask(ctx, db, s, id)
		if err != nil || !ok {
			return contract.Record{}, false, err
		}
		cp, err := latestCheckpoint(ctx, db, id)
		if err != nil {
			return contract.Record{}, false, err
		}
		var children int
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM checkpoints WHERE task_id = ? AND accepted = 0`, id).Scan(&children)
		return contract.Record{ID: id, Type: "task", Data: raw(map[string]any{"task": t, "checkpoint": cp,
			"unaccepted_child_conclusions": children}), Status: t.State}, true, nil
	case strings.HasPrefix(id, "ckp_"):
		var body, origin, created, task, project string
		var accepted bool
		var revision int64
		err := db.QueryRowContext(ctx, `SELECT k.body, k.origin, k.created_at, k.task_id, t.project_id, k.accepted, k.revision
			FROM checkpoints k JOIN tasks t ON t.id = k.task_id WHERE k.id = ?`, id).Scan(&body, &origin, &created, &task, &project, &accepted, &revision)
		if err != nil || project != s.ProjectID {
			return contract.Record{}, false, nil
		}
		return contract.Record{ID: id, Type: "checkpoint", Data: raw(map[string]any{"task_id": task, "revision": revision,
			"accepted": accepted, "origin": originLabel(origin), "created_at": created, "checkpoint": json.RawMessage(body)})}, true, nil
	case strings.HasPrefix(id, "evt_"):
		var e struct {
			Session, Kind, Ingested, Source, Digest, Adapter, Flags, Tool, Outcome, Text, Invalid, Basis string
			Time                                                                                         sql.NullString
			Seq                                                                                          sql.NullInt64
		}
		err := db.QueryRowContext(ctx, `SELECT session_id, kind, event_time, seq, ingested_at, source, source_digest, adapter_version,
			flags, tool_name, outcome, text, invalid, id_basis FROM events WHERE id = ? AND project_id = ?`, id, s.ProjectID).
			Scan(&e.Session, &e.Kind, &e.Time, &e.Seq, &e.Ingested, &e.Source, &e.Digest, &e.Adapter, &e.Flags, &e.Tool, &e.Outcome,
				&e.Text, &e.Invalid, &e.Basis)
		if err != nil {
			return contract.Record{}, false, nil
		}
		data := map[string]any{"envelope": contract.EventEnvelope, "session": e.Session, "kind": e.Kind,
			"event_time": nullable(e.Time), "seq": e.Seq.Int64, "seq_known": e.Seq.Valid, "ingested_at": e.Ingested,
			"source": e.Source, "source_digest": e.Digest, "adapter_version": e.Adapter, "flags": json.RawMessage(e.Flags),
			"tool": e.Tool, "outcome": e.Outcome, "text": e.Text, "invalidated": e.Invalid, "id_basis": e.Basis}
		return contract.Record{ID: id, Type: "event", Data: raw(data), Status: "observed", Applicability: "historical"}, true, nil
	case strings.HasPrefix(id, "trn_"):
		var session, text, digest, invalid string
		var at, prev, next sql.NullString
		var truncated bool
		err := db.QueryRowContext(ctx, `SELECT session_id, text, digest, invalid, event_time, prev_id, next_id, truncated
			FROM turns WHERE id = ? AND project_id = ?`, id, s.ProjectID).Scan(&session, &text, &digest, &invalid, &at, &prev, &next, &truncated)
		if err != nil {
			return contract.Record{}, false, nil
		}
		return contract.Record{ID: id, Type: "turn", Data: raw(map[string]any{"session": session, "text": text, "digest": digest,
			"truncated": truncated, "invalidated": invalid, "event_time": nullable(at), "previous": nullable(prev), "next": nullable(next)}),
			Status: "observed", Applicability: "historical"}, true, nil
	case strings.HasPrefix(id, "ses_"):
		var harness, native, boundaries, first, last string
		var parent sql.NullString
		var events, unattributed int
		err := db.QueryRowContext(ctx, `SELECT harness, native_id, parent_id, boundaries, first_seen, last_seen,
			(SELECT count(*) FROM events WHERE session_id = s.id), (SELECT count(*) FROM events WHERE session_id = s.id AND project_id IS NULL)
			FROM sessions s WHERE id = ? AND project_id = ?`, id, s.ProjectID).Scan(&harness, &native, &parent, &boundaries, &first, &last, &events, &unattributed)
		if err != nil {
			return contract.Record{}, false, nil
		}
		return contract.Record{ID: id, Type: "session", Data: raw(map[string]any{"harness": harness, "native_id": native,
			"parent": nullable(parent), "boundaries": json.RawMessage(boundaries), "first_seen": first, "last_seen": last,
			"events": events, "unattributed_events": unattributed,
			"completion": "a session ending is not evidence that its task was accepted"})}, true, nil
	}
	return contract.Record{}, false, nil
}

func (c *Core) claimAlternatives(ctx context.Context, q querier, id string) string {
	var a string
	_ = q.QueryRowContext(ctx, `SELECT alternatives FROM claims WHERE id = ?`, id).Scan(&a)
	return a
}

func raw(v any) json.RawMessage {
	b, _ := contract.Marshal(v)
	return b
}

func nullable(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

// Evidence validates each support reference and dependency of a claim
// against current sources and returns bounded excerpts where available.
func (c *Core) Evidence(ctx context.Context, caller Caller, req contract.EvidenceRequest) (*contract.EvidenceResult, int, error) {
	if !caller.Has(contract.RoleReader) {
		return nil, 0, errForbidden
	}
	budget, err := contract.Budget(req.MaxBytes)
	if err != nil {
		return nil, 0, err
	}
	if req.ExcerptLines == 0 {
		req.ExcerptLines = 20
	}
	if req.ExcerptLines < 1 || req.ExcerptLines > 40 {
		return nil, 0, contract.Errf(contract.CodeInvalid, "excerpt_lines must be 1-40")
	}
	s, err := c.scope(ctx, caller)
	if err != nil {
		return nil, 0, err
	}
	db := c.store.Read()
	if _, ok, err := inScopeClaim(ctx, db, s, req.ID); err != nil {
		return nil, 0, err
	} else if !ok {
		return nil, 0, contract.Errf(contract.CodeNotFound, "no such claim in the caller's scope")
	}
	cl, err := scanClaim(db.QueryRowContext(ctx, `SELECT `+claimColumns+` FROM claims c WHERE c.id = ?`, req.ID))
	if err != nil {
		return nil, 0, err
	}
	refs, err := loadRefs(ctx, db, req.ID)
	if err != nil {
		return nil, 0, err
	}
	ev, err := c.evaluator(ctx)
	if err != nil {
		return nil, 0, err
	}
	v := ev.claim(cl.Kind, cl.Worktree, cl.Expires, depsOf(refs))
	res := &contract.EvidenceResult{ID: req.ID, Status: cl.Status, Applicability: v.state, Checks: []contract.SupportCheck{},
		Gaps: []string{}, Complete: true, Notice: Notice, Budget: contract.BudgetInfo{MaxBytes: budget, Unit: contract.BudgetUnit}}
	root := ev.roots[cl.Worktree]
	for _, r := range refs {
		chk := contract.SupportCheck{Role: r.Role, Kind: r.Kind, Ref: r.Ref, Stored: raw(r.Detail), State: r.State}
		if r.State == "lost" {
			res.Gaps = append(res.Gaps, r.Kind+" "+r.Ref+": "+r.Lost)
		}
		switch r.Kind {
		case "event", "turn":
			table := map[string]string{"event": "events", "turn": "turns"}[r.Kind]
			var text, invalid string
			err := db.QueryRowContext(ctx, `SELECT text, invalid FROM `+table+` WHERE id = ? AND project_id = ?`, r.Ref, cl.Project).Scan(&text, &invalid)
			switch {
			case err != nil:
				chk.Validation = "missing: source record deleted or expired"
				res.Gaps = append(res.Gaps, r.Kind+" "+r.Ref+": source record missing")
			case invalid != "":
				chk.Validation = "invalidated: " + invalid
			default:
				chk.Validation = "present"
				ex, _ := contract.Truncate(text, 2048)
				chk.Excerpt = raw(map[string]string{"text": ex})
			}
		case "claim":
			var status string
			if err := db.QueryRowContext(ctx, `SELECT status FROM claims WHERE id = ?`, r.Ref).Scan(&status); err != nil {
				chk.Validation = "missing: source claim deleted"
			} else {
				chk.Validation = "source claim " + status
			}
		case "file", "ast":
			c.fileEvidence(ctx, ev, root, r, req.ExcerptLines, &chk, res)
		case "revision", "environment", "tabula":
			dv := verdict{state: ApCurrent}
			ev.dep(&dv, root, r.Detail)
			chk.Validation = dv.state + ": " + strings.Join(dv.reasons, "; ")
			if r.Kind == "tabula" {
				chk.Adapter = "tabula"
			}
		default:
			chk.Validation = "unverifiable: caller assertion"
		}
		res.Checks = append(res.Checks, chk)
	}
	if len(refs) == 0 {
		res.Gaps = append(res.Gaps, "no support or dependencies recorded")
	}
	res.GapsTotal = len(res.Gaps)
	return res, budget, nil
}

func (c *Core) fileEvidence(ctx context.Context, ev *evaluator, root string, r refRow, lines int, chk *contract.SupportCheck, res *contract.EvidenceResult) {
	if root == "" {
		chk.Validation = "unknown: worktree root no longer mapped"
		return
	}
	f := ev.file(root, r.Detail.Path)
	switch {
	case f.reason != "":
		chk.Validation = "missing: " + f.reason
		res.Gaps = append(res.Gaps, "file "+r.Detail.Path+" unavailable")
		return
	case f.digest != r.Detail.SHA256:
		chk.Validation = "changed since capture"
		chk.Current = f.digest
	default:
		chk.Validation = "unchanged"
	}
	// Exact excerpts come from agent-evidence with the stored digest as guard.
	start := r.Detail.Line
	if start < 1 {
		start = 1
	}
	out := c.tools.Read(ctx, root, r.Detail.Path, start, lines, 4096, r.Detail.SHA256)
	chk.Adapter = "agent-evidence:" + out.Status
	if out.Status == "ok" {
		chk.Excerpt = out.Output
	}
	if r.Kind == "ast" {
		abs, err := c.pol.Source(root, r.Detail.Path)
		if err == nil {
			a := c.tools.ASTFreshness(ctx, abs)
			chk.Adapter += "; ast-index:" + a.Status
		}
	}
}

// scopeGeneration identifies the state visible to a scope: the policy
// generation, the project's own generation and those of projects that share
// claims into it. Changes elsewhere leave it, and its cursors, unchanged.
func (c *Core) scopeGeneration(ctx context.Context, q querier, s Scope) (string, error) {
	policyGen, err := store.Generation(ctx, q)
	if err != nil {
		return "", err
	}
	parts := []string{itoa(int(policyGen)), itoa(int(store.ProjectGeneration(ctx, q, s.ProjectID)))}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT c.project_id FROM shares sh JOIN claims c ON c.id = sh.claim_id
		WHERE sh.to_project = ? AND sh.revoked_at IS NULL ORDER BY c.project_id`, s.ProjectID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return "", err
		}
		parts = append(parts, p+"="+itoa(int(store.ProjectGeneration(ctx, q, p))))
	}
	return strings.Join(parts, ":"), rows.Err()
}
