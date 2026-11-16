package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
)

// futureExpiry bounds a fixture decision that has no dependencies, which keeps
// it in the working view as a durable decision.
func futureExpiry() string {
	return time.Now().UTC().Add(24 * time.Hour).Format(contract.TimeFormat)
}

// A decision without dependencies cannot be revalidated against anything.
// Unless an expiry bounds it, it must stay an unchecked lead instead of
// entering the working view and task resume as a standing decision.
func TestDependencyFreeDecisionNeedsExpiryForWorkingView(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	source := filepath.Join(e.alpha, "convention.go")
	if err := os.WriteFile(source, []byte("package convention\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := searchTask(t, e, "Convention task")
	ids := e.captureIDs(e.alpha,
		contract.ClaimInput{Kind: "decision", Title: "Convention unbounded short implementations", TaskID: task},
		contract.ClaimInput{Kind: "decision", Title: "Convention bounded release freeze", TaskID: task, ExpiresAt: futureExpiry()},
		contract.ClaimInput{Kind: "decision", Title: "Convention anchored parser layout", TaskID: task,
			Depends: []contract.Ref{{Kind: "file", Path: "convention.go"}}},
	)
	unbounded, bounded, anchored := ids[0], ids[1], ids[2]

	out := e.search(e.alpha, "convention", "--view", "all", "--limit", "10")
	for _, want := range []struct{ id, group, applicability string }{
		{unbounded, "lead", "unchecked"},
		{bounded, "working", "durable"},
		{anchored, "working", "current"},
	} {
		item := find(out.Items, want.id)
		if item == nil || item.Group != want.group || item.Applicability != want.applicability {
			t.Fatalf("%s: want %s/%s, got %+v", want.id, want.group, want.applicability, item)
		}
	}

	r := e.must(e.alpha, "", "resume", task)
	_, by := resumeLines(t, r.Result)
	sections := map[string]string{}
	for section, lines := range by {
		for _, line := range lines {
			sections[line.Ref] = section
		}
	}
	if sections[unbounded] != "lead" || sections[bounded] != "decision" || sections[anchored] != "decision" {
		t.Fatalf("resume sections: %v", sections)
	}
}
