package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
)

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, body)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// Stand-in tools exercise Ichnora's side of the composition contract
// (argument shape, digest guard, section matching). They are mocks: the live
// test below is the evidence that the installed tools behave this way.
func TestCompositionContractWithStandInTools(t *testing.T) {
	bin := t.TempDir()
	tabulaOut := filepath.Join(bin, "tabula.json")
	writeScript(t, filepath.Join(bin, "tabula"), "#!/bin/sh\ncat \"$ICHNORA_FAKE_TABULA\"\n")
	writeScript(t, filepath.Join(bin, "agent-evidence"), "#!/bin/sh\nprintf '{\"status\":\"ok\",\"argv\":\"%s\"}' \"$*\"\n")
	t.Setenv("ICHNORA_FAKE_TABULA", tabulaOut)
	e := newEnv(t, func(c *config.Config) {
		c.Tools = config.Tools{Tabula: filepath.Join(bin, "tabula"), AgentEvidence: filepath.Join(bin, "agent-evidence")}
	})
	e.enroll(e.alpha)
	writeFile(t, tabulaOut, `{"status":"ok","sections":[{"id":"entry/2-cache-1","section_fingerprint":"f1","freshness":"current"}]}`)
	digest := sha(t, filepath.Join(e.alpha, "parser", "cache.go"))
	id := e.captureIDs(e.alpha, contract.ClaimInput{Kind: "observation", Title: "Cache card describes digest keys",
		Support: []contract.Ref{{Kind: "file", Path: "parser/cache.go", Line: 3}},
		Depends: []contract.Ref{{Kind: "tabula", Card: ".tabula/ENTRY.md", Section: "entry/2-cache-1", Fingerprint: "f1", Path: "parser/cache.go"}}})[0]
	if it := find(e.search(e.alpha, "cache card").Items, id); it == nil || it.Group != "working" {
		t.Fatalf("a matching Tabula section must keep the claim current: %+v", it)
	}
	r := e.must(e.alpha, "", "evidence", id)
	if !strings.Contains(r.Raw, "--expected-sha256 "+digest) || !strings.Contains(r.Raw, "--root "+e.alpha) || !strings.Contains(r.Raw, "agent-evidence:ok") {
		t.Fatalf("excerpts must be requested with the stored digest guard: %s", r.Raw)
	}
	// The heading is renamed: same fingerprint, new section id.
	writeFile(t, tabulaOut, `{"status":"ok","sections":[{"id":"entry/2-parser-cache-1","section_fingerprint":"f1","freshness":"current"}]}`)
	out := e.search(e.alpha, "cache card")
	if find(out.Items, id) != nil || out.Omitted.Stale != 1 {
		t.Fatalf("a renamed Tabula section must not stay current: %+v", out)
	}
	// The tool disappears: unknown applicability, core retrieval continues.
	os.Remove(filepath.Join(bin, "tabula"))
	out = e.search(e.alpha, "cache card")
	if it := find(out.Items, id); it == nil || it.Group != "lead" || it.Applicability != "unknown" {
		t.Fatalf("a missing adapter must yield unknown applicability: %+v", out.Items)
	}
	if !strings.Contains(e.must(e.alpha, "", "status").Raw, "tabula: missing_adapter") {
		t.Fatal("status must report the missing adapter")
	}
}

// TestLiveComposition drives the installed Tabula, agent-evidence and AST
// Index CLIs on a synthetic repository. It runs only with ICHNORA_LIVE_TOOLS=1.
func TestLiveComposition(t *testing.T) {
	if os.Getenv("ICHNORA_LIVE_TOOLS") != "1" {
		t.Skip("set ICHNORA_LIVE_TOOLS=1 to exercise the installed evidence tools")
	}
	tools := map[string]string{}
	for _, name := range []string{"tabula", "agent-evidence", "ast-index"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("%s not installed", name)
		}
		tools[name], _ = filepath.EvalSymlinks(p)
	}
	var alpha string
	e := newEnv(t, func(c *config.Config) {
		alpha = filepath.Join(c.WorkspaceRoots[0], "alpha")
		c.Tools = config.Tools{Tabula: tools["tabula"], AgentEvidence: tools["agent-evidence"], ASTIndex: tools["ast-index"], ASTRoot: alpha}
	})
	e.enroll(alpha)
	run := func(name string, args ...string) []byte {
		t.Helper()
		out, err := exec.Command(tools[name], args...).Output()
		if err != nil {
			t.Fatalf("%s %v: %v", name, args, err)
		}
		return out
	}
	card := "# alpha\nFormat: tabula/1. Source labels are repository-relative; links resolve from this card.\nPurpose: fixture.\n\n## Cache\nThe [cache](../parser/cache.go) keys parses by digest.\n"
	writeFile(t, filepath.Join(alpha, ".tabula", "ENTRY.md"), card)
	run("tabula", "update", alpha)
	var tctx struct {
		Sections []struct {
			ID          string `json:"id"`
			Fingerprint string `json:"section_fingerprint"`
			Text        string `json:"text"`
		} `json:"sections"`
	}
	json.Unmarshal(run("tabula", "context", alpha, "--files", "parser/cache.go", "--max-bytes", "8000", "--json"), &tctx)
	var section, fp string
	for _, s := range tctx.Sections {
		if strings.Contains(s.Text, "keys parses by digest") {
			section, fp = s.ID, s.Fingerprint
		}
	}
	if section == "" {
		t.Fatalf("tabula context returned no cache section: %+v", tctx)
	}
	run("ast-index", "--root", alpha, "index", "--json")
	id := e.captureIDs(alpha, contract.ClaimInput{Kind: "observation", Title: "Parser Key is documented by the cache card",
		Support: []contract.Ref{{Kind: "file", Path: "parser/cache.go", Line: 3}, {Kind: "ast", Path: "parser/cache.go", Symbol: "Key", Line: 4}},
		Depends: []contract.Ref{{Kind: "tabula", Card: ".tabula/ENTRY.md", Section: section, Fingerprint: fp, Path: "parser/cache.go"}}})[0]
	if it := find(e.search(alpha, "parser key documented").Items, id); it == nil || it.Group != "working" {
		t.Fatalf("live Tabula revalidation must keep the claim current: %+v", it)
	}
	r := e.must(alpha, "", "evidence", id)
	if !strings.Contains(r.Raw, "Cache keys include the source digest") || !strings.Contains(r.Raw, "ast-index:ok") {
		t.Fatalf("live excerpts and AST freshness: %s", r.Raw)
	}
	// Rename the card heading: the section must need revalidation.
	writeFile(t, filepath.Join(alpha, ".tabula", "ENTRY.md"), strings.Replace(card, "## Cache", "## Parser cache", 1))
	run("tabula", "update", alpha)
	if out := e.search(alpha, "parser key documented"); find(out.Items, id) != nil {
		t.Fatalf("renamed live section stayed current: %+v", out.Items)
	}
	// Change the source: agent-evidence must refuse the old digest.
	writeFile(t, filepath.Join(alpha, "parser", "cache.go"), "package parser\n\nfunc Key() string { return \"v3\" }\n")
	r = e.must(alpha, "", "evidence", id)
	if !strings.Contains(r.Raw, "changed since capture") || strings.Contains(r.Raw, "Cache keys include the source digest") {
		t.Fatalf("a changed file must not be excerpted under the old digest: %s", r.Raw)
	}
}
