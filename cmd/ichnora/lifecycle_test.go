package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
)

// Deletion removes dependent projections, survives replay and re-enrollment,
// and backup/restore preserves scope, provenance and acknowledged records.
func TestRetentionTombstonesBackupAndRestore(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	session := "claude-ret"
	transcript := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "p", session+".jsonl"), session, e.alpha)
	stop := map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": transcript, "cwd": e.alpha}
	e.hook(e.alpha, "claude", stop)
	src := e.search(e.alpha, "cache invalidation fix")
	if len(src.Items) == 0 {
		t.Fatal("transcript not ingested")
	}
	obs := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Planning turn asked for cache invalidation",
		Support: []contract.Ref{{Kind: "turn", Ref: src.Items[0].ID}}})[0]
	keep := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Keep UTC timestamps"})[0]
	sessID := "ses_" + strings.TrimPrefix(readSessionID(t, e, src.Items[0].ID), "ses_")

	r := e.must(e.root, "", "retention", "preview", "--selector", "session", "--target", sessID)
	var preview struct {
		Counts     map[string]int `json:"counts"`
		Dependants []string       `json:"dependent_claims_losing_support"`
		Confirm    string         `json:"confirm"`
		Erasure    string         `json:"erasure"`
	}
	r.into(t, &preview)
	if preview.Counts["events"] == 0 || len(preview.Dependants) != 1 || !strings.Contains(preview.Erasure, "not guaranteed") {
		t.Fatalf("preview must count projections, dependants and state the erasure limit: %s", r.Raw)
	}
	if r := e.cli(e.root, "", "retention", "apply", "--selector", "session", "--target", sessID, "--confirm", "wrong"); r.errCode() != contract.CodeConflict {
		t.Fatalf("a wrong confirmation must not delete: %s", r.Raw)
	}
	e.must(e.root, "", "retention", "apply", "--selector", "session", "--target", sessID, "--confirm", preview.Confirm)
	if out := e.search(e.alpha, "cache invalidation fix"); find(out.Items, src.Items[0].ID) != nil {
		t.Fatal("deleted turn still found through FTS")
	}
	if it := find(e.search(e.alpha, "planning turn", "--view", "all").Items, obs); it == nil || it.Status != "unsupported" {
		t.Fatalf("dependent claim must lose its support: %+v", it)
	}
	// Replay of the deleted session and re-enrollment do not resurrect it.
	e.hook(e.alpha, "claude", stop)
	e.must(e.root, "", "withdraw", "alpha")
	e.enroll(e.alpha, "--collect")
	e.hook(e.alpha, "claude", stop)
	if out := e.search(e.alpha, "cache invalidation fix"); out.Totals.Sources != 0 {
		t.Fatalf("tombstoned session was re-imported: %+v", out.Items)
	}
	r = e.must(e.root, "", "import", "preview", "--harness", "claude", transcript)
	if !strings.Contains(r.Raw, "tombstone") {
		t.Fatalf("historical import must respect the tombstone: %s", r.Raw)
	}

	// Backups stay outside workspaces; restore preserves acknowledged records.
	if r := e.cli(e.root, "", "backup", filepath.Join(e.alpha, "b.db")); r.errCode() != contract.CodePolicyDenied {
		t.Fatalf("backup inside a workspace must be refused: %s", r.Raw)
	}
	backup := filepath.Join(e.root, "backup.db")
	e.must(e.root, "", "backup", backup)
	if st, _ := os.Stat(backup); st.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions %v", st.Mode().Perm())
	}
	after := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "note", Title: "Written after the backup"})[0]
	var stderr bytes.Buffer
	if code := restore(e.cfgPath, backup, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("restore must refuse while the service holds the state lock")
	}
	e.stop()
	if code := restore(e.cfgPath, backup, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("restore failed: %s", stderr.String())
	}
	e.start()
	r = e.must(e.alpha, "", "read", keep, after)
	var rd contract.ReadResult
	r.into(t, &rd)
	if len(rd.Records) != 1 || rd.Records[0].ID != keep || !strings.Contains(string(rd.Records[0].Data), "capture") {
		t.Fatalf("restore must bring back the backed-up state with provenance: %s", r.Raw)
	}
	if r := e.cli(e.beta, "", "search", "UTC"); r.errCode() != contract.CodeOutOfScope {
		t.Fatalf("restored state widened scope: %s", r.Raw)
	}
}

func readSessionID(t *testing.T, e *testEnv, turn string) string {
	t.Helper()
	r := e.must(e.alpha, "", "read", turn)
	var rd contract.ReadResult
	r.into(t, &rd)
	var data struct {
		Session string `json:"session"`
	}
	json.Unmarshal(rd.Records[0].Data, &data)
	return data.Session
}

// Crash/restart keeps acknowledged checkpoints and cursors; storage limits
// pause ingestion instead of dropping acknowledged work.
func TestRestartAndStorageBackpressure(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect")
	r := e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{NextAction: "continue after restart"}), "task", "create", "--title", "Durable task", "--checkpoint-stdin")
	var task contract.TaskResult
	r.into(t, &task)
	session := "codex-restart"
	rollout := e.fixture("codex-rollout.jsonl", filepath.Join(e.codexRoot, "r.jsonl"), session, e.alpha)
	e.hook(e.alpha, "codex", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": rollout, "cwd": e.alpha})
	events := count(e.must(e.alpha, "", "status").Raw, "events")
	// Simulated crash: stop without SessionEnd, append, restart, reconcile.
	e.stop()
	f, _ := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"timestamp":"2026-10-19T09:30:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Written while the service was down."}]}}` + "\n")
	f.Close()
	e.start()
	r = e.must(e.alpha, "", "resume", task.Task.ID)
	if !strings.Contains(r.Raw, "continue after restart") {
		t.Fatalf("acknowledged checkpoint lost across restart: %s", r.Raw)
	}
	e.must(e.root, "", "reconcile")
	if got := count(e.must(e.alpha, "", "status").Raw, "events"); got != events+1 {
		t.Fatalf("reconcile after restart: events %d → %d, want +1", events, got)
	}

	// A tiny storage limit pauses ingestion and refuses captures explicitly.
	e.stop()
	e.cfg.Limits.MaxDBBytes, e.cfg.Limits.CaptureReserve = 1, 1
	e.start()
	out := e.hook(e.alpha, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": session,
		"transcript_path": rollout, "cwd": e.alpha, "model": "m", "permission_mode": "default"})
	if !strings.Contains(out, "paused") {
		t.Fatalf("hook must report paused ingestion: %s", out)
	}
	if r := e.cli(e.alpha, capture(t, contract.ClaimInput{Kind: "note", Title: "over limit"}), "capture"); r.errCode() != contract.CodeStorageFull || r.Code != 6 {
		t.Fatalf("capture over the limit must be storage_full: %s", r.Raw)
	}
	if !strings.Contains(e.must(e.alpha, "", "status").Raw, "ingestion paused") {
		t.Fatal("status must show the pause")
	}
}

// A changed host epoch makes only environment-dependent claims stale.
func TestEnvironmentEpochRevalidation(t *testing.T) {
	desc := ""
	e := newEnv(t, func(c *config.Config) {
		desc = filepath.Join(filepath.Dir(c.StateDir), "environment.json")
		c.EnvironmentDescriptor = desc
	})
	e.enroll(e.alpha)
	// Without a descriptor the core still works and reports unknown host applicability.
	if r := e.cli(e.alpha, capture(t, contract.ClaimInput{Kind: "observation", Title: "x", Depends: []contract.Ref{{Kind: "environment"}}}), "capture"); r.errCode() != contract.CodeInvalid {
		t.Fatalf("current epoch without descriptor must be rejected: %s", r.Raw)
	}
	unknown := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Sandbox exposes the agent socket",
		Support: []contract.Ref{{Kind: "external", Locator: "host check"}}, Depends: []contract.Ref{{Kind: "environment", Epoch: "epoch-0"}}})[0]
	if it := find(e.search(e.alpha, "sandbox socket").Items, unknown); it == nil || it.Group != "lead" || it.Applicability != "unknown" {
		t.Fatalf("missing descriptor must yield unknown applicability: %+v", it)
	}
	writeEpoch := func(epoch string) {
		writeFile(t, desc, `{"format":"ichnora.environment/1","system_identity":"fixture-host","activation_epoch":"`+epoch+`"}`)
	}
	writeEpoch("epoch-1")
	hostClaim := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Host exposes the agent socket to the sandbox",
		Support: []contract.Ref{{Kind: "external", Locator: "host check"}}, Depends: []contract.Ref{{Kind: "environment", Epoch: "current"}}})[0]
	decision := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Sandbox agents use the agent socket only"})[0]
	if it := find(e.search(e.alpha, "agent socket").Items, hostClaim); it == nil || it.Group != "working" {
		t.Fatalf("host claim must be current under its epoch: %+v", it)
	}
	writeEpoch("epoch-2")
	out := e.search(e.alpha, "agent socket")
	if find(out.Items, hostClaim) != nil || find(out.Items, decision) == nil || out.Omitted.Stale != 2 {
		t.Fatalf("epoch change must stale only the dependent claim: %+v", out)
	}
	// A rollback is a new activation epoch; the old observation stays stale.
	writeEpoch("epoch-3-rollback")
	if find(e.search(e.alpha, "agent socket").Items, hostClaim) != nil {
		t.Fatal("rollback restored a stale host observation")
	}
	if r := e.must(e.root, "", "environment"); !strings.Contains(r.Raw, "epoch-3-rollback") {
		t.Fatalf("environment status: %s", r.Raw)
	}
}

// Discovery offers candidates without reading them; excluded, nested,
// unmapped and symlinked paths and foreign transcripts are not ingested.
func TestEnrollmentAndSourceBoundaries(t *testing.T) {
	e := newEnv(t, nil)
	gamma := filepath.Join(e.ws, "gamma")
	mkRepo(t, gamma, strings.Repeat("c", 40))
	mkRepo(t, filepath.Join(e.ws, "archive", "old"), strings.Repeat("d", 40))
	nested := filepath.Join(e.alpha, "third_party", "lib")
	mkRepo(t, nested, strings.Repeat("e", 40))
	writeFile(t, filepath.Join(nested, "lib.go"), "package lib\n")
	writeFile(t, filepath.Join(e.alpha, "node_modules", "x.js"), "x\n")
	writeFile(t, filepath.Join(e.alpha, ".env"), "TOKEN=x\n")
	writeFile(t, filepath.Join(e.root, "outside.txt"), "secret outside\n")
	if err := os.Symlink(filepath.Join(e.root, "outside.txt"), filepath.Join(e.alpha, "link.txt")); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(e.ws, "alpha-wt")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(e.alpha, ".git", "worktrees", "wt")+"\n")

	r := e.must(e.root, "", "discover")
	var disc struct {
		Candidates []struct {
			Path     string `json:"path"`
			Enrolled bool   `json:"enrolled"`
		} `json:"candidates"`
	}
	r.into(t, &disc)
	paths := map[string]bool{}
	for _, c := range disc.Candidates {
		paths[c.Path] = c.Enrolled
	}
	if enrolled, ok := paths[gamma]; !ok || enrolled {
		t.Fatalf("new repository must be an unenrolled candidate: %s", r.Raw)
	}
	if _, ok := paths[filepath.Join(e.ws, "archive", "old")]; ok {
		t.Fatal("discovery entered an excluded archive")
	}
	for _, bad := range []string{e.ws, filepath.Join(e.ws, "archive", "old"), e.root, e.state} {
		if r := e.cli(e.root, "", "enroll", bad); r.errCode() != contract.CodePolicyDenied {
			t.Fatalf("enroll %s must be denied: %s", bad, r.Raw)
		}
	}
	e.enroll(e.alpha, "--collect")
	for path, want := range map[string]string{
		filepath.Join(e.alpha, "node_modules", "x.js"): "dependency tree",
		filepath.Join(e.alpha, ".env"):                 "credential store",
		filepath.Join(e.alpha, "link.txt"):             "outside the project root",
		filepath.Join(nested, "lib.go"):                "nested repository",
		filepath.Join(gamma, "x"):                      "not inside an enrolled project",
	} {
		r := e.must(e.root, "", "policy", path)
		if !strings.Contains(r.Raw, want) || strings.Contains(r.Raw, `"allowed":true`) {
			t.Fatalf("policy %s: want %q, got %s", path, want, r.Raw)
		}
	}
	for _, ref := range []string{"../beta/README.md", "link.txt", "third_party/lib/lib.go", "node_modules/x.js", "/etc/passwd"} {
		if r := e.cli(e.alpha, capture(t, contract.ClaimInput{Kind: "observation", Title: "x", Support: []contract.Ref{{Kind: "file", Path: ref}}}), "capture"); r.errCode() != contract.CodePolicyDenied {
			t.Fatalf("file reference %s must be denied: %s", ref, r.Raw)
		}
	}
	// Hooks from a nested repository, an unmapped worktree or an unenrolled
	// project record nothing, even with a transcript inside a session root.
	foreign := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "other", "foreign.jsonl"), "foreign", gamma)
	for _, dir := range []string{nested, worktree, gamma} {
		out := e.hook(dir, "claude", map[string]any{"hook_event_name": "Stop", "session_id": "foreign", "transcript_path": foreign, "cwd": dir})
		if out != "" {
			t.Fatalf("hook from %s produced output %q", dir, out)
		}
	}
	if count(e.must(e.alpha, "", "status").Raw, "sessions") != 0 {
		t.Fatal("an unrelated session was recorded")
	}
	// A transcript outside the session roots or behind a symlink is denied.
	outside := e.fixture("claude-transcript.jsonl", filepath.Join(e.root, "elsewhere.jsonl"), "s-out", e.alpha)
	linked := filepath.Join(e.claudeRoot, "linked.jsonl")
	os.Symlink(outside, linked)
	for _, path := range []string{outside, linked, filepath.Join(e.claudeRoot, "..", "elsewhere.jsonl")} {
		r := e.must(e.root, "", "policy", path, "--harness", "claude")
		if strings.Contains(r.Raw, `"allowed":true`) {
			t.Fatalf("transcript %s must be denied: %s", path, r.Raw)
		}
		out := e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": "s-out", "transcript_path": path, "cwd": e.alpha})
		if !strings.Contains(out, "not recorded") && out != "" {
			t.Fatalf("unexpected hook output %q", out)
		}
	}
	if strings.Contains(e.must(e.alpha, "", "search", "cache invalidation").Raw, `"kind":"turn"`) {
		t.Fatal("a denied transcript was ingested")
	}
	// Mapping the worktree explicitly makes it part of alpha's scope.
	e.must(e.root, "", "enroll", worktree, "--worktree-of", "alpha")
	if r := e.must(worktree, "", "status"); !strings.Contains(r.Raw, `"project":"alpha"`) {
		t.Fatalf("mapped worktree must resolve to alpha: %s", r.Raw)
	}
}
