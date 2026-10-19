# Native adapters

Evidence was taken from the pinned binaries on 2026-10-19 (versions from
`coding-agents-nix/agents/*/pin.json`), not from configuration or online
documentation alone. Fixtures in `cmd/ichnora/testdata/` are synthetic and
follow the fields listed here. "Fixture-verified" means the adapter entrypoint
parsed these fixtures in tests; no live client was driven.

## Inspected interfaces

| Harness     | Version | Evidence                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| ----------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claude Code | 2.1.302 | Hook input schemas in the binary: common `session_id`, `transcript_path`, `cwd`, `agent_id`; `SessionStart.source` ∈ startup/resume/clear/compact/fork with `hookSpecificOutput.additionalContext`; `PreCompact.trigger`, `PostCompact.compact_summary`; `SubagentStart/Stop` with `agent_id`, `agent_type`, `agent_transcript_path`; `SessionEnd.reason`. Child processes see `CLAUDE_CODE_SESSION_ID`. Native auto-memory: `autoMemoryEnabled` setting and `CLAUDE_CODE_DISABLE_AUTO_MEMORY`.                      |
| Codex       | 0.163.0 | `codex features list`: `hooks` stable/enabled, `memories` stable/disabled. Embedded JSON schemas `session-start.command.input` (same `source` values, nullable `transcript_path`), `pre-compact`, `post-compact`, `subagent-start` (context output), `subagent-stop` (nullable `agent_transcript_path`), `session-end`, `stop`. Hooks are configured as `[hooks]` event tables in `config.toml` (`HookEventsToml`, matcher groups of `{type = "command", command, timeout}`). Child processes see `CODEX_THREAD_ID`. |
| OpenCode    | 2.0.32  | V2 plugin context exposes `session.hook(name, fn)` with `context`, `compaction`, `generate`, `title`, `retry`, `model.request`; the `context` payload carries `sessionID`, `agent`, `messages` and a `system` list that plugins extend. Plugins are `{id, setup(ctx)}` modules loaded from the `plugins` setting. `OPENCODE_SESSION_ID` appears in the binary. No native auto-memory route was found.                                                                                                                |

## Coverage matrix

| Boundary           | Claude Code                                                                                                                                  | Codex                                                      | OpenCode                                                             |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- | -------------------------------------------------------------------- |
| Session start      | `SessionStart(startup\|clear)` → index                                                                                                       | same                                                       | first `context` hook per session → index                             |
| Resume             | `SessionStart(resume\|fork)` → bound task context                                                                                            | same                                                       | first `context` hook after plugin start                              |
| Tool outcomes      | transcript `tool_result` (`is_error`), outputs excluded                                                                                      | rollout `function_call_output` exit code, outputs excluded | forwarded `tool` parts (`state.status`), inputs/outputs excluded     |
| Checkpoint capture | MCP `memory_task` / CLI `task`                                                                                                               | same                                                       | same                                                                 |
| Compaction         | `PreCompact`, `PostCompact` markers; context rebuilt at `SessionStart(compact)`; compaction summaries stored as derived markers without text | same; `compacted` rollout lines derived                    | `compaction` hook marker; context rebuilt into the compaction prompt |
| Task switch        | explicit `resume` or `task create` binds the session (via the session environment variable)                                                  | same                                                       | same                                                                 |
| Session end        | `SessionEnd` marker; not required (next hook or `reconcile` continues the cursor)                                                            | same                                                       | unsupported: no end hook; next `context` call reconciles             |
| Child sessions     | `SubagentStop` ingests `agent_transcript_path` as `parent/agent_id`                                                                          | same; a null `agent_transcript_path` is a reported gap     | forwarded `parentID` marks a child session                           |

Status reports the declared matrix plus observed boundary counts per project.

## Hook registration

`programs.coding-agents.ichnora.hooks.enable` registers the same command for
`SessionStart`, `PreCompact`, `PostCompact`, `SubagentStop`, `Stop` and
`SessionEnd`: `ichnora hook <harness> --socket <agent socket>`, plus
`--no-context` unless Ichnora owns recall. Existing native hooks are kept and
Ichnora's matcher group is appended. Codex shows its startup hook review for
new config hooks; the owner must trust them before they run. OpenCode loads the
plugin directory from the Ichnora package; it inherits `ICHNORA_SOCKET` and
`ICHNORA_HOOK_CONTEXT` from the session environment. The host currently also
writes `/etc/codex/hooks.json` for the skill engine; whether Codex merges that
file with `[hooks]` in `config.toml` has not been observed live.

## Known limits

- Subagents of Claude Code and Codex share the parent's MCP bridge and
  environment, so a checkpoint they write through MCP is attributed to the
  parent session. Their transcript events stay attributed to the child session.
- Codex rollout lines have no native event IDs; identities are digests of the
  raw line, so byte-identical lines in one session collapse into one event.
- Hidden reasoning, tool arguments and tool outputs are never stored; only the
  tool name, call ID, outcome and literal command name are.
- Ichnora can stop re-injecting context; it cannot remove text already in a
  running conversation. Context refresh happens only at the boundaries above;
  otherwise a fresh conversation is needed.
- The OpenCode plugin was executed in the pinned binary's embedded runtime
  (Bun 1.4.2 via `BUN_BE_BUN=1`) with a stand-in plugin context against the
  real service. OpenCode's plugin loader, its `session.get` result shape and
  its handling of plain strings in `system` were not observed live; the plugin
  falls back to no injection if they differ.
- Hook scope comes from the hook process's working directory, which the
  harnesses are expected to set to the session directory; this has been
  fixture-tested, not observed in a live client.
