package ingest

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Codex parses Codex rollout JSONL (`{"timestamp","type","payload"}` lines).
// Rollout lines carry no native event IDs, so identities are digests of the
// raw line: byte-identical lines in one session collapse into one event.
type Codex struct{}

func (Codex) Harness() string { return "codex" }
func (Codex) Version() string { return "codex-adapter/1" }

// Codex inserts instruction and environment blocks as user-role messages.
var codexInjectedPrefixes = []string{"<environment_context>", "<user_instructions>", "# AGENTS.md instructions", "<INSTRUCTIONS>"}

func (Codex) Parse(line []byte, st *State) ([]Event, error) {
	m, err := decode(line)
	if err != nil {
		return nil, err
	}
	digest := Digest(line)
	p := obj(m["payload"])
	base := Event{Session: st.Session, EventTime: Timestamp(m["timestamp"]), Cwd: st.Cwd,
		Version: st.Version, SourceDigest: digest, IDBasis: "fallback"}
	if base.EventTime == "" {
		base.Coverage = append(base.Coverage, "event_time")
	}
	one := func(i int, kind string) Event {
		e := base
		e.Kind = kind
		e.NativeID = digest
		if i > 0 {
			e.NativeID = digest + "#" + strconv.Itoa(i)
		}
		return e
	}
	switch str(m["type"]) {
	case "session_meta":
		st.Session = str(p["id"])
		if c := str(p["cwd"]); c != "" {
			st.Cwd = c
		}
		st.Version = str(p["cli_version"])
		e := one(0, KindSessionStart)
		e.Session, e.Cwd, e.Version = st.Session, st.Cwd, st.Version
		if src := p["source"]; src != nil {
			if s, ok := src.(string); ok && s == "subagent" || obj(src)["subagent"] != nil {
				e.Child = true
			}
		}
		return []Event{e}, nil
	case "turn_context":
		if c := str(p["cwd"]); c != "" {
			st.Cwd = c
		}
		return nil, nil
	case "compacted":
		e := one(0, KindCompaction)
		e.Derived = true
		return []Event{e}, nil
	case "event_msg":
		if str(p["type"]) == "context_compacted" {
			return []Event{one(0, KindCompaction)}, nil
		}
		// user_message/agent_message duplicate response items; counters are telemetry.
		return nil, nil
	case "response_item":
	default:
		return nil, nil
	}
	switch str(p["type"]) {
	case "message":
		role := str(p["role"])
		if role != "user" && role != "assistant" {
			return nil, nil
		}
		parts, _ := p["content"].([]any)
		var out []Event
		for i, raw := range parts {
			part := obj(raw)
			t := str(part["type"])
			if t != "input_text" && t != "output_text" && t != "text" {
				continue
			}
			kind := KindUser
			if role == "assistant" {
				kind = KindAssistant
			}
			e := one(i, kind)
			e.Text = str(part["text"])
			for _, prefix := range codexInjectedPrefixes {
				if strings.HasPrefix(strings.TrimSpace(e.Text), prefix) {
					e.Kind, e.Text = KindOther, ""
					e.Coverage = append(e.Coverage, "harness_injected_instructions")
				}
			}
			out = append(out, e)
		}
		return out, nil
	case "function_call", "custom_tool_call":
		e := one(0, KindToolCall)
		e.ToolName, e.CallID = str(p["name"]), str(p["call_id"])
		cmd := ""
		if args := str(p["arguments"]); args != "" {
			var a map[string]any
			if json.Unmarshal([]byte(args), &a) == nil {
				if c, ok := a["command"]; ok {
					cmd = commandName(c)
				} else if c, ok := a["cmd"]; ok {
					cmd = commandName(c)
				}
			}
		}
		e.Coverage = append(e.Coverage, "tool_arguments_excluded")
		e.Text = toolText(e.ToolName, cmd, "")
		return []Event{e}, nil
	case "function_call_output", "custom_tool_call_output":
		e := one(0, KindToolResult)
		e.CallID = str(p["call_id"])
		e.Outcome = codexOutcome(p["output"])
		e.Coverage = append(e.Coverage, "tool_output_excluded")
		e.Text = toolText("result", "", e.Outcome)
		return []Event{e}, nil
	}
	return nil, nil
}

func codexOutcome(output any) string {
	var data map[string]any
	switch o := output.(type) {
	case string:
		if json.Unmarshal([]byte(o), &data) != nil {
			return "unknown"
		}
	case map[string]any:
		data = o
	}
	meta := obj(data["metadata"])
	if code, ok := meta["exit_code"].(float64); ok {
		if code == 0 {
			return "succeeded"
		}
		return "failed"
	}
	if b, ok := data["success"].(bool); ok {
		if b {
			return "succeeded"
		}
		return "failed"
	}
	return "unknown"
}
