package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commandFixtures(command any) []struct {
	adapter Adapter
	line    map[string]any
	want    string
} {
	args, _ := json.Marshal(map[string]any{"command": command})
	return []struct {
		adapter Adapter
		line    map[string]any
		want    string
	}{
		{Claude{}, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": command}},
		}}}, "tool Bash"},
		{Codex{}, map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "function_call", "name": "shell", "arguments": string(args),
		}}, "tool shell"},
		{OpenCode{}, map[string]any{"sessionID": "synthetic", "message": map[string]any{
			"role": "assistant", "parts": []any{map[string]any{"type": "tool", "name": "bash",
				"state": map[string]any{"status": "completed", "input": map[string]any{"command": command}}}},
		}}, "tool bash: succeeded"},
	}
}

func TestAdaptersKeepEmptyToolCommandsWithoutPanic(t *testing.T) {
	for _, command := range []any{"", " \t\r\n", []any{"bash", "-lc", ""}, []any{"sh", "-c", " \t"}} {
		for _, fixture := range commandFixtures(command) {
			line, err := json.Marshal(fixture.line)
			if err != nil {
				t.Fatal(err)
			}
			events, err := fixture.adapter.Parse(line, &State{})
			if err != nil || len(events) != 1 || events[0].Text != fixture.want {
				t.Fatalf("%s command %#v: events=%+v error=%v", fixture.adapter.Harness(), command, events, err)
			}
		}
	}
}

func FuzzAdaptersParseToolCommands(f *testing.F) {
	for _, command := range []string{"", " \t\n", "git status", "'quoted command'", "$(secret)", "/usr/bin/printf argument"} {
		f.Add(command)
	}
	f.Fuzz(func(t *testing.T, command string) {
		if len(command) > 1<<16 {
			t.Skip()
		}
		for _, value := range []any{command, []any{"bash", "-lc", command}} {
			for _, fixture := range commandFixtures(value) {
				line, err := json.Marshal(fixture.line)
				if err != nil {
					t.Fatal(err)
				}
				events, err := fixture.adapter.Parse(line, &State{})
				if err != nil || len(events) != 1 || events[0].Kind != KindToolCall {
					t.Fatalf("%s: events=%+v error=%v", fixture.adapter.Harness(), events, err)
				}
			}
		}
	})
}

func TestIncrementAtReadLimitAndOversizedProgress(t *testing.T) {
	for _, lineLimit := range []int{8, 32} {
		path := filepath.Join(t.TempDir(), "synthetic.jsonl")
		if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
			t.Fatal(err)
		}
		cur := Cursor{Path: path}
		lines, cur, err := Increment(cur, 8, lineLimit)
		if err != nil || len(lines) != 0 || cur.Offset != 0 || cur.Oversized != 0 {
			t.Fatalf("partial at limit: lines=%v cursor=%+v error=%v", lines, cur, err)
		}
		if err := os.WriteFile(path, []byte("12345678\nnext\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		lines, cur, err = Increment(cur, 8, lineLimit)
		if err != nil || len(lines) != 1 || string(lines[0].Data) != "12345678" || cur.Offset != 9 {
			t.Fatalf("complete at limit: lines=%v cursor=%+v error=%v", lines, cur, err)
		}
		lines, cur, err = Increment(cur, 8, lineLimit)
		if err != nil || len(lines) != 1 || string(lines[0].Data) != "next" || cur.LineNo != 2 {
			t.Fatalf("following line: lines=%v cursor=%+v error=%v", lines, cur, err)
		}
	}

	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 31)), 0o600); err != nil {
		t.Fatal(err)
	}
	cur := Cursor{Path: path}
	for cur.Offset < 31 {
		before := cur.Offset
		lines, next, err := Increment(cur, 8, 8)
		if err != nil || len(lines) != 0 || next.Offset <= before || next.Oversized != 1 || !next.Skipping {
			t.Fatalf("oversized progress: lines=%v cursor=%+v error=%v", lines, next, err)
		}
		cur = next
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\nok\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	lines, cur, err := Increment(cur, 8, 8)
	if err != nil || len(lines) != 1 || string(lines[0].Data) != "ok" || cur.LineNo != 2 || cur.Oversized != 1 || cur.Skipping {
		t.Fatalf("after skipped line: lines=%v cursor=%+v error=%v", lines, cur, err)
	}
}

func TestIncrementRejectsInvalidLimits(t *testing.T) {
	for _, limits := range []struct {
		bytes int64
		line  int
	}{{0, 8}, {-1, 8}, {8, 0}, {8, -1}, {int64(int(^uint(0) >> 1)), 8}} {
		if _, _, err := Increment(Cursor{}, limits.bytes, limits.line); err == nil {
			t.Fatalf("accepted limits %+v", limits)
		}
	}
}
