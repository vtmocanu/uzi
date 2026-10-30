# ADR-2506: server discard of merged-run predecessor custody

**Status**: Implemented narrow server scope; partial, nonclosing #2506. Physical disposal remains deferred to #2823 and #1373.
**Date**: 2026-10-14
**Issue**: [#2506](https://github.com/vtmocanu/uzi/issues/2506)

## Amendment 2026-10-30 — same-issue merged witness (#3654)

**Status**: Approved policy amendment; implementation and acceptance are not established by this documentation change. The implemented same-run Option D below remains unchanged, including issue-less completed runs. This amendment adds the narrow cross-run basis `same_issue_witness` to existing `merged_predecessor_discard` evidence; it is intentional abandonment, not preservation proof.

### Eligible target and witness

Target A must be a failed or cancelled `kind = 'issue'` run with its own positive `issue_iid`, `mr_iid IS NULL` (SQL NULL, not zero), no `worker_active_runs` row at any generation, and `fail_origin IS DISTINCT FROM 'run_timeout'`. Its `finished_at` must be finite and strictly before the `created_at` of a distinct completed issue run B. A and B must have the same owner, repository and positive **own** `issue_iid`; a continued issue alone does not qualify either run. Task and other run kinds cannot be A or B. Generations belong to their own runs: do not compare A's generation with B's or copy one into the other's identity.

Any eligible merged witness suffices; neither the latest witness nor agreement from every witness is required. Before locking, the server freshly observes B's exact recorded PR/MR using the exact owner-scoped forge, connection, base URL, project and PR/MR IID. The returned IID must match and its normalized state must be merged. Cached application MR state supplies no authority. Missing, mismatched, unreadable, unmerged or timed-out evidence refuses that attempt. The transaction rechecks the complete snapshot, A/B eligibility and dependencies under locks before discard.

A nonterminal run in the same owner/repository matching the issue through either `issue_iid` **or** `continued_issue_iid` vetoes discard, including `pool_wait` runs and task runs. This is broader than A/B eligibility. Dependency audit covers A's hold rows in every state. A valid already-released `live_ancestry` transfer wholly within terminal A remains byte-identical and is **not** a veto. Unknown or shared dependencies, inconsistent transfers and future #2969 transfer evidence refuse; the witness is not authority to abandon another run's custody.

### Publication, loss and retention boundary

A publication in `prepared`, `invoked` or `create_unknown` state for any A hold, at any generation, vetoes discard, including an indefinitely stalled prepared publication. A created, unacknowledged, receipt-unreferenced orphan may subsequently be deleted through existing cleanup once A has no OPEN hold: this is an accepted abandonment consequence. Acknowledged publications retain their existing TTL; receipt-referenced publications retain their existing cleanup protection.

This policy intentionally permits loss of unique nonancestor, untracked or sibling work within A's discarded custody. A merged issue witness does not prove that work survived. OPEN holds through A's terminal generation are discarded together, with atomic cleanup of non-ready captures and partial chunks. Available capture bytes, archives, prerequisites, existing TTL and export eligibility remain unchanged; settlement starts no fresh retention window. Existing owner-discard teardown consequences remain. This adds no source/PVC retention guarantee, deletion endpoint or worker deletion authority. Option D physical disposal remains deferred to #2823/#1373.

A late `ReconcileRunMR` after commit and a newly inserted live run after the locked recheck are accepted races; either condition observed before that recheck refuses discard. `run_timeout` targets and indefinitely stalled prepared publications remain conservative residuals with no shortcut. Model-free preservation, live predecessor transfer and final plan rejection remain owned by #3031, #2969 and #2884 respectively. This amendment neither closes those issues nor ticks unrelated [PRD #2746](../prds/2746-automatic-hold-settlement.md) acceptance.

### Transaction and polling boundary

Reuse the single existing post-polling phase and original parent context: 30 seconds, at most 100 repository visits and a **shared** 100 transaction attempts across same-run and cross-run work. Observe a witness freshly at most once per phase, with a five-second observation timeout and the existing client timeout; do not reuse that observation in a later phase. Refusal advances keyset progress, and a later phase may reobserve. There is no immediate or eventual convergence guarantee.

Each target uses one transaction without internal retry. Bound the target workset to 1000 holds and 1000 non-ready captures, with dependency/publication work separately bounded to 1000; overflow or refusal rolls back the entire target. Audit of closed holds and publications must stay within those bounds rather than escaping through an unbounded dependency sweep. Oversized worksets remain protected and cursor progress continues.

Acquire A/B run locks in sorted UUID order, B in SHARE mode and A in UPDATE mode, then repository/connection locks. Lock A's OPEN holds in generation/id order through its terminal generation **inclusive**, then publications in NO KEY UPDATE mode, then captures. Recheck the snapshot and vetoes before mutation. After commit invoke the existing custody-settled hook for **A**, not B.

### Binding and rollout boundary

Reuse `merged_predecessor_discard_binding` with the explicit `basis = same_issue_witness`. Bind the exact target hold/generation, A's terminal identity, B's distinct completed identity, shared owner/repository/positive own issue IID, exact connection/forge/base URL/project/PR IID and fresh verification time. Require strict immutable audit and a Down migration that refuses to lose new bindings. The additive migration changes only the universal binding guard; it must not replace migration 00340's inventory guard or change the evidence CHECK. Existing same-run bindings and Option D semantics remain valid unchanged. These are requirements for the implementation, not a claim that a new migration has shipped.

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
