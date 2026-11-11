package core

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
	"github.com/timfewi/ichnora/internal/store"
)

func supportFixture(t *testing.T, fn func(context.Context, *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "memory.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES ('project', 'synthetic', '2026-01-01')`); err != nil {
			return err
		}
		for _, id := range []string{"ses_first", "ses_second"} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, harness, native_id, project_id, first_seen, last_seen)
				VALUES (?, 'opencode', ?, 'project', '2026-01-01', '2026-01-01')`, id, id); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}
}

func seedSupportEvent(ctx context.Context, tx *sql.Tx, session, turn, version string, flags ingest.Flags, invalid string) (string, error) {
	id := ingest.ID("evt_", session, version)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO turns (id, session_id, project_id, first_event, last_event, text, digest)
		VALUES (?, ?, 'project', ?, ?, 'synthetic source', 'digest')`, turn, session, id, id); err != nil {
		return "", err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events (id, session_id, native_id, id_basis, project_id, turn_id, kind,
		ingested_at, source, source_digest, adapter_version, flags, text, invalid)
		VALUES (?, ?, ?, 'native', 'project', ?, 'user', '2026-01-01', 'synthetic', 'digest', 'opencode-adapter/1', ?, 'synthetic source', ?)`,
		id, session, version, turn, mustJSON(flags), invalid)
	return id, err
}

func capturedSupportRoots(ctx context.Context, tx *sql.Tx, refs []preparedRef) ([]string, []preparedRef, error) {
	return capturedSupportRootsFor(ctx, tx, "clm_target", refs)
}

func capturedSupportRootsFor(ctx context.Context, tx *sql.Tx, id string, refs []preparedRef) ([]string, []preparedRef, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO claims (id, project_id, kind, title, body, asserted, status, origin, lineage, created_at)
		VALUES (?, 'project', 'summary', 'Synthetic summary', '', 'inferred', 'inferred', '{}', ?, '2026-01-01')`, id, "self:"+id); err != nil {
		return nil, nil, err
	}
	for i := range refs {
		if err := bindRef(ctx, tx, Scope{ProjectID: "project"}, &refs[i]); err != nil {
			return nil, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO refs (claim_id, role, kind, ref, root, state, lost_reason)
			VALUES (?, 'support', ?, ?, ?, ?, ?)`, id, refs[i].kind, refs[i].ref, refs[i].root, refs[i].state, refs[i].lost); err != nil {
			return nil, nil, err
		}
	}
	roots, err := supportRoots(ctx, tx, id, 0, map[string][]string{})
	return roots, refs, err
}

func TestVersionedSourceSupportSharesMessageIdentity(t *testing.T) {
	for _, kinds := range [][]string{{"event"}, {"turn"}, {"event", "turn"}} {
		t.Run(fmt.Sprint(kinds), func(t *testing.T) {
			var roots []string
			supportFixture(t, func(ctx context.Context, tx *sql.Tx) error {
				flags := ingest.Flags{MessageKey: "native-message#0"}
				first, err := seedSupportEvent(ctx, tx, "ses_first", "trn_first", "version-one", flags, "")
				if err != nil {
					return err
				}
				second, err := seedSupportEvent(ctx, tx, "ses_first", "trn_second", "version-two", flags, "")
				if err != nil {
					return err
				}
				var refs []preparedRef
				for _, kind := range kinds {
					ids := []string{first, second}
					if kind == "turn" {
						ids = []string{"trn_first", "trn_second"}
					}
					for _, id := range ids {
						refs = append(refs, preparedRef{role: "support", kind: kind, ref: id, state: "ok"})
					}
				}
				roots, _, err = capturedSupportRoots(ctx, tx, refs)
				return err
			})
			if len(roots) != 1 {
				t.Fatalf("streaming versions must share one source root: %v", roots)
			}
		})
	}
}

func TestVersionedSourceIdentityIsScopedToSessionAndPart(t *testing.T) {
	var roots []string
	supportFixture(t, func(ctx context.Context, tx *sql.Tx) error {
		var refs []preparedRef
		for i, source := range []struct{ session, key string }{
			{"ses_first", "native-message#0"},
			{"ses_second", "native-message#0"},
			{"ses_first", "native-message#1"},
		} {
			id, err := seedSupportEvent(ctx, tx, source.session, fmt.Sprint("trn_", i), fmt.Sprint("version-", i), ingest.Flags{MessageKey: source.key}, "")
			if err != nil {
				return err
			}
			refs = append(refs, preparedRef{role: "support", kind: "event", ref: id, state: "ok"})
		}
		var err error
		roots, _, err = capturedSupportRoots(ctx, tx, refs)
		return err
	})
	if len(roots) != 3 {
		t.Fatalf("different sessions and message parts must retain distinct roots: %v", roots)
	}
}

func TestVersionedTurnSupportSnapshotsPermittedSources(t *testing.T) {
	var before, after []string
	supportFixture(t, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := seedSupportEvent(ctx, tx, "ses_first", "trn_first", "version-one", ingest.Flags{MessageKey: "native-message#0"}, ""); err != nil {
			return err
		}
		if _, err := seedSupportEvent(ctx, tx, "ses_first", "trn_first", "injected", ingest.Flags{MessageKey: "injected#0", Derived: true}, ""); err != nil {
			return err
		}
		if _, err := seedSupportEvent(ctx, tx, "ses_first", "trn_first", "invalid", ingest.Flags{MessageKey: "invalid#0"}, "synthetic correction"); err != nil {
			return err
		}
		var err error
		before, _, err = capturedSupportRoots(ctx, tx, []preparedRef{{role: "support", kind: "turn", ref: "trn_first", state: "ok"}})
		if err != nil {
			return err
		}
		if _, err := seedSupportEvent(ctx, tx, "ses_first", "trn_first", "later-source", ingest.Flags{MessageKey: "later-message#0"}, ""); err != nil {
			return err
		}
		after, err = supportRoots(ctx, tx, "clm_target", 0, map[string][]string{})
		return err
	})
	if len(before) != 1 || len(after) != 1 || before[0] != after[0] {
		t.Fatalf("derived/invalid and later appended sources must not add corroboration: before=%v after=%v", before, after)
	}
}

func TestVersionedTurnSupportLimitIsExplicit(t *testing.T) {
	for _, limit := range []string{"roots", "events"} {
		t.Run(limit, func(t *testing.T) {
			var failure error
			supportFixture(t, func(ctx context.Context, tx *sql.Tx) error {
				count := maxTurnSupportRoots
				if limit == "events" {
					count = maxTurnSupportEvents
				}
				for i := 0; i <= count; i++ {
					key := fmt.Sprint("message-", i)
					if limit == "events" {
						key = "same-message"
					}
					if _, err := seedSupportEvent(ctx, tx, "ses_first", "trn_first", fmt.Sprint("version-", i), ingest.Flags{MessageKey: key}, ""); err != nil {
						return err
					}
				}
				_, _, failure = capturedSupportRoots(ctx, tx, []preparedRef{{role: "support", kind: "turn", ref: "trn_first", state: "ok"}})
				return nil
			})
			protocol, ok := failure.(*contract.Error)
			if !ok || protocol.Code != contract.CodeInvalid || protocol.Message != errTurnSupportLimit.Message {
				t.Fatalf("an oversized source set must reject capture rather than partially count support: %v", failure)
			}
		})
	}
}

func TestLegacyTurnSupportKeepsExistingIdentity(t *testing.T) {
	var roots []string
	supportFixture(t, func(ctx context.Context, tx *sql.Tx) error {
		for _, version := range []string{"first", "second"} {
			if _, err := seedSupportEvent(ctx, tx, "ses_first", "trn_"+version, version, ingest.Flags{}, ""); err != nil {
				return err
			}
		}
		var err error
		roots, _, err = capturedSupportRoots(ctx, tx, []preparedRef{
			{role: "support", kind: "turn", ref: "trn_first", state: "ok"},
			{role: "support", kind: "turn", ref: "trn_second", state: "ok"},
		})
		return err
	})
	if len(roots) != 2 || roots[0] != "turn:trn_first" || roots[1] != "turn:trn_second" {
		t.Fatalf("legacy turn identities must remain unchanged: %v", roots)
	}
}

func TestLegacyAndVersionedSupportSharePersistedRoots(t *testing.T) {
	for _, part := range []string{"", "#1"} {
		t.Run("part"+part, func(t *testing.T) {
			var before, upgraded, derived, appended []string
			supportFixture(t, func(ctx context.Context, tx *sql.Tx) error {
				legacy, err := seedSupportEvent(ctx, tx, "ses_first", "trn_legacy", "native-message"+part, ingest.Flags{}, "")
				if err != nil {
					return err
				}
				before, _, err = capturedSupportRoots(ctx, tx, []preparedRef{
					{role: "support", kind: "event", ref: legacy, state: "ok"},
					{role: "support", kind: "turn", ref: "trn_legacy", state: "ok"},
				})
				if err != nil {
					return err
				}
				key := "native-message" + part
				if part == "" {
					key += "#0"
				}
				updated, err := seedSupportEvent(ctx, tx, "ses_first", "trn_updated", "changed-version", ingest.Flags{MessageKey: key}, "")
				if err != nil {
					return err
				}
				upgraded, err = supportRoots(ctx, tx, "clm_target", 0, map[string][]string{})
				if err != nil {
					return err
				}
				derived, _, err = capturedSupportRootsFor(ctx, tx, "clm_derived", []preparedRef{
					{role: "support", kind: "claim", ref: "clm_target", state: "ok"},
					{role: "support", kind: "event", ref: updated, state: "ok"},
					{role: "support", kind: "turn", ref: "trn_legacy", state: "ok"},
					{role: "support", kind: "turn", ref: "trn_updated", state: "ok"},
				})
				if err != nil {
					return err
				}
				later, err := seedSupportEvent(ctx, tx, "ses_first", "trn_legacy", "later-source", ingest.Flags{MessageKey: "later-message#0"}, "")
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE events SET ingested_at = '2026-01-02' WHERE id = ?`, later); err != nil {
					return err
				}
				appended, err = supportRoots(ctx, tx, "clm_derived", 0, map[string][]string{})
				return err
			})
			if len(before) != 2 || len(upgraded) != 1 || len(derived) != 1 || len(appended) != 1 ||
				upgraded[0] != derived[0] || derived[0] != appended[0] {
				t.Fatalf("legacy references and changed versions must resolve to one historical source: before=%v upgraded=%v derived=%v appended=%v", before, upgraded, derived, appended)
			}
		})
	}
}
