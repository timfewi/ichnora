# Acceptance inventory

This is a map of test scenarios, not a record that the current checkout passed
its checks. Run the [development gates](../README.md#development) for current
results. Tests use synthetic repositories, sessions and credentials; they do
not import a user's native session history.

Coverage levels:

- **Fixture**: service, CLI, hook or MCP behavior exercised by Go tests with
  synthetic input. The MCP parity test uses an in-memory transport.
- **Optional tool**: an explicitly enabled test against installed external tools
  on synthetic data. These tests are skipped unless configured.
- **Evaluation**: NixOS module evaluation, not activation of a host.
- **Build**: Nix package/check derivation, not a deployment assertion.
- **Open**: no end-to-end coverage claimed.

Dependency versions are recorded in `go.mod` and `flake.lock`. Native fixtures
in `cmd/ichnora/testdata/` model the inspected Claude Code 2.1.302, Codex 0.163.0
and OpenCode 2.0.32 interfaces; see [Adapters](adapters.md) for evidence provenance
and limits. Test names below are in `cmd/ichnora/` unless otherwise noted.

| Scenario                                                                                                                                                  | Check                                                                                                                           | Level                    |
| --------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- | ------------------------ |
| Second harness resumes an unfinished task with decisions, evidence, remaining work and applicable checks                                                  | `TestHarnessAdaptersAndCrossHarnessResume`                                                                                      | Fixture                  |
| Supersession, completed tasks and changed source stay correct across session boundaries, compaction, replay, delayed children and conflicting checkpoints | `TestStaleContextAcrossBoundaries`                                                                                              | Fixture                  |
| Invalidation propagates through summaries and cursors; retrieval and repetition renew nothing                                                             | `TestInvalidationPropagatesAndRepetitionIsNotEvidence`                                                                          | Fixture                  |
| Conflicts and unknown applicability survive small budgets; below-minimum budgets are rejected                                                             | `TestSmallBudgetKeepsConflictsAndUnknowns`                                                                                      | Fixture                  |
| Unenrolled projects, excluded paths, nested repositories, unmapped worktrees, escaping symlinks and foreign transcripts are not ingested                  | `TestEnrollmentAndSourceBoundaries`                                                                                             | Fixture                  |
| Withdrawal/revocation affects held IDs and cursors; agents cannot administer; reader bridges cannot capture                                               | `TestRevocationRolesAndMCPParity`                                                                                               | Fixture                  |
| Mixed-project and child sessions cannot widen access; gaps are reported                                                                                   | `TestMixedProjectAndChildSessionsCannotWidenAccess`                                                                             | Fixture                  |
| Unrelated projects do not affect results, counts or cursors                                                                                               | `TestUnrelatedProjectDoesNotAffectResults`                                                                                      | Fixture                  |
| Changed source and dirty worktrees invalidate applicability                                                                                               | `TestDirtyWorktreeIsNotCurrent`, `TestStaleContextAcrossBoundaries`                                                             | Fixture                  |
| Changed Tabula sections and missing evidence tools report stale/unknown support                                                                           | `TestCompositionContractWithStandInTools`                                                                                       | Fixture (stand-in tools) |
| Tabula, AST reindexing and agent-evidence composition against installed CLIs                                                                              | `TestLiveComposition`                                                                                                           | Optional tool            |
| Environment epoch changes affect only declared dependencies; missing descriptors stay unknown; rollback does not restore old working context              | `TestEnvironmentEpochRevalidation`                                                                                              | Fixture                  |
| Summaries are not independent corroboration; missing evidence remains a gap                                                                               | `TestSmallBudgetKeepsConflictsAndUnknowns`, `TestInvalidationPropagatesAndRepetitionIsNotEvidence`                              | Fixture                  |
| Duplicate delivery, restart and reconciliation preserve acknowledged checkpoints                                                                          | `TestStaleContextAcrossBoundaries`, `TestRestartAndStorageBackpressure`                                                         | Fixture                  |
| Partial lines, rotation, oversized/malformed records and transcript symlinks                                                                              | `internal/ingest` parser tests, including `TestIncrementHandlesPartialRotationAndOversize`                                      | Fixture                  |
| Concurrent writers, bounded lock waits and storage backpressure                                                                                           | `TestUnavailableServiceAndHostileInputs`, `TestRestartAndStorageBackpressure`, `internal/store` `TestCancellationAndQueueBound` | Fixture                  |
| Unicode, large metadata, omissions and batch/read-family budgets                                                                                          | `TestBudgetsAcrossOperations`, `internal/contract` tests                                                                        | Fixture                  |
| Retention removes projections and survives replay/re-enrollment; backup/restore preserves records and scope                                               | `TestRetentionTombstonesBackupAndRestore`, `internal/store` `TestMigrationsAndVerify`                                           | Fixture                  |
| Missing sockets, forged scope, foreign session hints, path escapes, unsupported events and hostile remembered text                                        | `TestUnavailableServiceAndHostileInputs`, `TestEnrollmentAndSourceBoundaries`                                                   | Fixture                  |
| CLI/MCP parity and role narrowing                                                                                                                         | `TestRevocationRolesAndMCPParity`                                                                                               | Fixture                  |
| Capture-only hooks do not inject context                                                                                                                  | `TestHookNoContextKeepsCapture`                                                                                                 | Fixture                  |
| Injected memory is not re-imported as independent evidence                                                                                                | `TestHarnessAdaptersAndCrossHarnessResume`                                                                                      | Fixture                  |
| Excluded secret markers are absent from database, WAL and logs                                                                                            | `assertNoMarkers` in `TestHarnessAdaptersAndCrossHarnessResume`, `internal/ingest` `TestCleanRedactsAndStripsInjectedContext`   | Fixture                  |
| OpenCode plugin forwards and injects once in an embedded runtime with a stand-in plugin context                                                           | `TestOpenCodePluginInEmbeddedRuntime`                                                                                           | Optional tool            |
| SQLite pragmas, linked FTS5, cancellation and newer-schema refusal                                                                                        | `internal/store` tests                                                                                                          | Fixture                  |
| CLI socket variables override unrelated XDG/config defaults                                                                                               | `TestCLISocketEnvironment`                                                                                                      | Fixture                  |
| NixOS service options and generated socket configuration                                                                                                  | `tests/module-eval.nix`                                                                                                         | Evaluation               |
| Package build, vendor hash and package test phase                                                                                                         | Flake `checks.<system>.package`                                                                                                 | Build                    |
| Live harness registration, process working directories, Codex hook trust and OpenCode plugin loader                                                       | No full live harness test                                                                                                       | Open                     |
| Historical import against real user sessions                                                                                                              | Intentionally excluded from the synthetic test suite                                                                            | Open                     |
| Model task quality, provider cost and comparison with a baseline                                                                                          | No such evaluation claimed                                                                                                      | Open                     |

## Optional checks and interpretation

[Operations](operations.md#development-and-optional-integration-checks) describes
the installed-tool switches. A successful optional tool test is not evidence
that an entire live harness integration works: the OpenCode test supplies its
own plugin context, and evidence-tool tests operate on a synthetic repository.

`TestMeasureSyntheticRetrieval` is an opt-in measurement harness using
`ICHNORA_METER` and `ICHNORA_MEASURE_LOG`; keep its log outside the repository.
It measures response bytes and elapsed time, not model tokens, provider cost or
task quality. No local measurement results or deployment receipts are published
here. Source-policy and redaction tests cover their fixtures, not every possible
secret format or same-UID threat.
