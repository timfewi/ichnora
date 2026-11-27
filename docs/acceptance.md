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
- **VM**: a NixOS virtual machine starts the real packaged systemd service.
- **Open**: no end-to-end coverage claimed.

Dependency versions are recorded in `go.mod` and `flake.lock`. Native fixtures
in `cmd/ichnora/testdata/` model the inspected Claude Code 2.1.302, Codex 0.163.0
and OpenCode 2.0.32 interfaces; see [Adapters](adapters.md) for evidence provenance
and limits. Test names below are in `cmd/ichnora/` unless otherwise noted.

| Scenario                                                                                                                                                  | Check                                                                                                                                                                                               | Level                    |
| --------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------ |
| Second harness resumes an unfinished task with decisions, evidence, remaining work and applicable checks                                                  | `TestHarnessAdaptersAndCrossHarnessResume`                                                                                                                                                          | Fixture                  |
| Supersession, completed tasks and changed source stay correct across session boundaries, compaction, replay, delayed children and conflicting checkpoints | `TestStaleContextAcrossBoundaries`                                                                                                                                                                  | Fixture                  |
| Invalidation propagates through summaries and cursors; retrieval and repetition renew nothing                                                             | `TestInvalidationPropagatesAndRepetitionIsNotEvidence`                                                                                                                                              | Fixture                  |
| Conflicts and unknown applicability survive small budgets; below-minimum budgets are rejected                                                             | `TestSmallBudgetKeepsConflictsAndUnknowns`                                                                                                                                                          | Fixture                  |
| Unenrolled projects, excluded paths, nested repositories, unmapped worktrees, escaping symlinks and foreign transcripts are not ingested                  | `TestEnrollmentAndSourceBoundaries`                                                                                                                                                                 | Fixture                  |
| Withdrawal/revocation affects held IDs and cursors; agents cannot administer; reader bridges cannot capture                                               | `TestRevocationRolesAndMCPParity`                                                                                                                                                                   | Fixture                  |
| Mixed-project and child sessions cannot widen access; gaps are reported                                                                                   | `TestMixedProjectAndChildSessionsCannotWidenAccess`                                                                                                                                                 | Fixture                  |
| Unrelated projects do not affect results, counts or cursors                                                                                               | `TestUnrelatedProjectDoesNotAffectResults`                                                                                                                                                          | Fixture                  |
| Changed source and dirty worktrees invalidate applicability                                                                                               | `TestDirtyWorktreeIsNotCurrent`, `TestStaleContextAcrossBoundaries`                                                                                                                                 | Fixture                  |
| Changed Tabula sections and missing evidence tools report stale/unknown support                                                                           | `TestCompositionContractWithStandInTools`                                                                                                                                                           | Fixture (stand-in tools) |
| Tabula, AST reindexing and agent-evidence composition against installed CLIs                                                                              | `TestLiveComposition`                                                                                                                                                                               | Optional tool            |
| Environment epoch changes affect only declared dependencies; missing descriptors stay unknown; rollback does not restore old working context              | `TestEnvironmentEpochRevalidation`                                                                                                                                                                  | Fixture                  |
| Summaries are not independent corroboration; missing evidence remains a gap                                                                               | `TestSmallBudgetKeepsConflictsAndUnknowns`, `TestInvalidationPropagatesAndRepetitionIsNotEvidence`                                                                                                  | Fixture                  |
| Duplicate delivery, restart and reconciliation preserve acknowledged checkpoints                                                                          | `TestStaleContextAcrossBoundaries`, `TestRestartAndStorageBackpressure`                                                                                                                             | Fixture                  |
| Partial lines, rotation, oversized/malformed records and transcript symlinks                                                                              | `internal/ingest` parser tests, including `TestIncrementHandlesPartialRotationAndOversize`                                                                                                          | Fixture                  |
| Concurrent writers, bounded lock waits and storage backpressure                                                                                           | `TestUnavailableServiceAndHostileInputs`, `TestRestartAndStorageBackpressure`, `internal/store` `TestCancellationAndQueueBound`                                                                     | Fixture                  |
| Unicode, large metadata, omissions and batch/read-family budgets                                                                                          | `TestBudgetsAcrossOperations`, `internal/contract` tests                                                                                                                                            | Fixture                  |
| Retention removes projections and survives replay/re-enrollment; backup/restore preserves records and scope                                               | `TestRetentionTombstonesBackupAndRestore`, `internal/store` `TestMigrationsAndVerify`                                                                                                               | Fixture                  |
| Missing sockets, forged scope, foreign session hints, path escapes, unsupported events and hostile remembered text                                        | `TestUnavailableServiceAndHostileInputs`, `TestEnrollmentAndSourceBoundaries`                                                                                                                       | Fixture                  |
| CLI/MCP parity and role narrowing                                                                                                                         | `TestRevocationRolesAndMCPParity`                                                                                                                                                                   | Fixture                  |
| Capture-only hooks do not inject context                                                                                                                  | `TestHookNoContextKeepsCapture`                                                                                                                                                                     | Fixture                  |
| Injected memory is not re-imported as independent evidence                                                                                                | `TestHarnessAdaptersAndCrossHarnessResume`                                                                                                                                                          | Fixture                  |
| Excluded secret markers are absent from database, WAL and logs                                                                                            | `assertNoMarkers` in `TestHarnessAdaptersAndCrossHarnessResume`, `internal/ingest` `TestCleanRedactsAndStripsInjectedContext`                                                                       | Fixture                  |
| OpenCode plugin forwards and injects once in an embedded runtime with a stand-in plugin context                                                           | `TestOpenCodePluginInEmbeddedRuntime`                                                                                                                                                               | Optional tool            |
| SQLite pragmas, linked FTS5, cancellation and newer-schema refusal                                                                                        | `internal/store` tests                                                                                                                                                                              | Fixture                  |
| CLI socket variables override unrelated XDG/config defaults                                                                                               | `TestCLISocketEnvironment`                                                                                                                                                                          | Fixture                  |
| NixOS service options and generated socket configuration                                                                                                  | `tests/module-eval.nix`                                                                                                                                                                             | Evaluation               |
| NixOS custom socket directories, owner permissions, role enforcement and memory after service restart                                                     | `tests/module-vm.nix`                                                                                                                                                                               | VM                       |
| Package build, vendor hash and package test phase                                                                                                         | Flake `checks.<system>.package`                                                                                                                                                                     | Build                    |
| Live harness registration, process working directories, Codex hook trust and OpenCode plugin loader                                                       | No full live harness test                                                                                                                                                                           | Open                     |
| Historical import against real user sessions                                                                                                              | Intentionally excluded from the synthetic test suite                                                                                                                                                | Open                     |
| Model task quality, provider cost and comparison with a baseline                                                                                          | No such evaluation claimed                                                                                                                                                                          | Open                     |
| Inactive claim support is consistent before and after capture, across recursive summaries and project sharing                                             | `TestInactiveClaimSupportIsIndependentOfCaptureOrder`, `TestInactiveSharedClaimSupportRespectsProjectScope`                                                                                         | Fixture                  |
| Streamed and legacy message versions do not add independent corroboration; schema upgrades preserve old records                                           | `TestOpenCodeStreamingVersionsDoNotAddIndependentSupport`, `internal/core` `TestLegacyAndVersionedSupportSharePersistedRoots`, `internal/store` `TestSupportRootFormatMigrationPreservesLegacyData` | Fixture                  |
| Searches and resume include older records beyond the former candidate limits; scopes and cursors remain valid                                             | `TestSearchAndResumeTraverseAllCandidateBatches`, `TestSearchSourceTaskFilterAndNormalizedQueries`, `TestSearchCursorExpiresWhenSourceChangesBetweenPages`, `internal/core` search tests            | Fixture                  |
| Empty tool commands and transcript byte/line boundary cases cannot stall ingestion                                                                        | `internal/ingest` `TestAdaptersKeepEmptyToolCommandsWithoutPanic`, `TestIncrementAtReadLimitAndOversizedProgress`                                                                                   | Fixture                  |
| Actual process termination preserves acknowledged memory and transcript cursors; interrupted restore can restart and retry                                | `TestSIGKILLPreservesAcknowledgedMemoryAndTranscriptCursor`, `TestRestoreInterruptedPreparationCanRestartAndRetry`                                                                                  | Fixture                  |
| Corrupt state and failed staged migration do not prevent safe recovery; exact SQLite path names are preserved                                             | `TestRestoreReplacesCorruptDatabaseAndRetainsOriginalBytes`, `TestRestoreMigrationFailureLeavesCanonicalDatabase`, `internal/store` path tests                                                      | Fixture                  |
| OpenCode acknowledgements require committed batches, and legacy replay respects changed attribution                                                       | `TestOpenCodeAcknowledgesCommittedVersionsAndCoverage`, `TestOpenCodePausedBatchIsNotAcknowledged`, `TestOpenCodeLegacyReplayKeepsChangedAttribution`                                               | Fixture                  |
| OpenCode byte-bounded backlog, retry, compaction and message updates work in an embedded runtime                                                          | `TestOpenCodeBatchTransportInEmbeddedRuntime`                                                                                                                                                       | Optional tool            |
| MCP input schemas accept the contract's values; an agent following them keeps a claim validated, checkpoints, resumes and records an attempt              | `internal/mcpbridge` `TestAdvertisedValues`, `TestMCPWorkflowFollowsAdvertisedSchemas`                                                                                                              |

`TestSimPredictionSurvivesReplayAndScope` exercises the improvement history CLI
and service with synthetic evidence files, restart, replay, conflicting writes,
pagination, project scope, evidence-path policy and project retention preview.

The CLI fixtures in `memory_review_test.go` check transitive source drift,
diverging worktrees, saved verification inputs, exact JSON values, independent
facts in one file, scoped review, and retention of unrelated claims after a
confirmed claim-tree deletion. They also verify the evidence for turning a
failed SIM trial into a reusable rule.

### Paired coding tasks

`TestPairedCodingReuse` prepares fresh synthetic handler repositories for a
stable route, a changed route and a task without prior evidence. Each task
receives three conditions in rotated order: no prior memory, the original
plain note, and Ichnora's actual search response. The prompt, starting files
and 2,048-byte context allowance are identical within each pair. The scorer
runs the dispatcher and checks the probe, ordinary behavior, route selection
and the standby handler. Logs include task success, standby edits, context
bytes and elapsed time. A standby edit on a switched route flags a change to
the former handler; it does not establish the agent's reason.

The default editor verifies the fixtures and scorer. To measure an agent,
supply an absolute executable via `ICHNORA_REUSE_RUNNER` and run
`go test -tags libsqlite3 ./cmd/ichnora -run '^TestPairedCodingReuse$' -count=1 -timeout=50m -v`
in the development shell in a disposable guest. The runner receives one JSON
object on stdin with `task` and `memory`, edits the current project and
exits within five minutes. It gets a fresh home and a minimal environment.
Use the same model, tool limits and sampling settings across conditions.
Read the per-task scores from the test log; unsuccessful agent solutions are
reported as outcomes. Keep model runs and measurements outside the repository.

## Optional checks and interpretation

[Operations](operations.md#development-and-optional-integration-checks) describes
the installed-tool switches. A successful optional tool test is not evidence
that an entire live harness integration works: the OpenCode test supplies its
own plugin context, and evidence-tool tests operate on a synthetic repository.

The GitHub Actions workflow runs the fast checks, the pinned Linux package and
module evaluation, then the x86-64 NixOS VM check on a runner with KVM. The VM
check is also available as `nix build .#checks.x86_64-linux.module-vm`. It tests
an isolated synthetic host, not activation of an operator's existing machine.

`TestMeasureSyntheticRetrieval` is an opt-in measurement harness using
`ICHNORA_METER` and `ICHNORA_MEASURE_LOG`; keep its log outside the repository.
It measures response bytes and elapsed time, not model tokens, provider cost or
task quality. No local measurement results or deployment receipts are published
here. Source-policy and redaction tests cover their fixtures, not every possible
secret format or same-UID threat.
