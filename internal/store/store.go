// Package store owns the SQLite database: connection pragmas, migrations, the
// bounded single-writer path and the read-only pool.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var migrations embed.FS

const driverName = "ichnora_sqlite3"

// Pragmas applied to every connection, reader or writer.
const connectionPragmas = `PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA secure_delete = ON;
PRAGMA cache_size = -8192;
PRAGMA trusted_schema = OFF;`

func init() {
	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			_, err := conn.Exec(connectionPragmas, nil)
			return err
		},
	})
}

var (
	// ErrBusy reports a full writer queue; nothing was written.
	ErrBusy = errors.New("writer queue full")
	// ErrClosed reports a write after Close.
	ErrClosed = errors.New("store closed")
	// ErrNewerSchema refuses a database written by a newer binary.
	ErrNewerSchema = errors.New("database schema is newer than this binary")
)

// Options bounds the store's resources.
type Options struct {
	Readers    int
	QueueDepth int
}

// Store is the database handle shared by the service.
type Store struct {
	Path   string
	writer *sql.DB
	reader *sql.DB
	queue  chan job
	mu     sync.RWMutex
	closed bool
	done   chan struct{}
}

type job struct {
	ctx    context.Context
	fn     func(*sql.Tx) error
	result chan error
}

// Open creates or opens the database at path, applies migrations, verifies
// FTS5 in the linked SQLite and starts the writer.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if opts.Readers <= 0 {
		opts.Readers = 4
	}
	if opts.QueueDepth <= 0 {
		opts.QueueDepth = 64
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	writer, err := sql.Open(driverName, "file:"+path+"?_journal_mode=WAL&_synchronous=FULL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	writer.SetConnMaxLifetime(0)
	s := &Store{Path: path, writer: writer, queue: make(chan job, opts.QueueDepth), done: make(chan struct{})}
	if err := s.migrate(ctx); err != nil {
		writer.Close()
		return nil, err
	}
	if err := probeFTS5(ctx, writer); err != nil {
		writer.Close()
		return nil, err
	}
	reader, err := sql.Open(driverName, "file:"+path+"?mode=ro")
	if err != nil {
		writer.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(opts.Readers)
	s.reader = reader
	restrictFiles(path)
	go s.run()
	return s, nil
}

// restrictFiles keeps the database and its WAL/SHM companions private.
func restrictFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
}

func probeFTS5(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE VIRTUAL TABLE temp.ichnora_fts5_probe USING fts5(x)`); err != nil {
		return fmt.Errorf("linked SQLite lacks FTS5: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DROP TABLE temp.ichnora_fts5_probe`)
	if _, err := conn.ExecContext(ctx, `INSERT INTO temp.ichnora_fts5_probe (x) VALUES ('probe value')`); err != nil {
		return err
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.ichnora_fts5_probe WHERE ichnora_fts5_probe MATCH 'probe'`).Scan(&n); err != nil || n != 1 {
		return fmt.Errorf("FTS5 probe failed: %v", err)
	}
	return nil
}

// SQLiteVersion reports the linked library version.
func (s *Store) SQLiteVersion(ctx context.Context) string {
	var v string
	_ = s.reader.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&v)
	return v
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	type migration struct {
		version int
		name    string
	}
	var all []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return fmt.Errorf("bad migration name %q", e.Name())
		}
		all = append(all, migration{v, e.Name()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	var current int
	if err := s.writer.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if len(all) > 0 && current > all[len(all)-1].version {
		return ErrNewerSchema
	}
	for _, m := range all {
		if m.version <= current {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := s.writer.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.reader.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func (s *Store) run() {
	defer close(s.done)
	for j := range s.queue {
		if err := j.ctx.Err(); err != nil {
			j.result <- err
			continue
		}
		j.result <- s.apply(j)
	}
}

func (s *Store) apply(j job) (err error) {
	tx, err := s.writer.BeginTx(j.ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			err = fmt.Errorf("write panicked: %v", p)
		}
	}()
	if err := j.fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Write runs fn in one short immediate transaction on the single writer. It
// returns ErrBusy without waiting when the queue is full. Once queued, Write
// waits for the outcome, so a returned nil always means committed.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrClosed
	}
	j := job{ctx: ctx, fn: fn, result: make(chan error, 1)}
	select {
	case s.queue <- j:
	default:
		s.mu.RUnlock()
		return ErrBusy
	}
	s.mu.RUnlock()
	return <-j.result
}

// Read returns the read-only pool.
func (s *Store) Read() *sql.DB { return s.reader }

// Checkpoint truncates the WAL after deletions; it waits for the writer connection.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.writer.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Backup writes a consistent snapshot to a new private file.
func (s *Store) Backup(ctx context.Context, out string) (int64, error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	f.Close()
	if _, err := s.reader.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		os.Remove(out)
		return 0, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Size reports the database plus WAL size in bytes.
func (s *Store) Size() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(s.Path + suffix); err == nil {
			total += st.Size()
		}
	}
	return total
}

// Close stops accepting writes, drains the queue and closes the pools.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.queue)
	s.mu.Unlock()
	<-s.done
	err := s.reader.Close()
	if werr := s.writer.Close(); err == nil {
		err = werr
	}
	return err
}

// BumpGeneration records a policy change (enrollment, sharing, revocation)
// that can alter any caller's scope.
func BumpGeneration(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT) WHERE key = 'generation'`)
	return err
}

// BumpProject records a change to one project's records. Cursors of callers
// without access to that project are unaffected.
func BumpProject(ctx context.Context, tx *sql.Tx, project string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('generation:' || ?, '1')
		ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`, project)
	return err
}

// ProjectGeneration returns a project's change counter (0 if unchanged).
func ProjectGeneration(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, project string) int64 {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'generation:' || ?`, project).Scan(&v); err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// Generation returns the current change counter.
func Generation(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'generation'`).Scan(&v); err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// IsBusy reports SQLite lock contention that outlasted the busy timeout.
func IsBusy(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked
	}
	return errors.Is(err, ErrBusy)
}

// IsFull reports disk or database-size exhaustion.
func IsFull(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && se.Code == sqlite3.ErrFull
}

// Verify checks a database file offline: integrity, a known schema version
// and FTS5 support. It opens the file read-only.
func Verify(ctx context.Context, path string) (int, error) {
	db, err := sql.Open(driverName, "file:"+path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil {
		return 0, err
	}
	if check != "ok" {
		return 0, errors.New("integrity check failed")
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, errors.New("not an Ichnora database")
	}
	entries, _ := fs.ReadDir(migrations, "migrations")
	if v > len(entries) {
		return v, ErrNewerSchema
	}
	return v, nil
}
