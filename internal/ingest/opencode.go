package ingest

import (
	"encoding/json"
	"strconv"
)

// OpenCode parses message records forwarded by the thin OpenCode plugin or
// taken from an explicit v2 session export. One line is
// `{"sessionID","parentID","directory","version","message":{…}}`.
type OpenCode struct{}

func (OpenCode) Harness() string { return "opencode" }
func (OpenCode) Version() string { return "opencode-adapter/1" }

// Line wraps one OpenCode message for the line parser.
type OpenCodeLine struct {
	SessionID string          `json:"sessionID"`
	ParentID  string          `json:"parentID,omitempty"`
	Directory string          `json:"directory,omitempty"`
	Version   string          `json:"version,omitempty"`
	Message   json.RawMessage `json:"message"`
}

func (OpenCode) Parse(line []byte, st *State) ([]Event, error) {
	m, err := decode(line)
	if err != nil {
		return nil, err
	}
	msg := obj(m["message"])
	if msg == nil {
		return nil, errMalformed
	}
	if s := str(m["sessionID"]); s != "" {
		st.Session = s
	}
	if d := str(m["directory"]); d != "" {
		st.Cwd = d
	}
	if v := str(m["version"]); v != "" {
		st.Version = v
	}
	digest := Digest(line)
	id, basis := str(msg["id"]), "native"
	if id == "" {
		id, basis = digest, "fallback"
	}
	base := Event{Session: st.Session, Cwd: st.Cwd, Version: st.Version, SourceDigest: digest, IDBasis: basis,
		Child: str(m["parentID"]) != ""}
	if t := obj(msg["time"]); t != nil {
		base.EventTime = Timestamp(t["created"])
	} else {
		base.EventTime = Timestamp(msg["time"])
	}
	if base.EventTime == "" {
		base.Coverage = append(base.Coverage, "event_time")
	}
	one := func(i int, kind string) Event {
		e := base
		e.Kind = kind
		e.NativeID = id
		if i > 0 {
			e.NativeID = id + "#" + strconv.Itoa(i)
		}
		return e
	}
	role := str(msg["role"])
	if role == "" {
		role = str(msg["type"])
	}
	parts, _ := msg["content"].([]any)
	if parts == nil {
		parts, _ = msg["parts"].([]any)
	}
	switch role {
	case "user":
		if t := str(msg["text"]); t != "" {
			e := one(0, KindUser)
			e.Text = t
			return []Event{e}, nil
		}
		var out []Event
		for i, raw := range parts {
			part := obj(raw)
			if str(part["type"]) == "text" {
				e := one(i+1, KindUser)
				e.Text = str(part["text"])
				out = append(out, e)
			}
		}
		return out, nil
	case "assistant":
		var out []Event
		for i, raw := range parts {
			part := obj(raw)
			switch str(part["type"]) {
			case "text":
				e := one(i+1, KindAssistant)
				e.Text = str(part["text"])
				out = append(out, e)
			case "tool":
				e := one(i+1, KindToolCall)
				e.ToolName, e.CallID = str(part["name"]), str(part["id"])
				if e.ToolName == "" {
					e.ToolName = str(part["tool"])
				}
				state := obj(part["state"])
				cmd := ""
				if e.ToolName == "bash" || e.ToolName == "shell" {
					cmd = commandName(obj(state["input"])["command"])
				}
				switch str(state["status"]) {
				case "completed":
					e.Outcome = "succeeded"
				case "error":
					e.Outcome = "failed"
				case "cancelled":
					e.Outcome = "cancelled"
				}
				e.Coverage = append(e.Coverage, "tool_arguments_excluded", "tool_output_excluded")
				e.Text = toolText(e.ToolName, cmd, e.Outcome)
				out = append(out, e)
			}
		}
		return out, nil
	case "compaction", "summary":
		e := one(0, KindCompaction)
		e.Derived = true
		return []Event{e}, nil
	}
	return nil, nil
}
