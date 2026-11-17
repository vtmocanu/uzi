# Live Codex switch: recovered validation record

This is a partial evidence ledger for issue #4202, recorded on 2026-11-17.
It does not declare m3 or m4 complete. Earlier scratch logs were absent after
recovery; historical commit messages are not treated as passing checks.

Candidate tested: 73e662c846ac9df92356dd61831967167c44d8ae.
The primary worktree remained unchanged during the controls below. Both mutants
were applied only to disposable exports of this exact candidate, with provisioned
agent dependencies linked into each export. No dependencies were installed.
Each export was removed after its command exited.

## Historical refusal and proof classification

Command:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts
```

At that revision, lines 25735–25738 in `enterCredentialSwitch` read:

```ts
    if (this.isCodexCapture(claim, flight)) {
      this.retainCaptureSource(flight);
      runLog.warn("Codex credential switch refused; source and session retained");
      return "retained_stop";
```

The retained entry also refuses at lines 27130–27133. These are refusals of
the whole operation, not a prior allowed-switch ordering. The following two
proofs are **ordering controls**, class (c) of the owner proof-method amendment.
They are not pre-feature baseline evidence.

## Real Runner ordering controls

Both exports first ran `npm run typecheck` from their agent directory, exit 0.
Each then ran:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner .*account resumes saved thread' \
  test/codex-executor.test.ts
```

The cases exercise the actual Runner/Codex switch path: subscription to API key,
API key to subscription, and subscription to subscription, each during factory
preparation and an already running body.

| Control | Exact candidate change | Observed result |
| --- | --- | --- |
| Reconciliation ordering | Move the existing awaited `a.reconcileA(bounds, remaining())` ready check from immediately after executor suspension to after the rollout-persistence try/catch. | Exit 1; 0 passed, 6 failed; assertion `A-reconcile-ready precedes A-persist`. |
| Now drain ordering | Move the existing awaited `executor.suspendCodexEpoch!(bounds)` call ahead of `nowSummary?.suspendCodexSwitch(...)`, rather than after its awaited drain. | Exit 1; 0 passed, 6 failed; assertion `Now switch suspension precedes executor suspension`. |

The unchanged candidate then ran that same command: exit 0, 6 passed, 0 failed.
The controls therefore demonstrate sensitivity to these two specific call
relocations through the real Runner path, not merely through a helper.

Candidate anchors (line numbers at the tested SHA): Runner ordering at
`agent/src/runner.ts:17386`; completed A reconciliation assertions at
`agent/test/codex-executor.test.ts:33259`; Now drain observations at
`agent/test/codex-executor.test.ts:33857`.

Initial experimental setup failures were excluded: a relocation between
`try` and `catch` did not compile, and an export without its scratch directory
failed with ENOENT. Neither counts as regression evidence.

## Additional checks observed on this candidate

From agent:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner credential replacement|Codex repeated actualRunner switching' \
  test/codex-executor.test.ts
```

Exit 0: 85 passed, 0 failed, 211491.665 ms. This includes repeated A→B→C
switching plus the actualRunner replacement matrix. It is a focused run,
not the full agent suite.

From the worktree root:

```sh
UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent
git diff --check HEAD
```

Both exited 0. The task's observed stages were dependency check, lint,
deadcode and TypeScript checks.

## Transactional recovery and compatibility repair

On 2026-11-17, the lead recreated a unique throwaway PostgreSQL 17 container
using the already available image, with no persistent volumes and a loopback
port. The container was removed by exact name after the command exited.
With its database URL in UZI_TEST_DATABASE_URL, the lead ran from api:

```sh
GOFLAGS=-p=1 GOMAXPROCS=4 go test -race -count=1 -v ./internal/workersvc \
  -run 'Test(Codex.*(Switch|Apply|Transactional|RefreshAdmitted|InitialSubscription)|OwnerCodex|SetRunCredentialCodex)'
```

At 2f1808dbadc98a3d7980c5d75af6fc08333d7de7 this passed, exit 0:
49 top-level tests, no skips, package execution 64.278 seconds.
This includes apply rollback, lost replies, account-lock ordering, refresh
admission, policy races, stopped settlement and replay fences. It is not the
full API suite.

Independent DBA review then found that migration 00464 had replaced the
supplied default branch's worker-roll migration. Commit 7cd40f3b1 had renamed
the Codex additions into the occupied version instead of preserving it.
The repair restores the default branch's 00464_worker_roll_marker.sql
byte-for-byte and moves the Codex snapshot and pending-target additions to
465 and 466 respectively. Final landing-time renumbering is still required
if the default branch advances.

The same focused command passed again with this repair and the following
test changes present, exit 0, package execution 72.306 seconds:

- The transaction snapshot compares complete ordered base/switch journal
  rows, rather than row counts.
- The spend join requires the exact original A credential at ordinal zero.
- TestCodexSwitchMigrationUpgradeLiveDB checks a fresh schema, downgrades
  only its independently created fixture database to version 464, confirms
  the preserved worker-roll columns and absent Codex additions, then upgrades
  and checks both sets of columns. Migrations at or below 464 match the
  supplied default-branch schema after the repair.

The same new migration test was copied into a disposable export of pre-repair
2f1808dbadc98a3d7980c5d75af6fc08333d7de7, leaving that revision's defective
migration set intact. With a separate fresh PostgreSQL fixture, the focused
command was:

```sh
GOFLAGS=-p=1 GOMAXPROCS=4 go test -race -count=1 -v ./internal/workersvc \
  -run '^TestCodexSwitchMigrationUpgradeLiveDB$'
```

It failed with exit 1 at
`worker_upgrade_reports: found 0 migration columns, want 2`.
The export and container were removed after the command exited.
The repaired candidate passed that test in the repeated focused run above
(7.93 seconds for this test). This is pre-repair regression evidence for the
migration collision; it is not a live-switch ordering control or evidence
against a233883de.

### Review scope and remaining merge regressions

Read-only reviewer, tester and DBA inspected the supplied-base range
9d62bc7925371e10d6e2833b43d469367842f8d7..2f1808dbadc98a3d7980c5d75af6fc08333d7de7.
The tester's two assertion findings are addressed by the changes above.
The DBA's migration blocker is addressed at the schema level, but broader
compatibility required further reconciliation: that reviewed revision had not
integrated the supplied default at all. Merge 5b16bb133 had integrated older
default 912661599 instead, leaving 20 later default commits absent. The
apparent removals included roll-marker reporting, hibernation roll history,
usage-poller refresh and Board coverage. A subsequent merge of supplied
9d62bc7925371e10d6e2833b43d469367842f8d7 restores these changes rather than
reimplementing or omitting them. Integration checks after that merge remain
separate from the pre-merge results recorded above.

Non-blocking notes from those reviews remain recorded for disposition:
the reviewer identified coordinator orchestration called only from tests,
and no comparable CI before/after critical-path measurement; the DBA noted
that interrupted post-refresh cleanup may leave switching busy until another
refresh recovers the committed account. No production failure was executed
for these notes, and no new cleanup subsystem is introduced by this repair.

## Supplied-default merge integration

The original recovery incorrectly assumed the supplied default was already
integrated. An independent history check established that merge 5b16bb133
used older default 912661599, not the requested 9d62bc792. The lead corrected
that assumption and performed a real merge, without rewriting any history:

```sh
git merge --no-commit 9d62bc7925371e10d6e2833b43d469367842f8d7
```

Committed merge: c17f8bd294fd2759132d4c9745ce7a636f760dd4.
Its second parent is the supplied default; ancestry was explicitly verified.
The merge was conflict-free. This restored the 20 absent default commits,
including roll-marker reporting and the corresponding tests. Comparing the
migration directory with the supplied default now shows only the two Codex
additions at 465 and 466.

Observed checks on the merged code, with the worktree frozen and clean:

- The focused workersvc command recorded above, with a newly created
  PostgreSQL 17 fixture: exit 0; 50 top-level tests, no skips; package
  execution 73.238 seconds.
- `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`: exit 0.
- The six real Runner cases selected by
  `M2 actualRunner .*account resumes saved thread`: exit 0, 6 passed,
  no failures or skips; execution 24074.676 ms.
- `task fmt-check:api fmt-check:controller check:migration-numbering check:migration-additive`:
  exit 0, before the merge commit with the same code present.
- `git diff --check HEAD`: exit 0.

Read-only merge review by reviewer, tester and DBA at c17f8bd29 found no new
blocking issue in their inspected overlaps. DBA confirmed restored marker
SQL/generated argument and scan order, retained Codex fields and unchanged
migrations. The independent auditor's focused cumulative boundary review
reported no demonstrated Medium-or-higher issue. Scanner claims made outside
that auditor's assigned scope are NOT credited: m4's lead-owned security
checks remain required.

Review limitations and optional notes remain explicit. At 3317c14e3, DBA
and tester noted that the isolated migration test checks column continuity,
not populated-row preservation, types/defaults/constraints or production lock
duration; the raw logs do not independently embed every environment/source
fingerprint. These limits do not turn unperformed checks into passing checks.
The reviewer at c17f8bd29 repeated the missing before/after CI critical-path
measurement note, including inherited real delays in worker-memory tests.
The coordinator duplication and interrupted-refresh cleanup notes recorded
above remain non-blocking review notes, with no new subsystem added.

## Later focused evidence and handler repair

The following results were supplied by the lead, not rerun for this ledger edit.
At 71ae949a9, 15 changed agent test files ran serially from agent with
`node --import tsx --test --test-concurrency=1 --test-timeout=120000`
(command timeout 900 seconds). The files were client-credential-switch-apply,
codex-advice-harness, codex-appserver-auth, codex-harness, codex-operation-scope,
codex-switch-coordinator, codex-switch-preflight, cross-check-runner, judge-runner,
now-summary, review-runner, runner-now-summary, runner-policy-hold, steering
and worker (each under agent/test with suffix .test.ts).
Log `.uzi/scratch/gate-log.qtJGzE`: 1765 passed, 0 failed, 0 skipped,
217287.644572 ms.

On the same 71ae949a9 candidate, focused codex-executor.test.ts selections
covered the nine describe blocks named in the behavior map below:
`.uzi/scratch/gate-log.XRdztp`, 144 passed, 0 failed, 0 skipped,
201804.648334 ms. These two runs are focused selections, not the full agent
suite. Runtime source remained unchanged through ed3455fbacdc164f7b9c0ba93abcdd88ddcf82a8;
the intervening changes were handler route tests. The recovered 50-test
workersvc result remains evidence at c17f8bd29 on unchanged code, not a new
latest-head run.

The initial focused handler run at 71ae949a9 with fresh PostgreSQL 17 failed
(`.uzi/scratch/gate-log.FlfQ8l`), then reproduced narrowly
(`.uzi/scratch/gate-log.5aou7y`): the original test expected fenced, but the
mandatory stopped-support admission guard returned
`not_applied/worker_upgrade_required`. The supplied-base 9d62bc792 export
with the original test failed earlier with 404 because the route was absent.
That comparison is inconclusive for the same failure; it does not establish
an unrelated failure.

Test-only repair b09e expected the exact upgrade outcome, then fenced Claude
with stopped support. Review found that the second case was masked by missing
Codex capability/apply registration. Minimal repair ed3455 supplies both Codex
protocols and a matching nonce while preserving Claude. Reviewer and tester
closed that masking finding on ed3455 with no new findings.

The candidate harness-fence sensitivity control
(`.uzi/scratch/gate-log.GRcJ2H`) used a disposable b09e export plus the intended
fixed test, identified by a SHA256 fingerprint. It compiled and removed only
`r.Harness == harnessCodex`; the actual real-route named test failed with
`not_applied/target_mismatch`, exit 1. The primary guard was untouched.
Candidate positives before/after (`.uzi/scratch/gate-log.RvT5LM` and
`.uzi/scratch/gate-log.goIKWF`) exited 0. This is a candidate guard sensitivity
control, not a class (c) ordering control or historical a233 baseline; it
supplies no missing proof-class inference.

Repaired lead-owned focused handler validation with fresh PostgreSQL 17
(`.uzi/scratch/gate-log.4MYoL9`) exited 0, metadata ed3455, with no skips.
The full capability package race run, count 1
(`.uzi/scratch/gate-log.Sqjnvy`), exited 0 at b09e on unchanged capability
source. Each Go command ran one package serially with
`GOFLAGS=-p=1 GOMAXPROCS=4`. Format validation
(`.uzi/scratch/gate-log.eA3Ddm`) exited 0.

## Behavior coverage map

These are concrete coverage starting points, not a claim that the frozen
assertion inventory is exhausted. E means agent/test/codex-executor.test.ts;
workersvc and handler filenames below are relative to api/internal/workersvc
and api/internal/handler respectively. Other agent files are under agent/test.

| Behavior | Named coverage starting points |
| --- | --- |
| Same flight/worktree, no new attempt, custody and B resume | E: `M2 actualRunner credential replacement`, `Codex repeated actualRunner switching`. |
| A persistence/reaping, fresh HOME and sessions-only transfer | E: `CodexExecutor: strict saved rollout seam`, `CodexExecutor: strict rollout real session store`, `CodexExecutor: strict rollout currency`, `CodexExecutor: suspended A boundary`, `CodexExecutor: replacement B boundary`. |
| Scope, refresh, identity, capability and summary | E: `CodexExecutor credential switch continuation`, `M2 external Now owned composition`, actualRunner replacement; codex-operation-scope.test.ts, now-summary.test.ts and runner-now-summary.test.ts. |
| Policy race and refresh serialization | workersvc credential_switch_codex_policy_livedb_test.go: `TestCodexPolicyEpisodeBetweenPreflightAndApplyLiveDB`; credential_switch_codex_apply_refresh_livedb_test.go: `TestCodexRefreshAdmittedBeforeApplyLiveDB`; credential_switch_codex_apply_races_livedb_test.go: `TestCodexApplyAccountLockOrderLiveDB`, `TestCodexApplyWinsLateRefreshAdmissionLiveDB`. |
| Ordinal-zero exact A, successor B and snapshots | workersvc credential_switch_codex_spend_livedb_test.go: `TestCodexSwitchSpendJoinsAuthorityEpochLiveDB`; credential_switch_codex_apply_livedb_test.go: `TestCodexTransactionalApplyBundleLiveDB`. |
| Lost reply, rollback, stale or foreign authority | workersvc credential_switch_codex_apply_lostreply_livedb_test.go: `TestCodexApplyCommittedLostReplyLiveDB`; credential_switch_codex_apply_rollback_livedb_test.go: `TestCodexApplyWriteRollbackLiveDB`; credential_switch_codex_livedb_test.go: `TestCodexSwitchPreflightLiveDB`; credential_switch_codex_policy_livedb_test.go: `TestOwnerCodexForeignRunOrCredentialRefusedLiveDB`. |
| Stopped full predicate, withdrawn X/Y, epoch/nonce, competing controls and replay | workersvc codex_switch_stopped_test.go: `TestCodexSwitchStoppedAuthorityBounds`; codex_switch_stopped_livedb_test.go: `TestCodexSwitchStoppedWithdrawnXYLiveDB`, `TestCodexSwitchStoppedEpochAdvanceAppliedSuccessorLiveDB`, `TestCodexSwitchStoppedCompetingControlsDelayedReportLiveDB`, `TestCodexSwitchStoppedResumeReclaimReregistrationLiveDB`, `TestCodexSwitchStoppedPreservationReplayLostReplyLiveDB`; E suspended/replacement boundaries and client-credential-switch-apply.test.ts. |
| Request 1 MiB, acknowledgement 16 KiB, no op_id | handler codex_switch_stopped_decode_test.go: `TestWorkerStateStoppedStrictBody`, `TestStoppedSettlementAck`; codex_switch_stopped_route_test.go: `TestWorkerRunStateCodexSwitchStoppedRoute`; client-credential-switch-apply.test.ts wire cases. |
| Targeted token absence | client-credential-switch-apply.test.ts: `Codex switch wire contract` and token/body error assertions; workersvc credential_switch_codex_apply_livedb_test.go: `TestCodexTransactionalApplyBundleLiveDB` journal/run/feed assertions; handler codex_switch_preflight_routes_livedb_test.go response assertions. These do not establish m4 security gates. |

## Outstanding evidence

Transactional and LiveDB evidence has been re-observed, including the 50-test
c17 result above. The behavior map and later focused results do not establish
m3 or m4 completion. The full (a)/(b)/(c) method has now been restored in the
[per-seam proof ledger](credential-switch-proof-ledger.md). That ledger records
six additional executed controls and retains the two earlier ordering controls.
The owner subsequently supplied class (d) for new guards without an a233
seam: one compiling guard-removal control per negative case, through the real
switch path or real API route and store transaction. This resolves the prior
method question only. The remaining mandatory clauses are still required and
unvalidated until those controls execute; no functional criterion is accepted,
waived or deferred.

`task test:codex-m4` and lead-owned m4 security, contracts, sqlc regeneration
and integration checks remain pending. Full suites remain pending exact-head
PR CI under the approved boundary (full agent suite: exact-head PR CI).
Live cross-account thread resume with two real accounts remains owner acceptance,
unverified and non-closing. Existing optional review notes above remain optional;
this record creates no new acceptance gates.
