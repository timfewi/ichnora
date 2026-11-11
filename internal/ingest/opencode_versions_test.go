package ingest

import "testing"

func TestOpenCodeMessageVersionsPreserveNativeLineage(t *testing.T) {
	parse := func(text string) Event {
		t.Helper()
		line := []byte(`{"sessionID":"s","directory":"/fixture","message":{"id":"native-message","role":"user","text":"` + text + `"}}`)
		events, err := (OpenCode{}).Parse(line, &State{})
		if err != nil || len(events) != 1 {
			t.Fatalf("parse version: %v %+v", err, events)
		}
		return events[0]
	}
	first, updated, repeated := parse("draft"), parse("complete"), parse("complete")
	if first.NativeID == updated.NativeID || updated.NativeID != repeated.NativeID {
		t.Fatal("version identity must change with content and stay stable on retry")
	}
	if first.MessageKey == "" || first.MessageKey != updated.MessageKey || updated.FlagsOf().MessageKey != first.MessageKey {
		t.Fatal("streaming versions must retain their native independence key")
	}
}
