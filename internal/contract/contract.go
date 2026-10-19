// Package contract defines the versioned request and response types shared by
// the service, CLI, hooks and MCP bridge, together with byte-budget encoding.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	Version         = "ichnora/1"
	EventEnvelope   = "ichnora.event/1"
	EnvironmentSpec = "ichnora.environment/1"

	DefaultBudget = 8192
	MinBudget     = 1024
	MaxBudget     = 262144
	PreviewBytes  = 280
	BudgetUnit    = "UTF-8 bytes"

	// TimeFormat is fixed-width UTC, so stored timestamps sort lexically.
	TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"
)

// Code is a documented protocol error code.
type Code string

const (
	CodeInvalid            Code = "invalid_request"
	CodeBudgetTooSmall     Code = "budget_too_small"
	CodeInsufficientBudget Code = "insufficient_budget"
	CodeForbidden          Code = "forbidden"
	CodeOutOfScope         Code = "out_of_scope"
	CodeNotFound           Code = "not_found"
	CodeConflict           Code = "conflict"
	CodeCursorExpired      Code = "cursor_expired"
	CodePolicyDenied       Code = "policy_denied"
	CodeBusy               Code = "busy"
	CodeStorageFull        Code = "storage_full"
	CodeUnavailable        Code = "service_unavailable"
	CodeInternal           Code = "internal"
)

// ExitCode maps an error code to the CLI exit status documented in contract.md.
func ExitCode(c Code) int {
	switch c {
	case CodeInvalid, CodeBudgetTooSmall, CodeInsufficientBudget:
		return 2
	case CodeUnavailable:
		return 3
	case CodeForbidden, CodeOutOfScope, CodeNotFound, CodePolicyDenied:
		return 4
	case CodeConflict, CodeCursorExpired:
		return 5
	case CodeBusy, CodeStorageFull:
		return 6
	default:
		return 1
	}
}

// Error is a protocol error. Messages are fixed strings chosen by the core;
// they never contain request payloads or source content.
type Error struct {
	Code            Code   `json:"code"`
	Message         string `json:"message"`
	CurrentRevision int64  `json:"current_revision,omitempty"`
	MinBytes        int    `json:"min_bytes,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Errf builds a protocol error.
func Errf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Role is an authority granted by the socket and optionally narrowed by the caller.
type Role string

const (
	RoleReader  Role = "reader"
	RoleCapture Role = "capture"
	RoleAdmin   Role = "admin"
)

// Response is the envelope of every operation.
type Response struct {
	Contract string `json:"contract"`
	OK       bool   `json:"ok"`
	Op       string `json:"op"`
	Result   any    `json:"result,omitempty"`
	Error    *Error `json:"error,omitempty"`
}

// Trimmable results drop their lowest-priority unit and record the omission.
type Trimmable interface {
	Trim() bool
}

// Budget validates a requested max_bytes, applying the default for zero.
func Budget(n int) (int, error) {
	switch {
	case n == 0:
		return DefaultBudget, nil
	case n < MinBudget:
		return 0, &Error{Code: CodeBudgetTooSmall, Message: "max_bytes is below the minimum representable response", MinBytes: MinBudget}
	case n > MaxBudget:
		return 0, Errf(CodeInvalid, "max_bytes exceeds %d", MaxBudget)
	}
	return n, nil
}

// Marshal encodes v without HTML escaping, so byte counts match what clients print.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Encode serializes a successful response within maxBytes (0 = unbounded),
// trimming whole units from a Trimmable result until it fits.
func Encode(op string, result any, maxBytes int) ([]byte, error) {
	resp := Response{Contract: Version, OK: true, Op: op, Result: result}
	for {
		out, err := Marshal(resp)
		if err != nil {
			return nil, err
		}
		if maxBytes <= 0 || len(out) <= maxBytes {
			return out, nil
		}
		t, ok := result.(Trimmable)
		if !ok || !t.Trim() {
			return Marshal(Response{Contract: Version, OK: false, Op: op,
				Error: &Error{Code: CodeInsufficientBudget, Message: "result skeleton exceeds max_bytes", MinBytes: len(out)}})
		}
	}
}

// EncodeError serializes a failure.
func EncodeError(op string, e *Error) []byte {
	out, _ := Marshal(Response{Contract: Version, OK: false, Op: op, Error: e})
	return out
}

// Truncate cuts s to at most n bytes at a UTF-8 boundary.
func Truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Decode reads one JSON request strictly: unknown fields and trailing data are invalid.
func Decode(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Errf(CodeInvalid, "request is not valid JSON for this operation")
	}
	if dec.More() {
		return Errf(CodeInvalid, "trailing data after request")
	}
	return nil
}
