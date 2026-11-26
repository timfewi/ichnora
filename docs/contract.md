# Contract `ichnora/1`

One core implements this contract. The service exposes it as `POST /v1/<op>`
with a JSON body on its Unix sockets; the CLI and MCP bridge are clients.
Additive fields keep the version; removing or changing a field's meaning
requires `ichnora/2`.

## Envelope and errors

Success: `{"contract":"ichnora/1","ok":true,"op":"<op>","result":{…}}`.
Failure: `{"contract":"ichnora/1","ok":false,"op":"<op>","error":{"code":"…","message":"…"}}`.
Messages are fixed, bounded strings and never echo request payloads, paths of
other projects or source content.

| Code                  | Meaning                                                                                                                          | CLI exit |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------- | -------- |
| `invalid_request`     | Malformed JSON, unknown field, invalid value or limit.                                                                           | 2        |
| `budget_too_small`    | `max_bytes` below the declared minimum (1024).                                                                                   | 2        |
| `insufficient_budget` | A result skeleton did not fit an accepted budget (prevented by the bounds below).                                                | 2        |
| `forbidden`           | Role lacks the operation (for example admin over the agent socket).                                                              | 4        |
| `out_of_scope`        | No enrolled project for the caller, or the ID is outside the caller's scope. Indistinguishable from `not_found` for foreign IDs. | 4        |
| `not_found`           | Unknown ID within the caller's scope.                                                                                            | 4        |
| `conflict`            | Revision precondition failed; the response names the current revision.                                                           | 5        |
| `cursor_expired`      | Cursor scope, generation or query no longer matches.                                                                             | 5        |
| `policy_denied`       | A path, transcript or event violates the source policy.                                                                          | 4        |
| `busy`                | Writer queue full or lock wait exceeded; retry later.                                                                            | 6        |
| `storage_full`        | Storage limit reached; ingestion is paused, nothing acknowledged was dropped.                                                    | 6        |
| `service_unavailable` | Client could not reach the socket (client-side only).                                                                            | 3        |
| `internal`            | Unexpected failure; details only in the service log, without payloads.                                                           | 1        |

## Output budget

Every read-family response (`search`, `read`, `evidence`, `resume`,
`status`) accepts `max_bytes` (default 8192, minimum 1024, maximum 262144).
Omitting the field or sending zero requests the default. The MCP tool
`memory_search` advertises these bounds in its input schema. An invalid
budget sent to that tool produces an MCP tool validation error before the
bridge contacts the service. Socket clients receive the service error above.

The limit covers the complete serialized UTF-8 response, including envelope,
provenance, cursor and omissions; `budget.unit` says `UTF-8 bytes`. The result
skeleton (scope, totals of conflicts, leads, unknown applicability, sources and
history, omission counts, next cursor) is bounded to fit the minimum; tests
check the worst case with a 64-byte project name. Whole items are then kept in
priority order (conflicts, working results, leads, sources, history) and the
rest are omitted and counted. Previews are truncated at a UTF-8 boundary to at
most 280 bytes and flagged. As a last resort `resume` shortens its free-text
header fields to 96 bytes with an ellipsis. `complete` is `false` whenever
anything was omitted. Byte counts are bytes, not tokens; no token estimate is
reported. `insufficient_budget` is reserved for a skeleton that still does not
fit, which the bounds above are designed to prevent. Improvement history also uses
this error when its first complete attempt exceeds the requested budget.

## Identifiers and cursors

`prj_`, `clm_`, `tsk_`, `ckp_`, `evt_`, `trn_`, `ses_`, `shr_` prefixes. Event,
turn and session IDs derive deterministically from native identity, so retries
are idempotent. Every read re-checks the record's project against the caller's
current scope; revocation or withdrawal makes previously returned IDs
`out_of_scope`. Cursors are HMAC-signed with a per-state secret and bind the
scope digest, database generation, query digest, evaluated result order and
offset; a relevant write, revocation or policy change expires them. A source-file
edit, environment change or expiry that changes result membership or ordering
also expires the cursor, even when the database has not been written.

## Operations

Roles: R = reader, C = capture, A = admin.

| Op                                      | Role | Request                                                                                                                                                                                                                                 | Result                                                                                                                                                             |
| --------------------------------------- | ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `search`                                | R    | `query`, `view` (`working` default, `history`, `all`), `task_id`, `session_id`, `kinds`, `files`, `since`, `until`, `limit` (≤50), `cursor`, `max_bytes`                                                                                | `scope`, `items[]` (`id`, `group`, `kind`, `title`, `preview`, `status`, `applicability`, `reasons`, `provenance`), `totals`, `omitted`, `complete`, `next_cursor` |
| `read`                                  | R    | `ids` (≤20), `max_bytes`                                                                                                                                                                                                                | `records[]` with full fields, support refs and links; `missing[]`                                                                                                  |
| `evidence`                              | R    | `id` (claim), `max_bytes`, `excerpt_lines` (≤40)                                                                                                                                                                                        | `support[]` with stored digest, current validation and optional excerpt; `gaps[]`                                                                                  |
| `capture`                               | C    | `claims[]` (≤20): `kind`, `title`, `body`, `rationale`, `alternatives`, `task_id`, `status` (`observed`/`inferred`), `support[]`, `depends[]`, `supersedes[]`, `contradicts[]`, `expires_at`, `derived_from[]`, `transform`             | `ids[]`                                                                                                                                                            |
| `update`                                | C    | `id`, `status` (`disputed`, `invalidated`), `reason`                                                                                                                                                                                    | claim status and recomputed dependants                                                                                                                             |
| `task`                                  | C    | `action`: `create` (`title`, `progress_ref`, optional `checkpoint` fields such as `objective`), `checkpoint` (`task_id`, `expected_revision`, checkpoint fields), `transition` (`task_id`, `expected_revision`, `to`, `reason`), `list` | task with `revision`                                                                                                                                               |
| `resume`                                | R    | `task_id`, `max_bytes`                                                                                                                                                                                                                  | working context: objective, constraints, current decisions, remaining work, blockers, applicable checks, next action, child conclusions, conflicts, gaps, refs     |
| `status`                                | R    | `max_bytes`                                                                                                                                                                                                                             | service health, scope, adapter coverage, cursors, gaps, storage, environment                                                                                       |
| `hook`                                  | C    | `harness`, `payload` (native hook JSON)                                                                                                                                                                                                 | `recorded`, `ingested`, `context` (optional text for the harness)                                                                                                  |
| `projects`                              | A    | —                                                                                                                                                                                                                                       | enrolled projects and roots                                                                                                                                        |
| `discover`                              | A    | —                                                                                                                                                                                                                                       | candidate repositories (not read)                                                                                                                                  |
| `enroll`                                | A    | `path`, `name`, `worktree_of`, `collect`, `recall`                                                                                                                                                                                      | project                                                                                                                                                            |
| `withdraw`                              | A    | `project`                                                                                                                                                                                                                               | project                                                                                                                                                            |
| `policy`                                | A    | `path`                                                                                                                                                                                                                                  | decision and reason                                                                                                                                                |
| `share` / `revoke`                      | A    | `claim_id`, `to_project` / `share_id`                                                                                                                                                                                                   | share                                                                                                                                                              |
| `retention_preview` / `retention_apply` | A    | `selector` (`claim`, `session`, `project`), `confirm`                                                                                                                                                                                   | affected counts, `confirm` token                                                                                                                                   |
| `import_preview` / `import_apply`       | A    | `harness`, `paths[]`, `confirm`                                                                                                                                                                                                         | sessions and their attribution                                                                                                                                     |
| `reconcile`                             | A    | —                                                                                                                                                                                                                                       | cursors advanced                                                                                                                                                   |
| `backup`                                | A    | `out`                                                                                                                                                                                                                                   | path and size                                                                                                                                                      |
| `environment`                           | A    | —                                                                                                                                                                                                                                       | current descriptor and epoch                                                                                                                                       |

The MCP tools describe these fields in their input schemas and advertise the
accepted values of `view`, `kinds`, claim `kind` and `status`, reference
`kind`, update `status`, task `action` and `to`, verification `outcome` and the
improvement `action`. Another value produces an MCP tool validation error
before the bridge contacts the service; an optional field may still be sent
empty. Socket clients receive `invalid_request`.

### Improvement attempts

`POST /v1/sim` takes `action`, `problem` and the action-specific fields below.
Problem and attempt identifiers start with an ASCII letter or digit and use
up to 80 ASCII letters, digits, periods, underscores or hyphens. The service
derives the owning project from the caller. Improvement history is project-local.
The CLI exposes this operation as `ichnora sim`, MCP as `memory_sim`.

- `propose` requires capture authority, an `attempt` key and nonempty `cause`,
  `change`, `prediction` and `context` fields (each up to 1200 bytes).
- `observe` requires capture authority and an existing `attempt`, `outcome`
  (up to 1200 bytes) and `evidence` (a permitted project-relative file).
  The service binds the observation to the file digest and worktree at capture.
  Both write operations return the problem and attempt keys. Replays with the
  same inputs succeed. Conflicting proposals or observations return `conflict`.
- `history` requires reader authority. Results include `attempts` in proposal
  order and the original evaluation context. An omitted outcome is pending.
  Historical results describe the observed run. Check the stored file digest
  before using the result as evidence for a new change. `limit` defaults to
  10 and permits up to 50. Pass the returned `next_offset` as `offset` to
  continue. Histories preserve whole attempts within `max_bytes`; a single
  attempt larger than the budget returns `insufficient_budget`.

### Search traversal

An empty `query` returns the project claim index. A nonempty query must contain
at least one searchable word after normalization; whitespace-only, symbol-only
and overlong-word-only queries return `invalid_request` instead of being passed
to FTS5. A `task_id` filter applies to claims and to source turns whose session
is assigned to that task. Unassigned sessions do not match a task filter.

Scope, kind, task, session and time filters are applied in SQL. Matching records
are fetched in batches of 500 within one read snapshot, then classified and
ranked before pagination. The batch size is not a total-result cutoff: old
claims, including decisions and conflicts, remain eligible. Totals describe the
full ranked result set, and cursors can traverse it beyond the first batch.

A request examining more than 10,000 matching records fails explicitly with
`invalid_request`; narrow the query or filters. It never returns an apparently
complete subset from that examination limit. Task resume uses the same explicit
bound for its linked claims rather than silently retaining the first 200.
Response item limits and `max_bytes` apply independently after ranking.

### Claim fields

`status` is the evidence status: `observed`, `inferred`, `disputed`,
`superseded`, `invalidated`, `unsupported` (no remaining support). It is
separate from `applicability`, computed at read time: `current` (all declared
dependencies validated now), `durable` (decision without dependencies but
with an unexpired `expires_at`), `unchecked` (observation without
dependencies, or decision with neither dependencies nor expiry), `stale` (a
dependency changed),
`expired`, `unknown` (a dependency cannot be validated now).

The working view contains only `observed`/`inferred` claims whose
applicability is `current` or `durable`. `unchecked`, `unknown` and
`unsupported` claims appear only as `lead` items. Disputed claims and
unresolved `contradicts` links appear as `conflict` items in every view. The
`history` view adds superseded, invalidated, expired and stale claims with
their replacement links.

Support references (`support[]`): `{"kind":"event"|"turn"|"claim"|"checkpoint"|"file"|"tabula"|"ast"|"check"|"external","ref":"…","digest":"sha256:…","locator":"…"}`.
Dependencies (`depends[]`): `{"kind":"file","path":"rel/path","sha256":"…"}`,
`{"kind":"revision","value":"<commit>"}`, `{"kind":"environment","epoch":"…"}`,
`{"kind":"tabula","card":"…","section":"…","fingerprint":"…"}`,
`{"kind":"ast","path":"…","sha256":"…","symbol":"…","line":N}`.
Support from injected memory or from claims sharing a derivation lineage counts
once. A summary (`derived_from`) is never independent corroboration.

### Task checkpoint fields

`objective`, `constraints[]`, `completed[]`, `remaining[]`, `blockers[]`,
`next_action`, `decisions[]` (claim IDs), `verification[]`
(`{"check":"…","outcome":"passed"|"failed"|"skipped","inputs":{"revision":"…","files":{"path":"sha256"}},"at":"…"}`).
States: `active`, `blocked`, `completed`, `cancelled`, `reopened`. Only an
explicit `transition` changes state; ingestion never does. A checkpoint from a
child session is stored as an unaccepted child conclusion of the parent task.

## Ingestion envelope `ichnora.event/1`

| Field                                             | Notes                                                                                                                                                |
| ------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `harness`, `adapter_version`                      | `claude`, `codex`, `opencode`; adapter semantic version.                                                                                             |
| `native_id`, `id_basis`                           | Native event ID, or `fallback`: SHA-256 of session ID, line digest and occurrence.                                                                   |
| `session`, `parent_session`, `child`              | Native session IDs; child sessions keep their own identity and parent link.                                                                          |
| `project_id`, `worktree_id`, `task_id`            | Attribution per event from its recorded working directory; unresolved stays empty and is reported as a gap.                                          |
| `kind`                                            | `user`, `assistant`, `tool_call`, `tool_result`, `compaction`, `session_start`, `resume`, `session_end`, `subagent_start`, `subagent_stop`, `other`. |
| `seq`, `event_time`, `ingested_at`                | Native order and time when known, else null; never fabricated.                                                                                       |
| `source`, `source_digest`                         | Transcript locator (path digest and byte offset, or plugin message ID) and SHA-256 of the raw line.                                                  |
| `coverage`, `redactions`, `truncated`, `injected` | Flags for missing fields, redacted spans, truncated text and Ichnora-injected context.                                                               |

Stored fields: user and assistant text (≤4096 bytes each, credential patterns
redacted), tool name, call ID, outcome and literal command name. Tool arguments
and tool outputs are excluded. Hidden reasoning is never available. Events are
grouped into turns (≤16384 bytes) that start at a user message; turns are the
FTS5 projection and carry neighbouring turn IDs.

## Limits

| Limit                                 | Default                                      |
| ------------------------------------- | -------------------------------------------- |
| Request body                          | 1 MiB                                        |
| Transcript bytes per ingestion call   | 4 MiB                                        |
| Transcript line                       | 1 MiB (longer lines are skipped and counted) |
| Writer queue                          | 64 operations                                |
| Reader connections                    | 4                                            |
| SQLite busy timeout                   | 5 s                                          |
| Page cache per connection             | 8 MiB                                        |
| Database size before ingestion pauses | 1 GiB (`max_db_bytes`)                       |
| Capture reserve beyond that           | 32 MiB, then `storage_full`                  |
| Hook client timeout                   | 3 s                                          |
| External tool timeout / output        | 5 s / 256 KiB                                |
