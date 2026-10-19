package ingest

import "strconv"

// Claude parses Claude Code transcript JSONL (verified field names against the
// pinned 2.1.302 binary and synthetic fixtures; see docs/adapters.md).
type Claude struct{}

func (Claude) Harness() string { return "claude" }
func (Claude) Version() string { return "claude-adapter/1" }

func (Claude) Parse(line []byte, st *State) ([]Event, error) {
	m, err := decode(line)
	if err != nil {
		return nil, err
	}
	digest := Digest(line)
	base := Event{
		Session:      str(m["sessionId"]),
		EventTime:    Timestamp(m["timestamp"]),
		Cwd:          str(m["cwd"]),
		Version:      str(m["version"]),
		AgentID:      str(m["agentId"]),
		SourceDigest: digest,
	}
	if b, ok := m["isSidechain"].(bool); ok {
		base.Child = b
	}
	if base.Cwd != "" {
		st.Cwd = base.Cwd
	} else {
		base.Cwd = st.Cwd
	}
	if base.Session != "" {
		st.Session = base.Session
	}
	if base.Version != "" {
		st.Version = base.Version
	}
	if base.EventTime == "" {
		base.Coverage = append(base.Coverage, "event_time")
	}
	id := str(m["uuid"])
	basis := "native"
	if id == "" {
		id, basis = digest, "fallback"
	}
	make1 := func(i int, kind string) Event {
		e := base
		e.Kind, e.IDBasis = kind, basis
		e.NativeID = id
		if i > 0 {
			e.NativeID = id + "#" + strconv.Itoa(i)
		}
		return e
	}
	switch str(m["type"]) {
	case "system":
		if str(m["subtype"]) == "compact_boundary" {
			return []Event{make1(0, KindCompaction)}, nil
		}
		return nil, nil
	case "user":
		msg := obj(m["message"])
		if b, _ := m["isCompactSummary"].(bool); b {
			// The compaction summary is model-derived text, not user evidence.
			e := make1(0, KindCompaction)
			e.Derived = true
			return []Event{e}, nil
		}
		meta, _ := m["isMeta"].(bool)
		var out []Event
		switch c := msg["content"].(type) {
		case string:
			e := make1(0, KindUser)
			if meta {
				e.Kind = KindOther
			}
			e.Text = c
			out = append(out, e)
		case []any:
			for i, raw := range c {
				block := obj(raw)
				switch str(block["type"]) {
				case "text":
					e := make1(i, KindUser)
					if meta {
						e.Kind = KindOther
					}
					e.Text = str(block["text"])
					out = append(out, e)
				case "tool_result":
					e := make1(i, KindToolResult)
					e.CallID = str(block["tool_use_id"])
					e.Outcome = "succeeded"
					if b, _ := block["is_error"].(bool); b {
						e.Outcome = "failed"
					}
					e.Coverage = append(e.Coverage, "tool_output_excluded")
					e.Text = toolText("result", "", e.Outcome)
					out = append(out, e)
				default:
					e := make1(i, KindOther)
					e.Coverage = append(e.Coverage, "unsupported_block")
					out = append(out, e)
				}
			}
		}
		return out, nil
	case "assistant":
		msg := obj(m["message"])
		blocks, _ := msg["content"].([]any)
		var out []Event
		for i, raw := range blocks {
			block := obj(raw)
			switch str(block["type"]) {
			case "text":
				e := make1(i, KindAssistant)
				e.Text = str(block["text"])
				out = append(out, e)
			case "tool_use":
				e := make1(i, KindToolCall)
				e.ToolName = str(block["name"])
				e.CallID = str(block["id"])
				input := obj(block["input"])
				cmd := ""
				if e.ToolName == "Bash" {
					cmd = commandName(input["command"])
				}
				e.Coverage = append(e.Coverage, "tool_arguments_excluded")
				e.Text = toolText(e.ToolName, cmd, "")
				out = append(out, e)
			}
			// Thinking blocks are not stored; hidden reasoning is never claimed.
		}
		return out, nil
	}
	return nil, nil
}
