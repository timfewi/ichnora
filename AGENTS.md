# Project policy

Ichnora is a private, local memory service for coding agents: one Go
executable (service, CLI, MCP bridge, hook adapter) over SQLite/FTS5.

- Start with [.tabula/ENTRY.md](.tabula/ENTRY.md); it holds the architecture
  summary and the current progress checkpoint. Update its claims in place
  instead of appending reports. Decisions live in
  [docs/architecture.md](docs/architecture.md), the versioned contract in
  [docs/contract.md](docs/contract.md), native-format evidence in
  [docs/adapters.md](docs/adapters.md) and scenario coverage in
  [docs/acceptance.md](docs/acceptance.md).
- The stack is fixed by the owner: Go with `github.com/mattn/go-sqlite3`
  linked to the Nix SQLite (`libsqlite3` tag, CGo). Do not add a second
  driver, an ORM, a web framework, a model client or any Node.js dependency.
  The OpenCode adapter is a thin plugin run by OpenCode's own runtime.
- The service owns the database. CLI, hooks and MCP bridges are socket
  clients; never add a direct-database or network fallback.
- Tests use synthetic fixtures only. Never import real sessions, read native
  transcript directories or put runtime databases, raw sessions, measurements
  or credentials in Git or the Nix store.
- Run `nix develop .` (or direnv), then `just fmt` and `just check`
  (`project-check fast`: gofmt/prettier/nixfmt, `go vet`, `go test`, statix,
  deadnix and an offline `nix flake check --no-build`). `just verify` builds
  the package and runs the tests in the Nix sandbox; run it only when that
  workload is authorized.
- After `go.mod`/`go.sum` changes, recompute `vendorHash` in
  [nix/package.nix](nix/package.nix) with `go mod vendor` and
  `nix hash path vendor` in a scratch copy.
- Task-scoped staging and local commits are authorized after review and the
  fast gate. The repository has no remote yet; creating one, publishing,
  installing, activating services, enabling collection and importing real
  sessions require separate owner authorization.
- Preserve unrelated changes. Write source, documentation and CLI text in
  English.
