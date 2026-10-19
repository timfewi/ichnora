package main

import (
	"path/filepath"
	"testing"

	"github.com/timfewi/ichnora/internal/contract"
)

// The NixOS module supplies socket variables, not a user configuration file.
// Exercise that environment with a real service and unrelated XDG defaults.
func TestCLISocketEnvironment(t *testing.T) {
	e := newEnv(t, nil)
	t.Setenv("ICHNORA_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(e.root, "empty-config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(e.root, "other-runtime"))
	e.enroll(e.alpha)
	e.must(e.alpha, "", "status")
	e.must(e.alpha, "", "projects")

	missing := filepath.Join(e.root, "missing.sock")
	for _, args := range [][]string{
		{"status", "--socket", missing},
		{"projects", "--admin-socket", missing},
	} {
		if r := e.cli(e.alpha, "", args...); r.errCode() != contract.CodeUnavailable || r.Code == 0 {
			t.Fatalf("explicit unavailable socket must not fall back: %+v", r)
		}
	}
}
