# ichnora

Format: tabula/1. Source labels are repository-relative; links resolve from this card.
Purpose: a local, project-scoped memory with provenance for enrolled Codex,
Claude Code and OpenCode sessions. It keeps source history, derived claims and
task checkpoints in SQLite and builds bounded, freshly validated working
context. The host owns activation, sockets, resources and credentials.

## Architecture

One executable, [cmd/ichnora](../cmd/ichnora/main.go): `serve` owns the
database ([serve.go](../cmd/ichnora/serve.go)); the CLI, `hook` and `mcp` are
clients of its Unix sockets ([server](../internal/server/server.go)). Peer
credentials and `/proc/PID/cwd` decide scope; the agent socket grants
reader+capture, the admin socket adds owner administration.
[core](../internal/core/core.go) implements the `ichnora/1` contract over
[store](../internal/store/store.go) (single writer queue, read-only pool,
embedded [migrations](../internal/store/migrations/0001_init.sql), FTS5 probe).
[policy](../internal/policy/policy.go) enforces workspace, transcript-root and
source rules. [ingest](../internal/ingest/ingest.go) normalizes native records;
[evidence](../internal/evidence/evidence.go) calls Tabula, agent-evidence and
AST Index through their CLIs. Decisions: [architecture](../docs/architecture.md);
schemas and limits: [contract](../docs/contract.md); native evidence and the
coverage matrix: [adapters](../docs/adapters.md).

## Retrieval semantics

Claim status (observed … unsupported) is stored; applicability (current,
durable, unchecked, stale, expired, unknown) is computed per request from file
digests, Git HEAD, the host epoch and Tabula sections
([applicability](../internal/core/applicability.go)). Search filters by scope in
SQL, uses FTS5 only for membership, ranks deterministically and groups
conflicts, working items, leads, sources and history
([search](../internal/core/search.go)). Summaries inherit their sources'
support roots, so repetition adds no corroboration ([claims](../internal/core/claims.go)).
Tasks change state only through explicit revisioned transitions; resume rebuilds
context from the accepted checkpoint ([tasks](../internal/core/tasks.go)).

## Verification

`just check` runs [.project-checks.json](../.project-checks.json) inside
`nix develop .`. Acceptance scenarios run the real service, CLI, hooks and MCP
bridge against synthetic fixtures ([tests](../cmd/ichnora/stale_test.go));
the scenario inventory is [acceptance](../docs/acceptance.md).

## Progress checkpoint

Phase: 3 of 7. Phases 1–2 are verified; phase 3 stale-context, invalidation,
revocation and isolation scenarios pass, retention/recovery tests are pending. Pins: Go 1.26.7 and SQLite 3.53.3 from nixpkgs
`eaad0894`, go-sqlite3 v1.14.52, MCP go-sdk v1.8.0. No remote exists; nothing
is installed, activated or collecting. Next: retention/restore and
ingestion robustness tests, OpenCode plugin, composition, host packaging.
