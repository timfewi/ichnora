package core

import (
	"context"
	"os"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/store"
)

func osStat(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Coverage is the per-harness boundary support, verified against the pinned
// native versions and fixtures (see docs/adapters.md). It is static
// declaration; observed counts come from ingested sessions.
var Coverage = map[string]map[string]string{
	"claude": {
		"session_start": "hook SessionStart(startup|clear)", "resume": "hook SessionStart(resume|fork)",
		"tool_outcomes": "transcript tool_result blocks (outputs excluded)", "checkpoint_capture": "MCP/CLI task checkpoint",
		"compaction":  "hooks PreCompact, PostCompact and SessionStart(compact); context refresh at SessionStart(compact)",
		"task_switch": "explicit resume or task create via MCP/CLI", "session_end": "hook SessionEnd (not required)",
		"child_sessions": "hooks SubagentStart/SubagentStop with agent_transcript_path",
	},
	"codex": {
		"session_start": "hook SessionStart(startup|clear)", "resume": "hook SessionStart(resume|fork)",
		"tool_outcomes": "rollout function_call_output (outputs excluded)", "checkpoint_capture": "MCP/CLI task checkpoint",
		"compaction":  "hooks PreCompact, PostCompact and SessionStart(compact); context refresh at SessionStart(compact)",
		"task_switch": "explicit resume or task create via MCP/CLI", "session_end": "hook SessionEnd (not required)",
		"child_sessions": "hooks SubagentStart/SubagentStop; agent_transcript_path may be null (reported gap)",
	},
	"opencode": {
		"session_start": "plugin context hook (first call per session)", "resume": "plugin context hook (first call per process)",
		"tool_outcomes": "forwarded message tool parts (inputs/outputs excluded)", "checkpoint_capture": "MCP/CLI task checkpoint",
		"compaction":  "plugin compaction hook; context refresh in the compaction prompt",
		"task_switch": "explicit resume or task create via MCP/CLI", "session_end": "unsupported: no end hook; reconciled at next context",
		"child_sessions": "forwarded parentID marks child sessions",
	},
}

// StatusResult is the bounded health and coverage report.
type StatusResult struct {
	Service     map[string]any            `json:"service"`
	Scope       *contract.ScopeInfo       `json:"scope"`
	ScopeGap    string                    `json:"scope_gap,omitempty"`
	Storage     map[string]any            `json:"storage"`
	Environment map[string]any            `json:"environment"`
	Adapters    map[string]map[string]any `json:"adapters"`
	Tools       map[string]bool           `json:"evidence_tools"`
	Counts      map[string]int            `json:"counts,omitempty"`
	Gaps        []string                  `json:"gaps"`
	Omitted     int                       `json:"omitted_budget"`
	Complete    bool                      `json:"complete"`
	Budget      contract.BudgetInfo       `json:"budget"`
}

// Trim drops adapter detail, gap texts, then per-project counts.
func (r *StatusResult) Trim() bool {
	switch {
	case len(r.Adapters) > 0:
		for _, k := range []string{"opencode", "codex", "claude"} {
			if _, ok := r.Adapters[k]; ok {
				delete(r.Adapters, k)
				break
			}
		}
	case len(r.Gaps) > 0:
		r.Gaps = r.Gaps[:len(r.Gaps)-1]
	case r.Counts != nil:
		r.Counts = nil
	default:
		return false
	}
	r.Omitted++
	r.Complete = false
	return true
}

// Status reports health, scope, storage, environment and adapter coverage.
func (c *Core) Status(ctx context.Context, caller Caller, req contract.StatusRequest) (*StatusResult, int, error) {
	if !caller.Has(contract.RoleReader) {
		return nil, 0, errForbidden
	}
	budget, err := contract.Budget(req.MaxBytes)
	if err != nil {
		return nil, 0, err
	}
	db := c.store.Read()
	schema, _ := c.store.SchemaVersion(ctx)
	gen, _ := store.Generation(ctx, db)
	res := &StatusResult{
		Service: map[string]any{"version": Version, "contract": contract.Version, "sqlite": c.store.SQLiteVersion(ctx),
			"schema": schema, "generation": gen, "model_calls": "none"},
		Storage: map[string]any{"bytes": c.store.Size(), "ingestion_limit_bytes": c.cfg.Limits.MaxDBBytes,
			"ingestion_paused": c.ingestionPaused()},
		Adapters: map[string]map[string]any{}, Tools: map[string]bool{}, Gaps: []string{}, Complete: true,
		Budget: contract.BudgetInfo{MaxBytes: budget, Unit: contract.BudgetUnit},
	}
	if env, gap := c.environment(); env != nil {
		res.Environment = map[string]any{"epoch": env.ActivationEpoch, "format": env.Format}
	} else {
		res.Environment = map[string]any{"epoch": nil, "gap": gap}
	}
	for _, t := range []string{"tabula", "agent-evidence", "ast-index"} {
		res.Tools[t] = c.tools.Available(t)
		if !res.Tools[t] {
			res.Gaps = append(res.Gaps, t+": missing_adapter; core retrieval continues without it")
		}
	}
	s, ok, err := c.resolvePath(ctx, db, caller.Cwd)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		res.ScopeGap = "working directory is not inside an enrolled project"
	} else {
		info := s.info()
		res.Scope = &info
		res.Counts = map[string]int{}
		for name, q := range map[string]string{
			"claims_working":       `SELECT count(*) FROM claims WHERE project_id = ? AND status IN ('observed', 'inferred')`,
			"claims_unsupported":   `SELECT count(*) FROM claims WHERE project_id = ? AND status = 'unsupported'`,
			"claims_disputed":      `SELECT count(*) FROM claims WHERE project_id = ? AND status = 'disputed'`,
			"claims_history":       `SELECT count(*) FROM claims WHERE project_id = ? AND status IN ('superseded', 'invalidated')`,
			"tasks_open":           `SELECT count(*) FROM tasks WHERE project_id = ? AND state IN ('active', 'blocked', 'reopened')`,
			"sessions":             `SELECT count(*) FROM sessions WHERE project_id = ?`,
			"events":               `SELECT count(*) FROM events WHERE project_id = ?`,
			"unattributed_events":  `SELECT count(*) FROM events e JOIN sessions x ON x.id = e.session_id WHERE x.project_id = ? AND e.project_id IS NULL`,
			"malformed_lines":      `SELECT coalesce(sum(k.malformed), 0) FROM cursors k JOIN sessions x ON x.id = k.session_id WHERE x.project_id = ?`,
			"oversized_lines":      `SELECT coalesce(sum(k.oversized), 0) FROM cursors k JOIN sessions x ON x.id = k.session_id WHERE x.project_id = ?`,
			"transcript_rotations": `SELECT coalesce(sum(k.rotations), 0) FROM cursors k JOIN sessions x ON x.id = k.session_id WHERE x.project_id = ?`,
		} {
			var n int
			if err := db.QueryRowContext(ctx, q, s.ProjectID).Scan(&n); err != nil {
				return nil, 0, err
			}
			res.Counts[name] = n
		}
		res.Counts["collect"] = boolInt(s.Collect)
		res.Counts["recall"] = boolInt(s.Recall)
		if res.Counts["unattributed_events"] > 0 {
			res.Gaps = append(res.Gaps, "some session events could not be attributed to this project and are excluded from retrieval")
		}
		for harness, boundaries := range Coverage {
			observed := map[string]int{}
			rows, err := db.QueryContext(ctx, `SELECT j.key, sum(j.value) FROM sessions x, json_each(x.boundaries) j
				WHERE x.project_id = ? AND x.harness = ? GROUP BY j.key`, s.ProjectID, harness)
			if err != nil {
				return nil, 0, err
			}
			for rows.Next() {
				var k string
				var n int
				if err := rows.Scan(&k, &n); err == nil {
					observed[k] = n
				}
			}
			rows.Close()
			res.Adapters[harness] = map[string]any{"declared": boundaries, "observed_boundaries": observed,
				"context_refresh": "supported at SessionStart(resume|compact) and OpenCode compaction; text already in a running conversation cannot be removed"}
		}
	}
	if res.Storage["ingestion_paused"] == true {
		res.Gaps = append([]string{"ingestion paused at the storage limit; cursors are retained and resume after space is freed"}, res.Gaps...)
	}
	return res, budget, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
