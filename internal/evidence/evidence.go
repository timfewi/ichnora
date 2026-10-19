// Package evidence calls the optional external evidence tools (Tabula,
// agent-evidence, AST Index) through their public CLIs with bounded time and
// output. A missing tool is an explicit outcome, never a fallback.
package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/timfewi/ichnora/internal/config"
)

// Outcome statuses.
const (
	OK             = "ok"
	MissingAdapter = "missing_adapter"
	Failed         = "failed"
	Timeout        = "timeout"
	OutputLimit    = "output_limit"
	Unsupported    = "unsupported_root"
)

// Outcome is the bounded result of one tool call.
type Outcome struct {
	Status string          `json:"status"`
	Tool   string          `json:"tool"`
	Output json.RawMessage `json:"output,omitempty"`
}

// Tools holds the configured executables.
type Tools struct {
	cfg     config.Tools
	timeout time.Duration
	maxOut  int
}

// New configures the adapters.
func New(cfg config.Tools, timeout time.Duration, maxOut int) *Tools {
	return &Tools{cfg: cfg, timeout: timeout, maxOut: maxOut}
}

type limitedBuffer struct {
	bytes.Buffer
	max      int
	exceeded bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.Len()+len(p) > l.max {
		l.exceeded = true
		return 0, errors.New("output limit")
	}
	return l.Buffer.Write(p)
}

// Available reports whether a tool is configured and executable.
func (t *Tools) Available(name string) bool {
	exe := t.exe(name)
	if exe == "" {
		return false
	}
	st, err := os.Stat(exe)
	return err == nil && st.Mode()&0o111 != 0
}

func (t *Tools) exe(name string) string {
	switch name {
	case "tabula":
		return t.cfg.Tabula
	case "agent-evidence":
		return t.cfg.AgentEvidence
	case "ast-index":
		return t.cfg.ASTIndex
	}
	return ""
}

func (t *Tools) run(ctx context.Context, name string, args ...string) Outcome {
	if !t.Available(name) {
		return Outcome{Status: MissingAdapter, Tool: name}
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.exe(name), args...)
	out := &limitedBuffer{max: t.maxOut}
	cmd.Stdout = out
	cmd.Stderr = nil
	cmd.Stdin = nil
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return Outcome{Status: Timeout, Tool: name}
	case out.exceeded:
		return Outcome{Status: OutputLimit, Tool: name}
	}
	raw := bytes.TrimSpace(out.Bytes())
	if !json.Valid(raw) {
		return Outcome{Status: Failed, Tool: name}
	}
	if err != nil {
		return Outcome{Status: Failed, Tool: name, Output: raw}
	}
	return Outcome{Status: OK, Tool: name, Output: raw}
}

// TabulaContext runs `tabula context ROOT --files … --max-bytes N --json`.
func (t *Tools) TabulaContext(ctx context.Context, root string, files []string, maxBytes int) Outcome {
	args := []string{"context", root, "--json", "--max-bytes", strconv.Itoa(maxBytes), "--files"}
	return t.run(ctx, "tabula", append(args, files...)...)
}

// Read runs `agent-evidence --root ROOT read PATH` with the digest guard, so
// a changed file is rejected instead of returned under an old digest.
func (t *Tools) Read(ctx context.Context, root, path string, start, lines, maxBytes int, expectedSHA string) Outcome {
	args := []string{"--root", root, "read", path, "--start", strconv.Itoa(start), "--lines", strconv.Itoa(lines),
		"--max-bytes", strconv.Itoa(maxBytes)}
	if expectedSHA != "" {
		args = append(args, "--expected-sha256", expectedSHA)
	}
	return t.run(ctx, "agent-evidence", args...)
}

// ASTFreshness compares one file with the AST snapshot. Paths outside the
// declared indexed root are not passed to the tool.
func (t *Tools) ASTFreshness(ctx context.Context, absPath string) Outcome {
	if t.cfg.ASTRoot == "" {
		return Outcome{Status: MissingAdapter, Tool: "ast-index"}
	}
	rel, err := filepath.Rel(t.cfg.ASTRoot, absPath)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == "../" {
		return Outcome{Status: Unsupported, Tool: "ast-index"}
	}
	return t.run(ctx, "ast-index", "freshness", "--root", t.cfg.ASTRoot, "--json", "--file", rel)
}
