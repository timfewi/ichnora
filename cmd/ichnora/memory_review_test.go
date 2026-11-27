package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

func TestDerivedClaimsFollowSourceAndCallingWorktree(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	e.enroll(e.beta, "--worktree-of", "alpha")
	task := searchTask(t, e, "Deploy")
	for _, dir := range []string{e.alpha, e.beta} {
		writeFile(t, filepath.Join(dir, "manual.txt"), "current deployment guide")
		writeFile(t, filepath.Join(dir, "config.txt"), "port=8000")
	}
	cp := contract.Checkpoint{Verification: []contract.Verification{{Check: "port probe", Outcome: "passed", Inputs: contract.VerificationInput{Files: map[string]string{"config.txt": sha(t, filepath.Join(e.alpha, "config.txt"))}}}}}
	e.must(e.alpha, checkpointJSON(t, cp), "task", "checkpoint", task, "--expected-revision", "1", "--checkpoint-stdin")
	config := []contract.Ref{{Kind: "file", Path: "config.txt"}}
	manual := []contract.Ref{{Kind: "file", Path: "manual.txt"}}
	source := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Port is 8000", Support: config, Depends: config, TaskID: task})[0]
	summary := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "summary", Title: "Deploy port 8000", DerivedFrom: []string{source}, Depends: manual, TaskID: task})[0]
	leaf := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "summary", Title: "Deploy checklist", DerivedFrom: []string{summary}, Depends: manual, TaskID: task})[0]
	check := func(dir, state string) {
		t.Helper()
		var read contract.ReadResult
		e.must(dir, "", "read", source, summary, leaf).into(t, &read)
		if len(read.Records) != 3 {
			t.Fatal("missing records", read)
		}
		all := e.search(dir, "", "--view", "all")
		for _, record := range read.Records {
			item := find(all.Items, record.ID)
			if record.Applicability != state || item == nil || item.Applicability != state {
				t.Fatalf("read/search %s want %s: %+v %+v", dir, state, record, item)
			}
		}
		var ev contract.EvidenceResult
		e.must(dir, "", "evidence", leaf).into(t, &ev)
		if ev.Applicability != state || len(ev.Reasons) == 0 {
			t.Fatalf("evidence: %+v", ev)
		}
		r := e.must(dir, "", "resume", task)
		_, lines := resumeLines(t, r.Result)
		checkVisible := slices.ContainsFunc(lines["check"], func(l contract.Line) bool { return strings.Contains(l.Text, "port probe") })
		if checkVisible != (state == "current") {
			t.Fatalf("incorrect saved check applicability in %s: %s", dir, r.Raw)
		}
		section := map[string]string{"current": "fact", "stale": "history", "unknown": "lead"}[state]
		if !slices.ContainsFunc(lines[section], func(l contract.Line) bool { return l.Ref == leaf && l.State == state }) {
			t.Fatalf("resume omitted %s from %s: %s", leaf, section, r.Raw)
		}
	}
	check(e.beta, "current")
	writeFile(t, filepath.Join(e.beta, "config.txt"), "port=9000")
	check(e.beta, "stale")
	check(e.alpha, "current")
	if item := find(e.search(e.beta, "", "--view", "review").Items, source); item == nil || item.Provenance.SourceWorktree == "" {
		t.Fatal("missing origin on worktree mismatch")
	}
	writeFile(t, filepath.Join(e.alpha, "config.txt"), "port=9200")
	check(e.alpha, "stale")
	review := e.search(e.alpha, "", "--view", "review", "--derived-from", source)
	if len(review.Items) != 1 || review.Items[0].ID != summary {
		t.Fatalf("review omitted direct derivative: %+v", review)
	}
	writeFile(t, filepath.Join(e.alpha, "config.txt"), "port=8000")
	check(e.alpha, "current")
	if r := e.search(e.alpha, "", "--view", "review"); len(r.Items) != 0 {
		t.Fatalf("review contains healthy claims: %+v", r)
	}
}

func TestJSONFieldAndIndependentFactsFromOneFile(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	path := filepath.Join(e.alpha, "settings.json")
	writeFile(t, path, `{"port":9007199254740993,"retries":3}`)
	fact := func(key, value string) contract.ClaimInput {
		refs := []contract.Ref{{Kind: "json_field", Path: "settings.json", Field: key, Value: value}}
		return contract.ClaimInput{Kind: "observation", Title: "Transport " + key, Support: refs, Depends: refs}
	}
	for _, bad := range []string{"9007199254740992", `"9007199254740993"`, "[]", "garbage"} {
		if r := e.cli(e.alpha, capture(t, fact("port", bad)), "capture"); r.errCode() != contract.CodeInvalid {
			t.Fatalf("accepted incorrect or uncheckable value %q: %s", bad, r.Raw)
		}
	}
	ids := e.captureIDs(e.alpha, fact("port", "9007199254740993"), fact("retries", "3"))
	if r := e.search(e.alpha, "transport"); len(r.Items) != 2 || r.Omitted.Duplicates != 0 {
		t.Fatalf("lost distinct facts: %+v", r)
	}
	for _, tc := range []struct{ json, state string }{
		{`{"port":9007199254740993,"retries":4}`, "current"},
		{`{"port":9007199254740992}`, "stale"},
		{`{"other":1}`, "stale"},
		{`{"port":9007199254740993,"port":0}`, "unknown"},
		{"{", "unknown"},
	} {
		writeFile(t, path, tc.json)
		var ev contract.EvidenceResult
		e.must(e.alpha, "", "evidence", ids[0]).into(t, &ev)
		if ev.Applicability != tc.state {
			t.Fatalf("%s: got %+v", tc.json, ev)
		}
	}
}

func TestClaimTreePreviewAndSelectiveForgetting(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	e.enroll(e.beta)
	base := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Palette secret is blue", ExpiresAt: futureExpiry()})[0]
	derive := func(dir, parent string) string {
		return e.captureIDs(dir, contract.ClaimInput{Kind: "summary", Title: "Palette copy", DerivedFrom: []string{parent}})[0]
	}
	mid := derive(e.alpha, base)
	leaf := derive(e.alpha, mid)
	keep := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Independent setting", ExpiresAt: futureExpiry()})[0]
	type preview struct {
		Selected, Deps []string
		Confirm        string
	}
	inspect := func() preview {
		t.Helper()
		var out struct {
			Selected []string `json:"selected_claims"`
			Deps     []string `json:"dependent_claims_losing_support"`
			Confirm  string   `json:"confirm"`
		}
		e.must(e.root, "", "retention", "preview", "--selector", "claim-tree", "--target", base).into(t, &out)
		return preview{out.Selected, out.Deps, out.Confirm}
	}
	e.must(e.root, "", "share", mid, "--to", "beta")
	before := inspect()
	foreign := derive(e.beta, mid)
	apply := func(token string) result {
		return e.cli(e.root, "", "retention", "apply", "--selector", "claim-tree", "--target", base, "--confirm", token)
	}
	if got := apply(before.Confirm); got.errCode() != contract.CodeConflict {
		t.Fatalf("old preview accepted: %s", got.Raw)
	}
	p := inspect()
	if len(p.Selected) != 3 || !slices.Contains(p.Selected, base) || !slices.Contains(p.Selected, mid) || !slices.Contains(p.Selected, leaf) || !slices.Equal(p.Deps, []string{foreign}) {
		t.Fatalf("unexpected selection: %+v", p)
	}
	if r := apply(p.Confirm); !r.OK {
		t.Fatal(r.Raw)
	}
	if r := e.search(e.alpha, "palette", "--view", "all"); len(r.Items) != 0 {
		t.Fatal("deleted text still searchable", r)
	}
	var rd contract.ReadResult
	e.must(e.alpha, "", "read", base, mid, leaf, keep).into(t, &rd)
	if len(rd.Records) != 1 || rd.Records[0].ID != keep || len(rd.Missing) != 3 {
		t.Fatalf("over/under deletion: %+v", rd)
	}
	item := find(e.search(e.beta, "palette").Items, foreign)
	if item == nil || item.Status != "unsupported" {
		t.Fatalf("foreign derivative should survive as unsupported: %+v", item)
	}
}

func TestObservedAttemptCanSupportReusableRule(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	e.enroll(e.beta)
	ref := contract.Ref{Kind: "attempt", Ref: "cache/one"}
	in := contract.ClaimInput{Kind: "summary", Title: "Retry alone did not fix cache", Support: []contract.Ref{ref}, Depends: []contract.Ref{{Kind: "file", Path: "parser/cache.go"}}}
	e.must(e.alpha, "", "sim", "propose", "cache", "one", "--cause", "old state", "--change", "retry", "--prediction", "probe passes", "--context", "synthetic")
	if r := e.cli(e.alpha, capture(t, in), "capture"); r.OK {
		t.Fatal("proposal presented as evidence")
	}
	writeFile(t, filepath.Join(e.alpha, "report.txt"), "failed")
	e.must(e.alpha, "", "sim", "observe", "cache", "one", "--outcome", "probe failed", "--evidence", "report.txt")
	id := e.captureIDs(e.alpha, in)[0]
	writeFile(t, filepath.Join(e.alpha, "report.txt"), "different")
	var ev contract.EvidenceResult
	e.must(e.alpha, "", "evidence", id).into(t, &ev)
	for _, c := range ev.Checks {
		if c.Kind == "attempt" {
			var stored contract.Ref
			if err := json.Unmarshal(c.Stored, &stored); err != nil || stored.SHA256 == "" || stored.SHA256 == sha(t, filepath.Join(e.alpha, "report.txt")) || !strings.Contains(string(c.Excerpt), "probe failed") {
				t.Fatalf("historical evidence: %+v", c)
			}
			in.Depends = nil
			if r := e.cli(e.beta, capture(t, in), "capture"); r.errCode() != contract.CodeOutOfScope {
				t.Fatalf("attempt crossed project: %s", r.Raw)
			}
			return
		}
	}
	t.Fatal("missing attempt")
}

func TestDerivativeCannotOutliveExpiredDisputedOrRevokedPremise(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	e.enroll(e.beta)
	bounded := contract.ClaimInput{Kind: "decision", Title: "Deployment convention", ExpiresAt: futureExpiry()}
	parent := e.captureIDs(e.alpha, bounded)[0]
	var grant struct{ ID string }
	e.must(e.root, "", "share", parent, "--to", "beta").into(t, &grant)
	child := contract.ClaimInput{Kind: "summary", Title: "Release checklist", Depends: []contract.Ref{{Kind: "file", Path: "README.md"}}, DerivedFrom: []string{parent}}
	local := e.captureIDs(e.beta, child)[0]
	expect := func(want string) {
		t.Helper()
		item := find(e.search(e.beta, "checklist", "--view", "all").Items, local)
		if item == nil || item.Applicability != want {
			t.Fatalf("wanted %s, got %+v", want, item)
		}
	}
	expect("current")
	e.must(e.root, "", "revoke", grant.ID)
	expect("unknown")
	if r := e.cli(e.beta, "", "search", "", "--derived-from", parent); r.errCode() != contract.CodeOutOfScope {
		t.Fatal("revoked premise usable as a filter", r.Raw)
	}
	bounded.ExpiresAt = "2000-01-01T00:00:00Z"
	expired := e.captureIDs(e.beta, bounded)[0]
	child.DerivedFrom = []string{expired}
	local = e.captureIDs(e.beta, child)[0]
	expect("expired")
	bounded.ExpiresAt = futureExpiry()
	decision := e.captureIDs(e.beta, bounded)[0]
	child.DerivedFrom = []string{decision}
	local = e.captureIDs(e.beta, child)[0]
	expect("current")
	bounded.Contradicts = []string{decision}
	e.captureIDs(e.beta, bounded)
	expect("unknown")
}
