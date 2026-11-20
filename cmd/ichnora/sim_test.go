package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

func TestSimPredictionSurvivesReplayAndScope(t *testing.T) {
	e := newEnv(t, nil)
	alpha := e.enroll(e.alpha)
	e.enroll(e.beta)
	propose := []string{"sim", "propose", "cache", "ttl", "--cause", "stale entry", "--change", "shorten TTL",
		"--prediction", "both cases pass", "--context", "synthetic cache suite v1"}
	e.must(e.alpha, "", propose...)
	writeFile(t, filepath.Join(e.alpha, "check.txt"), "1/2 passed\n")
	observe := []string{"sim", "observe", "cache", "ttl", "--outcome", "1/2 passed", "--evidence", "check.txt"}
	e.must(e.alpha, "", observe...)
	e.stop()
	e.start()
	e.must(e.alpha, "", propose...)
	e.must(e.alpha, "", observe...)
	changed := append([]string{}, propose...)
	changed[9] = "only one case passes"
	if r := e.cli(e.alpha, "", changed...); r.errCode() != contract.CodeConflict {
		t.Fatalf("rewrote a prior prediction: %s", r.Raw)
	}
	writeFile(t, filepath.Join(e.alpha, "check.txt"), "2/2 passed\n")
	if r := e.cli(e.alpha, "", observe...); r.errCode() != contract.CodeConflict {
		t.Fatalf("rewrote evidence: %s", r.Raw)
	}
	another := append([]string{}, propose...)
	another[3] = "version-key"
	e.must(e.alpha, "", another...)
	r := e.must(e.alpha, "", "sim", "history", "cache", "--limit", "1")
	var first contract.SimHistory
	r.into(t, &first)
	if len(first.Attempts) != 1 || first.NextOffset != 1 {
		t.Fatalf("bad first page: %s", r.Raw)
	}
	a := first.Attempts[0]
	if a.Attempt != "ttl" || a.Outcome != "1/2 passed" || a.Prediction != "both cases pass" ||
		len(a.EvidenceSHA256) != 64 || a.EvidenceWorktree == "" || a.ProposedAt == "" || a.ObservedAt == "" {
		t.Fatalf("missing historical evidence: %+v", a)
	}
	r = e.must(e.alpha, "", "sim", "history", "cache", "--offset", "1")
	var second contract.SimHistory
	r.into(t, &second)
	if len(second.Attempts) != 1 || second.Attempts[0].Attempt != "version-key" || second.Attempts[0].Outcome != "" {
		t.Fatalf("bad second page: %s", r.Raw)
	}
	if r := e.must(e.beta, "", "sim", "history", "cache"); strings.Contains(r.Raw, "ttl") {
		t.Fatalf("foreign attempt in history: %s", r.Raw)
	}
	writeFile(t, filepath.Join(e.beta, "check.txt"), "independent result\n")
	if r := e.cli(e.beta, "", observe...); r.errCode() != contract.CodeNotFound {
		t.Fatalf("observed a foreign proposal: %s", r.Raw)
	}
	anotherObserve := append([]string{}, observe...)
	anotherObserve[3] = "version-key"
	anotherObserve[7] = "../beta/check.txt"
	if r := e.cli(e.alpha, "", anotherObserve...); r.errCode() != contract.CodePolicyDenied {
		t.Fatalf("accepted out-of-scope evidence: %s", r.Raw)
	}
	r = e.must(e.root, "", "retention", "preview", "--selector", "project", "--target", alpha)
	if !strings.Contains(r.Raw, `"sim_attempts":2`) {
		t.Fatalf("retention preview missed attempts: %s", r.Raw)
	}
}
