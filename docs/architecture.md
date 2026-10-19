# Architecture decision

Status: accepted for the initial delivery. Owner-fixed stack; this record
documents the routine choices made inside it and their reasons.

## Runtime and dependencies

| Component    | Pin                                             | Reason                                                                                                                                                                                                  |
| ------------ | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Go toolchain | 1.26.7 from the flake's nixpkgs pin             | `go.mod` declares `go 1.26.7`; builds set `GOTOOLCHAIN=local`, so no toolchain download happens in builds or at runtime.                                                                                |
| SQLite       | 3.53.3 from the same nixpkgs pin                | Compiled with `SQLITE_ENABLE_FTS5` and `SQLITE_SECURE_DELETE` (checked with `nix eval` of `sqlite.NIX_CFLAGS_COMPILE`). FTS5 is additionally probed at startup and in tests against the linked library. |
| Driver       | `github.com/mattn/go-sqlite3` v1.14.52          | `CGO_ENABLED=1`, build tag `libsqlite3`: links the Nix SQLite instead of compiling the bundled amalgamation. No second driver, no fallback.                                                             |
| MCP          | `github.com/modelcontextprotocol/go-sdk` v1.8.0 | Official SDK; requires Go 1.25, compatible with 1.26.7. Stdio transport for the per-session bridge.                                                                                                     |

Everything else is the standard library: `flag`, `encoding/json`, `net/http`
over `net.Listen("unix", …)`, `log/slog`, `database/sql`, `embed`. No ORM, web
framework, graph or vector service, model client or Node.js dependency.

## One executable

`ichnora` has subcommands for the service (`serve`), the user CLI, the MCP
bridge (`mcp`), harness hooks (`hook`) and offline maintenance (`restore`).
Domain types live in `internal/contract`; business logic in
`internal/core`; SQLite access in `internal/store`; source policy in
`internal/policy`; native-format adapters in `internal/ingest`; the socket
transport in `internal/server`; external evidence tools in `internal/evidence`.

## Service lifecycle and database ownership

The service owns the database. The CLI, hooks and MCP bridges are clients of
its Unix sockets and never open the database; a missing socket is reported as
`service_unavailable` with no direct or remote fallback. Persistent lifetime
belongs to systemd (host capability), not to an agent session.

- One writer goroutine consumes a bounded queue (default 64 operations). A full
  queue returns `busy` immediately; work is never dropped after acknowledgement.
  Write transactions are short and never span external tool or model calls.
- Readers use a separate read-only pool (default 4 connections).
- Every connection gets `foreign_keys=ON`, `busy_timeout=5000`, `secure_delete=ON`
  and a bounded page cache; the writer also sets `journal_mode=WAL` and
  `synchronous=FULL` so an acknowledged checkpoint is durable.
- Numbered SQL migrations are embedded with `go:embed` and applied in one
  transaction each whenever the database is opened. A database newer than the
  binary is refused.
- `serve` holds an exclusive lock file in the state directory. Offline
  `restore` takes the same lock, so it cannot race the service.
- Cursors and confirmation tokens bind a scope generation: the policy
  generation (enrollment, sharing) plus the generations of the caller's
  project and of projects sharing into it. Writes in an unrelated project do
  not change another caller's results or cursors.
- Backups use `VACUUM INTO` through the service (consistent snapshot, 0600).

`GOMEMLIMIT` is not set by the binary; the host cgroup enforces memory.

## Authority and scope enforcement

Three independent permissions come from the host-supplied configuration file
and the owner's administration commands:

1. **Workspace roots** (for example `~/code`) bound where projects may be enrolled.
   Enrollment is an explicit allowlist in the database; new projects start
   unenrolled and discovery only reports candidates.
2. **Session-source roots** per harness bound which transcript files the
   service may read. A path inside a root still needs an enrolled project with
   collection enabled, or an explicit import selection.
3. **State directory** (`$XDG_STATE_HOME/ichnora`, else `~/.local/state/ichnora`),
   directories 0700, files 0600, outside source trees and the Nix store.

Roles: `reader` (search/read/evidence/status), `capture` (claims, checkpoints,
task transitions, hook ingestion) and `admin` (enrollment, sharing, retention,
backup, environment records). The service listens on two sockets: the agent
socket grants `reader`+`capture`; the admin socket additionally grants `admin`.
A client may request a narrower role, never a wider one. The host projects only
the agent socket into sandboxes.

Caller scope is never a caller-supplied project name. The service reads the
peer's PID with `SO_PEERCRED` and resolves `/proc/PID/cwd` against enrolled
project roots and worktree mappings. The caller's scope is that project plus
claims explicitly shared into it. Requests may narrow scope (task, session,
worktree) but not widen it. Scope is applied in SQL before ranking, snippets,
counts and pagination.

Residual trust: under the operator UID any process can connect to both
sockets or change its working directory, so roles are application policy, not
isolation. Stronger enforcement needs the host's restricted runtime, which
limits visible paths and exposes only the agent socket.

## Ingestion adapters

Harness hooks call `ichnora hook <harness>`, which forwards the native hook
payload to the service within a short timeout and prints the harness's native
output. The service validates the transcript path against the session-source
roots, then reads the transcript incrementally from a durable cursor (bounded
bytes and lines per call). Missed callbacks are reconciled by the next hook or
an explicit `reconcile`; a clean `SessionEnd` is not required.

| Harness     | Pinned version | Native mechanism                                                                                                                                                                 |
| ----------- | -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claude Code | 2.1.302        | Command hooks; JSONL transcript referenced by `transcript_path`, subagent transcripts by `agent_transcript_path`.                                                                |
| Codex       | 0.163.0        | `[hooks]` in `config.toml` (stable `hooks` feature) with Claude-compatible inputs; rollout JSONL via `transcript_path` (nullable).                                               |
| OpenCode    | 2.0.32         | V2 plugin `session.hook("context" \| "compaction")`, executed by OpenCode's own runtime. The plugin forwards new messages to `ichnora hook opencode`; it contains no core logic. |

The binary inspection behind this table and the per-boundary coverage matrix
are in [adapters.md](adapters.md).

## Model boundary

The core performs no model calls. Ingestion stores bounded, permitted fields;
retrieval is deterministic FTS5 plus structured ranking. Any future
model-assisted extraction must be an explicit optional command that uses the
owner's configured provider channel, records its origin and stores output as
derived. Semantic search is deferred until a concrete lexical miss is shown.

## Evidence composition

Tabula, AST Index and agent-evidence keep their own runtimes. The service calls
them through their public CLIs with absolute executables from configuration,
bounded time and output, and outside write transactions. A missing tool yields a
`missing_adapter` outcome; Ichnora's own SHA-256 file digests still decide
applicability. AST row IDs are never stored as identities; references keep
file path, digest, symbol and location. The AST snapshot generation is kept
only as provenance: `ast-index` increments it on every refresh, even without
changes (observed 1 → 2 → 3 on a synthetic repository), so validity follows
the file digest. Tabula references keep card, section id, section fingerprint
and source path; a renamed or changed section is stale.

## Host environment boundary

The host may supply an optional, versioned environment descriptor
(`ichnora.environment/1`: system identity, policy baseline, harness versions,
activation epoch). Ichnora never derives an epoch from a generation number or
flake revision. A changed epoch makes only claims that declared an environment
dependency stale; project decisions, tasks and history are untouched, and a
rollback does not restore an older working context. Without a descriptor,
environment-dependent claims report unknown applicability. There is no reset
coordinator, per-activation store or session termination.

## Retention

Source records, derived claims, checkpoints and the FTS projection share one
SQLite file. Deletion is an explicit admin action with a dry-run preview and a
confirmation token bound to the policy and affected project generations. It writes tombstones that
ingestion replay respects, marks dependent claim support as lost and
recomputes their status, removes FTS rows, then truncates the WAL. With
`secure_delete` freed pages are zeroed, but copy-on-write filesystems, SSD
remapping, filesystem snapshots and earlier backups can still retain data;
physical erasure is not guaranteed. Automatic pruning is disabled until the
owner selects a policy. Withdrawing enrollment stops ingestion and recall and
keeps history; re-enrollment respects tombstones.
