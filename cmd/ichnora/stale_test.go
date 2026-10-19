package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

func checkpointJSON(t *testing.T, cp contract.Checkpoint) string {
	t.Helper()
	b, _ := json.Marshal(cp)
	return string(b)
}

func (e *testEnv) taskRevision(dir, id string) int64 {
	e.t.Helper()
	r := e.must(dir, "", "read", id)
	var rd contract.ReadResult
	r.into(e.t, &rd)
	var data struct {
		Task contract.Task `json:"task"`
	}
	if len(rd.Records) != 1 || json.Unmarshal(rd.Records[0].Data, &data) != nil {
		e.t.Fatalf("task not readable: %s", r.Raw)
	}
	return data.Task.Revision
}

func resumeLines(t *testing.T, raw json.RawMessage) (contract.ResumeResult, map[string][]contract.Line) {
	t.Helper()
	var res contract.ResumeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	by := map[string][]contract.Line{}
	for _, l := range res.Lines {
		by[l.Section] = append(by[l.Section], l)
	}
	return res, by
}

// Decision A is superseded by B, the task completes and the tested source
// changes. A new session, a compaction, a replay, a delayed child and a
// conflicting checkpoint must not resurrect A, the TODO or the old check.
func TestStaleContextAcrossBoundaries(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect", "--recall")
	cache := filepath.Join(e.alpha, "parser", "cache.go")
	digest := sha(t, cache)

	session := "claude-s1"
	transcript := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "proj", session+".jsonl"), session, e.alpha)
	start := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": session,
		"transcript_path": transcript, "cwd": e.alpha, "model": "fixture", "permission_mode": "default"}
	out := e.hook(e.alpha, "claude", start)
	if !strings.Contains(out, "No open tasks") || !strings.Contains(out, "<ichnora-context") {
		t.Fatalf("startup must offer an index, not resume: %s", out)
	}

	a := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Use time-based cache expiry",
		Body: "Expire parser cache entries after five minutes."})[0]
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	r := e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{Objective: "Stop stale parser cache reuse",
		Remaining: []string{"TODO: implement time-based expiry"}, NextAction: "implement expiry", Decisions: []string{a},
		Verification: []contract.Verification{{Check: "go test ./parser", Outcome: "passed",
			Inputs: contract.VerificationInput{Files: map[string]string{"parser/cache.go": digest}}}}}),
		"task", "create", "--title", "Parser cache staleness", "--checkpoint-stdin")
	var created contract.TaskResult
	r.into(t, &created)
	task := created.Task.ID
	b := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Key the parser cache by source digest",
		Body: "Digest keys replace time-based expiry.", TaskID: task, Supersedes: []string{a}})[0]
	rev := e.taskRevision(e.alpha, task)
	e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{Remaining: []string{}, Completed: []string{"digest-keyed cache"},
		Decisions: []string{b}, NextAction: "none"}), "task", "checkpoint", task, "--expected-revision", itoaT(rev), "--checkpoint-stdin")
	// A conflicting writer still holding the old revision is rejected.
	r = e.cli(e.alpha, checkpointJSON(t, contract.Checkpoint{Remaining: []string{"TODO: implement time-based expiry"}}),
		"task", "checkpoint", task, "--expected-revision", itoaT(rev), "--checkpoint-stdin")
	if r.errCode() != contract.CodeConflict || r.Code != 5 || r.Error.CurrentRevision != rev+1 {
		t.Fatalf("stale checkpoint must conflict with the current revision: %s", r.Raw)
	}
	rev = e.taskRevision(e.alpha, task)
	e.must(e.alpha, "", "task", "transition", task, "--to", "completed", "--expected-revision", itoaT(rev), "--reason", "digest keys shipped")
	// The tested source changes after the check ran.
	writeFile(t, cache, "package parser\n\n// Cache keys include the source digest and version.\nfunc Key() string { return \"v2\" }\n")

	assertStale := func(label string, res contract.ResumeResult, by map[string][]contract.Line) {
		t.Helper()
		if res.Task.State != "completed" || res.NextAction != "" || len(by["remaining"]) != 0 {
			t.Fatalf("%s: completed task resurrected work: %+v", label, res)
		}
		if len(by["check"]) != 0 || res.Totals["checks_not_applicable"] != 1 {
			t.Fatalf("%s: an old check was marked applicable: %+v", label, res.Lines)
		}
		for _, l := range res.Lines {
			if l.Ref == a && l.Section != "history" {
				t.Fatalf("%s: superseded decision A is presented as %s", label, l.Section)
			}
		}
	}

	// A new session in another harness: index only, no automatic resume.
	codex := "codex-s2"
	rollout := e.fixture("codex-rollout.jsonl", filepath.Join(e.codexRoot, "2026", "rollout-"+codex+".jsonl"), codex, e.alpha)
	out = e.hook(e.alpha, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": codex,
		"transcript_path": rollout, "cwd": e.alpha, "model": "fixture", "permission_mode": "default"})
	if strings.Contains(out, task) || strings.Contains(out, "implement expiry") {
		t.Fatalf("a completed task appeared in the new-session index: %s", out)
	}
	working := e.search(e.alpha, "parser cache")
	if find(working.Items, a) != nil || find(working.Items, b) == nil || working.Omitted.Replaced != 1 {
		t.Fatalf("working view must hold B and omit superseded A: %+v", working)
	}
	history := e.search(e.alpha, "parser cache", "--view", "history")
	ha := find(history.Items, a)
	if ha == nil || ha.Status != "superseded" || ha.ReplacedBy != b {
		t.Fatalf("A must remain labeled history with its replacement: %+v", history.Items)
	}
	r = e.must(e.alpha, "", "resume", task)
	res, by := resumeLines(t, r.Result)
	assertStale("resume", res, by)

	// Compaction of the session that selected the task rebuilds context afresh.
	out = e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "compact", "session_id": session,
		"transcript_path": transcript, "cwd": e.alpha, "model": "fixture", "permission_mode": "default"})
	var hookOut struct {
		Specific struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &hookOut); err != nil {
		t.Fatalf("hook output is not native JSON: %s", out)
	}
	ctxText := hookOut.Specific.Context
	i := strings.Index(ctxText, "{")
	j := strings.LastIndex(ctxText, "}")
	if i < 0 || j < i {
		t.Fatalf("compaction context lacks the rebuilt task: %s", ctxText)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(ctxText[i:j+1]), &env); err != nil {
		t.Fatalf("compaction context JSON: %v", err)
	}
	res, by = resumeLines(t, env.Result)
	assertStale("compaction", res, by)

	// Replay and duplicate delivery of the same transcript change nothing.
	before := e.must(e.alpha, "", "status").Raw
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": transcript,
		"cwd": e.alpha, "stop_hook_active": false})
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": transcript,
		"cwd": e.alpha, "stop_hook_active": false})
	if count(before, "events") != count(e.must(e.alpha, "", "status").Raw, "events")-1 {
		// Only the first Stop boundary marker is new; transcript events are not duplicated.
		t.Fatalf("replay duplicated events:\n%s\n%s", before, e.must(e.alpha, "", "status").Raw)
	}

	// A delayed child session delivers a conclusion after completion.
	child := writeChild(t, e, session)
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "SubagentStop", "session_id": session, "agent_id": "agent-1",
		"agent_type": "general-purpose", "agent_transcript_path": child, "transcript_path": transcript, "cwd": e.alpha,
		"stop_hook_active": false})
	t.Setenv("CLAUDE_CODE_SESSION_ID", session+"/agent-1")
	r = e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{Remaining: []string{"TODO: implement time-based expiry"},
		NextAction: "reopen and add expiry"}), "task", "checkpoint", task, "--expected-revision", "1", "--checkpoint-stdin")
	var childRes contract.TaskResult
	r.into(t, &childRes)
	if childRes.Accepted == nil || *childRes.Accepted || childRes.Task.State != "completed" {
		t.Fatalf("child conclusion must stay unaccepted and not reopen the task: %s", r.Raw)
	}
	if r := e.cli(e.alpha, "", "task", "transition", task, "--to", "reopened", "--expected-revision", itoaT(childRes.Task.Revision),
		"--reason", "child wants more"); r.errCode() != contract.CodeForbidden {
		t.Fatalf("a child session must not change the parent's task state: %s", r.Raw)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	r = e.must(e.alpha, "", "resume", task)
	res, by = resumeLines(t, r.Result)
	assertStale("after child", res, by)
	if len(by["child_conclusion"]) != 1 || by["child_conclusion"][0].State != "unaccepted" {
		t.Fatalf("child conclusion must be visible and attributed: %+v", res.Lines)
	}
}

func itoaT(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// count extracts a numeric "name":N field from raw JSON.
func count(raw, name string) int {
	key := `"` + name + `":`
	i := strings.Index(raw, key)
	if i < 0 {
		return -1
	}
	n := 0
	for _, c := range raw[i+len(key):] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func writeChild(t *testing.T, e *testEnv, session string) string {
	t.Helper()
	path := filepath.Join(e.claudeRoot, "proj", session, "subagents", "agent-1.jsonl")
	line := `{"type":"assistant","uuid":"c1","sessionId":"` + session + `","agentId":"agent-1","isSidechain":true,` +
		`"timestamp":"2026-10-19T08:20:00.000Z","cwd":"` + e.alpha + `","version":"2.1.302",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"Child finding: expiry could still help."}]}}` + "\n"
	writeFile(t, path, line)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
