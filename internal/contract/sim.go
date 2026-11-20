package contract

type SimProposal struct {
	Cause      string `json:"cause,omitempty"`
	Change     string `json:"change,omitempty"`
	Prediction string `json:"prediction,omitempty"`
	Context    string `json:"context,omitempty"`
}

type SimRequest struct {
	Action  string `json:"action"`
	Problem string `json:"problem"`
	Attempt string `json:"attempt,omitempty"`
	SimProposal
	Outcome  string `json:"outcome,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
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
