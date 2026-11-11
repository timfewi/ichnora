package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestDatabasePathsPreserveURICharacters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state #fragment?mode=ro%25")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "memory#live?cache=shared%.db")
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES ('fixture', 'URI path fixture', '')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup#snapshot?mode=rw%.db")
	if _, err := st.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckpointFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(dir, "restored#copy?mode=ro%.db")
	if _, err := Snapshot(ctx, backup, copy); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path, backup, copy} {
		if _, err := Verify(ctx, target); err != nil {
			t.Fatalf("verify exact path %q: %v", target, err)
		}
		db, err := sql.Open(driverName, fileURI(target)+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var count int
		err = db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE id = 'fixture'`).Scan(&count)
		db.Close()
		if err != nil || count != 1 {
			t.Fatalf("source record lost in %q: count=%d error=%v", target, count, err)
		}
	}
}

func TestRelativeDatabasePathPreservesURICharacters(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	path := "relative#memory?mode=ro%.db"
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SchemaVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
