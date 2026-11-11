package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
	"github.com/timfewi/ichnora/internal/store"
)

func TestOpenCodeLegacyReplayPreservesStoredSources(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	payload := map[string]any{"event": "context", "sessionID": "legacy", "directory": e.alpha,
		"messages": []any{map[string]any{"id": "legacy-message", "role": "user", "text": "retained legacy content"}}}
	e.hook(e.alpha, "opencode", payload)
	e.stop()
	// Construct a synthetic adapter/1 storage fixture while the service is
	// offline; normal clients continue to use sockets exclusively.
	st, err := store.Open(context.Background(), filepath.Join(e.state, dbName), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var legacyEventID string
	err = st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE events SET native_id = 'legacy-message', adapter_version = 'opencode-adapter/1',
			flags = json_remove(flags, '$.message_key') WHERE kind = 'user'`)
		if err != nil {
			return err
		}
		return tx.QueryRow(`SELECT id FROM events WHERE native_id = 'legacy-message'`).Scan(&legacyEventID)
	})
	if closeErr := st.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	e.start()
	read := func() contract.HookResult {
		var envelope struct {
			Result contract.HookResult `json:"result"`
		}
		if err := json.Unmarshal([]byte(e.hook(e.alpha, "opencode", payload)), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Result
	}
	if got := read(); got.Ingested != 0 || got.AcceptedMessages != 1 {
		t.Fatalf("exact adapter/1 replay duplicated source: %+v", got)
	}
	payload["version"] = "wrapper-metadata-changed"
	if got := read(); got.Ingested != 0 || got.AcceptedMessages != 1 {
		t.Fatalf("wrapper-only change duplicated normalized legacy content: %+v", got)
	}
	payload["messages"] = []any{map[string]any{"id": "legacy-message", "role": "user", "text": "meaningfully changed content"}}
	if got := read(); got.Ingested != 1 || got.AcceptedMessages != 1 {
		t.Fatalf("new native version was lost after legacy replay: %+v", got)
	}
	if got := read(); got.Ingested != 0 {
		t.Fatalf("updated version was not idempotent: %+v", got)
	}
	message, _ := json.Marshal(payload["messages"].([]any)[0])
	line, _ := json.Marshal(ingest.OpenCodeLine{SessionID: "legacy", Directory: e.alpha, Version: "wrapper-metadata-changed", Message: message})
	events, err := (ingest.OpenCode{}).Parse(line, &ingest.State{})
	if err != nil || len(events) != 1 {
		t.Fatalf("updated legacy fixture parse: %v %+v", err, events)
	}
	updatedEventID := ingest.ID("evt_", ingest.ID("ses_", "opencode", "legacy"), events[0].NativeID)
	support := []contract.Ref{{Kind: "event", Ref: legacyEventID}, {Kind: "event", Ref: updatedEventID}}
	for _, item := range e.search(e.alpha, "content").Items {
		if item.Group == "source" {
			support = append(support, contract.Ref{Kind: "turn", Ref: item.ID})
		}
	}
	if len(support) != 4 {
		t.Fatalf("expected old and new events and turns, got %d references", len(support))
	}
	id := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "summary", Title: "Legacy streaming conclusion", Support: support})[0]
	var records contract.ReadResult
	e.must(e.alpha, "", "read", id).into(t, &records)
	var claim struct {
		IndependentSupport int `json:"independent_support"`
	}
	if len(records.Records) != 1 || json.Unmarshal(records.Records[0].Data, &claim) != nil || claim.IndependentSupport != 1 {
		t.Fatalf("legacy and updated source versions added corroboration: %+v", records)
	}
}

func TestOpenCodeLegacyReplayKeepsChangedAttribution(t *testing.T) {
	for _, kind := range []string{"unassigned", "worktree"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, nil)
			e.enroll(e.alpha, "--collect")
			oldDirectory, nextDirectory := e.beta, e.alpha
			if kind == "worktree" {
				e.enroll(e.beta, "--worktree-of", "alpha", "--collect")
				oldDirectory, nextDirectory = e.alpha, e.beta
			}
			payload := map[string]any{"event": "context", "sessionID": "legacy-scope", "directory": oldDirectory,
				"messages": []any{map[string]any{"id": "legacy-scope-message", "role": "user", "text": "scope repaired source"}}}
			e.hook(e.alpha, "opencode", payload)
			e.stop()
			st, err := store.Open(context.Background(), filepath.Join(e.state, dbName), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			err = st.Write(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE events SET native_id = 'legacy-scope-message', adapter_version = 'opencode-adapter/1',
					flags = json_remove(flags, '$.message_key') WHERE kind = 'user'`)
				return err
			})
			if closeErr := st.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			e.start()
			payload["directory"] = nextDirectory
			var envelope struct {
				Result contract.HookResult `json:"result"`
			}
			if err := json.Unmarshal([]byte(e.hook(e.alpha, "opencode", payload)), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Result.Ingested != 1 || envelope.Result.AcceptedMessages != 1 {
				t.Fatalf("new attribution suppressed: %+v", envelope.Result)
			}
			if got := e.search(e.alpha, "scope repaired"); got.Totals.Sources == 0 {
				t.Fatalf("attributed source unavailable: %+v", got)
			}
		})
	}
}

func TestOpenCodeStreamingVersionsDoNotAddIndependentSupport(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	var support []contract.Ref
	for _, text := range []string{"streaming version first", "streaming version updated"} {
		message := map[string]any{"id": "native-message", "role": "user", "text": text}
		e.hook(e.alpha, "opencode", map[string]any{"event": "context", "sessionID": "lineage", "directory": e.alpha, "messages": []any{message}})
		raw, _ := json.Marshal(message)
		line, _ := json.Marshal(ingest.OpenCodeLine{SessionID: "lineage", Directory: e.alpha, Message: raw})
		events, err := (ingest.OpenCode{}).Parse(line, &ingest.State{})
		if err != nil || len(events) != 1 {
			t.Fatalf("fixture version parse: %v %+v", err, events)
		}
		eventID := ingest.ID("evt_", ingest.ID("ses_", "opencode", "lineage"), events[0].NativeID)
		support = append(support, contract.Ref{Kind: "event", Ref: eventID})
	}
	for _, item := range e.search(e.alpha, "streaming version").Items {
		if item.Group == "source" {
			support = append(support, contract.Ref{Kind: "turn", Ref: item.ID})
		}
	}
	if len(support) != 4 {
		t.Fatalf("expected two event and two turn references, got %d", len(support))
	}
	id := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "summary", Title: "Streaming source conclusion", Support: support})[0]
	var read contract.ReadResult
	e.must(e.alpha, "", "read", id).into(t, &read)
	var claim struct {
		IndependentSupport int `json:"independent_support"`
	}
	if len(read.Records) != 1 || json.Unmarshal(read.Records[0].Data, &claim) != nil || claim.IndependentSupport != 1 {
		t.Fatalf("streaming versions added independent corroboration: %+v", read)
	}
}

func TestOpenCodeAcknowledgesCommittedVersionsAndCoverage(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	payload := map[string]any{"event": "context", "sessionID": "versions", "directory": e.alpha,
		"messages": []any{map[string]any{"id": "message", "role": "user", "text": "initial snapshot"}}}
	read := func() contract.HookResult {
		var envelope struct {
			Result contract.HookResult `json:"result"`
		}
		if err := json.Unmarshal([]byte(e.hook(e.alpha, "opencode", payload)), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Result
	}
	if got := read(); got.AcceptedMessages != 1 || got.Ingested != 1 {
		t.Fatalf("first batch: %+v", got)
	}
	if got := read(); got.AcceptedMessages != 1 || got.Ingested != 0 {
		t.Fatalf("idempotent acknowledgement: %+v", got)
	}
	payload["messages"] = []any{map[string]any{"id": "message", "role": "user", "text": "updated snapshot"}}
	if got := read(); got.AcceptedMessages != 1 || got.Ingested != 1 {
		t.Fatalf("updated native message: %+v", got)
	}
	if got := e.search(e.alpha, "updated"); got.Totals.Sources != 1 {
		t.Fatalf("updated text unavailable: %+v", got)
	}
	payload["messages"] = []any{map[string]any{"id": "large", "role": "ichnora_omission", "omitted_bytes": 70000}, nil}
	if got := read(); got.AcceptedMessages != 2 || got.OmittedMessages != 1 {
		t.Fatalf("coverage acknowledgement: %+v", got)
	}
	read()
	status := e.must(e.alpha, "", "status").Raw
	if !strings.Contains(status, `"oversized_messages":1`) || !strings.Contains(status, `"malformed_messages":1`) {
		t.Fatalf("coverage gaps missing or counted again after retry: %s", status)
	}
}

func TestOpenCodePausedBatchIsNotAcknowledged(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Limits.MaxDBBytes = 1 })
	e.enroll(e.alpha, "--collect")
	var envelope struct {
		Result contract.HookResult `json:"result"`
	}
	out := e.hook(e.alpha, "opencode", map[string]any{"event": "context", "sessionID": "paused", "directory": e.alpha,
		"messages": []any{map[string]any{"id": "message", "role": "user", "text": "pending snapshot"}}})
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Result.Paused || envelope.Result.AcceptedMessages != 0 {
		t.Fatalf("paused batch acknowledged: %s", out)
	}
}

// TestOpenCodeFakeHook is a subprocess protocol fixture for plugin transport
// tests. It never connects to a service or opens a runtime database.
func TestOpenCodeFakeHook(t *testing.T) {
	if os.Getenv("ICHNORA_FAKE_HOOK") != "1" {
		return
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	envelope, _ := json.Marshal(contract.HookRequest{Harness: "opencode", Payload: data})
	if len(envelope) > 1<<20 {
		os.Exit(2)
	}
	var payload struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &payload) != nil {
		os.Exit(2)
	}
	path := os.Getenv("ICHNORA_FAKE_LOG")
	previous, _ := os.ReadFile(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
	result := map[string]any{"accepted_messages": len(payload.Messages)}
	if len(previous) == 0 {
		switch os.Getenv("ICHNORA_FAKE_FIRST") {
		case "paused":
			result = map[string]any{"ingestion_paused": true, "accepted_messages": 0}
		case "skipped":
			result = map[string]any{"skipped": "collection disabled", "accepted_messages": 0}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "result": result})
	os.Exit(0)
}

func TestOpenCodeBatchTransportInEmbeddedRuntime(t *testing.T) {
	runtime := os.Getenv("ICHNORA_OPENCODE_RUNTIME")
	if runtime == "" {
		t.Skip("set ICHNORA_OPENCODE_RUNTIME to the OpenCode executable")
	}
	message := func(id, text string) map[string]any { return map[string]any{"id": id, "role": "user", "text": text} }
	for _, name := range []string{"backlog", "backlog-spill", "bytes", "oversized", "paused", "skipped", "paused-compacted", "updated", "compacted"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var messages []any
			switch name {
			case "backlog", "backlog-spill":
				count := 450
				if name == "backlog-spill" {
					count = 900
				}
				for i := 0; i < count; i++ {
					messages = append(messages, message(fmt.Sprintf("m-%d", i), "synthetic backlog"))
				}
			case "bytes":
				for i := 0; i < 50; i++ {
					messages = append(messages, message(fmt.Sprintf("m-%d", i), strings.Repeat("<&界", 4000)))
				}
			case "oversized":
				messages = []any{message("huge", strings.Repeat("界", 30000)), message("after", "following message")}
			default:
				messages = []any{message("same", "original text")}
			}
			scenario := []map[string]any{{"messages": messages}}
			if name == "bytes" || name == "backlog-spill" {
				scenario = append(scenario, map[string]any{"event": "compaction", "messages": []any{}})
			}
			if name == "paused-compacted" {
				scenario = append(scenario, map[string]any{"event": "compaction", "messages": []any{message("new", "after compaction")}})
			}
			if name == "paused" || name == "skipped" {
				scenario = append(scenario, map[string]any{"messages": messages})
			}
			if name == "updated" {
				scenario = append(scenario, map[string]any{"messages": []any{message("same", "updated text")}})
			}
			if name == "compacted" {
				scenario = append(scenario, map[string]any{"event": "compaction", "messages": []any{message("new", "compacted text")}})
			}
			input, _ := json.Marshal(scenario)
			scenarioPath, logPath := filepath.Join(dir, "scenario.json"), filepath.Join(dir, "calls.jsonl")
			writeFile(t, scenarioPath, string(input))
			self, _ := os.Executable()
			bin := filepath.Join(dir, "fake-hook")
			writeScript(t, bin, "#!/bin/sh\nexec "+self+" -test.run '^TestOpenCodeFakeHook$'\n")
			plugin, _ := filepath.Abs(filepath.Join(testdataAbs, "..", "..", "..", "adapters", "opencode", "index.js"))
			cmd := exec.Command(runtime, filepath.Join(testdataAbs, "opencode-batch-harness.mjs"))
			cmd.Env = append(os.Environ(), "BUN_BE_BUN=1", "ICHNORA_BIN="+bin, "PLUGIN_PATH="+plugin, "SESSION_DIR="+dir, "SCENARIO="+scenarioPath,
				"ICHNORA_FAKE_HOOK=1", "ICHNORA_FAKE_LOG="+logPath, "ICHNORA_FAKE_FIRST="+strings.TrimSuffix(name, "-compacted"), "ICHNORA_TEST_MAIN=")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("plugin runtime: %v %s", err, out)
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			var forwarded []map[string]any
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
			for _, line := range calls {
				if len(line) > 512<<10 {
					t.Fatalf("request exceeded aggregate budget: %d", len(line))
				}
				var payload struct {
					Messages []map[string]any `json:"messages"`
				}
				if json.Unmarshal([]byte(line), &payload) != nil {
					t.Fatal("invalid payload")
				}
				if len(payload.Messages) > 200 {
					t.Fatal("batch exceeded message limit")
				}
				forwarded = append(forwarded, payload.Messages...)
			}
			want := len(messages)
			if name == "paused-compacted" {
				want += 2
			}
			if name == "paused" || name == "skipped" || name == "updated" || name == "compacted" {
				want++
			}
			if len(forwarded) != want {
				t.Fatalf("forwarded %d messages, want %d in %d calls", len(forwarded), want, len(calls))
			}
			if name == "backlog" && (forwarded[0]["id"] != "m-0" || forwarded[len(forwarded)-1]["id"] != "m-449") {
				t.Fatal("backlog order was lost")
			}
			if name == "oversized" && (forwarded[0]["role"] != "ichnora_omission" || forwarded[1]["id"] != "after") {
				t.Fatal("oversized message lacks an explicit omission or blocks following messages")
			}
			if name == "updated" && forwarded[1]["text"] != "updated text" {
				t.Fatal("same-length update was skipped")
			}
			if name == "compacted" && forwarded[1]["id"] != "new" {
				t.Fatal("same-length compaction was skipped")
			}
		})
	}
}
