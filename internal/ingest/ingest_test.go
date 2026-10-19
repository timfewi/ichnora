package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseAll(t *testing.T, a Adapter, lines ...string) ([]Event, int) {
	t.Helper()
	var st State
	var out []Event
	malformed := 0
	for _, l := range lines {
		evs, err := a.Parse([]byte(l), &st)
		if err != nil {
			malformed++
			continue
		}
		for i := range evs {
			Normalize(&evs[i], 4096)
		}
		out = append(out, evs...)
	}
	return out, malformed
}

func TestClaudeParserKeepsPermittedFieldsOnly(t *testing.T) {
	evs, bad := parseAll(t, Claude{},
		`{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-10-19T08:00:05Z","cwd":"/w/p","isSidechain":true,"agentId":"ag","message":{"content":[{"type":"thinking","thinking":"HIDDEN"},{"type":"text","text":"answer"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"/usr/bin/git push --token=SECRET_ARG"}}]}}`,
		`{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"SECRET_OUTPUT","is_error":true}]}}`,
		`{"type":"user","uuid":"u3","isCompactSummary":true,"message":{"content":"SUMMARY_TEXT"}}`,
		`{"type":"user","uuid":"u4","isMeta":true,"message":{"content":"caveat"}}`,
		`not json`,
		`{"type":"file-history-snapshot"}`,
	)
	if bad != 1 {
		t.Fatalf("malformed count %d", bad)
	}
	joined := ""
	for _, e := range evs {
		joined += e.Text + "|" + e.Kind + ";"
	}
	for _, leaked := range []string{"HIDDEN", "SECRET_ARG", "SECRET_OUTPUT", "SUMMARY_TEXT", "push"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("excluded field %q stored: %s", leaked, joined)
		}
	}
	if evs[0].Kind != KindAssistant || !evs[0].Child || evs[0].AgentID != "ag" || evs[0].NativeID != "a1#1" {
		t.Fatalf("assistant event: %+v", evs[0])
	}
	if evs[1].Kind != KindToolCall || evs[1].Text != "tool Bash (git)" || evs[1].Cwd != "/w/p" {
		t.Fatalf("tool call must keep only name and command: %+v", evs[1])
	}
	if evs[2].Kind != KindToolResult || evs[2].Outcome != "failed" || evs[2].Cwd != "/w/p" {
		t.Fatalf("tool result: %+v", evs[2])
	}
	if !evs[3].Derived || evs[3].Kind != KindCompaction {
		t.Fatalf("compaction summary must be derived: %+v", evs[3])
	}
	if evs[4].Kind != KindOther {
		t.Fatalf("meta message must not be user evidence: %+v", evs[4])
	}
	if evs[2].EventTime != "" || !contains(evs[2].Coverage, "event_time") {
		t.Fatal("missing timestamps must stay unknown and be reported")
	}
}

func TestCodexParserCarriesStateAndSkipsDuplicates(t *testing.T) {
	evs, _ := parseAll(t, Codex{},
		`{"timestamp":"2026-10-19T09:00:00Z","type":"session_meta","payload":{"id":"th","cwd":"/w/p","cli_version":"0.163.0","source":{"subagent":{"parent":"x"}}}}`,
		`{"timestamp":"2026-10-19T09:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<user_instructions>AGENTS</user_instructions>"}]}}`,
		`{"timestamp":"2026-10-19T09:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"real prompt"}]}}`,
		`{"timestamp":"2026-10-19T09:00:04Z","type":"event_msg","payload":{"type":"user_message","message":"real prompt"}}`,
		`{"timestamp":"2026-10-19T09:00:05Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"nix build SECRET\"]}","call_id":"c"}}`,
		`{"timestamp":"2026-10-19T09:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c","output":"{\"metadata\":{\"exit_code\":0}}"}}`,
	)
	if len(evs) != 5 || !evs[0].Child || evs[0].Session != "th" {
		t.Fatalf("events: %+v", evs)
	}
	if evs[1].Kind != KindOther || evs[1].Text != "" || evs[2].Text != "real prompt" || evs[2].Cwd != "/w/p" {
		t.Fatalf("injected instructions must not become user evidence: %+v", evs[1:3])
	}
	if evs[3].Text != "tool shell (nix)" || evs[4].Outcome != "succeeded" {
		t.Fatalf("tool events: %+v", evs[3:])
	}
	if evs[2].NativeID == evs[1].NativeID || evs[2].IDBasis != "fallback" {
		t.Fatal("fallback identities must be per-line digests")
	}
}

func TestOpenCodeParser(t *testing.T) {
	evs, _ := parseAll(t, OpenCode{},
		`{"sessionID":"oc","parentID":"op","directory":"/w/p","message":{"id":"m1","type":"assistant","time":{"created":1760864405000},"content":[{"type":"text","text":"done"},{"type":"tool","id":"t","name":"bash","state":{"status":"error","input":{"command":"rm -rf SECRET"},"output":"SECRET"}}]}}`,
	)
	if len(evs) != 2 || !evs[0].Child || evs[0].EventTime == "" || evs[1].Outcome != "failed" || strings.Contains(evs[1].Text, "SECRET") {
		t.Fatalf("opencode events: %+v", evs)
	}
}

func TestCleanRedactsAndStripsInjectedContext(t *testing.T) {
	in := "before <ichnora-context id=\"x\">REMEMBERED</ichnora-context> after ghp_" + strings.Repeat("A", 30) +
		" password=hunter22 -----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY----- and AKIA" + strings.Repeat("B", 16)
	out, red, inj, _ := Clean(in, 4096)
	if !inj || strings.Contains(out, "REMEMBERED") || strings.Contains(out, "hunter22") || strings.Contains(out, "abc") ||
		strings.Contains(out, "ghp_") || strings.Contains(out, "AKIA") || len(red) != 4 {
		t.Fatalf("clean: %q %v", out, red)
	}
	unterminated, _, inj, _ := Clean("x <ichnora-context>open forever", 4096)
	if !inj || strings.Contains(unterminated, "open forever") {
		t.Fatal("unterminated injected context must be stripped")
	}
	cut, _, _, trunc := Clean(strings.Repeat("é", 10), 5)
	if !trunc || cut != "éé" {
		t.Fatalf("truncation must respect UTF-8: %q", cut)
	}
}

func TestIncrementHandlesPartialRotationAndOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	os.WriteFile(path, []byte("{\"a\":1}\n{\"b\":2}\n{\"partial\":"), 0o600)
	lines, cur, err := Increment(Cursor{Path: path}, 1<<20, 1024)
	if err != nil || len(lines) != 2 || cur.LineNo != 2 {
		t.Fatalf("first read: %v %d %+v", err, len(lines), cur)
	}
	// Completing the partial line makes it readable exactly once.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("3}\n" + strings.Repeat("x", 2000) + "\n{\"c\":4}\n")
	f.Close()
	lines, cur, _ = Increment(cur, 1<<20, 1024)
	if len(lines) != 2 || string(lines[0].Data) != `{"partial":3}` || cur.Oversized != 1 {
		t.Fatalf("after append: %d lines, %+v", len(lines), cur)
	}
	again, same, _ := Increment(cur, 1<<20, 1024)
	if len(again) != 0 || same.Offset != cur.Offset {
		t.Fatal("re-reading at EOF must be empty")
	}
	// Rotation: a replaced file restarts at zero and is counted.
	os.Remove(path)
	os.WriteFile(path, []byte("{\"a\":1}\n{\"d\":5}\n"), 0o600)
	lines, cur, _ = Increment(cur, 1<<20, 1024)
	if cur.Rotations != 1 || len(lines) != 2 {
		t.Fatalf("rotation: %+v %d", cur, len(lines))
	}
	// An oversized line still being written is skipped through its newline.
	os.WriteFile(path, []byte(strings.Repeat("y", 3000)), 0o600)
	cur = Cursor{Path: path}
	lines, cur, _ = Increment(cur, 1<<20, 1024)
	if len(lines) != 0 || !cur.Skipping || cur.Oversized != 1 {
		t.Fatalf("oversized partial: %+v", cur)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("tail\n{\"e\":6}\n")
	f.Close()
	lines, cur, _ = Increment(cur, 1<<20, 1024)
	if len(lines) != 1 || string(lines[0].Data) != `{"e":6}` || cur.Skipping {
		t.Fatalf("after oversized line: %d %+v", len(lines), cur)
	}
	// Symlinks are never followed.
	link := filepath.Join(dir, "link.jsonl")
	os.Symlink(path, link)
	if _, _, err := Increment(Cursor{Path: link}, 1<<20, 1024); err == nil {
		t.Fatal("symlinked transcript was read")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
