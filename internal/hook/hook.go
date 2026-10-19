// Package hook adapts native harness hook invocations to the service. It
// always exits successfully within its timeout so coding never stalls, and it
// reports failures visibly instead of claiming a durable capture.
package hook

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/server"
)

// Timeout bounds the whole hook round trip.
const Timeout = 3 * time.Second

const maxInput = 1 << 20

// quiet events are frequent; failures there are reported at the next start.
var quiet = map[string]bool{"Stop": true, "PostToolUse": true, "UserPromptSubmit": true}

// Run forwards one hook payload and writes the harness-native output.
func Run(ctx context.Context, client server.Client, harness string, in io.Reader, out io.Writer) {
	payload, err := io.ReadAll(io.LimitReader(in, maxInput+1))
	var native struct {
		Event string `json:"hook_event_name"`
	}
	_ = json.Unmarshal(payload, &native)
	if err != nil || len(payload) > maxInput || !json.Valid(payload) {
		emit(out, harness, native.Event, "", "Ichnora: hook input was unreadable or too large; nothing was recorded.")
		return
	}
	client.Timeout = Timeout
	raw, perr := client.Call(ctx, "hook", contract.HookRequest{Harness: harness, Payload: payload})
	if harness == "opencode" {
		// The plugin parses the contract response itself.
		if raw == nil {
			raw = contract.EncodeError("hook", perr)
		}
		_, _ = out.Write(raw)
		return
	}
	if perr != nil {
		msg := "Ichnora memory unavailable (" + string(perr.Code) + "); this event was not recorded."
		ctxText := ""
		if native.Event == "SessionStart" {
			ctxText = "Ichnora memory is unavailable (" + string(perr.Code) + "); no remembered context was loaded."
		}
		if !quiet[native.Event] {
			emit(out, harness, native.Event, ctxText, msg)
		}
		return
	}
	var resp struct {
		Result contract.HookResult `json:"result"`
	}
	_ = json.Unmarshal(raw, &resp)
	msg := ""
	if resp.Result.Paused && !quiet[native.Event] {
		msg = "Ichnora ingestion is paused at its storage limit; events will be reconciled after space is freed."
	}
	emit(out, harness, native.Event, resp.Result.Context, msg)
}

// emit writes Claude/Codex hook JSON. Only SessionStart carries context;
// both harnesses accept systemMessage on every event.
func emit(out io.Writer, harness, event, context, message string) {
	if harness == "opencode" {
		_, _ = out.Write(contract.EncodeError("hook", contract.Errf(contract.CodeInvalid, "%s", message)))
		return
	}
	resp := map[string]any{}
	if context != "" && event == "SessionStart" {
		resp["hookSpecificOutput"] = map[string]string{"hookEventName": "SessionStart", "additionalContext": context}
	}
	if message != "" {
		resp["systemMessage"] = message
	}
	if len(resp) == 0 {
		return
	}
	b, _ := contract.Marshal(resp)
	_, _ = out.Write(append(b, '\n'))
}
