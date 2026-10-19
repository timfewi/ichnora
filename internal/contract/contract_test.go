package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

// Trimming always terminates and the encoded response fits or is reported.
func TestEncodeTerminatesAndFits(t *testing.T) {
	res := &ResumeResult{NextAction: strings.Repeat("ß", 500), Authority: strings.Repeat("a", 900),
		Task: Task{Title: strings.Repeat("T", 200)}, Lines: make([]Line, 40)}
	out, err := Encode("resume", res, MinBudget)
	if err != nil || len(out) > MinBudget || !json.Valid(out) {
		t.Fatalf("encode: %d bytes, %v", len(out), err)
	}
	var env Response
	json.Unmarshal(out, &env)
	if !env.OK || res.Complete {
		t.Fatalf("expected a trimmed, incomplete success: %s", out)
	}
	if !strings.HasSuffix(res.NextAction, "…") || len(res.NextAction) != 99 {
		t.Fatalf("next action shortened to %d bytes", len(res.NextAction))
	}
}

func TestBudgetBounds(t *testing.T) {
	if n, err := Budget(0); err != nil || n != DefaultBudget {
		t.Fatal("default budget")
	}
	if _, err := Budget(MinBudget - 1); err == nil || err.(*Error).MinBytes != MinBudget {
		t.Fatal("below minimum must report the minimum")
	}
	if _, err := Budget(MaxBudget + 1); err == nil {
		t.Fatal("above maximum must be invalid")
	}
}
