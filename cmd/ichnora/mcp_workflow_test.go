package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/mcpbridge"
	"github.com/timfewi/ichnora/internal/server"
)

// An agent that follows the advertised schemas keeps a decision validated,
// checkpoints and resumes a task, and records an improvement attempt.
func TestMCPWorkflowFollowsAdvertisedSchemas(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	if err := os.Chdir(e.alpha); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	srv := mcpbridge.Server("test", server.Client{Socket: e.cfg.AgentSocket}, func() *contract.SessionHint { return nil })
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(tool string, args map[string]any, into any) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		out, err := mcpbridge.Decode(res)
		if err != nil || !out.OK {
			t.Fatalf("%s %v: %v %+v", tool, args, err, res.Content[0])
		}
		if into != nil {
			data, _ := json.Marshal(out.Result)
			if err := json.Unmarshal(data, into); err != nil {
				t.Fatal(err)
			}
		}
	}
	groups := func() map[string]string {
		t.Helper()
		var found contract.SearchResult
		call("memory_search", map[string]any{"query": "", "view": "all"}, &found)
		out := map[string]string{}
		for _, item := range found.Items {
			out[item.ID] = item.Group + "/" + item.Applicability
		}
		return out
	}

	var captured contract.CaptureResult
	call("memory_capture", map[string]any{"claims": []any{
		map[string]any{"kind": "decision", "title": "Cache keys include the source digest",
			"depends": []any{map[string]any{"kind": "file", "path": "parser/cache.go"}}},
		map[string]any{"kind": "decision", "title": "Keep the synthetic suite until the next review",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
		map[string]any{"kind": "decision", "title": "Prefer short keys"},
	}}, &captured)
	file, expiry, bare := captured.IDs[0], captured.IDs[1], captured.IDs[2]
	if got := groups(); got[file] != "working/current" || got[expiry] != "working/durable" || got[bare] != "lead/unchecked" {
		t.Fatalf("claims after capture: %v", got)
	}
	call("memory_update", map[string]any{"id": bare, "status": "disputed", "reason": "never measured"}, nil)

	var task contract.TaskResult
	call("memory_task", map[string]any{"action": "create", "title": "Review cache keys",
		"checkpoint": map[string]any{"objective": "Decide the cache key layout"}}, &task)
	id := task.Task.ID
	call("memory_task", map[string]any{"action": "checkpoint", "task_id": id, "expected_revision": task.Task.Revision,
		"checkpoint": map[string]any{"decisions": []any{file}, "remaining": []any{"Measure collisions"}, "next_action": "Run the suite",
			"verification": []any{map[string]any{"check": "go test ./parser", "outcome": "passed",
				"inputs": map[string]any{"revision": strings.Repeat("a", 40)}}}}}, &task)
	var resumed contract.ResumeResult
	call("memory_task", map[string]any{"action": "resume", "task_id": id}, &resumed)
	if resumed.NextAction != "Run the suite" || resumed.Totals["objective"] != 1 || resumed.Totals["decision"] != 1 || resumed.Totals["check"] != 1 {
		t.Fatalf("resume: %+v", resumed)
	}

	writeFile(t, filepath.Join(e.alpha, "parser", "cache.go"), "package parser\n\nfunc Key() string { return \"v2\" }\n")
	if got := groups(); got[file] != "history/stale" || got[bare] != "conflict/unchecked" {
		t.Fatalf("claims after the edit and the dispute: %v", got)
	}
	call("memory_task", map[string]any{"action": "transition", "task_id": id, "expected_revision": task.Task.Revision,
		"to": "completed", "reason": "layout decided"}, &task)
	if task.Task.State != "completed" {
		t.Fatalf("transition: %+v", task.Task)
	}

	attempt := map[string]any{"problem": "cache-collisions", "attempt": "digest-v1"}
	propose := map[string]any{"action": "propose", "cause": "short keys collide", "change": "add the digest",
		"prediction": "no collisions in the suite", "context": "synthetic cache suite v1"}
	observe := map[string]any{"action": "observe", "outcome": "0 collisions", "evidence": "check.txt"}
	for _, step := range []map[string]any{propose, observe} {
		for key, value := range attempt {
			step[key] = value
		}
	}
	call("memory_sim", propose, nil)
	writeFile(t, filepath.Join(e.alpha, "check.txt"), "0 collisions\n")
	call("memory_sim", observe, nil)
	var history contract.SimHistory
	call("memory_sim", map[string]any{"action": "history", "problem": "cache-collisions"}, &history)
	if len(history.Attempts) != 1 || history.Attempts[0].Outcome != "0 collisions" || history.Attempts[0].EvidenceSHA256 == "" {
		t.Fatalf("history: %+v", history)
	}
}
