# Live Codex switch: recovered validation record

This is a partial evidence ledger for issue #4202, recorded on 2026-11-17.
It does not declare m3 or m4 complete. Earlier scratch logs were absent after
recovery; historical commit messages are not treated as passing checks.

Latest runtime fixture candidate: e7ee697e9b63abdfd754760926b4b7fca7990175.
Earlier R1–R6 fixture candidate: 47b0d8e374555c504be67ded2f4960f4a6c2e6d5.
Stopped-policy candidate: 1cbcb917f156bfb8eb862f743acb1dde87a91a29.
Earlier stopped-authority control candidate: 2de82eafb2ace6b0a03c10de8f7a582b7b1d97bd.
Earlier stopped-guard controls: 76cc31aee1b0dafd644e9fa3fb7593442dc7ed6b.
Earlier Runner ordering candidate: 73e662c846ac9df92356dd61831967167c44d8ae.
The primary worktree remained unchanged during those ordering controls. Both mutants
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
using the already available image, with no named/shared volumes and a loopback
port. This does not establish removal of Docker's anonymous data volume. The container was removed by exact name after the command exited.
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

## Stopped guard route evidence at 76cc31a

The lead's `.uzi/scratch/stopped-guard-controls.py` completed with exit 0 at
76cc31aee1b0dafd644e9fa3fb7593442dc7ed6b. Its manifest
`.uzi/scratch/stopped-guard-results.json` records 24 compiling class (d)
**guard controls**, each with an individual log and exact one-guard change
in both SQL source and generated counterpart. These are not pre-feature
baseline evidence. The [per-case proof ledger](credential-switch-proof-ledger.md)
records five epoch, sixteen pending-kind/state and three budget/clock/write
cases and their failure witnesses.

The actual authenticated WorkerRoutes fixture negotiates registration,
seeds the Codex binding/journal, stamps an owner switch, latches epoch 41 via
HTTP preflight, withdraws via HTTP and reports `/state` through the pool-backed
transaction. The applied-successor case commits epoch 42 through HTTP apply
before reporting retained 41. Snapshots compare full run/base/switch/messages/
inputs/custody; fresh settlement allows the specified status/timestamp/health
changes, while replay allows no changes. The stalled-health reset and
committed minimal-acknowledgement test repairs were reviewed clean at 76cc31a.
These observations do not establish broader preservation or strict-wire proof.

Each mutant ran serially in its own candidate export, removed immediately,
with no base builds or installations. The owned PostgreSQL 17 container was
recreated for the wave and removed by exact name using `docker rm -fv`.
Commands used `GOFLAGS=-p=1 GOMAXPROCS=4` and
`go test -buildvcs=false -race -count=1 -json -timeout=5m ./internal/handler`
with the named route/subcase selector. For 23 mutants the selector also ran
`fresh_and_replay`, which passed as an independent SQL-prepare control;
only replay_update omitted it because it breaks that replay invariant.
All 24 intended negatives compiled and exited 1: 22 inappropriate acceptances
(`refusal mismatch: status=200 applied=true`), original-clock refusal
(`stop acceptance mismatch: status=409 applied=false`) and replay write
(`snapshot changed in run`). Before `.uzi/scratch/gate-log.oduxq5` and after
`.uzi/scratch/gate-log.COd4tJ` each exited 0: 25 subcases passed, no skips.

The primary focused route matrix (new 25 plus existing apply/preflight)
passed in `.uzi/scratch/gate-log.f9IftP`, package 20.676 seconds, EXIT=0.
Narrow fresh/replay after the test repairs passed in
`.uzi/scratch/gate-log.B5w1oi`, EXIT=0. These are focused results, not a full
suite or broad no-tokens proof. This documentation update inspected the
script, manifest, fixture and SQL, spot-checked logs and counted unchanged
passes; it did not rerun the commands.

## Additional authority-route cases at dda69dd2d

The test-only commit dda69dd2d82a11bf123066771decd3b6c5cec53f adds
api/internal/handler/codex_switch_stopped_authority_routes_livedb_test.go.
The exact committed file content passed the focused handler race run in
`.uzi/scratch/gate-log.5F4QD4`, EXIT=0, package 23.616 seconds: 28
running/paused authority-marker cases and four wire-shape cases, no skips.
The command selected only TestWorkerCodexStoppedAuthorityRouteLiveDB and
TestWorkerCodexStoppedWireAuthorityRouteLiveDB with GOFLAGS=-p=1 and
GOMAXPROCS=4 through the recreated throwaway handler database harness.
Generation/release cases explicitly require an audit-free fixture so the
outer reconciliation check does not mask the SQL guard. The wire cases
exercise a known extra typed field and a case alias for register_nonce.

Independent reviewer inspection at dda69dd2d found no blocking code defect.
Its one non-blocking note was unmeasured before/after CI job and critical-path
timing; the focused package duration above is not that measurement. No base
build or CI timing comparison was performed for that review.

### Qualifying authority controls at 2de82eaf

The lead subsequently completed `.uzi/scratch/stopped-authority-controls.py`,
exit 0, at 2de82eafb2ace6b0a03c10de8f7a582b7b1d97bd.
`.uzi/scratch/stopped-authority-results.json` records 32 individual class (d)
**guard controls**, each compiled, exited 1 and failed its intended assertion.
The [per-case ledger](credential-switch-proof-ledger.md#d25d56--stopped-authority-and-wire-guard-controls-class-d)
records every exact log path and guard mapping, including source/generated
SQL pair changes for 28 running/paused cases and one Go decoder condition
per wire mutant. Shared existing baseline refusal/absence quotes apply;
these are not pre-feature baseline proofs.

The real authenticated WorkerRoutes/pool-transaction fixture is the primary
negative-control path. Generation/release cases require an audit-free fixture
to avoid masking by outer reconciliation. Each mutant also ran
`fresh_and_replay`, which passed as an independent SQL-prepare control.
SQL negatives failed with `refusal mismatch: status=200 applied=true`;
wire negatives failed with `wire refusal status=200; want 400`.
Unchanged before `.uzi/scratch/gate-log.6TaSyI` and after
`.uzi/scratch/gate-log.XaxRVc` each exited 0, 32 passes, no skips.
The lead independently parsed all logs and verified counts. This edit read
the manifest, control script, authority fixture, SQL and decoder; it did not
execute controls or independently parse their logs.

One candidate export at a time was used serially and removed immediately;
no base build or installation was performed. The owned database container
was removed by exact name with `docker rm -fv`.

These qualify only nonce equality, claim generation/released checks without
reconciliation, frozen binding presence, the individually named markers,
claimed-status refusal, and the narrow known-extra-field/nonce-alias cases
listed in the ledger. Other statuses, ownership, lane/policy, worker-latched
bounds, op_id, minimal acknowledgement and broader preservation remain
unvalidated. The positive dda matrix is not proof of the whole predicate.

The owner resolved the
[duplicated-enforcement method](credential-switch-proof-ledger.md#duplicated-enforcement-method-boundary):
one control per negative case may neutralize all and only enumerated copies
of one semantic fence. Required evidence includes the copy census with
file/line anchors, search scope and lock trace, exact diff leaving every
other fence intact, compiling real route/store or Runner assertion red and
unchanged green. Unresolved copy coverage remains unvalidated; more than
one semantic fence per mutant remains invalid. Per-copy sufficiency controls
are optional unless the frozen criterion requires independent enforcement;
no independent sufficiency is claimed. Single-copy green is expected
redundancy, not proof of a defect. Four policy redundant-fence controls have subsequently executed as recorded
below; other controls remain required; no functional criterion is waived or
deferred, and m3/m4 remain **INCOMPLETE**.

## Stopped policy and individual Runner evidence through 47b0d8e37

The lead completed execution on a clean primary at
47b0d8e374555c504be67ded2f4960f4a6c2e6d5. This update records supplied
receipts, not a new execution or an exact-head full-suite claim.

`.uzi/scratch/stopped-policy-results.json` records four individual class (d)
**redundant-fence guard controls** at 1cbcb917f: running and paused, each
with policy_pending_fresh and unresolved_policy_outcome. Each compiled,
exited 1 at `refusal mismatch: status=200 applied=true`, and passed its
fresh_and_replay companion. Before `.uzi/scratch/gate-log.6IMNKT` and after
`.uzi/scratch/gate-log.pEa5Kp` exited 0, four policy leaves passed, no skips.
The [per-case ledger](credential-switch-proof-ledger.md#d57d60--stopped-policy-redundant-fence-guard-controls-class-d)
records each red log, exact three-file diff mapping and copy census. Two
runtime copies are the Go helper and executing generated SQL; source query
and generated counterpart are a mechanical pair, not three independent
fences. WorkerRunState → SetStateReportWithReconciliation → setStateWithTx
reaches STOP early dispatch before the generic policy guard. Worker/run
locks remain held in the same transaction through helper and SQL. The
search includes migrations 00397/00419/00420 shape constraints and the
direct route/service/query path, not exhaustive triggers/broadcast consumers.
Other fences remain intact in the exact diffs. No per-copy independent
sufficiency is claimed. These controls credit STOP settlement only;
owner request/apply policy rejection remains unvalidated.

`.uzi/scratch/qualified-runtime-controls.json` canonically merges six
individual real Runner controls: three separate persist/inspection variants
at 47b0d8e37 (runtime-safety-results-persist-per-case.json), applied-ACK at
1cbcb917f (attempt2), root reap pre-apply at 2a8fc55d2 (attempt3), and resume
classification at 47b0d8e37 (attempt5). The lead checked candidate/source and
old/new mutation hashes, typecheck exit 0, intended ERR_ASSERTION frames,
before/after greens and primary unchanged. The
[per-control table](credential-switch-proof-ledger.md#r1r6--individual-real-runner-safety-guard-controls-class-d)
records specific case, assertion, candidate and exact typecheck/red/green
paths. Each mutant compiled and failed its intended assertion with exit 1;
unchanged before/after passed. These are class (d) guard controls against
new seams; a233 runner.ts:25735–25738 refused the whole operation, not a
historically allowed operation restored by these mutations.

Persist controls cut only the failed-save block, retaining the save ternary;
each observes failedClearRequests empty instead of one (:33922). ACK cuts
the applied=true check with void ack while retaining reconcile/owned/nonce/
pending fences; continued_A appears instead of undefined (:33934). Root
reap cuts only the !reaped held block with void reaped; applies becomes 1
instead of 0 (:34099). Resume changes instanceof CodexResumeError to
Boolean(error) || !error; uncertain failure sends a forbidden fresh-thread request
(1 vs 0, :34724). Confirmed-refusal and preserve companions pass; they give
no independent preservation-guard credit. Named mode/phase coverage does
not establish tool-only reap or all consumers.

Reviewed committed fixture repairs: 2a8 observes bad-apply race settlement;
85bc observes bad fresh fallback; 47 skips the success-only prepared latch
for resumeVariant failure fixtures, since proper uncertain errors throw.
Lint `.uzi/scratch/gate-log.14E9jE`, typecheck
`.uzi/scratch/gate-log.7H3tkW` and eight selected cases
`.uzi/scratch/gate-log.X4rnyH` passed at the same content as 47. All mutants
used serial exports removed immediately, linked provisioned dependencies,
Node --test-timeout=120000 and TAP-selected names; no install or base build.

Excluded experiments remain excluded: stripped Git ENV ownership failure
before tests, ACK unused-variable typecheck failure (6qc_xh88), root
uIV7Ng timeout, resume attempt3/4 timeouts, and the mixed persist run
superseded by separate controls. Raw failed-attempt manifests remain retained;
no overall failed script receives PASS credit.

### Preservation-method clarification history

The [bounded source analysis and proposal](credential-switch-proof-ledger.md#preservation-effects--source-census-and-subsequent-method-amendment)
were an unapproved method proposal at 4db59ee48 after the bounded writer/trigger census below. A closed RUN
six-column projection might support class (d) allowlist widening like B3;
none has executed. At the direct-query seam, preservation of other tables has no existing write guard to
remove: direct STOP SQL writes RUN only and the service then rereads,
commits and broadcasts. Introducing journal/input/custody DELETE/UPDATE is
a new forbidden effect, not guard neutralization. Baseline refusal and
ordinary pause do not prove this preservation seam. The proposal is one
compiling mutant per named preservation seam introducing one narrow
forbidden effect through real route/store or Runner, other effects/fences
intact, preservation assertion red and candidate green. It is **not
approved**, supplies no functional waiver and leaves preservation rows
unvalidated. A subsequent source-only DBA census traced the valid four-field
route, effective Up migration triggers through 466 and configured broadcast
consumers. It found no effective writer to the four named target tables on
that accepted STOP path. The nearby completion-custody stamp is also fenced
by final-head normalization and independent publication identity; removing a
pause/status exclusion alone cannot supply a preservation red. The baseline
already has these guards.

The census does not say STOP has no other effects: trigger closure can write
cooldown reservations, cross-checks and other run fields, and Slack consumers
can change presentation anchors. Canonical gates are runs.gate_* plus
run_gate_presentations; question identity is runs.open_question_id and content
is kind=question run_messages. The ledger records this scope and its limits.
Accounting, trigger and presentation coverage remains a worker obligation,
not a deferred criterion. Formal owner method clarification was requested for
the proposed new forbidden-effect control, not a functional waiver. The subsequent
method amendment and first executed control are recorded below.

## Outstanding evidence

Transactional and LiveDB evidence has been re-observed, including the 50-test
c17 result above. The behavior map and later focused results do not establish
m3 or m4 completion. The full (a)/(b)/(c) method has now been restored in the
[per-seam proof ledger](credential-switch-proof-ledger.md). That ledger records
six additional executed controls and retains the two earlier ordering controls.
The owner subsequently supplied class (d) for new guards without an a233
seam: one compiling guard-removal control per negative case, through the real
switch path or real API route and store transaction. This resolves the prior
method question only. The 24 earlier stopped controls and 32 authority/wire controls above have
executed and credit only their specific seams. Four later policy controls and
six individual Runner controls add only the bounded credits above. The ledger splits credited
nonce, claim generation/release, binding presence, markers, claimed status
and narrow wire cases from remaining ownership concurrency/incarnation, other statuses,
lane/other policy, worker-latched bounds, withdrawn X/Y, broader preservation,
op_id, bounds and minimal-acknowledgement obligations.
m3 and m4 are **INCOMPLETE**; no functional criterion is accepted, waived or
deferred.

`task test:codex-m4` and lead-owned m4 security, contracts, sqlc regeneration
and integration checks remain pending. Full suites remain pending exact-head
PR CI under the approved boundary (full agent suite: exact-head PR CI).
Live cross-account thread resume with two real accounts remains owner acceptance,
unverified and non-closing. Existing optional review notes above remain optional;
this record creates no new acceptance gates.


## First authorized effect-injection sensitivity control

The owner subsequently permitted one narrow forbidden effect per named
preservation seam implemented by absence of writes. No functional criterion
was waived or deferred. E1 in the proof ledger records a single base-journal
deletion tied to changed run IDs during the first accepted stopped settlement,
through the real authenticated API route and store transaction at 4db59ee48.
Source and generated SQL add the same CTE; all fences and other effects remain
intact. The existing fixture and comparator were unchanged.

Receipt .uzi/scratch/stopped-journal-effect-results.json records unchanged
before exit 0 (gate-log.5OuGIS), compiling mutant exit 1 at
`snapshot changed in base` (gate-log.AJJe8D), and unchanged after exit 0
(gate-log.QnDTeg), under the serial handler race command quoted in E1. No skips,
SQLSTATE, build failure or timeout supplied the red. PostgreSQL trace and hashes
are recorded; the lead inspected receipt and log events. Preparation errors
before mutant execution are excluded. Export and exact owned fixture were
removed, primary unchanged. No new full-suite or security-gate claim is made.

Credit is only detection of this particular first-settlement journal deletion.
Other preservation seams, replay effects, transactional/runtime clauses and m4
remain required and unvalidated as listed. m3/m4 remain **INCOMPLETE**.


## Populated input and custody timestamp controls

Candidate 8ed61e8a2 adds separate historical applied-input and open custody
fixtures, verified nonempty and bound to the current owner/claim before first
settlement and replay through the authenticated route. Independent fixture
review found no findings. E2/E3 in the proof ledger record the exact credit:
one applied-input deletion and one custody released_at timestamp corruption,
respectively. The latter is not an actual custody lifecycle release.

The lead inspected .uzi/scratch/stopped-populated-effect-results.json, exact
mutant diffs/hashes, script and individual log events after the tester handoff
lacked a returned outer-command result. Recorded subprocess results are before
exit 0 (gate-log.jSzDhs), assertion-red input exit 1 (gate-log.p1Rpfr),
assertion-red custody timestamp exit 1 (gate-log.u8Buka), after exit 0
(gate-log.m4RCxZ). Both unchanged leaves passed without skips; each mutant
compiled and failed at its specific snapshot assertion, not infrastructure.
The receipt records final cleanup exit 0, unchanged primary and removed export.
No overall harness exit status or full-suite/security result is claimed.

All guards and other effects remained intact; controls used separate serial
exports of the exact candidate, no installs or base builds. They demonstrate
sensitivity only to the named effects during first settlement. Positive replay
checks do not independently validate replay-effect sensitivity. Actual custody
release, other preservation and transactional/runtime criteria, and m4 remain
required. m3/m4 remain **INCOMPLETE**.


## Paused policy-hold redundant-fence evidence

D61 in the proof ledger records candidate 096f614d7, with separate resolved-policy
and ordinary-hold cases through the real stopped route. Both unchanged cases pass
before and after; a narrow mutant admits only codex_policy_hold while preserving
ordinary-hold refusal, and the policy case assertion fails with status=200,
applied=true. It neutralizes the Go helper hold arm and matching SQL hold value
restriction, leaving fresh/outcome and all other fences intact.

Receipt .uzi/scratch/stopped-hold-results.json and logs under .uzi/scratch:
KW7aQb before exit 0; oxnx8K mutant exit 1; PJ29cW after exit 0. The real handler
race command, durations, copy census, exact diff/hash checks, and cleanup are
recorded in D61. The lead inspected these logs and original source hashes.
Fixture review found no findings. No installs, base build or skipped target cases.
CI timing impact remains unmeasured; full suites and m4 security are not credited.
Only paused policy-hold refusal is newly credited; other policy, lane, ownership,
preservation and runtime/transaction obligations remain required. m3/m4 incomplete.


## Stopped owner controls and repaired lane fixtures

D62/D63 record running and paused user-owner mismatch controls at
05169b18baf7af89550ea226543a1177abbf1f30. The real authenticated fixture changes
only the worker's stored owner, preserving worker identity, token, claim and run
binding; matched cohorts perform identical bookkeeping preparation. The mutant
neutralizes only the two user-owner comparisons in the stopped query, with source
and generated counterparts changed mechanically. It preserves worker-ID and all
other fences, with no per-copy sufficiency or concurrent-owner-change claim.

Receipt .uzi/scratch/stopped-owner-results-05169.json records before exit 0
(sL2joH), running and paused intended assertion-red exits 1 (0YkQk7, WLnI46),
and after exit 0 (zRS6LN). Log names use .uzi/scratch/gate-log. prefix. Each matched
positive passes on the mutant; both complete unchanged runs pass ten lane and
four owner leaves without skips. The lead inspected hashes, exact diffs and
individual log events. Cleanup exit 0, export removed, primary unchanged.

The earlier lane seed failure (42P08, CSHyW5 at 7a1054dec) is retained, not
credited as a mutant kill. The narrow UUID-cast repair passes focused check
674SKu and subsequent fourteen-leaf runs. Source fixture and repair reviews found
no findings. No production guard changed. Lane refusal assertions are positive
evidence only until their own sensitivity controls execute. Remaining m3
requirements and m4 checks remain required and incomplete; no new full-suite,
security, CI-performance or broad ownership proof is claimed.


## Stopped lane exclusion controls

D64–D73 add ten individual class (d) redundant-fence guard controls at
341c861df: running/paused for chat, judge, job, cross_check and review-target.
Each neutralizes only one semantic exclusion in the Go helper and executing
SQL, preserving every other fence. Exact three-file diffs, source/mutant hashes,
commands, timings and raw logs are in stopped-lane-results-a.json and
stopped-lane-results-b.json under .uzi/scratch/.

All ten selected real-route leaves compile and exit 1 at the intended
`refusal mismatch: status=200 applied=true` assertion, with no skipped target,
SQLSTATE, build failure or timeout. Green is reused from the actual exit-0
before/after logs sL2joH and zRS6LN at 05169b18b. Both harnesses verified the
5,946 API/fixture/Go/build input hashes and docs-only delta before reuse; the
lead independently checked current hashes, raw log hashes/outcomes, and exact
mutant reconstruction. No unchanged gate was rerun solely for reporting.

Each negative had its own serial removed export. Both recreated exact fixtures
were removed with cleanup exit 0; primary unchanged and prior evidence preserved.
The bounded worker/run-locked path and two executing copies are enumerated in
the ledger. Credit is STOP-only, not independent per-copy sufficiency or owner
request/preflight/apply/concurrency coverage. Remaining m3 and m4 obligations
stay required and incomplete; full suites/security and CI timing are not claimed.


## B publication-readiness admission control

R7 records one real Runner class (d) guard control at 0028b6249. The mutant
neutralizes only !replacementAdmissionOpen in the newly installed B root
preparation callback and retains the separate bundle-retirement fence.
The actual before-publication probe fails at its expected-rejection assertion;
this is early root preparation admission, not a claim that model work ran.

Receipt .uzi/scratch/runtime-publication-results.json and lead audit
runtime-publication-lead-audit.json record source/mutant hashes, precise diff,
TAP-selected leaf, commands and cleanup. Logs under .uzi/scratch: nbWG0z before
exit 0, B5egXj mutant typecheck exit 0, bPtPQR intended assertion-red exit 1,
9F5E18 after exit 0. The selected leaf executes; unrelated Node skips carry no
credit. The lead inspected exact TAP outcomes/frame and reconstructed the mutant.

The raw receipt's prose misnames 47b0d8e as a refusing baseline. The lead audit
corrects that citation to exact a233 runner.ts:25735–25738; raw run results remain
preserved. No baseline build or installation. Export removed, primary unchanged.
R7 does not prove capability-derived scope identity, retired-A refresh, whole
handoff or security/full suites. Other m3/m4 obligations remain required.


## Open custody owner-discard effect control

E4 adds one real stopped-route/store sensitivity control at 7e82c7be1, stronger
than the earlier timestamp-only E3 but still narrow. A single added CTE changes
the open hold to owner-discarded and clears both live references, using the four
coherent canonical disposition fields. Every guard, RUN assignment and other
effect stays intact; no test fixture changes. This is not completed-publication
release or exercise of owner API authorization.

Receipt .uzi/scratch/stopped-custody-disposition-results.json records before
exit 0 (eOP2LA), compiling mutant exit 1 at preservation test :101,
`snapshot changed in custody` (KpbaC1), and after exit 0 (bkwEJh). Logs use the
.uzi/scratch/gate-log. prefix. Named leaves execute, with no skipped target or
SQL/build/timeout error supplying the red. Lead source/mutant hash reconstruction
and raw outcome checks confirm the result; the stale pending marker does not
override recorded terminal results and cleanup exit 0. Exact fixture and export
removed; primary unchanged. Source feasibility was independently traced before
execution. E4 proves first-STOP detection of this custody disposition only.
Replay, other dispositions and other m3/m4 obligations remain required and
unvalidated; no full-suite, CI-performance or security result is claimed.


## First-STOP paused-time write control

E5 records a single accounting-field effect at d5834ca78: adding one to
budget_paused_seconds in the existing stopped UPDATE, with source and generated
SQL changed mechanically. No guard, fixture or other effect changes. The real
open_custody route/store run acknowledges settlement, then its full RUN-row
preservation assertion fails at :101, `first stop changed unrelated run fields`.

Receipt .uzi/scratch/stopped-timebank-effect-results.json records compiling
mutant exit 1 in gate-log.tRi9CC, with no skipped target, SQLSTATE, build failure
or timeout. Prior E4 greens eOP2LA and bkwEJh are reused: exact selected leaf,
command, raw log hashes, API tree and docs-only revision delta were verified.
No new E5 positive runs are claimed. The lead independently checked exact
mutation reconstruction, raw outcomes/hashes, tree equality and cleanup exit 0.
Export removed, primary unchanged. Only direct first-STOP paused-time credit
sensitivity is proved; replay/other accounting triggers and remaining m3/m4
criteria are still required and incomplete. No full-suite or security credit.


## Populated applied-switch journal control

E6 records candidate 7ff6c1644 with a real applied-X audit row, not a synthetic
or empty journal. A second Y operation preflights at epoch 42 and withdraws
without changing capability authority or recording withdrawn history. The test
requires exactly the actual X row and preserves it across first STOP and replay.
It does not relatch a failed report or claim withdrawn-X/Y coverage.

One added DELETE CTE in source/generated stopped SQL removes that journal row
only for changed run IDs. No fences, other effects or fixture code are changed.
Receipt .uzi/scratch/stopped-switch-journal-results.json records actual before
exit 0 (OkTFgQ), compiling assertion-red exit 1 at fixture :142,
`snapshot changed in switch` (8M32R4), and after exit 0 (JhaNNp). Logs use the
.uzi/scratch/gate-log. prefix. No skipped test, SQL/build error or timeout counts
as red. Lead reconstruction and raw-log checks verify the exact mutations and
results; a whitespace-only audit reconstruction error was corrected without
rerunning tests. Source fixture review found no findings, cleanup exit 0,
export removed and primary unchanged. First-STOP journal deletion only is newly
credited; replay mutation sensitivity and remaining transaction/runtime/security
criteria remain required. m3/m4 incomplete.


## Actual Runner capability-partitioned scope namespace

A3 records class (a) at e7ee697e9: exact a233 scope-key restoration from
three elements back to `JSON.stringify([runId, generation])` (baseline helper
:44). No guard or refcount changes. The strengthened real Runner Sub→Sub factory
case keeps a legitimate nonowning reference to actual A, requires resumed_B and
actual B's separate controller, and checks that a B-identity lookup finds B.
The legacy key collides with closed A and produces an explicit uncertain-hold
driver assertion before waiting for B, not a timeout or helper-only red.

Receipt .uzi/scratch/runtime-scope-key-results.json and lead audit record exact
baseline quote, mutation hashes/diff, selectors and raw outcomes. Logs use the
.uzi/scratch/gate-log. prefix: qLXsz4 seven-leaf before exit 0; 6HeSzU mutant
typecheck exit 0; HysbSl intended driver assertion exit 1; BrDwU1 seven-leaf after
exit 0. Selected leaves execute. Primary lint C9cRyH and typecheck 55Xb9J exit 0;
their source link comes from the observed parent execution and reconstructed
edit/commit sequence, not an embedded log revision header. No gate was rerun
solely to supply metadata. Source review found no findings.

Added references close even if cleanup joins reject. Export removed, primary
unchanged and provisioned dependencies reused without installs or base builds.
Only namespace availability/isolation in this retained-reference scenario is
newly credited. Unsafe B auth, other scope-mode transitions, retired refresh,
whole authority and remaining m3/m4 criteria are not claimed as complete.


## Retired A credential-reconciliation request guard

R8 records a new-guard sensitivity control at 795583992 through the actual Runner
API-key A → subscription B path. Neutralizing only assertCurrent's retirement
check lets the retained A reconcile closure make one credential-release request
using A's capability during B login. The driver fails at :35187, with releaseCalls
1 instead of 0 and result ready instead of retired; other guards remain intact.

The fixture flag releasedA records request capability equality, not response
token contents. This evidence therefore proves a retired-A request, not returned
A-token identity, real-server disclosure, unsafe B auth or subscription refresh.

Receipt runtime-retired-credential-results.json and lead audit under .uzi/scratch
record precise hashes/diff/counter semantics. Logs there: u1y7vJ mutant typecheck
exit 0, wYdX9k intended assertion-red exit 1. Prior qLXsz4/BrDwU1 target greens
are reused after raw pass/hash, agent-input and docs-only-delta checks; no new
primary green runs are claimed. Export removed and primary unchanged. All other
mixed authority and backend/refresh requirements, full suites and m4 security
remain required and incomplete.

## Post-switch release epoch and secret-hash guards

B5/B6 at ce54d754aa0257fa85bc22c2cb639b0cc297457a exercise the real worker
apply and release routes with the actual store. Separate mixed capabilities
isolate a stale epoch from an obsolete secret. B release succeeds before and
after the negative requests, with exact synthetic B token identity and no-store.

Both guards are identical to a233 codexauthz.go:680 and :686; current guards
are :693 and :699. Each preservation sensitivity control neutralizes only its
one equality check in an exact candidate export. The real release route then
returns 200 instead of 403, failing at test :150 before any service-only check.
Parsing, hash presence and every other fence remain intact. This does not
establish the final reauthorization's interleaving or apply atomicity.

Raw logs under .uzi/scratch/: x6KOS8 initial both-case green exit 0; l5v1Np epoch
control exit 1; 3vzPPG hash control exit 1; i4oWND final both-case green exit 0.
Commands use the single handler package, -race -count=1 -json -timeout=4m,
GOFLAGS=-p=1 and GOMAXPROCS=4. Both greens execute both cases without skips.
The tester's final-green command was interrupted by its child timeout with no
exit record (dkyM2l), so the lead completed that missing check on a fresh fixture.
Earlier valid results were not rerun. A t.Helper matcher correction reused the
observed red rather than repeating it.

Receipts release-fence-results.json, release-fence-finish-results.json and the
lead audit record the hashes, exact diffs, actual test actions and recovery.
All owned fixture cleanup exits are 0; exports are absent and the full tracked
fingerprint is unchanged. Source review a15d986e0..ce54d754a found no findings.
No pre-apply A-release positive, concurrent reauthorization proof, full suite
or security gate is claimed. Other m3/m4 requirements remain incomplete.


## Authenticated owner-request historical control

A4 at 21012454b1f03cf20782864392962d7f185316d3 restores a233's Codex
refusal by removing only the new service dispatch branch. Independent fixture
seeding prevents that mutation from failing before the real authenticated owner
route. All four live statuses are assertion-red at HTTP 422 versus 200; unchanged
before/after pass those four cases and the original-helper journal companion.

Corrected scratch receipt owner-request-fixed-results.json records exact diff,
hashes, commands and cleanup. Logs qKnKYY/EsWdnN/29NkvH exit 0/1/0 with no skips.
Runs use one handler package, -race -count=1 -json -timeout=4m, p=1 and GMP4.
The earlier 7lWFqQ setup-red is explicitly invalid, retained without credit.
Review found the setup defect; scoped repair review found no new findings.
Lead spot-checks verified actual HTTP assertion frames and reconstructed the
mutant hash. Export removed, owned fixture cleanup exit 0, primary unchanged.
Only request availability is credited; preservation checks are positive evidence.
Negative admission/ownership/policy controls and broader m3/m4 remain pending.


## Isolated owner policy guards

D74/D75 at e37fb6ca547800f99061b167b805fa2767106fb9 are separate class (d)
controls through the real authenticated owner route and store transaction.
One neutralizes pending-fresh only; one neutralizes unresolved-outcome only.
The opposite arm and hold condition remain intact. Each fails at the HTTP
assertion (200 versus 409); unchanged before/after pass both cases, including
resolved preserving companions. Existing predicate code does not supply a
historical allowed owner seam: a233 rejected Codex overrides wholesale.

Scratch owner-policy-results.json and owner-policy-lead-audit.json retain
exact diff/source hashes, verified raw actions and commands. Logs PMrcTX,
308UIB, Q6yexN, 2Gi9w5 (gate-log. prefix) exit 0/1/1/0 with no skips or credited
setup/build/SQL failures. Runs are serial handler-only, -race -count=1 -json
-timeout=4m, GOFLAGS=-p=1 and GOMAXPROCS=4. Exports removed, exact owned fixture
cleanup exits 0, primary fingerprints and prior receipts unchanged.
Source review found no findings. Snapshot preservation is positive evidence;
apply-time policy, paused hold admission, security and other m3/m4 checks are
not discharged by these two controls.
