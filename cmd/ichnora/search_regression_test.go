package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
)

func searchTask(t *testing.T, e *testEnv, title string) string {
	t.Helper()
	r := e.must(e.alpha, "", "task", "create", "--title", title)
	var out contract.TaskResult
	r.into(t, &out)
	return out.Task.ID
}

// Decisions and conflicts must remain discoverable when hundreds of newer
// records occupy earlier database batches. Pagination must cover the full set.
func TestSearchAndResumeTraverseAllCandidateBatches(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.Readers = 1 })
	e.enroll(e.alpha)
	task := searchTask(t, e, "Large synthetic catalog")
	old := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Catalog durable decision", TaskID: task})[0]
	conflict := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Catalog conflict one", TaskID: task})[0]
	e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Catalog conflict two", TaskID: task, Contradicts: []string{conflict}})
	for batch := 0; batch < 25; batch++ {
		var claims []contract.ClaimInput
		for i := 0; i < 20; i++ {
			claims = append(claims, contract.ClaimInput{Kind: "note", Title: fmt.Sprintf("Catalog note %03d", batch*20+i), TaskID: task})
		}
		e.captureIDs(e.alpha, claims...)
	}
	last := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Catalog late task decision", TaskID: task})[0]
	filtered := e.search(e.alpha, "catalog", "--kind", "decision", "--limit", "50", "--max-bytes", "262144")
	if len(filtered.Items) != 4 || find(filtered.Items, old) == nil || filtered.Totals.Conflicts != 2 || !filtered.Complete {
		t.Fatalf("older decisions or conflicts disappeared: %+v", filtered)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		args := []string{"catalog", "--limit", "50", "--max-bytes", "262144"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		out := e.search(e.alpha, args...)
		if out.Totals.Working != 2 || out.Totals.Conflicts != 2 || out.Totals.Leads != 500 {
			t.Fatalf("totals changed across pagination: %+v", out.Totals)
		}
		if page == 0 && (len(out.Items) < 2 || out.Items[0].Group != "conflict" || out.Items[1].Group != "conflict") {
			t.Fatal("old conflicts did not retain first priority")
		}
		for _, item := range out.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate result across pages: %s", item.ID)
			}
			seen[item.ID] = true
		}
		if out.NextCursor == "" {
			if !out.Complete || len(seen) != 504 {
				t.Fatalf("pagination ended before all records: %d, complete=%v", len(seen), out.Complete)
			}
			break
		}
		if page > 12 || out.Complete {
			t.Fatal("pagination did not advance consistently")
		}
		cursor = out.NextCursor
	}
	r := e.must(e.alpha, "", "resume", task, "--max-bytes", "262144")
	var resumed contract.ResumeResult
	r.into(t, &resumed)
	if resumed.Totals["decision"] != 2 || resumed.Totals["conflict"] != 2 || resumed.Totals["lead"] != 500 {
		t.Fatalf("resume silently omitted linked claims beyond 200: %+v", resumed.Totals)
	}
	found := false
	for _, line := range resumed.Lines {
		if line.Ref == last {
			found = true
		}
	}
	if !found {
		t.Fatal("resume omitted the late linked decision")
	}
}

func TestSearchSourceTaskFilterAndNormalizedQueries(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	taskA, taskB := searchTask(t, e, "Task A"), searchTask(t, e, "Task B")
	for _, session := range []struct{ native, task string }{{"task-a-session", taskA}, {"task-b-session", taskB}} {
		path := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "p", session.native+".jsonl"), session.native, e.alpha)
		e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session.native, "transcript_path": path, "cwd": e.alpha})
		t.Setenv("CLAUDE_CODE_SESSION_ID", session.native)
		e.must(e.alpha, "", "resume", session.task)
	}
	all := e.search(e.alpha, "cache invalidation fix", "--kind", "source", "--limit", "50", "--max-bytes", "262144")
	a := e.search(e.alpha, "cache invalidation fix", "--task", taskA, "--kind", "source", "--limit", "50", "--max-bytes", "262144")
	b := e.search(e.alpha, "cache invalidation fix", "--task", taskB, "--kind", "source", "--limit", "50", "--max-bytes", "262144")
	if len(a.Items) == 0 || len(b.Items) == 0 || len(all.Items) != len(a.Items)+len(b.Items) {
		t.Fatalf("source task filter did not partition sessions: all=%d a=%d b=%d", len(all.Items), len(a.Items), len(b.Items))
	}
	for _, item := range a.Items {
		if find(b.Items, item.ID) != nil {
			t.Fatal("task search returned an unrelated task's source")
		}
	}
	for _, query := range []string{"!!!", " \t\n", "😀", "***"} {
		if r := e.cli(e.alpha, "", "search", query); r.errCode() != contract.CodeInvalid {
			t.Fatalf("non-searchable query must be invalid_request, got %q: %s", query, r.Raw)
		}
	}
	// The explicitly empty query remains the supported project index operation.
	e.search(e.alpha, "")
}

// Applicability changes outside SQLite must expire offset cursors when they
// change the visible result order, even though no database generation changed.
func TestSearchCursorExpiresWhenSourceChangesBetweenPages(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.Readers = 1 })
	e.enroll(e.alpha)
	e.captureIDs(e.alpha,
		contract.ClaimInput{Kind: "decision", Title: "File-sensitive parser decision", Depends: []contract.Ref{{Kind: "file", Path: "parser/cache.go"}}},
		contract.ClaimInput{Kind: "decision", Title: "Durable parser decision one"},
		contract.ClaimInput{Kind: "decision", Title: "Durable parser decision two"})
	first := e.search(e.alpha, "parser decision", "--limit", "1")
	if first.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	// An unchanged filesystem permits continuation; only the changed result
	// membership below expires the same held cursor.
	e.search(e.alpha, "parser decision", "--limit", "1", "--cursor", first.NextCursor)
	writeFile(t, filepath.Join(e.alpha, "parser", "cache.go"), "package parser\n// Changed synthetic source.\n")
	if got := e.cli(e.alpha, "", "search", "parser decision", "--limit", "1", "--cursor", first.NextCursor); got.errCode() != contract.CodeCursorExpired {
		t.Fatalf("source change shifted a held cursor instead of expiring it: %s", got.Raw)
	}
	fresh := e.search(e.alpha, "parser decision", "--limit", "50")
	if fresh.Totals.Working != 2 || fresh.Omitted.Stale != 1 {
		t.Fatalf("fresh search did not reflect changed source: %+v", fresh)
	}
}
