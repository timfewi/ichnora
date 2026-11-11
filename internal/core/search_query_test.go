package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/store"
)

func TestCandidateExaminationLimitFailsExplicitlyAfterSQLFilters(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "synthetic.db"), store.Options{Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES ('p', 'synthetic', '')`); err != nil {
			return err
		}
		insert, err := tx.PrepareContext(ctx, `INSERT INTO claims
			(id, project_id, kind, title, body, asserted, status, origin, lineage, created_at)
			VALUES (?, 'p', ?, 'Synthetic', '', 'observed', 'observed', '{}', ?, '')`)
		if err != nil {
			return err
		}
		defer insert.Close()
		for n := 0; n <= maxSearchCandidates; n++ {
			id := fmt.Sprintf("synthetic-%05d", n)
			if _, err := insert.ExecContext(ctx, id, "note", id); err != nil {
				return err
			}
		}
		_, err = insert.ExecContext(ctx, "selected-decision", "decision", "selected-decision")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	visited := 0
	err = walkClaims(ctx, st.Read(), Scope{ProjectID: "p"}, contract.SearchRequest{}, "", "", "", func(claimRow) error {
		visited++
		return nil
	})
	if !errors.Is(err, errSearchTooBroad) || visited != maxSearchCandidates {
		t.Fatalf("broad search did not fail explicitly at its examination bound: visited=%d err=%v", visited, err)
	}
	visited = 0
	err = walkClaims(ctx, st.Read(), Scope{ProjectID: "p"}, contract.SearchRequest{Kinds: []string{"decision"}}, "", "", "", func(cl claimRow) error {
		visited++
		if cl.ID != "selected-decision" {
			t.Fatalf("kind filter selected %s", cl.ID)
		}
		return nil
	})
	if err != nil || visited != 1 {
		t.Fatalf("SQL filter did not narrow before examination limit: visited=%d err=%v", visited, err)
	}
}

func TestSearchResolvesEnrollmentInItsReadSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(root, "synthetic.db"), store.Options{Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES ('p', 'synthetic', '')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO roots (path, project_id, worktree_id, kind, added_at) VALUES (?, 'p', 'w', 'primary', '')`, project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := New(st, config.Config{WorkspaceRoots: []string{workspace}}, make([]byte, 32), slog.New(slog.NewTextHandler(io.Discard, nil)))
	var once sync.Once
	// The request clock is called while preparing external applicability state.
	// Withdraw then, before the read snapshot starts, to reproduce the race with
	// a previously resolved scope without nondeterministic goroutine timing.
	c.Now = func() time.Time {
		once.Do(func() {
			if err := st.Write(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `UPDATE projects SET enrolled = 0 WHERE id = 'p'`); err != nil {
					return err
				}
				return store.BumpGeneration(ctx, tx)
			}); err != nil {
				t.Fatal(err)
			}
		})
		return time.Now()
	}
	_, _, err = c.Search(ctx, Caller{Cwd: project, Roles: map[contract.Role]bool{contract.RoleReader: true}}, contract.SearchRequest{})
	if !errors.Is(err, errNoScope) {
		t.Fatalf("withdrawn enrollment was reused outside the data snapshot: %v", err)
	}
}

// Legacy stored lineage strings can differ even when current source resolution
// identifies the same evidence. Search must collapse their resolved lineage.
func TestSearchDeduplicatesResolvedSupportDespiteStoredLineage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(root, "synthetic.db"), store.Options{Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES ('p', 'synthetic', '')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO roots (path, project_id, worktree_id, kind, added_at) VALUES (?, 'p', 'w', 'primary', '')`, project); err != nil {
			return err
		}
		for _, id := range []string{"clm_first", "clm_second"} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO claims
				(id, project_id, kind, title, body, asserted, status, origin, lineage, created_at)
				VALUES (?, 'p', 'summary', 'Synthetic summary', '', 'inferred', 'inferred', '{}', ?, '')`, id, "legacy:"+id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO refs (claim_id, role, kind, ref, root) VALUES (?, 'support', 'external', 'same-source', 'external:synthetic-shared-root')`, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := New(st, config.Config{WorkspaceRoots: []string{workspace}}, make([]byte, 32), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, _, err := c.Search(ctx, Caller{Cwd: project, Roles: map[contract.Role]bool{contract.RoleReader: true}}, contract.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Omitted.Duplicates != 1 || out.Items[0].Provenance.Support != 1 {
		t.Fatalf("resolved shared evidence was treated as independent lineages: %+v", out)
	}
}
