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


## Apply policy after successful preflight

D76/D77 at 183c2755d7d2ec79a0f91f0067a81f44ae90e2f3 independently neutralize
pending-fresh and unresolved-outcome on the new Codex apply seam. Policy is
installed after actual READY while X remains pending. Each mutant reaches
actual worker apply and fails at test :100 on applied instead of not_applied;
HTTP 200 is intentional. The unchanged refusal contains only outcome/reason
and leaves all six snapshots equal. After fixture resolution, the same X
successfully binds B at epoch 42 and journals ordinal 1.

Baseline evidence combines the explicit Runner/owner Codex refusals with the
old apply's Anthropic chooser/lock at :327/:337. No explicit raw worker harness
rejection is claimed. This is class (d) new-seam guard evidence, not restoration
of a historical allowed Codex apply.

Scratch apply-policy-results.json and apply-policy-lead-audit.json record exact
diffs, reconstructed hashes, raw actions and cleanup. Logs HMqCw7/L840PQ/jaLtWN/
ePZY7K (gate-log. prefix) exit 0/1/1/0; both greens pass both leaves without
skips. Runs are serial, handler-only, -race -count=1 -json -timeout=4m, p=1/GMP4.
Exports removed; owned fixture cleanup exits 0; primary and prior receipts
unchanged. Source review found no findings. No owner-to-worker concurrency,
atomicity, full-suite, security or broader m3/m4 completion claim is made.


## Subscription A coordination and intent controls

D78/D79 at 09342accbe0cecb5860812a76f1329ff2aeec7e0 independently exercise
real worker apply after READY with subscription A and static API-key B.
A selective coordination control admits only the valid in_progress tuple in
addition to the original idle tuple, retaining intent/reauth/recovery checks.
The separate intent control changes only unresolved==0. Each reaches the
actual outcome assertion at :164 (applied instead of not_applied). Before/after
pass both new leaves and the original API-key helper journal companion.

Scratch apply-refresh-results.json and apply-refresh-lead-audit.json record
diffs, independently reconstructed mutant hashes, verified raw log actions and
cleanup. Logs jc9KJV/MtvoZU/S7TURV/z4Dm1C (gate-log. prefix) exit 0/1/1/0,
without skips or credited setup/build/SQL failures. Runs are serial handler-only,
-race -count=1 -json -timeout=4m, p=1/GMP4. Exports removed, owned fixture cleanup
exit 0, primary/prior receipts unchanged. Source and concrete-SQL reviews found
no findings. Synthetic states isolate the fences; this is not provider refresh
admission, worker reconciliation ordering, multi-account locking, atomicity,
full-suite, security or m3/m4 completion evidence.

## Default-branch merge and migration renumber (2026-11-17)

- Merge: `git merge 210a2232d122ea8a8b1b68d1d573e634b51d0147` landed as
  9b005c4c0 with no conflicts. `task check:sqlc-drift` on it: EXIT=0
  ("api/internal/store matches `sqlc generate` output").
- Owner decision 12 (narrow exception): the renumber helper's adjacent
  `.renum.*` / `.renumber-tmp-*` transient paths are permitted for this one
  step. Command, with the ancestry and clean-tree guards intact:
  `TMPDIR=$PWD/.uzi/scratch/tmp-renum MIGRATION_RENUMBER_NO_FETCH=1 task migration:renumber`
  (EXIT=0). It renamed `00465_codex_credential_epoch_snapshot.sql` to
  `00466_…` and `00466_codex_pending_switch.sql` to `00467_…`, above main's
  `00465_repo_initial_import.sql`. Afterwards `find … -name '.renum*'` was
  empty and `git status --porcelain --ignored` on the migrations directory
  showed only the two renames.
- Postflight references reviewed by migration identity: the ARCHITECTURE.md
  repo-initial-import paragraph names main's `00465` and stays unchanged; the
  proof ledger's "00465 Codex snapshot additions" names this branch's former
  `00465`.
- Owner decisions 11 and 12 are owner answers given at the #4202 successor
  plan gate on 2026-11-17 and are recorded in the approved plan.
- Earlier ledger prose (this file and the proof ledger) that says 464/465/466
  is evidence recorded at the time, including present-tense wording such as
  "new in migration 466", "fixture database to version 464" and "Up migration
  triggers through 466": read the Codex snapshot as 466 and the
  pending target as 467 from this point. The migration LiveDB test now
  downgrades to 465 and asserts that main's 464 and 465 columns survive.

## Decision 11 recovery controls and crash point 5 (2026-11-18)

Ledger U21: on same-worker retained recovery after crash points 1–4 (API-key A
to subscription B), the four crash leaves (now 32 with the survivors/unverified
modes) show delivered Codex material is not released or refreshed through the
executor's provider client before the `predecessor_capture` proof (insertion
controls, 32 of 32 red each). When the proof reports survivors or unverified,
the executor never runs and the run takes the `quiescence_failed` blocker
(gate and blocker controls, 8 of 8 forced leaves red each), all class (b)
preservation. The tests observe non-use only; "discarded" is not shown
observably. The eight forced leaves take about 1.1–1.6 s each (observed
1.08–1.58 s, about 10.5 s in the u21r green log); shard time before and after
is unmeasured. On `quiescence_failed` the run is reported `failed`
(`worker_residue_blocked`) with its clone and custody kept; it is a terminal
failure, not an owner pause.

Owner decision on crash point 5 (predecessor host unreachable), 2026-11-18:
recorded scope decision, **unvalidated and uncredited**, an environmental
limitation. Fencing refuses stale uzi operations, but a different B credential
and revoking A's uzi capability do not stop a provider credential that an
unreachable predecessor already holds. No option-B protocol work on this run.

The owner's stated requirement is that B is never used while predecessor
quiescence is unproven, and that the run is held for the owner instead.
"Held for the owner" is the owner's wording; on the same-worker path above the
run is a terminal `failed` report with clone and custody kept, not an owner
pause. **The code does not enforce that requirement on the cross-worker path.**
A stale worker's run is requeued (`RequeueRunsOfStaleWorkers`); `ClaimRun`'s
affinity pin lets another of the user's workers claim it once the owner is
heartbeat-stale, not draining or in maintenance, and past the stale-requeue grace, after the affinity ceiling (default 2h),
or when the worker row is deleted; `codexClaimSecrets` then mints a fresh
capability and delivers the run's current binding (B after an applied switch).
The new worker has no retained journal, so `if (retainedJournal)` in
`agent/src/runner.ts` is false, `recoverRetainedClaim` is skipped, the
`predecessor_capture` / `quiescence_failed` gate never runs, and the run
proceeds with B. B can therefore be used while an unreachable predecessor may still hold provider credential A, and B too if the switch applied on that host (the apply response delivers B's material: a static API key, or a subscription access token until it expires). Without a switch the same path delivers A to
the new worker; the Codex claim path (`codexClaimSecrets`) and `ClaimRun`'s affinity pin have no switch-specific logic (read from the code, not executed). U21 evidences the gate only for same-worker retained
recovery. The cross-worker path is unvalidated and uncredited, and the gap goes
to the owner as a formal decision; fail-closing it (for example refusing or holding a cross-worker Codex claim while the predecessor is unproven) is an option for the owner, its size not assessed, and outside the approved plan. Continuation is
never credited because the host is gone. This covers crash point 5 only.

## m3.2 candidate-level checks (2026-11-18)

Run serially by the lead, one command at a time, on the same worker with a warm
cache. Logs are under `.uzi/scratch/ctl-logs/m32-*`. All but five end with
`SHA=<tested revision> EXIT=<status>`; the five without it are
`m32-lint-web`, `m32-uzi-focused`, `m32-vitest-ccs-focus1`,
`m32-vitest-ccs-focus2` and `m32-vitest-ccs-2`, whose tested revision is not
recorded in the log.

- `task test:codex-m4` at 9555784fc: 209 pass, run-completeness OK, EXIT=0
  (121 s).
- `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` at 9555784fc: EXIT=0. No
  provisioning/lockfile mismatch.
- `task check:api` at 9555784fc: EXIT=0 (lint 0 issues, deadcode clean).
- `-race -count=1`, one package at a time, without `UZI_TEST_DATABASE_URL`
  (LiveDB tests compile and skip; their focused runs are recorded per unit on
  `cdr-*`):
  - workersvc, store and handler: ok at 9555784fc. `api/internal/capability`,
    which this branch touches (`capability.go` and tests), is not run with `-race` in
    this run; it is covered by `test:api` in exact-head PR CI;
  - `api/cmd/uzi` failed at 9555784fc in `TestPrintedInstructionsAreRegistered`
    (the set-token help and the ambiguous-label error named "`uzi token`" for
    renaming, which is web-only and has no registry entry). The fix at
    ca76b1a31 names the web UI; the same test is the regression test (red at
    9555784fc, green at ca76b1a31). The package then passed at ca76b1a31, and
    `task lint:api` reported 0 issues.
- `task check:web` at ca76b1a31: EXIT=0.
- Touched Vitest files:
  - `RunCredentialOverride.test.tsx`: 21 pass.
  - `CodexCredentialSwitch.test.tsx`: 1 of 39 failed in the full-file run at
    ca76b1a31. The focus check raced `submit`'s `busy` reset; it passed alone
    twice (`m32-vitest-ccs-focus1`/`-focus2`, 2 passed and 37 skipped each, no
    recorded revision: by file time they predate the e0287607a commit only).
    `m32-vitest-ccs-2` (39 passed) is likewise revision-unrecorded. The test now waits for focus (e0287607a, test only, component
    unchanged), and all 39 pass at e0287607a.
  - `CodexCredentialSwitch.browser.test.tsx` (Chromium): 10 pass at e0287607a.
  - d8717dd28 applies the same focus `waitFor` to
    `CodexCredentialSwitch.browser.test.tsx` (the "seeds ... Header" case).
    Chromium 10 of 10 and jsdom 39 of 39 pass on that tree (logs under
    `.uzi/scratch/gate-log.*`). Note: the Chromium log's `SHA=` line reads
    `de0da0237+wt` (the parent plus the then-uncommitted edit), and the jsdom
    log records no revision.
- The 16 agent test files this branch touches, each run alone with
  `--test-timeout=120000`, all pass at ca76b1a31, including
  `codex-executor.test.ts` (1966 tests, 863 s) and `worker.test.ts`.

The full agent unit suite, `test:api`, `test:web`, `test:web-browser` and the
run-store-it LiveDB sweep remain pending exact-head PR CI. These are landing
obligations, not claimed here.

Residual clauses: the m3.1 units U1–U21 are complete. The batched owner question
on clauses that fit no proof class, and on the "not controlled" residues in
the ledger's Clause-accounting table, could not reach a human. The lead's
stated assumption is to record them all as unvalidated and uncredited, run
m3.2 and m4, and request the owner's formal partial decision at completion.
The owner's partial decision (2026-11-19) defers m3; see "Owner partial decision and deferred m3 checklist (2026-11-19)".

## Final inventory (m4, 2026-11-18)

An index over evidence already recorded in
[the proof ledger](credential-switch-proof-ledger.md) (its "Clause accounting"
table and the U1-U21 sections) and in the sections above. It grants no new
credit. "Partly credited" is this index's own summary label, not ledger
wording; it means only the controls named in the ledger row, and every "not
controlled" residue in that row stays unvalidated. Symbols were
checked with grep at this tree; line numbers drift, so only the symbol and file
are given. A cell reading "not found in ledger" means no ledger test or control
names that surface.

### 1. Authority inventory

| Authority | Surface and route | Auth or fence that admits it | Code symbol (file) | Ledger tests and controls |
| --- | --- | --- | --- | --- |
| Owner request | `POST /api/runs/{id}/credential` | Owner-scoped run read, live status, target ownership, kind and enablement; U1 authenticates through CLI Bearer | `Handler.SetRunCredential` (api/internal/handler/run_credential.go); `Service.SetRunCredential`, `requestCodexCredentialSwitch` (api/internal/workersvc/run_credential.go) | A4 `TestWorkerCodexOwnerLiveRequestRouteLiveDB`; U1 `TestWorkerCodexOwnerRequestAdmissionRefusalsLiveDB`: u1-claimed, u1-status1, u1-usable, u1-anthropic, u1-foreignsecret, u1-foreignrun, u1-disabled; U7 and D74/D75 policy; U13 owner negotiation via `TestOwnerCodexSwitchUnsupportedWorkerLiveDB`: u13-own-negotiated, u13-own-stopped, u13-own-heartbeat, and u13-neg-cap, u13-neg-reg (mutate `codexSwitchNegotiated`; red at the owner-request unsupported assertion, :58). The negotiation conjunct of `codexSwitchFence` and the apply Step 0 copies are not controlled. Cookie-session path distinct from CLI Bearer: not found in ledger |
| CLI | `uzi run set-token` to the same route | Client-side kind scoping, then the owner request fences above | `newRunSetTokenCmd` (api/cmd/uzi/run_limits.go) | U1 `TestRunSetTokenHarnessKindsAndAmbiguity`: u1-cli-kind (client side only) |
| Web picker | `RunCredentialOverride` renders `CodexCredentialSwitch` for Codex runs | Client-side kind filter, then the owner request fences | `RunCredentialOverride` (web/src/components/RunCredentialOverride.tsx), `CodexCredentialSwitch` (web/src/components/CodexCredentialSwitch.tsx) | U1 `CodexCredentialSwitch.test.tsx`: u1-web-kind (client side only) |
| Worker preflight | `POST /api/worker/runs/{id}/credential-switch/codex/preflight` | Worker Bearer join token, claim generation, register nonce, released flag, live status, negotiated switch and stopped support | `Handler.WorkerCodexSwitchPreflight` (api/internal/handler/worker_protocol.go); `validateCodexSwitchLocked`, `codexSwitchFence`, `codexSwitchNegotiated` (api/internal/workersvc/credential_switch_codex.go) | `TestCodexSwitchPreflightLiveDB`, `TestWorkerCodexSwitchPreflightRouteLiveDB`: u12-fence-nonce, u12-fence-gen, u12-fence-released, u12-fence-status, u12-op-mismatch, u12-target-mismatch, u12-target-moved-rev, u12-target-moved-acct (red at the preflight assertion of `TestCodexApplyTargetMovedAfterPreflightLiveDB` (api/internal/workersvc/credential_switch_codex_apply_livedb_test.go, :168, "preflight accepted moved target"); apply not exercised), u13-preflight-stopped, u11-baseline-missing, u11-baseline-match; U7 `TestWorkerCodexPolicyEpisodeBeforePreflightRouteLiveDB` (api/internal/handler/codex_switch_apply_policy_routes_livedb_test.go): u7-preflight-fresh, u7-preflight-outcome. The U8 lock controls are listed on the apply row, not here |
| Worker apply | `POST /api/worker/runs/{id}/credential-switch/apply` | Same fence plus op id, target revision and the owning-worker query | `Handler.WorkerCredentialSwitchApply` (worker_protocol.go); `applyCodexSwitchLocked` (api/internal/workersvc/credential_switch_codex_apply.go) | `TestCodexTransactionalApplyBundleLiveDB`, `TestCodexApplyWriteRollbackLiveDB`, `TestCodexApplyZeroRowWriteRollbackLiveDB`, `TestCodexApplyForeignWorkerFencedLiveDB`, `TestCodexReplayCommittedOpUnderNewGenerationFencedLiveDB`, `TestCodexApplyCommittedLostReplyLiveDB`: u10-binding-n, u10-stamp-n, u10-message-ins, u10-lastseq-n, u10-commit-check, u10-return-before-commit, u10-mint-hash, u10-journal-err, u10-mint-redundant, u12-replay-codex, u12-replay-nomaterial, u12-foreign-owner, u12-replay-gen, u11-ordinal-genscope, u11-ordinal-n1, u11-ordinal-skip, u11-epoch-indep, u11-attrib-clamp. U8 lock controls (`TestCodexApplyAliasRowContentionLiveDB`, `TestCodexApplyReverseOrderReloginWriterLiveDB`, `TestCodexApplyRotatingIntentRowLockLiveDB`; apply arm; shared `validateCodexSwitchLocked`, preflight path not exercised): u8-alias-share, u8-alias-nowait, u8-enable-share, u8-enable-nowait, u8-account-call, u8-account-nowait, u8-busy-map, u8-intent-redundant (each red through the apply tests, "apply was APPLIED" or the missing busy decision). The target-moved controls u12-target-moved-rev and u12-target-moved-acct are not credited here: they went red at the preflight assertion, so the apply assertion (:172) was not exercised |
| Per-claim Codex capability | Minted on apply and on each fresh claim; A's revoked on apply | Run-scoped capability plus owning worker | `mintCodexCapability`, `codexClaimSecrets` (api/internal/workersvc/codexauthz.go) | Mint on apply: u10-mint-hash, u10-mint-redundant; A3 scope namespace; R8 retired A reconcile guard; B5/B6 (`TestWorkerCodexSwitchReleaseFencesRouteLiveDB`). Mint on a fresh claim: same-worker is exercised (U21, U11, U12 tests) but not separately controlled; cross-worker is traced only, not tested; see crash point 5 and section 4 |
| Refresh bridge and release | `POST /api/worker/runs/{id}/codex/refresh` and `/codex/release` | Capability, owning worker, authority epoch and secret hash | `CoordinatedCodexRefresh`, `ReleaseCodexCredential` (api/internal/workersvc/codexrefresh.go); `refreshCodex`, `releaseCodex` (agent/src/client.ts); `createCodexEpochAuthority` and the refresh wrapper (agent/src/codex/codex-executor.ts) | U5: u5-bridge-pre, u5-identity; U9 `TestCodexReleaseRecheckRefusesAfterSwitchApplyLiveDB`: u9-recheck, u9-recheck-relocate; B5/B6; U21: u21r-exec-release-before-proof, u21r-exec-refresh-before-proof. `createRunLaneInstallerReconcile` (agent/src/codex/codex-executor.ts, the installer-before-credential reconcile): U8 maps it through `capturedReconcile` / `shared.reconcile`, built by `buildRunLaneReconcile`, with control u8-reconcile-after-teardown (c); the ledger does not name `createRunLaneInstallerReconcile` itself |
| Owner-scoped refresh (usage) | The usage poller's read of a linked subscription account; no HTTP route reaches it (`/me/codex-rate-limits` only reads stored rows) | Owner principal, then `coordinatedRefresh`'s lease and intent and the generation retry. No run, epoch, owning-worker or switch fence: its comment says "owner-scoped principal path, not a worker capability". It can rotate the provider material behind A or B during a switch | `handleCodexUsageUnauthorized`, reached from `CollectCodexAccountUsage` (api/internal/workersvc/codexusage.go), called by the usage poller engine (api/internal/codexusagepoller/engine.go) | Not found in ledger. U8's lease and intent controls (u8-admitted-refresh-redundant via `TestCodexRefreshAdmittedBeforeApplyLiveDB`) drive the shared `coordinatedRefresh` core through the worker wrapper `CoordinatedCodexRefresh`, so they show the lease and intent block apply but do not exercise this owner-scoped caller |
| Fresh-claim pending expiry | `claimCheckpointRun` (worker claim) | Claim savepoint plus run lock; on a fresh claim whose pending stamp is from an older generation it clears the stamp and writes a not_applied `expired_by_worker_loss` message; fails closed by rolling back the whole claim | `expireCodexSwitchOnFreshClaim` (api/internal/workersvc/credential_switch_codex_expiry.go), called from api/internal/workersvc/checkpoint_coordinates.go | Not found in ledger. Tests named in the plan, present in the tree and not run or mutated here: `TestCodexSwitchWorkerLossClaimIntegrationLiveDB`, `TestCodexFreshClaimPendingExpiryLiveDB`. Relevant to crash point 1 (a pending stamp can survive A teardown) and crash point 5 (cross-worker reclaim) only when recovery is a fresh claim; same-generation readoption never calls it (see the notes in the crash point table) |
| Now and summary | Frames to `NowSummaryController`; executor summary factory | Switch gate and executor authority guard | `NowSummaryController`, `switchGate` (agent/src/now-summary.ts); summary `assertAuthority` (codex-executor.ts) | U5: u5-summary-redundant (executor summary guard only). U20: Now `switchGate` and B refresh suspension are not credited; plan and intent summaries are now exercised at the publication barriers by #4573 leaves (behaviour tested; the plan-summary callback is bound to its admitting authority, commit 5288bdce6), with individual-fence proofs for P2, I2, I3 and I1b post isolation uncredited (fence matrix in the ledger) (diagnostic rows u20-now-gate, u20-exec-summary-switch, u20-exec-summary-both, u20-summary-all, u20-now-gate-lifecycle, u20-now-switch-gates; u20r-b-refresh is a timeout red) |
| Stopped settlement | `POST /api/worker/runs/{id}/state` with `codex_switch_stopped`; `credential_switch_failed` is a separate state on the same route | Worker Bearer, claim generation, expected epoch, register nonce, owning worker, marker and status fences | `Handler.WorkerRunState` (worker_protocol.go); `setStateWithTx` (api/internal/workersvc/service.go); `stoppedSettlementAck` (worker_protocol.go) | D1-D73 stopped guards; U13: u13-w-latch-frozen, u13-w-latch-successor, u13-w-withdrawal; U14 u14-*, u14r-*; U15 u15-*; U16 u16-*, u16r-*; U17 u17-*; U18 u18-*; U19 u19-ack-leak, u19-log-leak. `credential_switch_failed` on the same route: u16-stale-withdraw (b) mutates the op conjunct of the op-carrying withdrawal, and only that withdrawal is credited; the op-less v1 withdrawal is unfenced by op and not controlled |
| Recovery | Runner recovery of a retained same-worker claim | Predecessor proof (`predecessor_capture`) and `quiescence_failed` gate | `recoverRetainedClaimBody` (agent/src/runner.ts) | U21: u21r-exec-release-before-proof, u21r-exec-refresh-before-proof, u21-release-before-proof (Runner-client counter only), u21-gate (rerun u21f-gate), u21r-blocker; U6 crash controls. Cross-worker claim: known unfenced, not merely untested: the code does not enforce predecessor quiescence there (a new worker has no retained journal, so the gate does not run); unvalidated, see section 4 |
| Redactor | Log and run-message redaction of B material and the apply result | Registration of delivered tokens | `flight.secrets.add` and `this.log.addSecret` in the switch loop, canaries (agent/src/runner.ts); `CredentialSwitchApplyResult.LogValue` (api/internal/workersvc/credential_switch_apply.go) | U19: u19-agent-log-redactor, u19-agent-message-redactor, u19r-log-value (`TestWorkerCodexApplyLogRedactionRouteLiveDB`), u19-ack-leak, u19-log-leak, u19r-ack-leak, u19r-log-leak (`TestWorkerCodexStoppedTokenAbsenceRouteLiveDB`). The executor's second log registration and the remaining surfaces: not controlled, owner batch |

### 2. Requirement to test to control map

Status summarizes the ledger's Clause-accounting entries ("partly credited" is this
index's own label) and is not a new assessment. No
requirement below is fully credited: each carries the residual the ledger names.

| # | Requirement | Tests | Controls (class) | Status | Residual named in the ledger |
| --- | --- | --- | --- | --- | --- |
| U1 | Owner request admission | `TestWorkerCodexOwnerRequestAdmissionRefusalsLiveDB`, `TestRunSetTokenHarnessKindsAndAmbiguity`, `CodexCredentialSwitch.test.tsx`; A4 | u1-claimed, u1-status1, u1-usable, u1-disabled (d); u1-anthropic, u1-foreignsecret (redundant-fence); u1-foreignrun (b); u1-cli-kind, u1-web-kind (client-side d) | partly credited | pool_wait, terminal statuses, pinned-without-secret, lane, no-live-worker, unsupported-worker and foreign-subscription refusals unvalidated; no control proves the no-write snapshot comparison; stamp SQL's own status filter; the non-live-run copies (`applyLiveStatus` and the stamp SQL's status filter) have no redundant-fence control |
| U2 | Same flight, generation, worktree | `M2 actualRunner api_key to subscription factory account resumes saved thread`, `Codex real Runner switches A to B to C without a new claim or attempt` | u2-claim, u2-custody, u2-transition, u2-worktree (effect-injection). Baseline test: all red (u2-attemptdir included). A to B to C test: only u2-claim, u2-custody and u2-transition are red; u2-worktree is green there (it checks the clone path, not `flight.worktreePath`) and u2-attemptdir is red but not credited | partly credited | u2-attemptdir has no A to B to C credit; new flight object, `reserveRecoveryCapture`, `settleRecoveryHold(Live)`, clone seeding and the `fresh_B` return not separately controlled; claim generation assertions not shown sensitive; a plan-revision `awaiting_approval` restore (`restartAndResume`, `epochSourceAdmission`, `PlanRevisionRecovery.beforeTurn`, only with an active silent revision) is not covered by u2-transition |
| U3 | Strict persistence variants | `Codex real rollout persist failure before teardown (<variant>)`; R1-R4 | u3-owner-before, u3-op-before, u3-target-before, u3-owner-after, u3-pending-after (d); u3-nonce-redundant (redundant-fence) | partly credited | pending-absent-before, hold/status/generation legs of `owned`, lost-reply clear and reopen's other guards not controlled |
| U4 | A reap before B, fresh CODEX_HOME | `tool-reap-unproven`, `root-reap-unproven`, baseline api_key to subscription; R5 | u4-exec-reap, u4-epoch-index (d); u4-after-reap (redundant-fence); u4-fresh-home (b) | partly credited | in-flight tool callbacks and pending launches at teardown, launcher must-not-exist check, "created after reap" beyond the combined fence |
| U5 | Authority retirement of A | codex-executor.ts probes P1-P4 (four cases) | u5-bridge-pre (d); u5-summary-redundant (redundant-fence); u5-identity (b) | partly credited | A-account rejection under subscription to subscription, bridge post-call copy, `assertRefreshAdmission`, Now stale-result drops not controlled |
| U6 | Exact saved-ID resume, lineage notice | real Runner resume and crash cases in codex-executor.test.ts | u6-resume-id (d data-flow); u6-lineage-notice, u6-crash-missing-id (d); u6-preserve-3copy (redundant-fence); u6-crash-preserve-saved (b); u6-preserve-redundant, u6-crash-preserve-attempt (cause attribution only) | partly credited | confirmed-refusal classification (timeout reds, uncredited), the `sessionId !== saved` check, `session_missing` on the switch path, three of four crash publications not mutated |
| U7 | Policy at request and apply | `TestWorkerCodexPolicyEpisodeBeforePreflightRouteLiveDB`; D74-D77 | u7-preflight-fresh, u7-preflight-outcome (d) | partly credited | hold arm unreachable in live statuses (`runs_policy_hold_shape`); paused hold refused by status (u1-status1); concurrent lock-order contention not controlled |
| U8 | Pre-revoke reconciliation, lease and intent, lock order | `TestCodexApplyAliasRowContentionLiveDB`, `TestCodexApplyReverseOrderReloginWriterLiveDB`, `TestCodexApplyRotatingIntentRowLockLiveDB`, `TestCodexRefreshAdmittedBeforeApplyLiveDB`; D78/D79 | u8-alias-share, u8-alias-nowait, u8-enable-share, u8-enable-nowait, u8-account-call, u8-account-nowait, u8-busy-map (d); u8-intent-redundant, u8-admitted-refresh-redundant (redundant-fence); u8-reconcile-after-teardown (c) | partly credited | per-account lock isolation, target account's intent check, refresh admission's binding recheck in isolation, global lock sorting; the "no A call after reconcile" assertions are asserted only (no control); other writers not covered |
| U9 | Final reauthorization timing | `TestCodexReleaseRecheckRefusesAfterSwitchApplyLiveDB`; B5/B6 | u9-recheck (b); u9-recheck-relocate (c) | partly credited | API-key A path and the vault-locked branch after a switch; an apply that commits after the final reauthorization passed, before the response is written, can still return A's token (the control covers one point of the window) |
| U10 | Transactional binding rewrite, material after commit | `TestCodexTransactionalApplyBundleLiveDB`, `TestCodexApplyWriteRollbackLiveDB`, `TestCodexApplyZeroRowWriteRollbackLiveDB` | u10-binding-n, u10-stamp-n, u10-message-ins, u10-lastseq-n, u10-commit-check, u10-journal-err (d); u10-mint-hash (d behavioural); u10-mint-redundant (redundant-fence); u10-return-before-commit (c) | partly credited | per-statement server-error returns (argued equivalent), empty-AccessToken check, the caller's deferred rollback; storage and power-loss durability (process-loss model only); concurrent-reader isolation beyond Postgres READ COMMITTED |
| U11 | Ordinal and epoch attribution | `TestCodexSwitchPreflightLiveDB`, `TestCodexTransactionalApplyBundleLiveDB`, `TestCodexSwitchSpendJoinsAuthorityEpochLiveDB`, `TestCodexSwitchOrdinalRestartsPerGenerationLiveDB` | u11-baseline-missing, u11-baseline-match, u11-ordinal-genscope (d); u11-ordinal-n1, u11-ordinal-skip, u11-epoch-indep (d behavioural); u11-attrib-clamp (b) | partly credited | per-field baseline comparisons, epoch<=0 refusal, uncommitted-ordinal and generation-scope clamp branches on the Codex path, cross-generation spend joins |
| U12 | Rollback, lost-reply replay, stale or foreign apply | `TestCodexApplyCommittedLostReplyLiveDB`, `TestCodexApplyForeignWorkerFencedLiveDB`, `TestCodexReplayCommittedOpUnderNewGenerationFencedLiveDB`, `TestCodexSwitchPreflightLiveDB`, `TestCodexApplyTargetMovedAfterPreflightLiveDB` (api/internal/workersvc/credential_switch_codex_apply_livedb_test.go); rollback by U10 | u12-replay-codex, u12-fence-nonce, u12-fence-gen, u12-fence-released, u12-fence-status, u12-op-mismatch, u12-target-mismatch, u12-target-moved-rev, u12-target-moved-acct (d; the two target-moved controls ran through `TestCodexApplyTargetMovedAfterPreflightLiveDB` (api/internal/workersvc/credential_switch_codex_apply_livedb_test.go) and went red at its preflight assertion :168, so its apply assertion :172 was not exercised); u12-replay-nomaterial (redundant-fence); u12-foreign-owner, u12-replay-gen (b) | partly credited | preflight's own non-owner refusal, validator replay copy, Step 1 worker check, apply Step 0 copies, other fence conjuncts, stamp checks, target sub-checks, old-alias refusals |
| U13 | Stopped-support negotiation | `TestOwnerCodexSwitchUnsupportedWorkerLiveDB`, `TestWorkerCodexSwitchPreflightRouteLiveDB`, codex-switch-preflight.test.ts, client tests, executor latch cases | u13-own-negotiated, u13-own-stopped, u13-own-heartbeat, u13-neg-cap, u13-neg-reg, u13-preflight-stopped, u13-w-unknown-reason, u13-w-admit-cap, u13-w-admit-feature, u13-w-latch-frozen, u13-w-latch-successor (d); u13-w-withdrawal (d behavioural) | partly credited | the runner's own admission check, fence/Step 0 negotiation copies, null-heartbeat and registration sub-conjuncts, in-place conjunct, identical-tuple replay, first-READY latch, unchanged-stamp assertions, worker-side `worker_upgrade_required` reason |
| U14 | Stopped pending input and wall budget | `TestWorkerCodexStoppedGuardsRouteLiveDB` | u14-applied, u14-superseded, u14-cutoff, u14-cutoff-kind, u14-interactive, u14r-unstarted-isolated, u14r-cutoff-boundary, u14r-cutoff-equal, u14r-coalesce (d); u14-running-clock, u14-extension, u14-finalize, u14-paused-secs, u14-default-timeout (d behavioural) | partly credited | awaiting_* statuses, paused-run carve-out variants, cancel/reject_plan under the cutoff, cutoff run correlation, chat/judge exemption (unreachable), `RunTimeout` value, future-`started_at` clamp, production default |
| U15 | Stopped bounds, ownership, statuses | `TestWorkerCodexStoppedAuthorityBoundsRouteLiveDB`, `TestWorkerCodexStoppedOwnershipRouteLiveDB`, `TestCodexSwitchStoppedAuthorityBounds` | u15-gen-edge, u15-epoch-edge, u15-nonce-length, u15-nonce-edge, u15-nonce-nul, u15-status (d); u15-gen-lower, u15-gen-upper, u15-epoch-lower, u15-epoch-upper, u15-nonce-empty, u15-recon-generation, u15-recon-released, u15-foreign-worker (redundant-fence); u15-binding-fk (b); u15-utf8 (unit, supporting only, uncredited) | partly credited | NULL snapshot nonce, reconciled `*g <= 0` copy, binding identity beyond ownership (no predicate clause; owner batch) |
| U16 | Stopped markers, competing controls, withdrawn X/Y | `TestWorkerCodexStoppedWithdrawnXYRouteLiveDB`, `TestCodexSwitchStoppedLateControlLockOrderLiveDB`, `TestCodexSwitchStoppedRunningPolicyHoldLiveDB`, `TestCodexSwitchStoppedResumeReclaimReregistrationLiveDB` | u16-y-markers, u16-resume, u16-locks, u16r-run-locks, u16r-locks, u16-policy-hold (redundant-fence); u16-reclaim, u16-registration (d); u16-stale-withdraw, u16-worker-lock, u16r-worker-lock (b) | partly credited | op-less v1 withdrawals unfenced by op; xy_settles positive only; paused-origin replay under concurrency, owner writers that take no worker or run row lock |
| U17 | Stopped preservation | `TestWorkerCodexStoppedStatePreservationRouteLiveDB`; E1-E6 | u17-session, u17-gate-run, u17-question-id, u17-gate-presentation, u17-question-message, u17-replay-run, u17-replay-presentation (effect-injection) | partly credited | other gate columns and other replay tables compared but not individually injected; Slack, `cross_checks` and other trigger-closure tables outside the snapshot |
| U18 | Bounded strict stopped request | `TestWorkerCodexStoppedRequestShapeRouteLiveDB`, `TestWorkerStateStoppedStrictBody`, `TestStoppedSettlementAck`, `TestWorkerRunStateCodexSwitchStoppedRoute` | u18-body-bound, u18-body-edge, u18-trailing, u18-minimal-ack, u18-ack-full-run, u18-ack-extra-key (d); u18-op-id, u18-unknown, u18-epoch-other (redundant-fence) | partly credited | other statuses and the envelope key count; null/missing fields and case aliases beyond D25-D56 |
| U19 | Token absence | `TestWorkerCodexStoppedTokenAbsenceRouteLiveDB`, `TestWorkerCodexApplyLogRedactionRouteLiveDB`, executor leaves in codex-executor.test.ts | u19-agent-log-redactor, u19-agent-message-redactor, u19r-log-value (d); u19-ack-leak, u19-log-leak, u19r-ack-leak, u19r-log-leak (effect-injection) | partly credited | executor's second log registration; `flight.runScopedSecrets.push` is not controlled; the withdrawn target's absence from the stopped acknowledgement and server log is asserted only; remaining surfaces are guardless by construction and go to the owner batch; m4 security gates remain required |
| U20 | Publication ordering and admission | the two "publication gates Now, retired A and B preparation children" leaves, eleven "partial B publication never reopens A" leaves, "startup refresh stays deferred until B finish", six baseline leaves; R7; #4573 plan summary leaves (gated-plan-summary, -late, -unacked) and intent summary leaves (gated-intent-summary, -retained, -late, resume-refuse-preserve-intent-summary; the gated variants run in both U20 gated directions, the preserve-only variant once, api_key to subscription) | u20f-exec-admission (d); R7 (d); P1a/I1a join (characterization); P1b post check, `allowed()` before `postPlanSummary` (codex-executor.ts:3385 at 8be78a548) (characterization); P1b/I1b abort observation (characterization); P3 authorityCurrent (regression, red on base 8be78a548); P4 binding and delivered ordinal (characterization); I4 post, credential and follow-on baseline (characterization) | partly credited | Now advice and `switchGate` (masked by an undetermined layer, no leaf on the preserve-only resume-refusal path), B refresh suspension (u20r-b-refresh, timeout red) not credited; individual fence proofs for P2/I2/I3 (fence matrix, owner decision 2026-11-21) and for I1b post isolation (outside that decision; an unmet clause), REST post-body epoch attribution (no field exists, client.ts postPlanSummary/postIntentSummary) and I4 B-ordinal attribution (follow-on frames unstamped; open observation) not credited; wait-time effect unmeasured |
| U21 | Decision 11 non-use before the predecessor proof | the 32 "Codex switch crash" leaves (four crash points, eight recovery modes) | u21r-exec-release-before-proof, u21r-exec-refresh-before-proof (c, b); u21-release-before-proof (c, Runner-client counter only); u21-gate, u21r-blocker (d, b) | partly credited | API-key A to subscription B, same-worker retained recovery only; "discarded" not shown (non-use only); subscription A and API-key B, the `kind` half of the gate, the re-hold variant and cross-worker recovery not controlled; zero executor bodies, provider launches and requests before the proof, and bodies, launches and requests on the forced leaves, are asserted only |

Crash points (U21 leaves, with the controls named in the ledger):

| Crash point | Tests | Controls | Status | Residual |
| --- | --- | --- | --- | --- |
| 1. After A teardown, before apply | "Codex switch crash after A teardown before apply recovers committed A without switching" | u21r-exec-release-before-proof, u21r-exec-refresh-before-proof, u21-gate, u21r-blocker | partly credited (API-key A to subscription B, same-worker only) | as U21; delivered material here is API-key A. `expireCodexSwitchOnFreshClaim` (see the authority table) acts here only if recovery is a fresh claim; not found in ledger |
| 2. API commit, reply lost | "Codex switch crash after apply commit loses auth response and recovers committed B"; `TestCodexApplyCommittedLostReplyLiveDB` (U12) | same four U21 controls; u12-replay-codex, u12-replay-nomaterial | partly credited | as U21 and U12 |
| 3. After apply acknowledgment, before B login | "Codex switch crash after apply acknowledgement before B login recovers committed B" | same four U21 controls | partly credited | as U21; U6 crash controls ran through the crash-during-login publication only |
| 4. During B login | "Codex switch crash during B login retains committed B"; U6 crash cases | same four U21 controls; u6-crash-missing-id, u6-crash-preserve-saved (u6-crash-preserve-attempt cause only) | partly credited | as U21 and U6 |
| 5. Predecessor host unreachable | none | none | **unvalidated and uncredited** (owner decision recorded 2026-11-18, environmental limitation; its fail-closed premise is false cross-worker; owner partial decision 2026-11-19 defers m3 with the cross-worker path a known nonconformance) | The code does not enforce predecessor quiescence on cross-worker reclaim; recorded in "Decision 11 recovery controls and crash point 5". A fresh claim also goes through `expireCodexSwitchOnFreshClaim` (not found in ledger), which expires a still-pending switch from an older generation and is a no-op after an applied one, so B is still delivered. Deferred by the owner partial decision of 2026-11-19 |

### 3. PRD #1147 constraints

Line numbers checked against `prds/done/1147-codex-credentials-foundation.md`
(Approved policies, lines 15-19); all match. The "Plan's proof units" column is
the plan's own; the last column is the ledger evidence.

| PRD #1147 constraint (line) | Status | Plan's proof units | Ledger evidence |
| --- | --- | --- | --- |
| Freeze credential/auth mode at creation (:15) | Amended narrowly: only the owner-authorized fenced apply may change it (ADR-4201/1590 amendment, `adr/4201-in-place-credential-switch.md`, `adr/1590-codex-binding-same-identity-readmission.md`) | U1, U12, U13; m4 docs | U1 u1-*; U12 u12-fence-*, u12-op-mismatch, u12-target-mismatch; U13 u13-own-*, u13-neg-*; this section is the m4 docs index |
| "Never silently switch a subscription run to a paid key" (:15) | Retained; change only on explicit owner choice | U1, U2, U12 | U1 u1-foreignrun, u1-usable, u1-anthropic; U12 u12-foreign-owner. A control for the subscription-to-paid-key direction specifically: not found in ledger |
| A replaced saved row must not silently move a bound run (:15) | Retained; revision and identity checked under lock (credential_switch_codex.go, the old-alias account key and revision check) | U12 | U12 u12-target-moved-rev, u12-target-moved-acct. Old-alias refusals: listed as not controlled in the U12 row |
| Verified provider identity (:16) | Retained | U12, U5 | U5 u5-identity (b; shows B's account is carried, not that A's is rejected); U12 u12-target-moved-acct. A-account rejection under subscription to subscription: not controlled |
| Concurrent runs on one subscription; no seat lock (:17) | Unaffected | reviewer check | not found in ledger (reviewer check, not evidenced here) |
| Persist refreshed login before pause or park; reconcile lost acknowledgments (:18) | Retained | U8 | U8 u8-reconcile-after-teardown (c); C2 reconciliation before strict persistence; U8 residues listed in section 2 |
| Capability and owning worker; target derived from binding (:19) | Retained; worker apply carries no secret id | U12, U10, U5 | U12 u12-foreign-owner, u12-fence-*; U10 u10-mint-hash, u10-mint-redundant; U5 u5-bridge-pre. The worker preflight request does carry `target_secret_id` (`CodexSwitchPreflightRequest`, api/internal/workersvc/credential_switch_codex.go), and u12-target-mismatch refuses a mismatch. "Apply carries no secret id" as a request-shape assertion: not found in ledger as a control |

### 4. Decisions 11 and 12

| Decision | What was decided | Evidence | What remains open |
| --- | --- | --- | --- |
| 11 | The runtime stays fail-closed: B is never used while predecessor quiescence is unproven; the run holds instead. Crash point 5 is a recorded owner scope decision, unvalidated and uncredited, with no option-B protocol work. This is the decision as recorded, not a description of the code across workers | "Decision 11 recovery controls and crash point 5 (2026-11-18)" above; ledger U21 and Clause-accounting rows for decision 11 and crash point 5 | Open on the cross-worker path: the code does not enforce it there (a new worker has no retained journal, so the `predecessor_capture` and `quiescence_failed` gate does not run and the run proceeds with B). The owner's crash point 5 decision is recorded, but its premise (the runtime stays fail-closed) does not hold across workers; the owner partial decision of 2026-11-19 defers this as a known nonconformance, not an accepted residual. On the same-worker path the held outcome is a terminal `failed` report with clone and custody kept, not an owner pause |
| 12 | Narrow exception: the renumber helper's adjacent `.renum.*` and `.renumber-tmp-*` transient paths are permitted for one step, with the ancestry and clean-tree guards intact | "Default-branch merge and migration renumber (2026-11-17)" above: command and EXIT=0, `find ... -name '.renum*'` empty afterwards, `git status --porcelain --ignored` on the migrations directory showing only the two renames, 00465 to 00466 and 00466 to 00467 | Nothing open for this step. The record's verification is the post-run check; no later renumber is covered by the exception |

### 5. Pending, not claimed

- Landing obligations (exact-head PR CI, not run here): the full agent unit
  suite; the run-store-it LiveDB sweep; `test:api`, `test:web` and
  `test:web-browser`. `api/internal/capability` is not run with `-race` in the
  m3.2 checks and is covered only by `test:api` in that CI.
- Owner acceptance: live cross-account `thread/resume`, not verified.
- Owner partial decision on every unvalidated residue: the "not controlled"
  residues in the ledger's Clause-accounting table, the clauses that fit no
  proof class, crash point 5, the cross-worker path of decision 11, and the two authorities with no Clause-accounting row and no ledger evidence: the owner-scoped usage refresh (`handleCodexUsageUnauthorized`) and the fresh-claim pending expiry (`expireCodexSwitchOnFreshClaim`), both unvalidated and uncredited. The owner's case 5
  decision (unvalidated and uncredited, environmental limitation) is recorded,
  but it rested on the runtime staying fail-closed, which the code does not
  hold across workers. The owner partial decision of 2026-11-19 defers m3 and
  these residues; see "Owner partial decision and deferred m3 checklist (2026-11-19)".
- Optional deferred doc notes from the last review round (not applied here):
  - the affinity pin can also be released by a forge-origin cooldown
    (`fn_forge_origin_owner_pins`), and the case 5 texts do not list it;
  - the user doc omits "in maintenance";
  - the subscription sign-out advice does not say an already-issued access
    token may stay valid until expiry;
  - the ADR's appositive reads as a list.

## m4 repo checks (2026-11-18)

The lead ran each check serially at 59416337d (a docs-only parent of the
final docs commits). Logs are `.uzi/scratch/ctl-logs/m4-*.log`, each ending
`SHA=59416337d… EXIT=0`:

- `task check:changelog-entry`: OK.
- `task check:token-literals`: OK, canary detected.
- `task check:migration-numbering`: OK, canary detected.
- `task check:migration-additive`: OK, canary detected.
- `task scan:secrets`: OK, canary detected.
- `task sast:semgrep`: 0 results, 0 errors.

On every docs commit, `task docs:sync` then `task check-docs:web` ran with
EXIT=0, and the `api/internal/uzidocs` test passed.

E2E sweep: method, double-quoted string literals on removed lines of the
api/agent/src non-test production code, each checked for presence under `e2e/`
and for absence from product code; no stale literal was found. No count is
claimed. The branch changes nothing under
forge, e2e, scripts, workflows, shell or YAML, so `test:e2e-lib`,
`test:forge-fake`, `lint:shell`, `lint:yaml` and `check:guidance-size` are not
affected and were not run.

The `task scan:secrets` log reports non-gating findings in untracked
`.uzi/scratch` logs (which, from a sampled line, appear to be UUID-shaped run and operation ids); those files are not
tracked or published.

Final-head re-run: the lead re-ran `check:api-v1-compat`, `check:token-literals`, `scan:secrets` and `sast:semgrep` on 607b1ebff, the commit that first added this paragraph. All four logs (`.uzi/scratch/ctl-logs/m4f-*.log`) end `SHA=607b1ebff… EXIT=0`. Later commits change only this record.

## Owner partial decision and deferred m3 checklist (2026-11-19)

The owner decided a partial delivery: m4, every reviewed commit and the
qualifying clause-level evidence above are retained; milestone m3 is
**deferred, not complete**. This authorizes a non-closing PR for review only,
not merge or activation. No unvalidated criterion below is accepted or waived.
Decision 11's fail-closed requirement stands. Landing integration on 2026-11-20
includes #4500's cross-worker Codex claim restriction; the remaining
**known nonconformance** is the same-worker new-incarnation path described in
ADR-4201. Historical cross-worker traces below predate that restriction and
remain uncredited; integrating #4500 adds no proof credit to them. Claude
reclaim and the forge PAT remain separate gaps in ADR-4476. Landing renumbering
puts the Codex epoch snapshot at 00496 and the pending switch at 00497, above
main's 00495; earlier migration numbers in dated evidence describe those runs.

Each unchecked item is carried from the ledger's Clause-accounting table
(`agent/test/credential-switch-proof-ledger.md`, "Clause accounting"), the
U table and crash-point table of section 2, or sections 3 and 5 above. Credited parts of each row stay credited as recorded there.

### Deferred m3 checklist (unvalidated, uncontrolled or unclassified)

- [ ] Admission refusals: pool_wait, terminal statuses, pinned-without-secret,
      lane, no-live-worker, unsupported-worker and foreign-subscription refusals; the no-write snapshot comparison, the
      stamp SQL's own status filter and the non-live-run copies (no control).
- [ ] Same flight/generation/worktree: effects beyond the U2 injections and the
      fresh_B return; u2-attemptdir has no A→B→C credit and u2-worktree stays
      green in the A→B→C test; the claim-generation assertions were not shown
      sensitive; the plan-revision `awaiting_approval` restore is not covered by
      u2-transition.
- [ ] Strict persistence: pending-absent-before, other ownership legs, the
      lost-reply clear and reopen's other guards.
- [ ] A reap before B: in-flight tool callbacks, launcher must-not-exist and other
      phases, including pending launches at teardown and "created after reap"
      beyond the combined fence.
- [ ] Scope/refresh/identity/summary replacement: B refresh suspension, Now and
      summary gating during publication, A-account rejection, the bridge
      post-call copy, `assertRefreshAdmission` and Now stale-result drops.
- [ ] Resume: confirmed-refusal classification (timeout), the :4148 check and
      session_missing on the switch path.
- [ ] Policy under the apply lock: concurrent lock-order contention.
- [ ] Lease/intent/lock order: per-account lock isolation, the target account's
      intent check, the refresh admission's binding recheck in isolation and
      global lock sorting; the "no A call after reconcile" assertions (asserted
      only) and other writers.
- [ ] Final reauthorization: the API-key A path and the vault-locked branch after
      a switch; an apply committing after the final reauthorization and before
      the response can still return A's token (one point of the window
      controlled).
- [ ] Transactional apply: per-statement server-error returns, the empty
      AccessToken check and the caller's deferred rollback; storage and
      power-loss durability (process-loss model only) and concurrent-reader
      isolation beyond Postgres READ COMMITTED (not claimed).
- [ ] Ordinal/attribution: per-field baseline comparisons, the epoch<=0 refusal,
      the uncommitted-ordinal and generation-scope clamp branches on the Codex
      path, and cross-generation spend joins.
- [ ] Rollback/replay/fences: preflight's own non-owner refusal, the validator
      replay copy, Step 1 worker check, apply Step 0 copies, other fence
      conjuncts, the stamp checks, target sub-checks and old-alias refusals; the apply-path non-owner refusal
      copies are not isolated.
- [ ] Stopped negotiation: the runner's own admission check, fence/Step 0
      negotiation copies, null-heartbeat and registration sub-conjuncts, the
      in-place conjunct, identical-tuple replay, first-READY latch, the
      unchanged-stamp assertions and the worker-side worker_upgrade_required
      reason.
- [ ] Stopped pending input: paused-run carve-out variants, cancel/reject_plan
      under the cutoff and the cutoff's run correlation.
- [ ] Wall budget: the RunTimeout value, the future-started_at clamp and the
      production default; the awaiting_* statuses; the chat/judge exemption
      (unreachable, not credited).
- [ ] Stopped nonce: a NULL snapshot nonce and the UTF-8 check at the service
      entry (supporting only, uncredited); claim generation: the reconciled
      `*g <= 0` copy.
- [ ] Stopped binding identity beyond ownership (no predicate clause).
- [ ] Stopped ownership/policy: paused-origin replay under concurrency, op-less
      withdrawals, settlement after withdrawn X/Y (`xy_settles`, positive only,
      no guard to remove), and owner writers that take no worker or run row lock.
- [ ] Stopped preservation: other gate columns and replay tables not
      individually injected; Slack, `cross_checks` and other trigger-closure
      tables outside the snapshot.
- [ ] Stopped request: other statuses, the envelope key count, null/missing fields
      and case aliases beyond D25–D56.
- [ ] Decision 11 (same worker): other auth directions, the `kind` half of the
      gate, the re-hold variant, observable discard of delivered material, and
      zero executor bodies, provider launches and requests before the proof,
      including on the forced leaves (asserted only).
- [ ] Token absence: the executor's second log registration and the surfaces
      that are guardless by construction; `flight.runScopedSecrets.push`; the
      withdrawn target's absence from the stopped acknowledgement and server
      log, and both secrets' absence from run messages and inputs (asserted
      only).
- [ ] Authorities: the cookie-session path distinct from CLI Bearer (not found
      in ledger) and fresh-claim mint on the same worker (not separately
      controlled); cross-worker mint on a fresh claim (traced only, not
      tested).
- [ ] Publication (U20): plan and intent summary behaviour is tested (#4573);
      still open for them: individual-fence proofs (P2, I2, I3, I1b post
      isolation), REST post-body epoch attribution, I4 B-ordinal attribution
      (follow-on frames unstamped); wait-time effect unmeasured.
- [ ] U6 crash controls: three of four crash publications not mutated (crash
      points 1, 2 and 3; only the crash-during-login publication was).
- [ ] PRD #1147: a control for the subscription-to-paid-key direction
      specifically, an apply-request-shape control showing no secret id, and
      evidence for concurrent runs on one subscription with no seat lock
      (reviewer check, not evidenced).
- [ ] Clauses that fit no proof class ("owner batch" in the ledger): beyond the
      binding-identity and guardless token-surface lines above, the ledger's
      U15 not-credited and U19 owner-batch items (cross-user worker, egress-lane laneMismatch,
      the two `GetRunOwnedByWorkerForUpdate` copies, and the uncredited
      token surfaces).
- [ ] Two unledgered authorities, with no Clause-accounting row and no ledger
      evidence: the owner-scoped usage refresh (`handleCodexUsageUnauthorized`)
      and the fresh-claim pending expiry (`expireCodexSwitchOnFreshClaim`).
- [ ] **Known nonconformance, decision 11 same-worker new incarnation:**
      #4500 refuses peer-worker Codex claims, but the recorded worker id can
      return in a new pod while the old pod still runs. The local predecessor
      proof cannot see that old pod. No positive takeover proof or credit is
      supplied by the worker-id restriction.
- [ ] **Decision 11 held outcome (same worker):** on `quiescence_failed` the
      run ends in a terminal `failed` report with clone and custody kept, not an
      owner hold as decision 11 requires.
- [ ] Crash point 5 (predecessor host unreachable): owner-recorded environmental
      limitation, unvalidated and uncredited; its fail-closed premise depends
      on closing the item above.

### Still pending outside m3

- [ ] Landing obligations (exact-head PR CI): full agent suite; the run-store-it
      LiveDB sweep (test-api-store-it / test-livedb-workersvc); `test:api`
      (including `api/internal/capability`, not run with -race in-run),
      `test:web` and `test:web-browser`.
- [ ] Owner acceptance: live cross-account `thread/resume` on two real Codex
      accounts, not verified.
