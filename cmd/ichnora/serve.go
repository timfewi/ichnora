package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/timfewi/ichnora/internal/config"
	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/core"
	"github.com/timfewi/ichnora/internal/policy"
	"github.com/timfewi/ichnora/internal/server"
	"github.com/timfewi/ichnora/internal/store"
)

const dbName = "ichnora.db"

// prepareState creates the private state directory and takes its exclusive
// lock, shared by the service and offline maintenance.
func prepareState(cfg config.Config) (*os.File, error) {
	dir := filepath.Clean(cfg.StateDir)
	if strings.HasPrefix(dir, "/nix/store") {
		return nil, errors.New("state directory must be outside the Nix store")
	}
	for _, w := range cfg.WorkspaceRoots {
		if policy.Within(filepath.Clean(w), dir) {
			return nil, errors.New("state directory must be outside the workspace roots")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("the state directory is locked by a running service or maintenance command")
	}
	return lock, nil
}

func serve(configPath string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Error("configuration rejected", "reason", err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := runService(ctx, cfg, log, nil); err != nil {
		log.Error("service stopped", "reason", err.Error())
		return 1
	}
	return 0
}

// runService owns the state lock, database and sockets until ctx ends.
// ready, if set, is closed once both sockets accept connections.
func runService(ctx context.Context, cfg config.Config, log *slog.Logger, ready chan<- struct{}) error {
	lock, err := prepareState(cfg)
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := store.Open(ctx, filepath.Join(cfg.StateDir, dbName), store.Options{Readers: cfg.Limits.Readers, QueueDepth: cfg.Limits.QueueDepth})
	if err != nil {
		return fmt.Errorf("database unavailable: %w", err)
	}
	defer st.Close()
	secret, err := core.LoadSecret(cfg.StateDir)
	if err != nil {
		return errors.New("state secret unavailable")
	}
	core.Version = version
	c := core.New(st, cfg, secret, log)
	srv := server.New(c, log)
	errs := make(chan error, 2)
	for _, l := range []struct {
		path  string
		roles []contract.Role
	}{
		{cfg.AgentSocket, []contract.Role{contract.RoleReader, contract.RoleCapture}},
		{cfg.AdminSocket, []contract.Role{contract.RoleReader, contract.RoleCapture, contract.RoleAdmin}},
	} {
		ln, err := server.Listen(l.path)
		if err != nil {
			srv.Shutdown(context.Background())
			return fmt.Errorf("socket %s unavailable: %w", l.path, err)
		}
		go func(roles []contract.Role) { errs <- srv.Serve(ln, roles) }(l.roles)
	}
	log.Info("serving", "version", version, "contract", contract.Version, "sqlite", st.SQLiteVersion(ctx),
		"agent_socket", cfg.AgentSocket, "admin_socket", cfg.AdminSocket)
	if ready != nil {
		close(ready)
	}
	var result error
	select {
	case <-ctx.Done():
	case err := <-errs:
		result = err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return result
}

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
	version, err := store.Verify(ctx, backup)
	if err != nil {
		fmt.Fprintln(stderr, "ichnora restore: backup rejected:", err)
		return 1
	}
	target := filepath.Join(cfg.StateDir, dbName)
	staged := target + ".restore"
	if err := copyFile(backup, staged); err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 1
	}
	if _, err := os.Stat(target); err == nil {
		previous := target + ".previous-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(target, previous); err != nil {
			fmt.Fprintln(stderr, "ichnora restore:", err)
			return 1
		}
		fmt.Fprintln(stdout, "previous database kept at", previous)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(target + suffix)
	}
	if err := os.Rename(staged, target); err != nil {
		fmt.Fprintln(stderr, "ichnora restore:", err)
		return 1
	}
	// Opening applies pending migrations to an older backup.
	st, err := store.Open(ctx, target, store.Options{})
	if err != nil {
		fmt.Fprintln(stderr, "ichnora restore: restored database cannot be opened:", err)
		return 1
	}
	st.Close()
	fmt.Fprintln(stdout, "restored schema version", version)
	return 0
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
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
