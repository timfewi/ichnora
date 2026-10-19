// Package config loads the host-supplied service configuration. Paths are
// host facts; the reusable binary contains no personal paths.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Tools names optional evidence executables by absolute path.
type Tools struct {
	Tabula        string `json:"tabula,omitempty"`
	AgentEvidence string `json:"agent_evidence,omitempty"`
	ASTIndex      string `json:"ast_index,omitempty"`
	// ASTRoot is the root declared to the AST service; references outside it
	// are reported as unsupported instead of being passed through.
	ASTRoot string `json:"ast_root,omitempty"`
}

// Limits bounds service resources. Zero values select the documented defaults.
type Limits struct {
	MaxDBBytes        int64 `json:"max_db_bytes,omitempty"`
	CaptureReserve    int64 `json:"capture_reserve_bytes,omitempty"`
	QueueDepth        int   `json:"queue_depth,omitempty"`
	Readers           int   `json:"readers,omitempty"`
	IngestBytes       int64 `json:"ingest_bytes_per_call,omitempty"`
	LineBytes         int   `json:"line_bytes,omitempty"`
	EventTextBytes    int   `json:"event_text_bytes,omitempty"`
	TurnTextBytes     int   `json:"turn_text_bytes,omitempty"`
	ToolTimeoutMillis int   `json:"tool_timeout_ms,omitempty"`
	ToolOutputBytes   int   `json:"tool_output_bytes,omitempty"`
}

// Config is the service configuration.
type Config struct {
	StateDir       string              `json:"state_dir,omitempty"`
	AgentSocket    string              `json:"agent_socket,omitempty"`
	AdminSocket    string              `json:"admin_socket,omitempty"`
	WorkspaceRoots []string            `json:"workspace_roots"`
	SessionSources map[string][]string `json:"session_sources"`
	// EnvironmentDescriptor is an optional host-generated JSON file.
	EnvironmentDescriptor string `json:"environment_descriptor,omitempty"`
	Tools                 Tools  `json:"tools"`
	Limits                Limits `json:"limits"`
}

// Load reads a configuration file strictly. A missing file yields defaults
// with no workspace or session roots, so nothing can be enrolled or ingested.
func Load(path string) (Config, error) {
	var c Config
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return c, err
		}
		if err == nil {
			dec := json.NewDecoder(strings.NewReader(string(data)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				return c, errors.New("configuration is not valid JSON for this version")
			}
		}
	}
	return c.WithDefaults()
}

// WithDefaults fills defaults and validates that every path is absolute.
func (c Config) WithDefaults() (Config, error) {
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if c.AgentSocket == "" && runtime != "" {
		c.AgentSocket = filepath.Join(runtime, "ichnora", "agent.sock")
	}
	if c.AdminSocket == "" && runtime != "" {
		c.AdminSocket = filepath.Join(runtime, "ichnora", "admin.sock")
	}
	l := &c.Limits
	setDefault(&l.MaxDBBytes, 1<<30)
	setDefault(&l.CaptureReserve, 32<<20)
	setDefault(&l.QueueDepth, 64)
	setDefault(&l.Readers, 4)
	setDefault(&l.IngestBytes, 4<<20)
	setDefault(&l.LineBytes, 1<<20)
	setDefault(&l.EventTextBytes, 4096)
	setDefault(&l.TurnTextBytes, 16384)
	setDefault(&l.ToolTimeoutMillis, 5000)
	setDefault(&l.ToolOutputBytes, 256<<10)
	paths := append([]string{c.StateDir, c.AgentSocket, c.AdminSocket, c.EnvironmentDescriptor,
		c.Tools.Tabula, c.Tools.AgentEvidence, c.Tools.ASTIndex, c.Tools.ASTRoot}, c.WorkspaceRoots...)
	for _, roots := range c.SessionSources {
		paths = append(paths, roots...)
	}
	for _, p := range paths {
		if p != "" && !filepath.IsAbs(p) {
			return c, errors.New("configuration paths must be absolute")
		}
	}
	if c.AgentSocket == "" || c.AdminSocket == "" {
		return c, errors.New("agent_socket and admin_socket are required when XDG_RUNTIME_DIR is unset")
	}
	return c, nil
}

func setDefault[T int | int64](v *T, d T) {
	if *v <= 0 {
		*v = d
	}
}

// DefaultStateDir is $XDG_STATE_HOME/ichnora, else ~/.local/state/ichnora.
func DefaultStateDir() string {
	if s := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(s) {
		return filepath.Join(s, "ichnora")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ichnora")
}

// DefaultConfigPath is $ICHNORA_CONFIG, else $XDG_CONFIG_HOME/ichnora/config.json.
func DefaultConfigPath() string {
	if p := os.Getenv("ICHNORA_CONFIG"); p != "" {
		return p
	}
	if s := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(s) {
		return filepath.Join(s, "ichnora", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ichnora", "config.json")
}
