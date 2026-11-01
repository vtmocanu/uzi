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
mandatory before merge. Local focused checks do not establish hosted latency or
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
