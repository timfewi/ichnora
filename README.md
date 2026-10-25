# Ichnora

Local memory with provenance for coding agents. Ichnora keeps project-scoped
claims, decisions and task checkpoints, indexes permitted session text, and
helps a later session distinguish current evidence from stale history.

One Go executable provides the service, CLI, stdio MCP bridge and harness hook
adapters. It runs on Linux and uses CGo with `go-sqlite3` linked to SQLite/FTS5.
Retrieval is lexical and deterministic: no model calls, embeddings, network
service or direct-database client fallback.

## What it provides

- Project enrollment and worktree mappings, with collection and automatic
  recall **off by default**. Configuring a workspace does not import it.
- Explicit claim capture, revisioned task checkpoints and cross-session resume.
- Bounded search/read responses, evidence references, stale-dependency checks,
  conflicts and explicit history views.
- Claude Code and Codex transcript adapters, plus a thin OpenCode plugin run by
  OpenCode's own runtime. See the [interface evidence and limits](docs/adapters.md);
  fixture coverage is not a claim of end-to-end live harness compatibility.
- Owner-controlled sharing, preview/confirmation for historical import and
  retention, consistent backups and offline restore.

The service alone owns its SQLite database. Clients connect over separate
agent and admin Unix sockets. Scope comes from the connecting process's
working directory, not a project name supplied in a request.

## Build and install

The flake supports `x86_64-linux` and `aarch64-linux`. With Nix and flakes enabled:

```sh
nix build 'github:timfewi/ichnora'
./result/bin/ichnora version

# Optional: install the CLI into your user profile.
nix profile install 'github:timfewi/ichnora'
```

For a local checkout:

```sh
git clone https://github.com/timfewi/ichnora.git
cd ichnora
nix build .
```

Building or installing does **not** activate a service, enroll projects, register
hooks or enable collection. Follow [Operations](docs/operations.md) for a
standalone configuration, optional NixOS service module, socket discovery and
explicit enrollment. The CLI is not useful for memory operations until the
service is running.

## Privacy and trust

Only enrolled projects are eligible for scoped operations. Automatic transcript
collection requires a separate opt-in; historical import requires an explicit
preview and confirmation. Retained user/assistant text can still be sensitive:
credential-pattern redaction is not a guarantee that all secrets are removed.
Keep state and backups outside source workspaces and the Nix store.

The service accepts only clients with its own UID. The two sockets and MCP roles
are application policy, **not isolation from other processes running as that
user**. A restricted agent runtime should expose only the agent socket and
necessary project paths, never the admin socket or state directory. There is no
network fallback. See [Architecture](docs/architecture.md) for the trust boundary.

## Development

```sh
nix develop .
just fmt       # apply repository formatting
just check     # fast gate
just verify    # Nix build/test gate; potentially expensive
```

The dev shell supplies the pinned tools and enables CGo plus the `libsqlite3`
build tag. `just check` uses the public
[project-check](https://github.com/timfewi/project-check-nix) runner for formatting,
Go vet/tests, Nix lint and offline flake evaluation. `just verify` runs the Nix
flake checks, including package tests. Tests use synthetic data; optional live
tool checks are described in [Operations](docs/operations.md).

## Documentation

- [Operations](docs/operations.md): configuration, clients, collection and maintenance.
- [Architecture](docs/architecture.md): storage, scope, dependencies and trust.
- [Contract](docs/contract.md): versioned requests, responses, errors and limits.
- [Adapters](docs/adapters.md): native interfaces, provenance and compatibility limits.
- [Acceptance inventory](docs/acceptance.md): test scenarios and coverage boundaries.
- [Contributor guidance](AGENTS.md): repository engineering conventions.

## License

Original Ichnora source and documentation are licensed under
[Apache-2.0](LICENSE), copyright 2026 timfewi. Third-party components retain their
upstream licenses; see [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) and
[LICENSES/](LICENSES/). This source release is not a distribution or license
inventory of an entire NixOS system closure.
