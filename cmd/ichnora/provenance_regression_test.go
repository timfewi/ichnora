package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

type provenanceClaim struct {
	Support int `json:"independent_support"`
	Refs    []struct {
		Ref   string `json:"ref"`
		State string `json:"state"`
		Lost  string `json:"lost_reason"`
	} `json:"refs"`
}

func readProvenanceClaim(t *testing.T, e *testEnv, dir, id string) (contract.Record, provenanceClaim) {
	t.Helper()
	var out contract.ReadResult
	e.must(dir, "", "read", id).into(t, &out)
	if len(out.Records) != 1 || out.Records[0].ID != id {
		t.Fatalf("claim %s was not returned: %+v", id, out)
	}
	var data provenanceClaim
	if err := json.Unmarshal(out.Records[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return out.Records[0], data
}

func requireLostClaimSupport(t *testing.T, e *testEnv, dir, id, source, sourceStatus string) {
	t.Helper()
	record, data := readProvenanceClaim(t, e, dir, id)
	if record.Status != "unsupported" || data.Support != 0 {
		t.Fatalf("claim must have no support from an inactive source: status=%s data=%+v", record.Status, data)
	}
	if len(data.Refs) != 1 || data.Refs[0].Ref != source || data.Refs[0].State != "lost" ||
		data.Refs[0].Lost != "source claim is "+sourceStatus {
		t.Fatalf("inactive source must be an explicit lost reference: %+v", data.Refs)
	}
	var evidence contract.EvidenceResult
	e.must(dir, "", "evidence", id).into(t, &evidence)
	if len(evidence.Checks) != 1 || evidence.Checks[0].State != "lost" || evidence.GapsTotal != 1 ||
		!strings.Contains(evidence.Checks[0].Validation, sourceStatus) {
		t.Fatalf("lost support must be visible in evidence: %+v", evidence)
	}
}

// Capturing a summary before or after its source becomes inactive must give
// the same evidence state. Explicit support and derived_from share that rule.
func TestInactiveClaimSupportIsIndependentOfCaptureOrder(t *testing.T) {
	for _, status := range []string{"invalidated", "disputed", "superseded"} {
		for _, reference := range []string{"support", "derived_from"} {
			for _, order := range []string{"before", "after"} {
				t.Run(status+"/"+reference+"/"+order, func(t *testing.T) {
					e := newEnv(t, nil)
					e.enroll(e.alpha)
					source := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Use the original cache policy"})[0]
					deactivate := func() {
						if status == "superseded" {
							e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Use the replacement cache policy", Supersedes: []string{source}})
						} else {
							e.must(e.alpha, "", "update", source, "--status", status, "--reason", "Synthetic source correction")
						}
					}
					if order == "before" {
						deactivate()
					}
					in := contract.ClaimInput{Kind: "summary", Title: "Summary of the original cache policy"}
					if reference == "support" {
						in.Support = []contract.Ref{{Kind: "claim", Ref: source}}
					} else {
						in.DerivedFrom = []string{source}
					}
					summary := e.captureIDs(e.alpha, in)[0]
					if order == "after" {
						if record, data := readProvenanceClaim(t, e, e.alpha, summary); record.Status != "inferred" || data.Support != 1 {
							t.Fatalf("active decision must initially support the summary: status=%s data=%+v", record.Status, data)
						}
						deactivate()
					}
					requireLostClaimSupport(t, e, e.alpha, summary, source, status)
					chained := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "summary", Title: "Restatement of the unsupported summary", DerivedFrom: []string{summary}})[0]
					requireLostClaimSupport(t, e, e.alpha, chained, summary, "unsupported")
					// History still records that the original decision was made.
					if record, data := readProvenanceClaim(t, e, e.alpha, source); record.Status != status || data.Support != 1 {
						t.Fatalf("source decision history changed: status=%s data=%+v", record.Status, data)
					}
				})
			}
		}
	}
}

func TestInactiveClaimSupportPreservesOtherIndependentSources(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	sources := e.captureIDs(e.alpha,
		contract.ClaimInput{Kind: "decision", Title: "First independent cache decision"},
		contract.ClaimInput{Kind: "decision", Title: "Second independent cache decision"})
	e.must(e.alpha, "", "update", sources[0], "--status", "invalidated", "--reason", "Synthetic correction")
	summary := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "summary", Title: "Combined cache policy", DerivedFrom: sources})[0]
	record, data := readProvenanceClaim(t, e, e.alpha, summary)
	if record.Status != "inferred" || data.Support != 1 {
		t.Fatalf("remaining active source must supply exactly one support root: status=%s data=%+v", record.Status, data)
	}
	e.must(e.alpha, "", "update", sources[1], "--status", "disputed", "--reason", "Synthetic disagreement")
	record, data = readProvenanceClaim(t, e, e.alpha, summary)
	if record.Status != "unsupported" || data.Support != 0 {
		t.Fatalf("summary must lose support when the final active source is disputed: status=%s data=%+v", record.Status, data)
	}
}

func TestInactiveSharedClaimSupportRespectsProjectScope(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	beta := e.enroll(e.beta)
	source := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "decision", Title: "Private original cache policy"})[0]
	e.must(e.alpha, "", "update", source, "--status", "invalidated", "--reason", "Synthetic correction")
	in := contract.ClaimInput{Kind: "summary", Title: "Summary in another project", DerivedFrom: []string{source}}
	if out := e.cli(e.beta, capture(t, in), "capture"); out.errCode() != contract.CodeOutOfScope {
		t.Fatalf("private inactive source must remain out of scope: %s", out.Raw)
	}
	e.must(e.root, "", "share", source, "--to", beta)
	summary := e.captureIDs(e.beta, in)[0]
	requireLostClaimSupport(t, e, e.beta, summary, source, "invalidated")
}

// A search evaluates newer descendants before their older ancestors. Reusing
// an ancestor's nested result must not apply the descendant's depth limit to
// the ancestor's own top-level result.
func TestClaimSupportMemoPreservesTraversalDepth(t *testing.T) {
	e := newEnv(t, nil)
	e.enroll(e.alpha)
	var chain []string
	for i := 0; i < 11; i++ {
		in := contract.ClaimInput{Kind: "decision", Title: "Memo graph cache policy " + itoaT(int64(i))}
		if len(chain) > 0 {
			in.DerivedFrom = []string{chain[len(chain)-1]}
		}
		chain = append(chain, e.captureIDs(e.alpha, in)[0])
	}
	out := e.search(e.alpha, "memo graph", "--limit", "50", "--max-bytes", "65536")
	for i, id := range chain {
		item := find(out.Items, id)
		_, data := readProvenanceClaim(t, e, e.alpha, id)
		want := min(i+1, 9)
		if item == nil || item.Provenance.Support != want || data.Support != want {
			t.Fatalf("claim %d must have %d roots regardless of traversal order: search=%+v read=%+v", i, want, item, data)
		}
	}
}
