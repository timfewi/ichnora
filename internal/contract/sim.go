package contract

type SimProposal struct {
	Cause      string `json:"cause,omitempty" jsonschema:"propose: suspected root cause"`
	Change     string `json:"change,omitempty" jsonschema:"propose: the intervention"`
	Prediction string `json:"prediction,omitempty" jsonschema:"propose: expected result, recorded before the trial"`
	Context    string `json:"context,omitempty" jsonschema:"propose: how the trial is evaluated"`
}

type SimRequest struct {
	Action  string `json:"action"`
	Problem string `json:"problem" jsonschema:"stable key of the recurring problem"`
	Attempt string `json:"attempt,omitempty" jsonschema:"propose and observe: a new key for each trial"`
	SimProposal
	Outcome  string `json:"outcome,omitempty" jsonschema:"observe: what happened"`
	Evidence string `json:"evidence,omitempty" jsonschema:"observe: project-relative result file"`
	Offset   int    `json:"offset,omitempty" jsonschema:"history: next_offset of the previous page"`
	Limit    int    `json:"limit,omitempty" jsonschema:"history: 1 to 50, default 10"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type SimAttempt struct {
	Attempt string `json:"attempt"`
	SimProposal
	ProposedAt       string `json:"proposed_at"`
	Outcome          string `json:"outcome,omitempty"`
	Evidence         string `json:"evidence,omitempty"`
	EvidenceSHA256   string `json:"evidence_sha256,omitempty"`
	EvidenceWorktree string `json:"evidence_worktree,omitempty"`
	ObservedAt       string `json:"observed_at,omitempty"`
}

type SimHistory struct {
	Problem    string       `json:"problem"`
	Attempts   []SimAttempt `json:"attempts"`
	Offset     int          `json:"offset"`
	NextOffset int          `json:"next_offset,omitempty"`
}

func (h *SimHistory) Trim() bool {
	if len(h.Attempts) <= 1 {
		return false
	}
	h.Attempts = h.Attempts[:len(h.Attempts)-1]
	h.NextOffset = h.Offset + len(h.Attempts)
	return true
}
