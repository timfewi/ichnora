package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

// TestMeasureSyntheticRetrieval meters retrieval output bytes and latency on
// synthetic data with agent-tool-meter. It runs only when ICHNORA_METER names
// the meter and ICHNORA_MEASURE_LOG a log outside the repository. Byte counts
// are not model tokens and say nothing about task quality.
func TestMeasureSyntheticRetrieval(t *testing.T) {
	meter, logPath := os.Getenv("ICHNORA_METER"), os.Getenv("ICHNORA_MEASURE_LOG")
	if meter == "" || logPath == "" {
		t.Skip("set ICHNORA_METER and ICHNORA_MEASURE_LOG to measure")
	}
	e := newEnv(t, nil)
	e.enroll(e.alpha, "--collect", "--recall")
	session := "measure"
	transcript := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "p", session+".jsonl"), session, e.alpha)
	e.hook(e.alpha, "claude", map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": transcript, "cwd": e.alpha})
	var claims []contract.ClaimInput
	for i := 0; i < 40; i++ {
		claims = append(claims, contract.ClaimInput{Kind: "decision", Title: "Parser cache decision " + itoaT(int64(i)),
			Body: "Synthetic body for retrieval measurement."})
	}
	ids := e.captureIDs(e.alpha, claims[:20]...)
	e.captureIDs(e.alpha, claims[20:]...)
	r := e.must(e.alpha, checkpointJSON(t, contract.Checkpoint{Objective: "measure", Remaining: []string{"a", "b"}, Decisions: ids[:5]}),
		"task", "create", "--title", "Measured task", "--checkpoint-stdin")
	var task contract.TaskResult
	r.into(t, &task)
	self, _ := os.Executable()
	bin := filepath.Join(e.root, "bin", "ichnora")
	writeScript(t, bin, "#!/bin/sh\nICHNORA_TEST_MAIN=1 exec "+self+" \"$@\"\n")
	for _, m := range []struct {
		label string
		args  []string
	}{
		{"search-index", []string{"search", ""}},
		{"search-default", []string{"search", "parser cache"}},
		{"search-small", []string{"search", "parser cache", "--max-bytes", "1024"}},
		{"resume", []string{"resume", task.Task.ID}},
		{"read-batch", append([]string{"read"}, ids...)},
	} {
		for i := 0; i < 5; i++ {
			cmd := exec.Command(meter, append([]string{"run", "--log", logPath, "--label", m.label, "--", bin}, m.args...)...)
			cmd.Dir = e.alpha
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", m.label, err, out)
			}
		}
	}
	out, err := exec.Command(meter, "report", "--log", logPath).Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic retrieval measurements (bytes, not tokens):\n%s", out)
}
