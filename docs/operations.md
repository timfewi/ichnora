# Operations

Ichnora runs as the same Linux user as its clients. Choose either the standalone
service or the NixOS module below, not both against the same state directory.
All `/home/alice` paths are synthetic examples: replace them with your own
absolute paths. No private integration repository or optional evidence tool is
required.

## Build or install

Use the [README build instructions](../README.md#build-and-install). The examples
below assume `ichnora` is on `PATH`; alternatively use the absolute path to
`result/bin/ichnora`. Building or installing alone activates nothing.

## Standalone service

Create `/home/alice/.config/ichnora/config.json` with:

```json
{
  "state_dir": "/home/alice/.local/state/ichnora",
  "agent_socket": "/home/alice/.local/state/ichnora/run/agent.sock",
  "admin_socket": "/home/alice/.local/state/ichnora/run/admin.sock",
  "workspace_roots": ["/home/alice/code"],
  "session_sources": {}
}
```

Create the configuration's parent directory first. The workspace directory and
projects must exist before service startup. Configured workspace and transcript
roots that do not exist at startup are omitted; restart after creating them or
changing configuration. Paths in JSON must be absolute (`~` is not expanded).
The state directory must be outside all workspace roots and the Nix store; the
service creates it with mode 0700 and its sockets with mode 0600.

Start it in a terminal as Alice, not root:

```sh
ichnora serve --config /home/alice/.config/ichnora/config.json
```

Keep this process running while using clients in another terminal. Stop with
Ctrl-C. For persistent operation, use a service manager running the same command
as the same user, or the module below. A fresh database has no enrolled projects.
The empty `session_sources` allows manual claims/tasks but no transcript files.

To permit Claude Code or Codex transcripts later, replace `session_sources` with
only the existing roots you intend to allow, then restart:

```json
{
  "claude": ["/home/alice/.claude/projects"],
  "codex": ["/home/alice/.codex/sessions"]
}
```

These are permissions, not a recursive import request. OpenCode's plugin forwards
messages directly rather than scanning a native transcript directory. Optional
`tools`, `environment_descriptor` and `limits` fields are defined in
[`internal/config/config.go`](../internal/config/config.go); the default resource
limits and protocol are in [Contract](contract.md).

## Optional NixOS module

Add the source flake to your system flake's `inputs`:

```nix
ichnora.url = "github:timfewi/ichnora";
```

Include this module in your existing `nixpkgs.lib.nixosSystem` `modules` list,
where `inputs` is your flake's inputs argument:

```nix
{
  imports = [ inputs.ichnora.nixosModules.default ];

  services.ichnora = {
    enable = true;
    user = "alice"; # An existing account; clients must use this UID.
    stateDir = "/home/alice/.local/state/ichnora";
    workspaceRoots = [ "/home/alice/code" ];
    sessionSources = {}; # Add only the transcript roots you explicitly permit.
  };
}
```

The module installs the package and defines `ichnora.service` with private
networking, Unix-only sockets, a private runtime directory and read-only home
access except for the state directory. It does not create the operator account,
enroll projects or register harness hooks. Review and activate your own NixOS
configuration using your normal deployment process; activation is a separate
operator action, not a result of `nix build`.

The default sockets are `/run/ichnora/agent.sock` and
`/run/ichnora/admin.sock`; override `agentSocket` and `adminSocket` together with
client configuration if needed. `systemctl status ichnora` and
`journalctl -u ichnora` show service health. `systemctl cat ichnora` shows the
`ExecStart` configuration path needed for offline restore. The generated config
is in the Nix store, so never put secret data in module settings.

## Socket discovery and enrollment

Client precedence is the command's `--socket` / `--admin-socket` flag, then
`ICHNORA_SOCKET` / `ICHNORA_ADMIN_SOCKET`, then the loaded configuration. The
config path is `--config`, then `ICHNORA_CONFIG`, then
`$XDG_CONFIG_HOME/ichnora/config.json` (or `~/.config/ichnora/config.json`). With
no configured sockets, defaults are `$XDG_RUNTIME_DIR/ichnora/{agent,admin}.sock`;
if that variable is unset, both socket paths must be configured explicitly.
A missing config file yields defaults and no permitted roots, not enrollment.

For the standalone example, in the client terminal:

```sh
export ICHNORA_CONFIG=/home/alice/.config/ichnora/config.json
```

The NixOS module exports both socket variables in login sessions. Start a new
login session after activation, or set the configured paths explicitly:

```sh
export ICHNORA_SOCKET=/run/ichnora/agent.sock
export ICHNORA_ADMIN_SOCKET=/run/ichnora/admin.sock
```

Do not mix stale socket variables with a different config: the variables win.
For either setup:

```sh
ichnora discover                          # candidate repositories, not content import
ichnora enroll /home/alice/code/example   # automatic collection and recall remain off
ichnora projects
cd /home/alice/code/example
ichnora status
ichnora task create --title "Review parser behavior"
ichnora search
```

Run scoped CLI commands and MCP bridges from the enrolled project directory.
The service uses the connecting process's Linux peer credentials and working
directory, not a request's project name. Enroll a project _below_ a workspace
root, not the workspace root itself. Nested repositories enroll separately;
map additional worktrees with `enroll PATH --worktree-of PROJECT_ID`.

Manual `capture` (JSON on stdin), `task` and `resume TASK_ID` are explicit client
operations and do not require automatic collection. Their request fields are in
[Contract](contract.md). To opt into future hook-driven collection:

```sh
ichnora enroll /home/alice/code/example --collect
ichnora policy --harness claude /home/alice/.claude/projects/example/session.jsonl
```

The policy command requires an actual permitted file; the path above is an
example. Enrollment alone does not register hooks. Re-enrollment replaces both
`collect` and `recall` flags: repeat every option you want to keep. To opt into
both collection and automatic recall, use `enroll PATH --collect --recall`.
To turn both off while retaining manual access, re-enroll without either flag.
`withdraw PROJECT_ID` disables project access and ingestion but retains history;
it is not deletion.

## Register clients and hooks explicitly

Configure your harness's stdio MCP server entry to launch the installed binary
with arguments `mcp --role reader` for read-only access, or
`mcp --role reader,capture` for explicit writes. Give it `ICHNORA_SOCKET` and a
working directory inside the enrolled project. These are command/argument values,
not a universal harness configuration schema. No MCP server registration is
performed by the package or service module.

For Claude Code and Codex command hooks, register
`ichnora hook claude --no-context` or `ichnora hook codex --no-context` for the
supported events listed in [Adapters](adapters.md#hook-registration). Use an
absolute executable path if the harness does not inherit your `PATH`. Preserve
existing hooks and any native approval/trust workflow. The harness supplies
native JSON on stdin; do not replace it with a project name. Start capture-only.

For OpenCode, load the installed `share/ichnora/opencode` plugin directory using
the inspected V2 plugin interface. Pass `ICHNORA_SOCKET` and set
`ICHNORA_HOOK_CONTEXT=off` for capture-only operation. A source-tree plugin also
needs `ICHNORA_BIN` set to the absolute built executable. Consult the documented
[OpenCode compatibility limits](adapters.md#known-limits) before enabling it.

Choose one automatic recall owner. To let Ichnora inject context, explicitly
enroll with `--recall`, remove `--no-context` and unset `ICHNORA_HOOK_CONTEXT=off`
for its hooks/plugin. Disable competing native automatic memory (the inspected
interfaces are Claude Code `autoMemoryEnabled` and Codex `features.memories`).
Do not assume this is configured for you. A hook's fail-open behavior keeps a
harness running when Ichnora is unavailable; inspect `status` for actual coverage
rather than treating a successful hook exit as proof of ingestion.

Only expose the agent socket to restricted agent runtimes. The admin socket and
state directory must remain outside their filesystem view. This is not protection
from an unrestricted process with the same UID: it can reach either socket or
change directories. There is no TCP listener or network fallback.

## Historical import and retention

These are owner/admin operations. Preview only files you explicitly choose from
a configured transcript root; their projects must be enrolled:

```sh
ichnora import preview --harness claude /home/alice/.claude/projects/example/session.jsonl
ichnora import apply --harness claude --confirm TOKEN /home/alice/.claude/projects/example/session.jsonl

ichnora retention preview --selector session --target SESSION_ID
ichnora retention apply --selector session --target SESSION_ID --confirm TOKEN
```

Replace `TOKEN` with the corresponding preview's confirmation token and
`SESSION_ID` with an actual `ses_...` identifier. Review affected records before
applying; changes can invalidate a token. `reconcile` advances known transcript
cursors; it is not a blanket historical scan. Automatic retention is disabled.

## Backup and restore

Backups require an existing writable directory outside workspace roots and the
Nix store, and a destination that does not exist. A path inside `stateDir` also
works with the NixOS module's write restrictions:

```sh
ichnora backup /home/alice/.local/state/ichnora/backup.db
```

Copy the resulting private snapshot to a suitably protected backup location.
To restore, stop the service first (Ctrl-C for the foreground service, or
`sudo systemctl stop ichnora` for NixOS). Run as the service user with its exact
configuration:

```sh
ichnora restore --config /home/alice/.config/ichnora/config.json /home/alice/.local/state/ichnora/backup.db
```

For NixOS, substitute the config path from `systemctl cat ichnora`, then restart
with `sudo systemctl start ichnora`. Restore takes the service's state lock,
validates the backup and keeps the previous database beside it. Backups and
previous databases retain subsequently deleted data; handle those copies
separately. Secure deletion does not guarantee erasure from snapshots,
copy-on-write filesystems or physical media.

## Development and optional integration checks

Run the [development gates](../README.md#development) in the development shell.
The ordinary suite uses synthetic data. Optional installed-tool checks are:

```sh
ICHNORA_LIVE_TOOLS=1 go test -tags libsqlite3 ./cmd/ichnora -run '^TestLiveComposition$' -count=1
ICHNORA_OPENCODE_RUNTIME=/absolute/path/to/opencode go test -tags libsqlite3 ./cmd/ichnora -run '^TestOpenCodePluginInEmbeddedRuntime$' -count=1
```

The first requires `tabula`, `agent-evidence` and `ast-index` on `PATH`; they are
optional, not core setup prerequisites. The second needs an OpenCode executable
that supports its embedded runtime's `BUN_BE_BUN=1` mode, not a restricted
launcher. It supplies a stand-in plugin context; it does not test the real plugin
loader or a live model conversation. See [Acceptance](acceptance.md) for coverage
and remaining limits. Keep all runtime data and measurement output out of Git.
