package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/mcpbridge"
	"github.com/timfewi/ichnora/internal/server"
)

// Invalidating a source propagates through derived summaries; repetition
// neither renews validity nor adds corroboration; cursors expire.
func TestInvalidationPropagatesAndRepetitionIsNotEvidence(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	session := "claude-inv"
	transcript := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "p", session+".jsonl"), session, e.alpha)
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": transcript, "cwd": e.alpha})
	src := e.search(e.alpha, "cache invalidation fix")
	var turn string
	for _, it := range src.Items {
		if it.Group == "source" {
			turn = it.ID
		}
	}
	if turn == "" {
		t.Fatalf("ingested turn not searchable: %+v", src)
	}
	obs := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Parser cache misses digest changes",
		Body: "Observed in the planning turn.", Support: []contract.Ref{{Kind: "turn", Ref: turn}}})[0]
	sums := e.captureIDs(e.alpha,
		contract.ClaimInput{Kind: "summary", Title: "Summary one: parser cache misses digests", Body: "Restated.", DerivedFrom: []string{obs}},
		contract.ClaimInput{Kind: "summary", Title: "Summary two: parser cache misses digests", Body: "Restated again.", DerivedFrom: []string{obs}})
	r := e.must(e.alpha, "", "read", obs, sums[0], sums[1])
	var rd contract.ReadResult
	r.into(t, &rd)
	for _, rec := range rd.Records {
		var data struct {
			Support int `json:"independent_support"`
		}
		json.Unmarshal(rec.Data, &data)
		if data.Support != 1 {
			t.Fatalf("repeated summaries must not add corroboration: %s has %d", rec.ID, data.Support)
		}
	}
	first := e.search(e.alpha, "parser cache digests", "--limit", "1")
	if first.Omitted.Duplicates < 1 {
		t.Fatalf("duplicate lineage was not collapsed: %+v", first)
	}
	for i := 0; i < 3; i++ {
		e.search(e.alpha, "parser cache digests")
	}
	again := e.search(e.alpha, "parser cache digests", "--limit", "1")
	if again.Items[0].Provenance.Support != first.Items[0].Provenance.Support || again.Items[0].Provenance.ValidatedAt != first.Items[0].Provenance.ValidatedAt {
		t.Fatal("retrieval changed support or validation time")
	}
	cursor := e.search(e.alpha, "parser", "--limit", "1").NextCursor
	if cursor == "" {
		t.Fatal("expected a next cursor")
	}
	r = e.must(e.alpha, "", "update", turn, "--status", "invalidated", "--reason", "transcript was wrong")
	var up contract.UpdateResult
	r.into(t, &up)
	if len(up.Recomputed) != 3 {
		t.Fatalf("invalidation must recompute the observation and both summaries: %s", r.Raw)
	}
	after := e.search(e.alpha, "parser cache digests", "--view", "all")
	for _, id := range append([]string{obs}, sums...) {
		it := find(after.Items, id)
		if it == nil || it.Status != "unsupported" || it.Group == "working" {
			t.Fatalf("%s must be unsupported and out of the working view: %+v", id, it)
		}
	}
	if r := e.cli(e.alpha, "", "search", "parser", "--limit", "1", "--cursor", cursor); r.errCode() != contract.CodeCursorExpired {
		t.Fatalf("a cursor must not survive an invalidation: %s", r.Raw)
	}
	r = e.must(e.alpha, "", "evidence", obs)
	var ev contract.EvidenceResult
	r.into(t, &ev)
	if len(ev.Gaps) == 0 || !strings.Contains(ev.Checks[0].Validation, "invalidated") {
		t.Fatalf("lost support must be an explicit gap: %s", r.Raw)
	}
}

// Conflicts and unknown applicability survive a minimal budget.
func TestSmallBudgetKeepsConflictsAndUnknowns(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	x := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Ship the parser behind a flag " + strings.Repeat("ü", 80),
		Body: strings.Repeat("Unicode 😀 metadata ", 40)})[0]
	e.captureIDs(e.alpha,
		contract.ClaimInput{Kind: "decision", Title: "Ship the parser without a flag", Contradicts: []string{x}},
		contract.ClaimInput{Kind: "observation", Title: "Parser flag usage is unclear", Body: "No dependency declared."})
	for _, budget := range []string{"1024", "1300", "2048"} {
		r := e.must(e.alpha, "", "search", "parser flag", "--max-bytes", budget)
		if len(r.Raw) > atoiT(budget) || !json.Valid([]byte(r.Raw)) {
			t.Fatalf("budget %s exceeded or invalid JSON (%d bytes)", budget, len(r.Raw))
		}
		var out contract.SearchResult
		r.into(t, &out)
		if out.Totals.Conflicts != 2 || out.Totals.Unknown != 1 {
			t.Fatalf("budget %s lost conflict/unknown totals: %+v", budget, out.Totals)
		}
		if len(out.Items) < 3 && out.Complete {
			t.Fatalf("budget %s presented a partial answer as complete", budget)
		}
	}
	for _, small := range []string{"1", "100", "1023"} {
		if r := e.cli(e.alpha, "", "search", "parser", "--max-bytes", small); r.errCode() != contract.CodeBudgetTooSmall || r.Code != 2 || r.Error.MinBytes != 1024 {
			t.Fatalf("below-minimum budget must be rejected with the minimum: %s", r.Raw)
		}
	}
}

func atoiT(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// Revocation and withdrawal stop recall, including held IDs and cursors.
// Reader/capture callers cannot administer; the MCP bridge shares behavior.
func TestRevocationRolesAndMCPParity(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	beta := e.enroll(e.beta)
	shared := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Use UTC timestamps everywhere"})[0]
	r := e.must(e.root, "", "share", shared, "--to", beta)
	var sh struct {
		ID string `json:"id"`
	}
	r.into(t, &sh)
	seen := e.search(e.beta, "UTC timestamps", "--limit", "1")
	if it := find(seen.Items, shared); it == nil || it.Provenance.SharedFrom == "" {
		t.Fatalf("shared claim must be visible and attributed in beta: %+v", seen)
	}
	e.captureIDs(e.beta, contract.ClaimInput{Kind: "note", Title: "UTC timestamps in beta logs"},
		contract.ClaimInput{Kind: "note", Title: "UTC timestamps in beta reports"})
	page := e.search(e.beta, "UTC timestamps", "--limit", "1")
	e.must(e.root, "", "revoke", sh.ID)
	r = e.must(e.beta, "", "read", shared)
	var rd contract.ReadResult
	r.into(t, &rd)
	if len(rd.Records) != 0 {
		t.Fatalf("a held ID still resolves after revocation: %s", r.Raw)
	}
	if r := e.cli(e.beta, "", "search", "UTC timestamps", "--limit", "1", "--cursor", page.NextCursor); r.errCode() != contract.CodeCursorExpired {
		t.Fatalf("a cursor still works after revocation: %s", r.Raw)
	}
	if find(e.search(e.beta, "UTC timestamps").Items, shared) != nil {
		t.Fatal("revoked claim still recalled")
	}
	// Sharing a claim does not share the origin project's source text.
	evidenced := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Parser cache key comment",
		Support: []contract.Ref{{Kind: "file", Path: "parser/cache.go", Line: 3}}})[0]
	e.must(e.root, "", "share", evidenced, "--to", beta)
	if r := e.must(e.beta, "", "evidence", evidenced); strings.Contains(r.Raw, `"excerpt"`) || !strings.Contains(r.Raw, "excerpts withheld") {
		t.Fatalf("shared evidence leaked origin excerpts: %s", r.Raw)
	}

	// Agent socket roles cannot administer.
	for _, args := range [][]string{{"enroll", e.beta}, {"withdraw", "alpha"}, {"share", shared, "--to", beta}, {"projects"}} {
		t.Setenv("ICHNORA_ADMIN_SOCKET", e.cfg.AgentSocket)
		if r := e.cli(e.alpha, "", args...); r.errCode() != contract.CodeForbidden {
			t.Fatalf("agent socket allowed %v: %s", args, r.Raw)
		}
		t.Setenv("ICHNORA_ADMIN_SOCKET", e.cfg.AdminSocket)
	}

	// The MCP bridge uses the same core; a reader-only bridge cannot capture.
	ctx := context.Background()
	call := func(role, tool string, args any) contract.Response {
		t.Helper()
		srv := mcpbridge.Server("test", server.Client{Socket: e.cfg.AgentSocket, Role: role}, func() *contract.SessionHint { return nil })
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
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		out, err := mcpbridge.Decode(res)
		if err != nil {
			t.Fatalf("%v: %+v", err, res.Content[0])
		}
		return out
	}
	if err := os.Chdir(e.alpha); err != nil {
		t.Fatal(err)
	}
	viaMCP := call("", "memory_search", map[string]any{"query": "UTC timestamps"})
	viaCLI := e.must(e.alpha, "", "search", "UTC timestamps")
	mcpJSON, _ := json.Marshal(viaMCP.Result)
	var cliResult any
	json.Unmarshal(viaCLI.Result, &cliResult)
	cliJSON, _ := json.Marshal(cliResult)
	if !viaMCP.OK || string(mcpJSON) != string(cliJSON) {
		t.Fatalf("CLI and MCP disagree:\nmcp %s\ncli %s", mcpJSON, cliJSON)
	}
	if out := call("reader", "memory_capture", map[string]any{"claims": []map[string]any{{"kind": "note", "title": "x"}}}); out.OK || out.Error.Code != contract.CodeForbidden {
		t.Fatalf("reader bridge captured: %+v", out)
	}

	// Withdrawal stops recall and collection; history remains for the owner.
	e.must(e.root, "", "withdraw", "alpha")
	if r := e.cli(e.alpha, "", "search", "UTC"); r.errCode() != contract.CodeOutOfScope {
		t.Fatalf("withdrawn project still recalled: %s", r.Raw)
	}
	out := e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "late",
		"transcript_path": nil, "cwd": e.alpha})
	if strings.Contains(out, "ichnora-context") {
		t.Fatalf("withdrawn project still injected context: %s", out)
	}
}

// An unrelated project's records never change results, counts or cursors.
func TestUnrelatedProjectDoesNotAffectResults(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	e.enroll(e.beta)
	e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Parser tokens are interned"},
		contract.ClaimInput{Kind: "note", Title: "Parser token table grows"},
		contract.ClaimInput{Kind: "observation", Title: "Parser token reuse observed"})
	baseline := e.must(e.alpha, "", "search", "parser token", "--limit", "2").Raw
	var claims []contract.ClaimInput
	for i := 0; i < 15; i++ {
		claims = append(claims, contract.ClaimInput{Kind: "decision", Title: "Parser token parser token parser token " + itoaT(int64(i))})
	}
	e.captureIDs(e.beta, claims...)
	e.captureIDs(e.beta, contract.ClaimInput{Kind: "observation", Title: "Parser token conflict", Contradicts: nil})
	if again := e.must(e.alpha, "", "search", "parser token", "--limit", "2").Raw; again != baseline {
		t.Fatalf("beta's records changed alpha's results:\n%s\n%s", baseline, again)
	}
}

// Events of an alpha session that ran in beta, and a child session working in
// beta, stay unattributed: neither project can retrieve them.
func TestMixedProjectAndChildSessionsCannotWidenAccess(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	e.enroll(e.beta, "--collect")
	session := "claude-mixed"
	path := filepath.Join(e.claudeRoot, "p", session+".jsonl")
	line := func(uuid, cwd, text string) string {
		b, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "sessionId": session, "timestamp": "2026-10-19T08:00:00Z",
			"cwd": cwd, "message": map[string]any{"role": "user", "content": text}})
		return string(b) + "\n"
	}
	writeFile(t, path, line("m1", e.alpha, "alpha work on the lexer")+line("m2", e.beta, "beta private roadmap detail")+
		line("m3", filepath.Join(e.root, "elsewhere"), "unenrolled scratch notes"))
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": path, "cwd": e.alpha})
	child := filepath.Join(e.claudeRoot, "p", session, "subagents", "agent-9.jsonl")
	writeFile(t, child, `{"type":"assistant","uuid":"k1","sessionId":"`+session+`","isSidechain":true,"cwd":"`+e.beta+
		`","message":{"content":[{"type":"text","text":"child read beta private roadmap"}]}}`+"\n")
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "SubagentStop", "session_id": session, "agent_id": "agent-9",
		"agent_transcript_path": child, "transcript_path": path, "cwd": e.alpha})
	if s := e.search(e.alpha, "lexer"); s.Totals.Sources != 1 {
		t.Fatalf("alpha's own event must be retrievable: %+v", s.Totals)
	}
	for _, dir := range []string{e.alpha, e.beta} {
		if s := e.search(dir, "roadmap scratch"); s.Totals.Sources != 0 {
			t.Fatalf("misattributed events visible from %s: %+v", dir, s.Items)
		}
	}
	if s := e.search(e.alpha, "lexer"); s.Omitted.Unattributed != 3 {
		t.Fatalf("the attribution gap must be reported: %+v", s.Omitted)
	}
	// Beta cannot extend alpha's session by reusing its native ID.
	e.hook(e.beta, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": path, "cwd": e.beta})
	if count(e.must(e.beta, "", "status").Raw, "sessions") != 0 || count(e.must(e.beta, "", "status").Raw, "events") != 0 {
		t.Fatal("a foreign project's hook extended the session")
	}
}

// Every read operation stays within its byte budget with valid JSON, even
// with a maximal project name, Unicode and many omissions.
func TestBudgetsAcrossOperations(t *testing.T) {
	e := newEnv(t, nil)
	e.must(e.root, "", "enroll", e.alpha, "--name", strings.Repeat("ä", 32))
	var claims []contract.ClaimInput
	for i := 0; i < 20; i++ {
		claims = append(claims, contract.ClaimInput{Kind: "observation", Title: "Ünïcödé budget probe 😀 " + itoaT(int64(i)),
			Body: strings.Repeat("Großer Inhalt 字 ", 200), Support: []contract.Ref{{Kind: "file", Path: "parser/cache.go"}}})
	}
	ids := e.captureIDs(e.alpha, claims...)
	r := e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{NextAction: strings.Repeat("ß", 500), Objective: strings.Repeat("o", 3000),
		Remaining: []string{strings.Repeat("r", 1000), strings.Repeat("s", 1000)}}), "task", "create", "--title", strings.Repeat("T", 200), "--checkpoint-stdin")
	var task contract.TaskResult
	r.into(t, &task)
	for _, args := range [][]string{
		{"search", "budget probe", "--limit", "50"},
		append([]string{"read"}, ids...),
		{"evidence", ids[0]},
		{"resume", task.Task.ID},
		{"status"},
	} {
		for _, budget := range []string{"1024", "1500", "4096"} {
			r := e.must(e.alpha, "", append(args, "--max-bytes", budget)...)
			if len(r.Raw) > atoiT(budget) || !json.Valid([]byte(r.Raw)) {
				t.Fatalf("%s at %s: %d bytes", args[0], budget, len(r.Raw))
			}
			if budget == "1024" && strings.Contains(r.Raw, `"complete":true`) && args[0] != "evidence" {
				t.Fatalf("%s at 1024 claims completeness: %s", args[0], r.Raw)
			}
		}
	}
}

// A dirty worktree: HEAD is unchanged but the file differs, so the claim is stale.
func TestDirtyWorktreeIsNotCurrent(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	cache := filepath.Join(e.alpha, "parser", "cache.go")
	id := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Key returns v1",
		Support: []contract.Ref{{Kind: "file", Path: "parser/cache.go"}},
		Depends: []contract.Ref{{Kind: "revision", Value: "current"}, {Kind: "file", Path: "parser/cache.go", SHA256: sha(t, cache)}}})[0]
	if it := find(e.search(e.alpha, "key returns").Items, id); it == nil || it.Applicability != "current" {
		t.Fatalf("clean claim must be current: %+v", it)
	}
	writeFile(t, cache, "package parser\n\nfunc Key() string { return \"uncommitted\" }\n")
	r := e.must(e.alpha, "", "read", id)
	if !strings.Contains(r.Raw, `"applicability":"stale"`) || !strings.Contains(r.Raw, "revision unchanged") || !strings.Contains(r.Raw, "file changed: parser/cache.go") {
		t.Fatalf("an uncommitted change must make the claim stale despite an unchanged HEAD: %s", r.Raw)
	}
}
