package contract

import "encoding/json"

// SessionHint is an optional attribution hint read from the harness
// environment. It never grants scope; the service accepts it only for a known
// session already attributed to the caller's project.
type SessionHint struct {
	Harness  string `json:"harness"`
	NativeID string `json:"native_id"`
}

// Ref is a support reference or an applicability dependency.
type Ref struct {
	Kind        string `json:"kind"`
	Ref         string `json:"ref,omitempty" jsonschema:"record ID for event, turn, claim or checkpoint; name for check or external"`
	Path        string `json:"path,omitempty" jsonschema:"project-relative file for file, ast or tabula"`
	SHA256      string `json:"sha256,omitempty" jsonschema:"file digest; defaults to the current one"`
	Digest      string `json:"digest,omitempty"`
	Locator     string `json:"locator,omitempty"`
	Value       string `json:"value,omitempty" jsonschema:"commit for revision; defaults to the current HEAD"`
	Epoch       string `json:"epoch,omitempty" jsonschema:"environment epoch from status; defaults to the current one"`
	Card        string `json:"card,omitempty"`
	Section     string `json:"section,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Symbol      string `json:"symbol,omitempty"`
	Line        int    `json:"line,omitempty"`
	Snapshot    string `json:"snapshot,omitempty"`
}

// ScopeInfo describes the caller's resolved scope without naming other projects.
type ScopeInfo struct {
	Project   string `json:"project"`
	ProjectID string `json:"project_id"`
	Worktree  string `json:"worktree"`
}

// Provenance summarizes where a record came from.
type Provenance struct {
	Origin       string `json:"origin"`
	CreatedAt    string `json:"created_at"`
	EventTime    string `json:"event_time,omitempty"`
	ValidatedAt  string `json:"validated_at,omitempty"`
	Support      int    `json:"independent_support"`
	LostSupport  int    `json:"lost_support,omitempty"`
	SharedFrom   string `json:"shared_from,omitempty"`
	Derivation   string `json:"derivation,omitempty"`
	SourceDigest string `json:"source_digest,omitempty"`
}

// Item is one search result.
type Item struct {
	ID            string     `json:"id"`
	Group         string     `json:"group"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title,omitempty"`
	Preview       string     `json:"preview"`
	Truncated     bool       `json:"truncated,omitempty"`
	Status        string     `json:"status"`
	Applicability string     `json:"applicability"`
	Reasons       []string   `json:"reasons"`
	Provenance    Provenance `json:"provenance"`
	TaskID        string     `json:"task_id,omitempty"`
	ReplacedBy    string     `json:"replaced_by,omitempty"`
	ConflictsWith []string   `json:"conflicts_with,omitempty"`
	Neighbors     []string   `json:"neighbors,omitempty"`
}

// Totals preserve the existence of each group even when items are omitted.
type Totals struct {
	Working   int `json:"working"`
	Conflicts int `json:"conflicts"`
	Leads     int `json:"leads"`
	Unknown   int `json:"unknown"`
	Sources   int `json:"sources"`
	History   int `json:"history"`
}

// Omitted counts exclusions. It never names inaccessible projects.
type Omitted struct {
	Budget       int `json:"budget"`
	Page         int `json:"page"`
	Stale        int `json:"stale"`
	Replaced     int `json:"replaced"`
	Duplicates   int `json:"duplicates"`
	Unattributed int `json:"unattributed"`
}

// BudgetInfo labels the byte limit.
type BudgetInfo struct {
	MaxBytes int    `json:"max_bytes"`
	Unit     string `json:"unit"`
}

// SearchRequest is the `search` operation.
type SearchRequest struct {
	Query     string       `json:"query" jsonschema:"words to match; empty lists the project's claims"`
	View      string       `json:"view,omitempty" jsonschema:"working (default) holds validated claims and leads; history adds replaced, stale and expired ones"`
	TaskID    string       `json:"task_id,omitempty"`
	SessionID string       `json:"session_id,omitempty"`
	Kinds     []string     `json:"kinds,omitempty"`
	Files     []string     `json:"files,omitempty" jsonschema:"project-relative paths that rank related records first"`
	Since     string       `json:"since,omitempty" jsonschema:"RFC 3339"`
	Until     string       `json:"until,omitempty" jsonschema:"RFC 3339"`
	Limit     int          `json:"limit,omitempty" jsonschema:"1 to 50, default 10"`
	Cursor    string       `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
	MaxBytes  int          `json:"max_bytes,omitempty"`
	Session   *SessionHint `json:"session,omitempty" jsonschema:"attribution hint set by the client; omit"`
}

// SearchResult is the `search` result.
type SearchResult struct {
	Scope      ScopeInfo  `json:"scope"`
	View       string     `json:"view"`
	Items      []Item     `json:"items"`
	Totals     Totals     `json:"totals"`
	Omitted    Omitted    `json:"omitted"`
	Complete   bool       `json:"complete"`
	NextCursor string     `json:"next_cursor,omitempty"`
	Notice     string     `json:"notice"`
	Budget     BudgetInfo `json:"budget"`

	Start  int              `json:"-"`
	Cursor func(int) string `json:"-"`
}

// Trim drops the lowest-priority item; totals keep its existence visible.
func (r *SearchResult) Trim() bool {
	if len(r.Items) == 0 {
		return false
	}
	r.Items = r.Items[:len(r.Items)-1]
	r.Omitted.Budget++
	r.Complete = false
	if r.Cursor != nil {
		r.NextCursor = r.Cursor(r.Start + len(r.Items))
	}
	return true
}

// ReadRequest is the `read` operation.
type ReadRequest struct {
	IDs      []string `json:"ids"`
	MaxBytes int      `json:"max_bytes,omitempty"`
}

// Record is a detailed record returned by `read`.
type Record struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data"`
	Status        string          `json:"status,omitempty"`
	Applicability string          `json:"applicability,omitempty"`
	Reasons       []string        `json:"reasons,omitempty"`
}

// ReadResult is the `read` result.
type ReadResult struct {
	Records  []Record   `json:"records"`
	Missing  []string   `json:"missing"`
	Omitted  int        `json:"omitted_budget"`
	Complete bool       `json:"complete"`
	Notice   string     `json:"notice"`
	Budget   BudgetInfo `json:"budget"`
}

func (r *ReadResult) Trim() bool {
	if len(r.Records) == 0 {
		return false
	}
	r.Records = r.Records[:len(r.Records)-1]
	r.Omitted++
	r.Complete = false
	return true
}

// EvidenceRequest is the `evidence` operation.
type EvidenceRequest struct {
	ID           string `json:"id" jsonschema:"claim ID"`
	ExcerptLines int    `json:"excerpt_lines,omitempty" jsonschema:"up to 40"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
}

// SupportCheck is one validated support reference or dependency.
type SupportCheck struct {
	Role       string          `json:"role"`
	Kind       string          `json:"kind"`
	Ref        string          `json:"ref"`
	Stored     json.RawMessage `json:"stored"`
	State      string          `json:"state"`
	Validation string          `json:"validation"`
	Current    string          `json:"current,omitempty"`
	Adapter    string          `json:"adapter,omitempty"`
	Excerpt    json.RawMessage `json:"excerpt,omitempty"`
}

// EvidenceResult is the `evidence` result.
type EvidenceResult struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"`
	Applicability string         `json:"applicability"`
	Checks        []SupportCheck `json:"checks"`
	Gaps          []string       `json:"gaps"`
	GapsTotal     int            `json:"gaps_total"`
	Omitted       int            `json:"omitted_budget"`
	Complete      bool           `json:"complete"`
	Notice        string         `json:"notice"`
	Budget        BudgetInfo     `json:"budget"`
}

// Trim drops excerpts, then checks, then gap texts; gaps_total keeps their count.
func (r *EvidenceResult) Trim() bool {
	switch {
	case len(r.Checks) > 0:
		last := &r.Checks[len(r.Checks)-1]
		if last.Excerpt != nil {
			last.Excerpt = nil
		} else {
			r.Checks = r.Checks[:len(r.Checks)-1]
		}
	case len(r.Gaps) > 0:
		r.Gaps = r.Gaps[:len(r.Gaps)-1]
	default:
		return false
	}
	r.Omitted++
	r.Complete = false
	return true
}

// ClaimInput is one claim to capture.
type ClaimInput struct {
	Kind         string   `json:"kind"`
	Title        string   `json:"title" jsonschema:"the claim in one line, up to 200 bytes"`
	Body         string   `json:"body,omitempty"`
	Rationale    string   `json:"rationale,omitempty" jsonschema:"why, for a decision"`
	Alternatives []string `json:"alternatives,omitempty" jsonschema:"rejected options"`
	TaskID       string   `json:"task_id,omitempty" jsonschema:"task whose resume should include this claim"`
	Status       string   `json:"status,omitempty"`
	Support      []Ref    `json:"support,omitempty" jsonschema:"evidence the claim rests on"`
	Depends      []Ref    `json:"depends,omitempty" jsonschema:"what must stay unchanged for the claim to apply; a claim without it is an unvalidated lead unless a decision sets expires_at"`
	Supersedes   []string `json:"supersedes,omitempty" jsonschema:"IDs of claims this one replaces"`
	Contradicts  []string `json:"contradicts,omitempty" jsonschema:"IDs of claims this one conflicts with"`
	DerivedFrom  []string `json:"derived_from,omitempty" jsonschema:"source claim IDs of a summary"`
	Transform    string   `json:"transform,omitempty"`
	EventTime    string   `json:"event_time,omitempty" jsonschema:"RFC 3339 time of the observation"`
	ExpiresAt    string   `json:"expires_at,omitempty" jsonschema:"RFC 3339; keeps a decision without depends valid until then"`
}

// CaptureRequest is the `capture` operation.
type CaptureRequest struct {
	Claims  []ClaimInput `json:"claims"`
	Session *SessionHint `json:"session,omitempty" jsonschema:"attribution hint set by the client; omit"`
}

// CaptureResult lists the new claim IDs in request order.
type CaptureResult struct {
	IDs     []string `json:"ids"`
	Durable bool     `json:"durable"`
}

// UpdateRequest is the `update` operation (dispute or invalidate a claim).
type UpdateRequest struct {
	ID     string `json:"id" jsonschema:"claim ID, or an event or turn ID to invalidate a source"`
	Status string `json:"status"`
	Reason string `json:"reason" jsonschema:"required, up to 500 bytes"`
}

// UpdateResult reports the new status and recomputed dependants.
type UpdateResult struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Recomputed []string `json:"recomputed"`
}

// Verification records a check run with its exact tested inputs.
type Verification struct {
	Check   string            `json:"check" jsonschema:"command or check name"`
	Outcome string            `json:"outcome"`
	Inputs  VerificationInput `json:"inputs"`
	At      string            `json:"at,omitempty"`
}

// VerificationInput identifies what a check ran against.
type VerificationInput struct {
	Revision string            `json:"revision,omitempty" jsonschema:"commit the check ran against"`
	Files    map[string]string `json:"files,omitempty" jsonschema:"project-relative path to the SHA-256 the check ran against"`
	Epoch    string            `json:"epoch,omitempty" jsonschema:"environment epoch the check ran against"`
}

// Checkpoint holds the structured state of a task.
type Checkpoint struct {
	Objective    string         `json:"objective,omitempty"`
	Constraints  []string       `json:"constraints,omitempty"`
	Completed    []string       `json:"completed,omitempty"`
	Remaining    []string       `json:"remaining,omitempty"`
	Blockers     []string       `json:"blockers,omitempty"`
	NextAction   string         `json:"next_action,omitempty"`
	Decisions    []string       `json:"decisions,omitempty" jsonschema:"claim IDs"`
	Verification []Verification `json:"verification,omitempty" jsonschema:"check results with the inputs they ran against"`
}

// TaskRequest is the `task` operation.
type TaskRequest struct {
	Action           string       `json:"action"`
	TaskID           string       `json:"task_id,omitempty" jsonschema:"required except for create and list"`
	Title            string       `json:"title,omitempty" jsonschema:"create"`
	ProgressRef      string       `json:"progress_ref,omitempty" jsonschema:"create: project-relative progress file that stays authoritative"`
	ExpectedRevision int64        `json:"expected_revision,omitempty" jsonschema:"checkpoint and transition: the task's current revision"`
	To               string       `json:"to,omitempty" jsonschema:"transition: the new state"`
	Reason           string       `json:"reason,omitempty" jsonschema:"transition: required"`
	Checkpoint       *Checkpoint  `json:"checkpoint,omitempty" jsonschema:"create and checkpoint: given lists and non-empty strings replace the stored ones"`
	Session          *SessionHint `json:"session,omitempty" jsonschema:"attribution hint set by the client; omit"`
}

// Task is a task summary.
type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Revision    int64  `json:"revision"`
	Worktree    string `json:"worktree,omitempty"`
	UpdatedAt   string `json:"updated_at"`
	ProgressRef string `json:"progress_ref,omitempty"`
}

// TaskResult is the `task` result.
type TaskResult struct {
	Task         *Task  `json:"task,omitempty"`
	Tasks        []Task `json:"tasks,omitempty"`
	Accepted     *bool  `json:"accepted,omitempty"`
	Durable      bool   `json:"durable"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

// ResumeRequest is the `resume` operation.
type ResumeRequest struct {
	TaskID   string       `json:"task_id"`
	MaxBytes int          `json:"max_bytes,omitempty"`
	Session  *SessionHint `json:"session,omitempty"`
}

// Line is one entry in the working context.
type Line struct {
	Section string   `json:"section"`
	Text    string   `json:"text"`
	Ref     string   `json:"ref,omitempty"`
	State   string   `json:"state,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

// ResumeResult is the freshly built working context of one task.
type ResumeResult struct {
	Scope      ScopeInfo      `json:"scope"`
	Task       Task           `json:"task"`
	NextAction string         `json:"next_action"`
	Authority  string         `json:"authority"`
	Lines      []Line         `json:"lines"`
	Totals     map[string]int `json:"totals"`
	Omitted    int            `json:"omitted_budget"`
	Complete   bool           `json:"complete"`
	Notice     string         `json:"notice"`
	Budget     BudgetInfo     `json:"budget"`
}

// Trim drops the lowest-priority line, then shortens the free-text header
// fields; totals keep every section's existence visible.
func (r *ResumeResult) Trim() bool {
	const keep, ellipsis = 96, "…"
	shorten := func(s *string) bool {
		if len(*s) <= keep+len(ellipsis) {
			return false
		}
		*s, _ = Truncate(*s, keep)
		*s += ellipsis
		return true
	}
	switch {
	case len(r.Lines) > 0:
		r.Lines = r.Lines[:len(r.Lines)-1]
	case shorten(&r.NextAction), shorten(&r.Authority), shorten(&r.Task.Title):
	default:
		return false
	}
	r.Omitted++
	r.Complete = false
	return true
}

// HookRequest carries a native hook payload to the service.
type HookRequest struct {
	Harness string          `json:"harness"`
	Payload json.RawMessage `json:"payload"`
}

// HookResult reports what was recorded and the optional context to inject.
type HookResult struct {
	Event    string `json:"event"`
	Recorded bool   `json:"recorded"`
	Ingested int    `json:"ingested"`
	Skipped  string `json:"skipped,omitempty"`
	Context  string `json:"context,omitempty"`
	Paused   bool   `json:"ingestion_paused,omitempty"`
	// AcceptedMessages acknowledges the entire committed OpenCode batch,
	// including replayed messages and explicitly recorded coverage omissions.
	AcceptedMessages int `json:"accepted_messages"`
	OmittedMessages  int `json:"omitted_messages,omitempty"`
}

// StatusRequest is the `status` operation.
type StatusRequest struct {
	MaxBytes int `json:"max_bytes,omitempty"`
}

// AdminRequest covers the owner-facing operations.
type AdminRequest struct {
	Path       string   `json:"path,omitempty"`
	Name       string   `json:"name,omitempty"`
	WorktreeOf string   `json:"worktree_of,omitempty"`
	Collect    bool     `json:"collect,omitempty"`
	Recall     bool     `json:"recall,omitempty"`
	Project    string   `json:"project,omitempty"`
	ClaimID    string   `json:"claim_id,omitempty"`
	ToProject  string   `json:"to_project,omitempty"`
	ShareID    string   `json:"share_id,omitempty"`
	Selector   string   `json:"selector,omitempty"`
	Target     string   `json:"target,omitempty"`
	Confirm    string   `json:"confirm,omitempty"`
	Harness    string   `json:"harness,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	Out        string   `json:"out,omitempty"`
}
