# ADR-2506: server discard of merged-run predecessor custody

**Status**: Implemented narrow server scope; partial, nonclosing #2506. Physical disposal remains deferred to #2823 and #1373.
**Date**: 2026-10-14
**Issue**: [#2506](https://github.com/vtmocanu/uzi/issues/2506)

## Decision

The maintainer selected automatic **server hold discard** after a fresh observation that a completed run's own exact recorded PR/MR is merged. This is a policy decision to stop retaining predecessor custody, including a prior recovery aggregate that is not an ancestor of the merged work. It is not proof that the earlier source was published, archived or preserved elsewhere.

For a completed run at generation G, the server selects exact-owner/repository OPEN holds with generation < G. Historical cached-merged and issue-less runs are eligible; cached MR state and issue state do not establish merge authority. Enabled repositories supply the workset. A fresh `GetMergeRequest` uses the candidate's recorded owner-scoped connection, forge type, base URL, project and MR IID. The returned IID must match and the normalized state must be `merged`. Open, closed-unmerged, unknown, unreadable, timed-out and wrong-IID observations refuse discard.

Forge I/O precedes database locking. Recovery locks the run, then shares locks on repository and connection, and rechecks exact owner, repository, connection, project, forge, base URL, MR IID, completed status and G. Drift refuses the transaction. Matching earlier OPEN holds lock in generation/id order before non-ready captures; completing, successor and already-closed holds are excluded. A sibling refusal rolls back the transaction; this operation performs no internal retry.

## Evidence and lifecycle

The transaction sets each selected hold to `discarded`, clears its live worker/run pointers and stamps `release_evidence = merged_predecessor_discard`. The immutable `merged_predecessor_discard_binding` records hold ID/generation, run, owner, repository, connection, forge type, base URL, project, MR IID, completed generation G and verification timestamp. Migration 00338 validates the exact binding and transition, forbids audit deletion or mutation apart from `updated_at`, and admits this evidence for guarded holds without changing their final-disposition identity.

This does not invent ancestry proof, archive FINAL coverage, or a completed-publication receipt. Preparing/uploading/needs-action captures become discarded and their partial chunks are deleted in the same transaction. Available archive bytes, prerequisites, TTL and export eligibility are preserved. Custody counts and the decision hotlist fall through existing discard semantics; the shared recovery service invokes its existing post-commit custody-settled hook.

A discarded hold grants **no new worker source-deletion authority**. Operator-only physical source disposal remains under [#1786's worker-local operator procedure](../docs/run-recovery.md#retained-sources-with-no-open-hold-worker-local-inspect-and-discard) and existing retention guards. The physical local-source **and available-archive** disposal previously promised by Option D is explicitly **DEFERRED** to [#2823](https://github.com/vtmocanu/uzi/issues/2823) and [#1373](https://github.com/vtmocanu/uzi/issues/1373). Clearing live custody pointers has the existing owner-discard lifecycle consequences, including allowing ordinary teardown; this change adds no worker/PVC fence or retention guarantee.

## Polling and resource boundary

After ordinary polling finishes, a separate reconciliation phase uses the original parent context, with a 30-second budget and at most 100 repository visits and 100 candidate attempts. Repository rotation and per-repository run-ID keysets select one candidate per visit. Attempted candidates advance even on verification/settlement refusal; enumeration failures retain the run cursor. Exhaustion resets it for a later phase, without repeating an attempted candidate in that phase. Cursors are in memory, restart from the beginning after process restart, and are pruned for disabled repositories. Each observation has a five-second child deadline plus the client's existing timeout. Parent cancellation stops progress. Synchronous ticks may coalesce; immediate backlog convergence is not promised.

Each run's discard transaction is bounded by `maxMergedPredecessorHolds = 1000` and `maxMergedPredecessorCaptures = 1000` in `api/internal/recovery/merged_predecessor.go`. The capture budget covers non-ready captures (`preparing`, `uploading`, `needs_action`) across the selected sibling holds, not per hold. In `api/internal/store/queries/merged_predecessor.sql`, hold selection uses `LIMIT 1001` as an overflow probe; capture selection uses the remaining capture budget plus one (initially 1001). Exceeding either budget returns `ErrMergedPredecessorWorkLimit` and rolls back the entire transaction, including any earlier sibling hold updates and capture cleanup; no partial batch commits. Oversized run worksets remain OPEN. Polling advances the attempted run cursor and may revisit the run after a later wrap, but retries alone do not reduce the workset or establish automatic convergence. Settlement requires owner action to reduce the workset within these limits or a separate follow-up; this operation performs no internal retry.

Failure logs contain identifiers and fixed refusal reasons rather than raw forge errors or credentials. The reused `GetMergeRequest` response decoding is not newly byte-bounded: the approved plan accepts this inherited risk. Candidate and time bounds are not response-byte bounds. No forge transport rewrite, routes, protocol, capabilities, flags, workflows or new worker cleanup obligations are part of this scope.

## Deployment and rollback

Migration 00338 adds validating CHECK constraints that scan the hold table under ACCESS EXCLUSIVE locking, with a five-second lock-acquisition timeout. Representative production cardinality and validation timing are unknown. Assess staging or replica measurements and the deployment window before rollout (M1 DBA review `3aef062f`); the timeout does not bound validation runtime. Down refuses when merged-predecessor audit evidence exists rather than erasing or relabelling it.

## Local validation cost

On Go 1.26.7 Linux/amd64, sequential exported-source, race-enabled runs of the
forgesvc, poller and recovery packages with LiveDB disabled took 56.82 seconds
at baseline `9b3db3fe` and 57.74 seconds at implementation `9d77c962`. Package
reported execution times were respectively 1.315/1.770/3.356 seconds and
1.430/1.536/3.047 seconds; wall time includes build and cache effects, so this
single pair is not a performance estimate. Both used `-count=1 -race -p 1`.
The new focused PostgreSQL 17 cases reported 7.37 seconds for keyset fairness,
1.18 seconds for the ordinary-deadline test, and 0.56 seconds for the central
same/different-worker regression in one local run. Hosted CI timing, queue
cost, cache behavior, rerun cost and critical-path impact remain unmeasured.

## Source and acceptance boundaries

Implementation: `api/internal/forgesvc/merged_predecessor.go`, `api/internal/recovery/merged_predecessor.go`, `api/internal/store/queries/merged_predecessor.sql`, migration `00338_merged_predecessor_discard.sql`, and the poller/main/handler shared-service wiring. Existing `DiscardNonReadyCapturesForHold` supplies capture cleanup.

Acceptance should cover nonancestor predecessors and available/thin archives, exact binding drift and refused merge observations, concurrent upload/owner discard, immutable audit and rollback refusal, historical/issue-less polling, fairness, cursor retry/wrap/restart, parent cancellation and independent deadlines. Relevant named cases include `TestMergedPredecessorSelectionAndArchiveLiveDB`, `TestMergedPredecessorSnapshotDriftLiveDB`, `TestMergedPredecessorRollbackLiveDB` and `TestMergedPredecessorFairnessAndRestart`; these are acceptance references, not claims of execution. Test and timing evidence is owned by the lead.

This narrowly amends [ADR-1296](1296-durable-run-recovery.md); it does not close #2506 or ship the deferred physical disposal.
