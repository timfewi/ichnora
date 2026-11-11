package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/store"
)

// restore replaces the database with a verified backup while the service is
// stopped. The previous database is kept beside it, never deleted.
func restore(configPath, backup string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 2
	}
	lock, err := prepareState(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 1
	}
	defer lock.Close()
	ctx := context.Background()
	staged, err := stageRestore(ctx, cfg.StateDir, backup)
	if err != nil {
		fmt.Fprintln(stderr, "ichnora restore: backup rejected:", err)
		return 1
	}
	defer os.RemoveAll(staged.dir)
	target := filepath.Join(cfg.StateDir, dbName)
	previous, err := preserveDatabase(ctx, target)
	if err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 1
	}
	if previous != "" {
		fmt.Fprintln(stdout, "previous database kept at", previous)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(target + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "ichnora restore:", err)
			return 1
		}
	}
	// Persist old sidecar removal before replacing the database they belong to.
	if err := syncDir(cfg.StateDir); err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 1
	}
	// The canonical path remains present until this single atomic replacement.
	if err := os.Rename(staged.path, target); err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 1
	}
	if err := syncDir(staged.dir); err != nil {
		fmt.Fprintln(stderr, "ichnora restore: replacement needs durability verification:", err)
		return 1
	}
	if err := syncDir(cfg.StateDir); err != nil {
		fmt.Fprintln(stderr, "ichnora restore: replacement needs durability verification:", err)
		return 1
	}
	fmt.Fprintln(stdout, "restored schema version", staged.version)
	return 0
}

type restoreStage struct {
	dir, path string
	version   int
}

// stageRestore validates and migrates a snapshot before changing live state.
// Unique staging names let a later restore retry after an interrupted attempt.
func stageRestore(ctx context.Context, stateDir, backup string) (stage restoreStage, err error) {
	if _, err := store.Verify(ctx, backup); err != nil {
		return stage, err
	}
	stage.dir, err = os.MkdirTemp(stateDir, ".ichnora-restore-")
	if err != nil {
		return stage, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(stage.dir)
		}
	}()
	stage.path = filepath.Join(stage.dir, dbName)
	if _, err = store.Snapshot(ctx, backup, stage.path); err != nil {
		return stage, err
	}
	st, err := store.Open(ctx, stage.path, store.Options{})
	if err != nil {
		return stage, err
	}
	if err = st.Checkpoint(ctx); err != nil {
		st.Close()
		return stage, err
	}
	if err = st.Close(); err != nil {
		return stage, err
	}
	stage.version, err = store.Verify(ctx, stage.path)
	if err != nil {
		return stage, err
	}
	if err = syncFile(stage.path); err != nil {
		return stage, err
	}
	if err = syncDir(stage.dir); err != nil {
		return stage, err
	}
	// Persist the staging directory entry before any replacement is possible.
	err = syncDir(stateDir)
	return stage, err
}

func preserveDatabase(ctx context.Context, target string) (string, error) {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(target + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
				return "", errors.New("database is missing but SQLite sidecars remain; recover the previous database first")
			}
		}
		return "", nil
	} else if err != nil {
		return "", err
	}
	previous := target + ".previous-" + time.Now().UTC().Format("20060102T150405Z") + "-" + rand.Text()[:12]
	if _, err := store.Snapshot(ctx, target, previous); err != nil {
		if store.IsCorrupt(err) {
			return preserveCorruptDatabase(target, previous)
		}
		return "", err
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	// Recover and flush a crashed service's WAL before unlinking its sidecars.
	if err := store.CheckpointFile(ctx, target); err != nil {
		if store.IsCorrupt(err) {
			return preserveCorruptDatabase(target, previous+"-raw")
		}
		return "", err
	}
	if err := syncFile(target); err != nil {
		return "", err
	}
	return previous, nil
}

// A corrupt live database is a reason to restore, not to reject a valid backup.
// Keep its original bytes and sidecars for recovery instead of pretending that
// a readable SQLite snapshot exists. Busy and I/O failures do not use this path.
func preserveCorruptDatabase(target, previous string) (string, error) {
	if err := os.Mkdir(previous, 0o700); err != nil {
		return "", err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(target+suffix, filepath.Join(previous, dbName+suffix)); err != nil {
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
	}
	if err := syncDir(previous); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	return previous, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(path string) error { return syncFile(path) }

// Refuse a fresh empty database when a prior restore left recovery artifacts.
// A normal interrupted replacement always retains the old or the new target.
func checkRestoreState(stateDir string) error {
	target := filepath.Join(stateDir, dbName)
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == dbName+".restore" || name == dbName+"-wal" || name == dbName+"-shm" ||
			strings.HasPrefix(name, dbName+".previous-") || strings.HasPrefix(name, ".ichnora-restore-") {
			return errors.New("database missing while restore artifacts remain; restore a verified backup before starting the service")
		}
	}
	return nil
}
