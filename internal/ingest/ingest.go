// Package ingest normalizes native harness records into the ichnora.event/1
// envelope. Adapters parse; they do not decide scope or write storage.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
)

// Event kinds of the envelope.
const (
	KindUser          = "user"
	KindAssistant     = "assistant"
	KindToolCall      = "tool_call"
	KindToolResult    = "tool_result"
	KindCompaction    = "compaction"
	KindSessionStart  = "session_start"
	KindResume        = "resume"
	KindSessionEnd    = "session_end"
	KindSubagentStart = "subagent_start"
	KindSubagentStop  = "subagent_stop"
	KindOther         = "other"
)

// Event is one normalized native record.
type Event struct {
	NativeID     string
	IDBasis      string
	Session      string
	Kind         string
	Seq          *int64
	EventTime    string
	Cwd          string
	Text         string
	ToolName     string
	CallID       string
	Outcome      string
	Child        bool
	AgentID      string
	Version      string
	Truncated    bool
	Injected     bool
	Derived      bool
	Redactions   []string
	Coverage     []string
	SourceDigest string
	Source       string
}

// Flags is the persisted flag set of an event.
type Flags struct {
	Coverage   []string `json:"coverage,omitempty"`
	Redactions []string `json:"redactions,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
	Injected   bool     `json:"injected,omitempty"`
	Derived    bool     `json:"derived,omitempty"`
	Child      bool     `json:"child,omitempty"`
}

// FlagsOf extracts the persisted flags.
func (e *Event) FlagsOf() Flags {
	return Flags{Coverage: e.Coverage, Redactions: e.Redactions, Truncated: e.Truncated,
		Injected: e.Injected, Derived: e.Derived, Child: e.Child}
}

// State is adapter state carried across incremental reads in the cursor.
type State struct {
	Cwd     string `json:"cwd,omitempty"`
	Session string `json:"session,omitempty"`
	Version string `json:"version,omitempty"`
}

// Adapter parses one native line.
type Adapter interface {
	Harness() string
	Version() string
	Parse(line []byte, st *State) ([]Event, error)
}

// ForHarness returns the adapter for a harness name.
func ForHarness(name string) Adapter {
	switch name {
	case "claude":
		return Claude{}
	case "codex":
		return Codex{}
	case "opencode":
		return OpenCode{}
	}
	return nil
}

// Digest is the hex SHA-256 of b.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ID derives a deterministic identifier with a prefix.
func ID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:24]
}

// InjectedOpen and InjectedClose delimit context injected by Ichnora. Text
// between them is never stored as evidence.
const (
	InjectedOpen  = "<ichnora-context"
	InjectedClose = "</ichnora-context>"
)

var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`)},
	{"api_key", regexp.MustCompile(`\b(?:sk|rk|pk)-[A-Za-z0-9_-]{16,}`)},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"aws_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"google_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"bearer", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)},
	{"assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[_-]?key)\s*[:=]\s*["']?[^\s"']{6,}`)},
}

// Clean strips injected context, redacts declared credential patterns and
// bounds the text. It does not claim to detect every secret.
func Clean(text string, limit int) (out string, redactions []string, injected, truncated bool) {
	out = text
	for {
		i := strings.Index(out, InjectedOpen)
		if i < 0 {
			break
		}
		injected = true
		rest := out[i:]
		j := strings.Index(rest, InjectedClose)
		if j < 0 {
			out = out[:i]
			break
		}
		out = out[:i] + "[ichnora context omitted]" + rest[j+len(InjectedClose):]
	}
	for _, p := range secretPatterns {
		if p.re.MatchString(out) {
			out = p.re.ReplaceAllString(out, "[redacted:"+p.name+"]")
			redactions = append(redactions, p.name)
		}
	}
	out, truncated = contract.Truncate(strings.TrimSpace(out), limit)
	return out, redactions, injected, truncated
}

// Normalize applies Clean to an event's text with the configured limit.
func Normalize(e *Event, limit int) {
	text, red, inj, trunc := Clean(e.Text, limit)
	e.Text = text
	e.Redactions = append(e.Redactions, red...)
	e.Injected = e.Injected || inj
	e.Truncated = e.Truncated || trunc
}

// Timestamp normalizes RFC 3339 strings or epoch milliseconds; unknown stays "".
func Timestamp(v any) string {
	switch t := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts.UTC().Format(contract.TimeFormat)
		}
	case float64:
		if t > 1e11 {
			return time.UnixMilli(int64(t)).UTC().Format(contract.TimeFormat)
		}
	case json.Number:
		if n, err := t.Int64(); err == nil && n > 1e11 {
			return time.UnixMilli(n).UTC().Format(contract.TimeFormat)
		}
	}
	return ""
}

// commandName keeps only the first literal word of a shell command.
func commandName(cmd any) string {
	var first string
	switch c := cmd.(type) {
	case string:
		first = strings.Fields(c + " ")[0]
	case []any:
		if len(c) > 0 {
			first, _ = c[0].(string)
			// Codex wraps commands as ["bash", "-lc", "..."].
			if (first == "bash" || first == "sh" || first == "zsh") && len(c) > 2 {
				if s, ok := c[2].(string); ok {
					first = strings.Fields(s + " ")[0]
				}
			}
		}
	}
	if i := strings.LastIndex(first, "/"); i >= 0 {
		first = first[i+1:]
	}
	if len(first) > 64 || strings.ContainsAny(first, "=$`'\"") {
		return ""
	}
	return first
}

func toolText(name, command, outcome string) string {
	s := "tool " + name
	if command != "" {
		s += " (" + command + ")"
	}
	if outcome != "" {
		s += ": " + outcome
	}
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func decode(line []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil || m == nil {
		return nil, errMalformed
	}
	return m, nil
}

type parseError string

func (e parseError) Error() string { return string(e) }

const errMalformed = parseError("malformed native record")
