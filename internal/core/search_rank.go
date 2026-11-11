package core

import (
	"context"
	"math"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
)

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

func (c *Core) rankClaim(ctx context.Context, q querier, ev *evaluator, s Scope, cl claimRow, req contract.SearchRequest, terms []string, supportMemo map[string][]string, now time.Time) (ranked, error) {
	refs, err := loadRefs(ctx, q, cl.ID)
	if err != nil {
		return ranked{}, err
	}
	v := ev.claim(cl.Kind, cl.Worktree, cl.Expires, depsOf(refs))
	conflicts, err := conflictsOf(ctx, q, s, cl.ID)
	if err != nil {
		return ranked{}, err
	}
	roots, err := supportRoots(ctx, q, cl.ID, 0, supportMemo)
	if err != nil {
		return ranked{}, err
	}
	cl.Lineage = lineageOf(cl.ID, roots)
	return rankEvaluatedClaim(cl, req, terms, s, refs, v, conflicts, len(roots), now), nil
}

// rankEvaluatedClaim scores already-evaluated data without database, filesystem
// or external-tool calls. Validation and ranking have independent responsibilities.
func rankEvaluatedClaim(cl claimRow, req contract.SearchRequest, terms []string, s Scope, refs []refRow, v verdict, conflicts []string, support int, now time.Time) ranked {
	group := classify(cl.Status, v.state, len(conflicts))
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
			ValidatedAt: cl.Validated, Support: support, LostSupport: lost, Derivation: cl.Transform}}
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
		days := now.Sub(t).Hours() / 24
		score += 1 / (1 + math.Max(days, 0)/30)
	}
	return ranked{item: item, group: group, score: score, created: cl.Created, lineage: cl.Lineage,
		unknown: v.state == ApUnknown || v.state == ApUnchecked}
}
