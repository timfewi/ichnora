package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

// Phase 2: task A is resumed with exact evidence through the real CLI, and an
// unauthorized project B cannot see any of it.
func TestOfflineVerticalSlice(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.cli(e.alpha, "", "status"); r.Code != 0 {
		t.Fatalf("status before enrollment should report a scope gap, got %s", r.Raw)
	} else if !strings.Contains(r.Raw, "not inside an enrolled project") {
		t.Fatalf("missing scope gap: %s", r.Raw)
	}
	if r := e.cli(e.alpha, "", "search", "parser"); r.errCode() != contract.CodeOutOfScope || r.Code != 4 {
		t.Fatalf("unenrolled search must be out_of_scope: %s", r.Raw)
	}
	e.enroll(e.alpha)
	e.enroll(e.beta)

	cache := filepath.Join(e.alpha, "parser", "cache.go")
	digest := sha(t, cache)
	decision := e.captureIDs(e.alpha, contract.ClaimInput{
		Kind: "decision", Title: "Invalidate the parser cache on digest change",
		Body: "Cache keys include the source digest so stale parses are never reused.", Rationale: "Timestamps missed edits.",
		Alternatives: []string{"time-based expiry"},
		Support:      []contract.Ref{{Kind: "file", Path: "parser/cache.go", Line: 3}},
		Depends:      []contract.Ref{{Kind: "file", Path: "parser/cache.go", SHA256: digest}},
	})[0]
	cp, _ := json.Marshal(contract.Checkpoint{Objective: "Fix stale parser cache reuse", Constraints: []string{"no new dependencies"},
		Remaining: []string{"add regression test for digest change"}, NextAction: "write the regression test",
		Decisions: []string{decision}, Verification: []contract.Verification{{Check: "go test ./parser", Outcome: "passed",
			Inputs: contract.VerificationInput{Revision: strings.Repeat("a", 40), Files: map[string]string{"parser/cache.go": digest}}}}})
	r := e.must(e.alpha, string(cp), "task", "create", "--title", "Parser cache", "--checkpoint-stdin")
	var task contract.TaskResult
	r.into(t, &task)

	r = e.must(e.alpha, "", "resume", task.Task.ID)
	var res contract.ResumeResult
	r.into(t, &res)
	if res.NextAction != "write the regression test" || res.Task.State != "active" {
		t.Fatalf("resume lost the next action: %s", r.Raw)
	}
	sections := map[string]string{}
	for _, l := range res.Lines {
		sections[l.Section] += l.Text + "|" + l.State + ";"
	}
	for section, want := range map[string]string{"decision": "Invalidate the parser cache", "remaining": "regression test",
		"check": "go test ./parser passed|applicable", "constraint": "no new dependencies", "objective": "stale parser"} {
		if !strings.Contains(sections[section], want) {
			t.Fatalf("resume section %s lacks %q: %s", section, want, r.Raw)
		}
	}
	r = e.must(e.alpha, "", "evidence", decision)
	var ev contract.EvidenceResult
	r.into(t, &ev)
	if len(ev.Checks) != 2 || ev.Checks[0].Validation != "unchanged" || !strings.Contains(ev.Checks[0].Adapter, "missing_adapter") {
		t.Fatalf("evidence must validate the digest and report the missing excerpt adapter: %s", r.Raw)
	}

	// Project B: nothing from A is visible, by search, ID, evidence or task.
	if out := e.search(e.beta, "parser cache"); len(out.Items) != 0 || out.Totals != (contract.Totals{}) {
		t.Fatalf("project B saw project A records: %+v", out)
	}
	r = e.must(e.beta, "", "read", decision, task.Task.ID)
	var rd contract.ReadResult
	r.into(t, &rd)
	if len(rd.Records) != 0 || len(rd.Missing) != 2 {
		t.Fatalf("project B read project A IDs: %s", r.Raw)
	}
	if r := e.cli(e.beta, "", "evidence", decision); r.errCode() != contract.CodeNotFound {
		t.Fatalf("project B evidence must be not_found: %s", r.Raw)
	}
	if r := e.cli(e.beta, "", "resume", task.Task.ID); r.errCode() != contract.CodeNotFound {
		t.Fatalf("project B resume must be not_found: %s", r.Raw)
	}
	if strings.Contains(e.must(e.beta, "", "status").Raw, "alpha") {
		t.Fatal("status of project B names project A")
	}
}
