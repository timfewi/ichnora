package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ichnora.db")
	s, err := Open(context.Background(), path, Options{Readers: 2, QueueDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// The linked library is the Nix SQLite with FTS5, and every connection
// carries the required pragmas.
func TestLinkedSQLiteAndPragmas(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	if v := s.SQLiteVersion(ctx); !strings.HasPrefix(v, "3.") {
		t.Fatalf("sqlite version %q", v)
	}
	var opts []string
	rows, _ := s.Read().QueryContext(ctx, `PRAGMA compile_options`)
	for rows.Next() {
		var o string
		rows.Scan(&o)
		opts = append(opts, o)
	}
	rows.Close()
	if !strings.Contains(strings.Join(opts, " "), "ENABLE_FTS5") {
		t.Fatal("linked SQLite lacks FTS5")
	}
	check := func(db *sql.DB, pragma, want string) {
		t.Helper()
		var got string
		if err := db.QueryRowContext(ctx, `PRAGMA `+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q (%v), want %q", pragma, got, err, want)
		}
	}
	for _, db := range []*sql.DB{s.writer, s.reader} {
		check(db, "foreign_keys", "1")
		check(db, "busy_timeout", "5000")
		check(db, "secure_delete", "1")
		check(db, "journal_mode", "wal")
	}
	check(s.writer, "synchronous", "2")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(path + suffix); err == nil && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", suffix, st.Mode().Perm())
		}
	}
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO roots (path, project_id, worktree_id, kind, added_at) VALUES ('/x', 'missing', 'w', 'primary', 'now')`)
		return err
	}); err == nil {
		t.Fatal("foreign keys are not enforced")
	}
}

// A cancelled write never commits; a full queue rejects instead of waiting.
func TestCancellationAndQueueBound(t *testing.T) {
	s, _ := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('cancelled', '1')`)
		return err
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
	var n int
	s.Read().QueryRow(`SELECT count(*) FROM meta WHERE key = 'cancelled'`).Scan(&n)
	if n != 0 {
		t.Fatal("a cancelled write committed")
	}
	release := make(chan struct{})
	started := make(chan struct{})
	go s.Write(context.Background(), func(*sql.Tx) error { close(started); <-release; return nil })
	<-started
	for i := 0; i < 2; i++ {
		go s.Write(context.Background(), func(*sql.Tx) error { return nil })
	}
	time.Sleep(50 * time.Millisecond)
	if err := s.Write(context.Background(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Fatalf("full queue must return ErrBusy, got %v", err)
	}
	close(release)
	// A long read is interrupted by its context.
	rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer rcancel()
	var max int64
	err := s.Read().QueryRowContext(rctx, `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT max(x) FROM c`).Scan(&max)
	if err == nil {
		t.Fatal("unbounded read was not interrupted")
	}
}

// Reopening applies nothing twice; a newer schema and foreign files are refused.
func TestMigrationsAndVerify(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	v, _ := s.SchemaVersion(ctx)
	s.Close()
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v2, _ := s2.SchemaVersion(ctx); v2 != v {
		t.Fatalf("reopen changed schema %d -> %d", v, v2)
	}
	backup := filepath.Join(t.TempDir(), "b.db")
	if _, err := s2.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if got, err := Verify(ctx, backup); err != nil || got != v {
		t.Fatalf("verify backup: %d %v", got, err)
	}
	s2.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (999, 'future', 'now')`)
		return err
	})
	s2.Close()
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema must be refused: %v", err)
	}
	foreign := filepath.Join(t.TempDir(), "other.db")
	os.WriteFile(foreign, []byte("not a database"), 0o600)
	if _, err := Verify(ctx, foreign); err == nil {
		t.Fatal("a foreign file passed verification")
	}
}
