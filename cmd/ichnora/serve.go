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
	if err := checkRestoreState(cfg.StateDir); err != nil {
		return err
	}
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
	cleanup := func() {
		removed, err := c.PruneMissingProjects(ctx)
		if err != nil {
			log.Warn("missing-project cleanup failed", "error_type", fmt.Sprintf("%T", err))
		}
		if removed > 0 {
			log.Info("removed missing projects", "count", removed)
		}
	}
	cleanup()
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
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-errs:
			result = err
			break loop
		case <-ticker.C:
			cleanup()
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return result
}
