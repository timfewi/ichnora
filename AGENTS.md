# Project policy

Ichnora is a local memory service for coding agents: one Go executable
(service, CLI, MCP bridge, hook adapter) over SQLite/FTS5.

- Start with [README.md](README.md) and [docs/operations.md](docs/operations.md).
  Design decisions live in [docs/architecture.md](docs/architecture.md), the
  versioned contract in [docs/contract.md](docs/contract.md), native-interface
  evidence in [docs/adapters.md](docs/adapters.md), and scenario coverage in
  [docs/acceptance.md](docs/acceptance.md). Keep these accurate rather than
  appending local deployment or verification reports.
- Use Go with `github.com/mattn/go-sqlite3` linked to the Nix SQLite
  (`libsqlite3` tag, CGo). Do not add a second driver, an ORM, a web framework,
  a model client or any Node.js dependency. The OpenCode adapter is a thin
  plugin run by OpenCode's own runtime.
- The service owns the database. CLI, hooks and MCP bridges are socket
  clients; never add a direct-database or network fallback.
- Collection, automatic recall and historical imports require explicit opt-in.
  Preserve project scope, evidence provenance and the distinction between
  application roles and same-UID isolation.
- Tests use synthetic fixtures only. Never import real sessions, read native
  transcript directories or put runtime databases, raw sessions, measurements,
  deployment identities or credentials in Git or the Nix store.
- When checks are authorized, run `nix develop .` (or direnv), then `just fmt`
  and `just check` (`project-check fast`: formatting, `go vet`, `go test`,
  statix, deadnix and offline flake evaluation). `just verify` runs the Nix
  build/test gate; run it only when that workload is authorized. Optional live
  tool tests are not evidence of full live harness coverage.
- After `go.mod`/`go.sum` changes, recompute `vendorHash` in
  [nix/package.nix](nix/package.nix) with `go mod vendor` and
  `nix hash path vendor` in a scratch copy. Update
  [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) and retained upstream
  licenses when dependency licensing changes.
- Git mutations, publication, installation, host activation, collection and
  real-session import must follow the operator's authorization for the task.
  A public source repository does not authorize access to private runtime data.
- Preserve unrelated changes. Write source, documentation and CLI text in
  English. Original contributions use the repository's Apache-2.0 license;
  retain upstream licenses for third-party material.
