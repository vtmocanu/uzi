# ADR-3105: Closed custody is refusal evidence for bindingless recovery

**Status**: Accepted.
**Date**: 2026-11-01
**Issue**: [#3105](https://github.com/vtmocanu/uzi/issues/3105)

## Context

A signed guarded journal can lack its original hold binding, with or without a
saved FINAL. An empty open-hold inventory cannot distinguish closed custody from
missing or inaccessible history. Repeated maintenance can therefore continue
after custody closes. Returned journal states and repeated logs do not establish
repeated uploads, storage growth or source loss.

## Decision

1. Strengthen the existing worker-authenticated, generation-scoped recovery-holds
   GET. In one SQL snapshot, probe up to two raw run/generation candidates before
   eligibility filters. Only one candidate can provide evidence, after separate
   owner/run authorization and validation of original worker provenance,
   authoritative repository, positive generation, guarded status and lane. The
   run's claim generation may have advanced. Historical C2 archive evidence keeps
   its immutable hold/capture/closure binding, checksum and ordered prerequisites;
   it does not require a still-current pin or unexpired capture.
2. Require explicit registration capability and server feature
   `recovery_bindingless_closed_v1`, plus HTTP 200 and the exact response header
   `Uzi-Recovery-Observation: recovery_bindingless_closed_v1`. Reads remain
   independent of publication write switches. Registration entry, including failed
   or same-ID registration, invalidates an in-flight read. Old APIs, missing
   guarantees, malformed/incomplete responses, inaccessible or ambiguous history
   and open observations provide no closed-custody authority. Neither inventory
   emptiness nor terminal run status substitutes for it.
3. For a MAC-verified bindingless guarded record, atomically persist a verified
   RELEASED/DISCARDED observation as the existing `exactHoldBinding` and
   `holdSettlement` pair. The locked writer rereads the authenticated record,
   rechecks immutable source/archive identity and binding, and preserves concurrent
   FINAL, ACK and completion-receipt updates. It reports settlement only after a
   matching persisted reread. Failed writes remain retryable. Open observations
   backfill neither binding nor `heldPolicy`.
4. Treat learned closure only as refusal evidence. It grants no capture, upload,
   publication, replacement FINAL, fallback, ACK, receipt, deletion or retirement.
   Discarded custody and mismatched immutable receipts suppress saved-request
   resends. A matching RELEASED receipt retains byte-identical FINAL replay to
   recover its original ACK under existing guards. C2 thin-FINAL replay remains
   before capture-status lookup; mint admission, retained-claim rules and early
   `heldCreateUncertain` denial remain separate. Existing ACKs and independently
   validated completion receipts still reach their own guarded closeout paths.
   The operator Case B disposition ([ADR-2417](2417-guarded-local-retention.md#amendment-2026-11-10-case-b-local-record-disposition-3885))
   does not change this: it never learns or transplants a binding and uses only an
   `exactHoldBinding` already persisted on the record.
5. Filter only persisted refusals from redundant discovery/materialization and
   live work. A context skip requires complete authenticated provenance, a canonical
   context match and no relevant unsettled/unknown sibling, matching replay or
   independent closeout obligation. Skipped Git runs still validate pin name/object
   identity, commit type and producing metadata. Publication records and crash-left
   temporary records continue protecting the run subtree. Initial discovery may
   precede learning closure in that same pass; later passes omit solely settled
   work. Existing admission, fairness, caps and cancellation budgets remain intact.

The implementation reuses [the observation service](../api/internal/recovery/exact_observation.go),
[worker client](../agent/src/client.ts), [journal coordinator](../agent/src/recovery.ts)
and [Git discovery](../agent/src/git.ts). There is no new inventory, endpoint,
custody transition, sidecar, journal format, global cache, retry lane or periodic
scan. Observation deduplication is local to the existing attempt/pass.

## Compatibility boundary

The standalone [compatibility proof](../agent/test/recovery-bindingless-compat.proof.ts)
executes untouched source readers at
`a07f813708ced206cd7b8c3f9d09e259b2fff64b` and
`ee17bf89c5cd84a9c61b8c4c8f799febb6f94d94` (`v0.86.1-rc.1`). Both preserve
`attemptedHeadShas` and the paired settlement fields. This proves those precise
reader boundaries, not arbitrary older versions or deployed package compatibility.
`v0.86.0-rc.32` already drops `attemptedHeadShas` and is not a supported boundary
for journals containing it.

Six authentic fixture variants exercise canonical MAC authentication, wrong-token
and tamper refusal, rollback/roll-forward, and complete byte/mode backup and
restore into the original absolute paths. Source, bundles, pins and reports are
retained; paths are neither rewritten nor re-signed to hide incompatibility.
The separately executed backup-runs/backup-directory validators cover Git-config
custody schemas, not the RecoveryRecord JSON MAC envelope.

Reproduce from `agent/`, with provisioned dependencies and both pinned commits:

```sh
node --import tsx --test --test-timeout=120000 test/recovery-bindingless-compat.proof.ts
node --import tsx --test --test-timeout=120000 test/client-bindingless-recovery-hold.test.ts test/recovery-bindingless-settlement.test.ts
```

## Scope and validation

The 2026-10-18 decision assigning open-hold settlement and already-bound
reconciliation to [#2494](https://github.com/vtmocanu/uzi/issues/2494) is unchanged.
Producer enablement is excluded. Owner pin visibility and restore guidance remain
[#3728](https://github.com/vtmocanu/uzi/issues/3728); self-contained archive send
ordering remains [#3754](https://github.com/vtmocanu/uzi/issues/3754). The parent
[prerequisite-closure PRD](../prds/2506-prerequisite-closure.md) is not completed
by this issue.

The isolated regression and mutation proofs were executed at runtime/test revision
`aa0b83adf73c9ca62bafe042043a059fb4dac811`, against the unfixed baseline above.
Candidate controls passed; baseline no-FINAL and ambiguous-custody cases failed
the intended behavioral assertions. Every candidate mutant compiled or typechecked
before failing its intended assertion: selecting one row as unique, filtering open
or invalid-provenance siblings before counting, granting ACK/retirement from
closure, and suppressing matching thin-FINAL replay. The ACK and actual journal
retirement variants separately exercised cleanup and retained-tree checks. The
matching positive control persisted its original ACK without reading capture status.
All mutation changes stayed in disposable exports.

The API baseline used an export-only capability-name alias to its existing V2
value; its production route and SQL stayed unchanged. The worker baseline ran the
backported behavioral probes with tsx; compiling those new test typings against
its absent APIs is not a claimed compatibility check.

Local full-module API dead-code analysis was canceled twice for memory pressure
and remains unverified;
it must not be reported as passed. Other canonical API constituents passed
serially with `UZI_TEST_DATABASE_URL` unset. Exact-head `lint-api` dead-code,
full agent/API/repository CI, inherited store-it sweep and hosted job timing remain
mandatory before merge.

The repository gate failed under Node 26.12.1 in the unchanged forge operation
contract test: `stripTypeScriptTypes` rejected transform mode. The same narrow
test failed on the untouched baseline. Separately, `test:ci-safe-paths` returned
instrument status 3 where its Brew failure fixture expected 1, on both candidate
and baseline exports. Its release-watch helper invokes a Git configuration form
that this worker rejects; this comparison does not establish a source defect.
Neither check is reported as passed or waived. The remaining repository
constituents ran serially: 45 passed and the already-passed E2E library check was
reused on the unchanged candidate. E2E library and fake-forge tests passed; secret
scanning and Semgrep passed. These constituent results do not make the full
repository gate green. Local focused checks do not establish hosted latency or
full compose acceptance. Compose phase 72 (`custody-lifecycle`) was not run.

### Local timing record

These are affected-test proxies, not full CI jobs or hosted latency measurements.

| Workload | Baseline | Candidate |
|---|---:|---:|
| Existing worker exact-read and discovery-cost tests, seven tests | 6.741s | 6.998s |
| Existing API exact-route and closure replay controls, nine named results | 23.267s | 17.983s |

The worker sample compared the baseline with `568d4a46`; the API sample compared
it with `aa0b83ad`. Shipped runtime code is identical at those candidate revisions.
Each row is one local serial sample, with matching executed names and zero skips.
Node was v26.12.1 with provisioned dependencies; OS/tsx caches were not controlled.
The additional 40-test worker regression file took 25.469s at `aa0b83ad`, separate
from the unchanged seven-test comparison. The historical-reader proof is a
standalone check, outside the ordinary agent test-file inventory.

The API command ran from `api/` with race, uncached tests, serial package builds
and build-VCS discovery disabled:

```sh
go test -race -count=1 -p=1 -buildvcs=false -timeout=5m -v -run '^(TestExactRecoveryHoldRouteLiveDB|TestRecoveryClosureFinalReplayNeedsNoForgeLiveDB)$' ./internal/handler ./internal/recovery
```

Both API samples followed precompilation. The initial candidate precompilation
was 330.363s; baseline/candidate timing precompilation was 167.042s/140.851s.
The local Go toolchain was 1.27.3 on Linux/amd64; this is not an exact CI-toolchain
measurement. The fixture used PostgreSQL 17, `GOMAXPROCS=2`, `GOMEMLIMIT=4GiB` and
`GOFLAGS=-p=1`, with run-owned module/build caches and offline module lookup.
The baseline database had served raw-candidate controls. Its recreated candidate
fixture was warmed by the comparison workload (29.232s) before measurement.
Shared module/build caches, different fixture warmups and concurrent host load
prevent attributing the elapsed difference to implementation performance. These
figures exclude queueing and do not establish a CI critical-path improvement.
Exact-head hosted job/critical-path timing remains a landing obligation. No
workflow, concurrency cap or production wait was changed.


## Repair validation, 2026-11-02

The early bindingless finalization probe seeks refusal evidence without falling
back to a legacy open-inventory read. Open, unknown, unsupported and failed
observations leave the existing inventory checks inside the source boundary.
Bindingless records with a FINAL ACK or completion receipt use the existing
ownership cancellation probe instead of learning closure that cannot refuse
their independent closeout.

The regression controls compare saved and unsaved FINAL paths at both retained
and passed source boundaries. They witnessed six failures before the repair
(extra pre-boundary inventory reads and unnecessary closeout observations),
then passed after it. The settlement file passed 53 tests, related focused files
passed 117, and the inventory/client run passed 763, all with zero skips or
cancellations. Agent typechecking and scoped lint passed. Repeated settled,
unknown and failed-write attempts each emitted one retained-FINAL diagnostic;
hostile response/error text was absent, with exact allowlisted fields asserted.

The compatibility proof now passes authentic before/after signed journal
payloads through the real backup harness's untracked-file archive. It compares
bytes and modes, restores the payload to its original absolute journal path,
then checks coordinator authentication and retained source/bundle/ref contents.
This covers opaque payload capture, not discovery of external recovery roots
or MAC authentication by the backup script. Git-config descriptor validation
remains separate. The extended proof passed all six variants with zero skips;
the separate Python backup-directory test also passed. Its initial attempt ran
the entire backup suite inside the uploaded variant and exceeded the 100-second
subprocess limit. The proof now explicitly selects the signed-payload case;
the ordinary backup suite still runs every case without that selector.

Fresh PostgreSQL 17 fixtures executed
`TestExactRecoveryRawCandidatesLiveDB`,
`TestExactRecoveryProductionBearerRoutesLiveDB`,
`TestExactHoldClosureImmutableEvidenceLiveDB`,
`TestExactHoldClosureOrderedPrerequisitesLiveDB`, and
`TestExactHoldArchiveHistoricalEvidenceLiveDB`, plus the other exact-observation
controls selected by the command below. All 57 named results passed, with
zero failures or skips. The fixture container was removed after execution.

```sh
go test -race -count=1 -p=1 -buildvcs=false -timeout=5m -v -run '^(TestExactRecovery.*LiveDB|TestExactHold.*LiveDB)$' ./internal/handler ./internal/recovery
```

This repair adds 13 settlement regression tests. The 53-test file took 50.085s
locally, compared with the historical 40-test sample of 25.469s above. These
are separate samples with different fixture/cache and host-load conditions,
not a controlled CI-job comparison. The extended six-case compatibility proof
took 34.536s; it remains outside the ordinary shard inventory. No production
wait, workflow or concurrency cap was added. Exact-head affected-job and
critical-path timing still belongs to landing.

The earlier mutation and timing results above are historical execution records.
Their scratch logs did not survive recovery into this clone. They are not
newly reproduced results from this repair session, and no mutation artifacts
are shipped as production code. Historical-reader compatibility remains an
explicit standalone proof requiring both pinned commits; ordinary agent CI
runs the bindingless client and settlement regression files. No workflow
change or broader compatibility claim is made.

The three unresolved checks above remain unsuccessful or unverified in-run:
API dead-code analysis, forge operation contracts, and CI safe-path fixtures.
Before merge, exact-head `lint-api` must pass `deadcode:api`, and
`lint-repo` must pass both `test:forge-fake-contract` and
`test:ci-safe-paths`. A non-closing delivery leaves #3105 open until those
results are verified and recorded. Ordinary steering does not waive the frozen
M2/M5 obligations; a formal owner partial decision is still required to defer
them.
