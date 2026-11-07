package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
)

func TestMissingProjectCleanupRemovesEnrollmentMemoryAndPreventsReplay(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PruneMissingProjects = true })
	e.enroll(e.alpha, "--collect", "--recall")
	e.enroll(e.beta, "--recall")
	e.captureIDs(e.alpha, contract.ClaimInput{Kind: "note", Title: "Deleted project memory"})
	keep := e.captureIDs(e.beta, contract.ClaimInput{Kind: "note", Title: "Retained project memory"})[0]
	session := "missing-project"
	transcript := e.fixture("claude-transcript.jsonl", filepath.Join(e.claudeRoot, "p", session+".jsonl"), session, e.alpha)
	stop := map[string]any{"hook_event_name": "Stop", "session_id": session, "transcript_path": transcript, "cwd": e.alpha}
	e.hook(e.alpha, "claude", stop)
	if len(e.search(e.alpha, "cache invalidation fix").Items) == 0 {
		t.Fatal("fixture was not ingested")
	}
	e.stop()
	if err := os.RemoveAll(e.alpha); err != nil {
		t.Fatal(err)
	}
	e.start()
	if projects := e.must(e.root, "", "projects"); strings.Contains(projects.Raw, `"alpha"`) || !strings.Contains(projects.Raw, `"beta"`) {
		t.Fatalf("cleanup changed the wrong project enrollment: %s", projects.Raw)
	}
	if got := e.must(e.beta, "", "read", keep); !strings.Contains(got.Raw, keep) {
		t.Fatal("unrelated project memory was lost")
	}
	mkRepo(t, e.alpha, strings.Repeat("a", 40))
	e.enroll(e.alpha, "--collect", "--recall")
	e.hook(e.alpha, "claude", stop)
	if got := e.search(e.alpha, "cache invalidation fix"); got.Totals.Sources != 0 {
		t.Fatal("deleted session was re-imported")
	}
	if got := e.search(e.alpha, "Deleted project memory"); len(got.Items) != 0 {
		t.Fatal("deleted claims survived re-enrollment")
	}
}

func TestMissingProjectCleanupPreservesUnavailableOrExistingRoots(t *testing.T) {
	for _, kind := range []string{"disabled", "workspace-unavailable", "worktree-present", "alias", "directory-present"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, func(c *config.Config) { c.PruneMissingProjects = kind != "disabled" })
			e.enroll(e.alpha, "--recall")
			if kind == "worktree-present" {
				e.enroll(e.beta, "--worktree-of", "alpha")
			}
			e.stop()
			if kind == "workspace-unavailable" {
				if err := os.Rename(e.ws, e.ws+"-offline"); err != nil {
					t.Fatal(err)
				}
			} else if kind != "directory-present" {
				if err := os.RemoveAll(e.alpha); err != nil {
					t.Fatal(err)
				}
				if kind == "alias" {
					if err := os.Symlink(e.beta, e.alpha); err != nil {
						t.Fatal(err)
					}
				}
			}
			e.start()
			if projects := e.must(e.root, "", "projects"); !strings.Contains(projects.Raw, `"alpha"`) {
				t.Fatalf("cleanup removed a protected project: %s", projects.Raw)
			}
		})
	}
}
