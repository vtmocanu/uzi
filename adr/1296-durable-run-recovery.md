# ADR-1296: custody holds are H-free and claim-scoped; archive captures are H-bound and immutable

**Status**: Implemented (PRD #1296, M1-M6 committed on this branch — M5, owner-facing
UX, landed in `9e3e7362`, before this ADR); M7 (integration proof and hosted
acceptance handoff) is still pending, ahead of merge/feature acceptance.
**Date**: 2026-09-13
**Issue**: [vtmocanu/uzi#1296](https://github.com/vtmocanu/uzi/issues/1296)
**PRD**: [prds/done/1296-durable-run-recovery.md](../prds/done/1296-durable-run-recovery.md) — carries
the full Decision Log (D1-D9), the milestone breakdown and the verified code anchors; this
ADR restates the invariants a future edit to a claim clause, a custody-release path or a
cleanup guard must not silently break.

## Amendment (#2969): predecessor guarded-hold settlement is built and disabled in production

**Status**: Built behind a constructor-only test option; **disabled in production**. Every retained re-claim skips the predecessor pass, so production behavior is unchanged. Owner decision 2026-11-06, fallback.

The design lets a retained re-claim settle a **lower** generation's guarded inventory hold while a later claim generation of the same run executes. What the branch contains:

- **Coordinator (`agent/src/recovery.ts`).** An in-memory, never-journaled `predecessor` option on `operateFrozenInventory`, carried by an `AsyncLocalStorage` scope. One exemption in `inactiveInventoryDisqualifier` tolerates an executing run only for that record, only for a generation strictly below the executing claim, and only while the scope is live. Grants 1, 3 and 4 are denied; grant 2 requires `current_claim_generation > generation` from a fresh exact hold observation, and a failed observation returns `predecessor_observation_refused` before any capture, upload or write. There is no freeze. Saved archive FINAL requests are preserved and replayed. Each pass has a 64 MiB upload allowance; a typed quota refusal skips, a bare 507 is not quota. Predecessor RPCs are awaited, not detached. An integrity channel classifies refusal codes (unknown means integrity, so STOP) and survives a thrown operation.
- **Runner (`agent/src/runner.ts`, `agent/src/git.ts`).** A per-call source boundary grant (exact execution tail, generation strictly lower, live held scope, Linux) proves each clone with quiescence before and after, plus a strict byte-exact content proof (`GitCache.inventorySourceContentProof`: binary-safe git output, `hash-object --no-filters --stdin-paths`, capped, refusing assume-unchanged, skip-worktree, gitlink and filter attributes). A predecessor-only `ignored_residue` gate skips when non-scratch ignored files exist (`node_modules`, build output); `.uzi/scratch/` is excluded. Per-clone head and device:inode baseline drift is a STOP. `settlePredecessorHolds` runs inside `recoverRetainedClaim` after the mandatory capture and before successor adoption, with a budget, an owned/joined action cap, a settlement watchdog, a Codex pre-entry single-source gate and STOP/skip classification.
- **Observed `--stdin-paths` behavior.** On git 2.54.0, `hash-object --stdin-paths` C-unquotes a line that starts with `"` and strips a trailing CR. The proof therefore refuses, before dispatch, any path containing LF, starting with `"`, or ending in CR, rather than hashing a different file than it named.
- **The ordinary terminal FINAL is unchanged.** It keeps `worktreeStatus`. The plan proposed the strict content proof there too; it was dropped because a clean repository with `eol`, `ident`, `working-tree-encoding` or filter attributes hashes differently under `--no-filters` and would be refused forever.
- **API.** No server change. `TestRecoveryPredecessorFinalWhileSuccessorRunsLiveDB` characterizes that the server already accepts an older-generation FINAL while the successor runs.

**Why disabled.** The existing quiescence scan targets only the runner uid and cannot see a leftover process of the separate command uid writing into a predecessor clone. An in-process procfs detector was tried and removed: review showed an unprivileged process can cycle the pid counter without being counted (clones that fail after pid allocation, thread pids), so absence could not be proven. A retained re-claim that has lower-generation guarded records to settle logs `command_uid_unproven` and skips the pass before any predecessor operation (other skips log their own reason, such as `budget`); no retained re-claim runs a predecessor operation, and the holds stay open and counted exactly as before. The same command-uid blind spot exists in the ordinary terminal FINAL's quiescence check; it is a known gap, deferred next to #3851.

**Enablement.** Turning the pass on requires a separately reviewed proof contract that rules out a command-uid writer into the predecessor source. Removing the test-only gate alone is not enough. The real UID-split end-to-end lane (M4) belongs to that enablement work; the existing worker-uid CI job does not satisfy it.

**Accepted residual (owner decision 2026-11-06), separate from the command-uid defect.** The scoped quiescence scan does not prove that no arbitrary writer exists: it excludes a process outside the target paths that carries no attempt marker. Such a process can modify the source after the last proof, and those later bytes can be absent from the archive even though the hold is released. The held callback and strict hashing do not exclude it. This residual applies once the pass is enabled and is disclosed, not fixed; [#3831](https://github.com/vtmocanu/uzi/issues/3831) is related design work, not a guaranteed fix. The command-uid defect above is what keeps the pass disabled.

**Deferred, not shipped:**

- A sound command-uid source proof (the prerequisite for enabling the pass).
- A Codex older-sibling source proof.
- A regenerable ignored-path allowlist.
- Empty or unadopted predecessor settlement.
- The shared-quota risk tracked in #2544.
- The real UID-split end-to-end lane; it was not built or run.

Cleanup of predecessor records is unchanged and stays with the [ADR-2417 amendment](2417-guarded-local-retention.md#amendment-2969-predecessor-record-cleanup-deferral).

## Amendment 2026-11-07: orphan reclaim and enrolled secret-scan remediation (#4058)

Source selection only. Retirement of an enrolled journal (one carrying an active secret-scan
`remediationRecovery` record bound to the same run, clone path and branch) still refuses. What changed is
the successor's outcome: it now holds for its owner at `recovery_wait` / `retained_recovery_adoption_failed`
instead of failing terminally with an untyped refusal, and a retained enrolled journal is no longer detached
by an orphan reclaim. No retirement, rebind or discard authority is added, and the owner-disposition surface
(D6) is unchanged. See the
[ADR-1197 amendment](1197-transient-recovery-park.md#amendment-2026-11-07-4058-a-successor-holds-for-its-owner-when-a-terminal-predecessors-source-is-under-remediation).

## Amendment 2026-11-04: operator local-record resolution (#3885)

This amendment added a worker-local operator command that retires guarded local
recovery records, and it covered Case A only: the exact closed hold was released with a receipt matching the saved
FINAL request, with or without the FINAL ACK already saved (an ACK with a
discarded hold is refused). The binding, lock order, retention exclusions and
failure model are in the
[ADR-2417 amendment](2417-guarded-local-retention.md#amendment-2026-11-04-operator-resolution-of-guarded-local-records-3885).
The owner-disposition surface (D6) is unchanged, and no new server authority is
added. Case B (a discarded hold, or a release without this record's matching
receipt) was not part of this amendment; it is now handled by an explicit,
operator-confirmed `--case-b` record-only disposition, recorded in the
[ADR-2417 Case B amendment](2417-guarded-local-retention.md#amendment-2026-11-10-case-b-local-record-disposition-3885)
(the former `case_b_pending_owner_review` refusal no longer exists).

## Amendment 2026-10-30 — same-issue witness policy

The approved #3654 policy adds the narrow cross-run `basis = same_issue_witness` to existing
`merged_predecessor_discard` evidence. A failed/cancelled issue run without a
recorded MR may abandon its OPEN custody after a distinct later completed run
of the same owner/repository/positive own issue is freshly verified merged,
subject to locked identity, live-run, dependency and publication vetoes. This is
intentional loss policy, including unique work, not preservation proof. Available
archives keep bytes, prerequisites and existing TTL; no new retention window,
source/PVC guarantee or worker deletion authority follows. Same-run Option D,
including issue-less completions, is unchanged. See the complete
[ADR-2506 amendment](2506-merged-predecessor-discard.md#amendment-2026-10-30--same-issue-merged-witness-3654)
for eligibility, refusals, accepted races and bounds.
#3031/#2969/#2884 retain their separate scope.

## Amendment 2026-10-14 — merged predecessor server discard (#2506)

The maintainer selected the narrow policy in [ADR-2506](2506-merged-predecessor-discard.md):
a fresh server observation of the completed run's own exact recorded PR/MR as merged,
followed by locked exact-binding revalidation, permits discard of earlier OPEN holds
at generation < the completed generation. This includes nonancestor prior aggregates
and guarded holds; it is discard policy, not ancestry, archive FINAL or publication
receipt authority. Non-ready capture cleanup is atomic; available archives retain bytes,
prerequisites, TTL and export eligibility. Existing discard semantics reduce custody
counts and decision hotlists.

This supplies no new worker source-deletion authority or worker/PVC retention fence;
existing operator-only #1786 disposal and retention guards remain. Option D's physical
local-source and available-archive disposal is deferred to #2823 and #1373. #2506 remains
partial and nonclosing. ADR-2506 records polling bounds, inherited unbounded response
decoding, migration validation/locking risk and rollback's refusal to lose audit evidence.

## Amendment 2026-10-29 — C1b prerequisite pin lifecycle (#3591)

[ADR-3591](3591-prerequisite-closure.md) records the API-only pin lifecycle.
Fresh default-tip D must positively cover each declared P (ancestor/equal);
the invocation freezes D and forge binding before one create-only send.
Unknown creation remains listing-only indefinitely; S7 cleans a pinned ref
after confirmed capture expiry/discard with expected-tip CAS and confirmed
absence. Cleanup exhaustion retains the live blocker, with no reset authority.

D5's thin archive FINAL refusal and local-source retention remain in force:
C1b has no feature advertisement, agent change or release predicate. T1–T5
is a conditional trusted-worker proof, not server parsing/verification of Git
bundle bytes. C2 owns the release amendment and FINAL-time revalidation.
The existing “Later user deletion or rewriting is outside the proof” residual
also applies to external prerequisite-pin deletion/history rewrite; pins can
retain pre-purge history. This amendment authorizes no purge, manual pin delete,
database override or clearing of unresolved evidence. Local native restore and
omitted-prerequisite negatives are recorded in ADR-3591; exact-head hosted
acceptance and timing remain pending.

## Context

A run can commit real, useful work and still fail before it publishes: a workflow or
secret preflight rejects the push, the remote rejects a non-fast-forward, or base
alignment against a moving default branch is exhausted. Before this PRD, the only
recovery paths were a mutable tracking ref inside the worker's own bare repository (gone
the moment the worker or its PVC is torn down) and, for some failures, a lossy unified
diff capped at 512 KiB with no binary content and no history. An owner without shell
access to the worker had no reliable way to get the original commits back, and an
ephemeral worker's own cleanup could destroy the only copy.

This PRD adds automatic, durable, owner-only capture of the original committed head at
the finalization boundary, stored encrypted in PostgreSQL (the only mandatory durable
store both compose and k8s already have), independent of the worker's continued
existence. It deliberately does not touch the pre-existing `recovery_wait` transient
in-place retry (ADR-1197) — that mechanism resumes a *live, still-running* turn from a
verified local checkpoint; this one preserves the *original committed history* of a run
that is finalizing (successfully or not) and may be about to terminate.

## Decision

### Two records with different lifetimes, not one

A **custody hold** (`recovery_custody_holds`) is claim-scoped and **H-free**: it is
created in the *same* successful `ClaimRun` transaction as the new
`runs.claim_generation` counter increment, for a recovery-capable worker on a
code-publishing profile, before any model work starts. It reserves owner-scoped custody
admission capacity with no dependency on crypto, byte quota, or even a captured
head existing yet.

An **archive capture** (`recovery_captures`, with its ordered `recovery_capture_chunks`)
is a later, immutable artifact bound to an *open* hold once a head H is actually
resolved. A hold's existence is never evidence that bytes are archived; only a capture's
`state='available'` with a bound manifest is. Repeated capture attempts within a hold are
distinct rows, keyed by an idempotency identity from the worker's local source journal,
so a retried registration returns the same row rather than duplicating it
(`UNIQUE (hold_id, idempotency_key)`).

Keeping these separate lets custody admission — the thing that must block new work when
an owner is holding too much unresolved recovery — be reserved cheaply and immediately,
without waiting on the worker to actually produce bytes, while the capture itself stays
free to fail, retry, or need action without ever un-reserving that admission slot.

### D5: independent import and the guarded cached-dependency exception

The original D5 contract uses a fresh verified forge default tip and its merge
base with H, falling back to self-contained capture when no suitable public
prerequisite exists. Import verification uses an isolated no-alternates repository
supplied only with the verified public prerequisite closure; byte verification
alone does not establish dependency availability.

For inventory-guarded capture (#2476), try self-contained first. Only a size-cap
refusal permits a capped thin fallback using the exact cached worker-bare
default-branch tip and all its merge bases with the captured source, without a
forge PAT. If a merge base equals the captured source H, exclude H's verified
parents instead so H remains advertised; a root H has no useful boundary and
keeps the self-contained/oversized outcome. Record actual bundle-header
dependencies. Production verifies the bundle in the trusted worker bare;
conformance tests verify and import it in isolated no-alternates destination
clones. These checks do not guarantee fresh public availability. A thin archive remains
owner-downloadable but never authorizes archive-backed FINAL or source cleanup,
even with complete root coverage. Without the distinct completed-publication
receipt described in the 2026-10-09 amendment, holds, pins and clones stay
retained until independently verified empty-inventory settlement or explicit
owner discard, except for server hold discard under the 2026-10-14 amendment above
(which supplies no new worker source-deletion authority).
Non-final capture TTL remains normal; custody does not expire and pod/PVC/hold
costs continue. Deploy the API release guard before the worker producer. Legacy
fresh-forge capture is unchanged. Historical oversized captures are not
automatically reproduced.

### The two-lifetime foreign-key model

`recovery_custody_holds` carries two *different* kinds of pointer to the same worker and
run, and conflating them is the mistake this schema is built to prevent:

- **Immutable provenance** — `original_worker_id` / `original_worker_identity` are plain
  value columns, not foreign keys, and are **never nulled** after insert. They exist so a
  worker's own post-terminal recovery retry (finishing registration or upload for a hold
  it already owns) can still be authenticated after the run and even after the worker row
  is gone.
- **Live custody enforcement** — `live_worker_id` / `live_run_id` are nullable foreign
  keys, `ON DELETE RESTRICT`, non-null only while the hold is `open`. Every destructive
  cleanup path (worker deletion, ephemeral teardown, the reaper, PVC recycle) is blocked
  by these while custody is open. Releasing a hold clears **both** to `NULL` atomically in
  the same update that flips `state` to `released`/`discarded` — that clears the
  restriction and lets ordinary teardown proceed, without touching the immutable
  provenance columns beside them.

A future change must not collapse these two pairs into one FK: doing so would either let
a delete race the RESTRICT (if it were nullable-without-provenance) or block deletion
forever after a legitimate release (if it were never nullable at all).

### Nothing recovery-related cascades from owner, repo, or run

`recovery_custody_holds.user_id`/`repo_id`/`run_id` and `recovery_captures.run_id`/
`user_id` are plain columns with **no** `ON DELETE CASCADE`. A capture is also `ON DELETE
RESTRICT` from its parent hold — releasing or discarding a hold must never silently
destroy an already-registered archive. The one deliberate cascade in the schema is
`recovery_capture_chunks ON DELETE CASCADE` from its capture: a capture's own explicit
discard takes its bytes with it, but nothing upstream of a capture (owner deletion,
account deletion, run deletion) is allowed to make custody or archived bytes disappear as
a side effect. Every one of those higher-level deletes must be taught to check custody
explicitly (M4); none of them may rely on the database silently doing the right thing.

### AAD binds capture, chunk index, and length

Each ~1 MiB chunk is sealed with `secretbox.SealWithAAD`, with the additional
authenticated data built from `(capture_id, chunk_index, length)`
(`api/internal/recovery/crypto.go`, `chunkAAD`). This is not merely "encrypted at rest":
it means a stored chunk cannot be reordered, truncated, or spliced from a different
capture and still decrypt — any substitution, reorder, or length mismatch fails
authentication rather than silently producing corrupt plaintext. A future storage
migration (e.g. moving chunks to object storage) must preserve this binding, not merely
the encryption.

### Upload and ready-transition commit together, or not at all

One streaming HTTP request carries a capture's complete bundle. The API splits it into
chunks and writes every chunk plus the `available` state transition inside **one bounded
database transaction**; oversize input is rejected mid-stream (`ErrOversize`) rather than
truncated, and an interrupted or rejected upload rolls back every chunk from that attempt
while leaving the separate hold/capture reservation intact. There is no partially-written
"ready" archive: a reader either sees the fully-committed manifest and chunk set, or sees
the capture still in `preparing`/`uploading`/`needs_action`. A retry of the same capture
identity starts the byte stream from zero rather than resuming or splicing.

### Generation-aware, per-hold custody release (no cross-generation data loss)

A run can carry more than one open hold at once — a crashed worker's generation-1 claim
and a re-claiming worker's generation-2 claim both left `open`. The release path is
**per-hold, not per-run**: `ReleaseCustodyHold` releases the one hold a completed
operation actually resolved, never every open hold sharing that run id. The reconciler's
"releasable on completion" query is additionally **generation-aware**
(`r.status = 'completed' AND h.generation = r.claim_generation`), so a run's terminal
generation releases its own matching hold through this completion-release path — a still-open
hold from an earlier, orphaned generation is retained until its own capture or explicit
discard, except for the narrow server discard in the 2026-10-14 amendment above.
This is the deliberately conservative, no-data-loss choice: a cross-worker orphaned hold
staying open (and thus counting against the owner's custody-admission limit and blocking
that worker's deletion) is an acceptable cost against silently dropping the last copy of
a generation's committed work. (Corrected 2026-09-13: an earlier per-run-id release
implementation did exactly the wrong thing here — releasing a completed generation-2 hold
also released an unrelated, still-uncaptured generation-1 hold on the same run. Fixed by
switching every release path — the worker's own terminal report and the boot/periodic
reconciler alike — to resolve and release one specific hold id.)

### Owner-only authorization, never the viewer seam

Every recovery-metadata and byte-serving endpoint sits under the authenticated
`RequireUser` route group and resolves ownership through `workersvc.GetRun(ctx, user.ID,
runID)` — the strict owner-only accessor, the same one private follow-up-input reads use.
None of them use `GetRunForViewer`, which is the ordinary owner-or-admin read used for run
display; an admin viewing a foreign owner's run gets exactly the same 404 an unrelated
user would. This is an application-level access rule, not an end-to-end secrecy
guarantee: see the Consequences section below.

### The checkpoint/park mechanism stays separate

`recovery_wait` (ADR-1197) and this PRD's archive capture are two different mechanisms
that must not be merged or made to depend on each other. `recovery_wait` resumes a live,
still-claimed, still-running turn from a verified local checkpoint and retained SDK
session — it exists to avoid re-planning after a transient empty SDK result.
Archive capture exists for a run that is finalizing, published or not, and whose worker
may be torn down entirely afterward; it never substitutes `checkpoint_tip` for the final
committed head, and it does not park or resume a run by itself. A future change that
wants to reuse one mechanism's plumbing for the other should treat that as a new decision,
not an implicit consequence of either existing one.

## Consequences

- **A stolen database dump or backup does not yield plaintext archives.** Every chunk is
  sealed under `UZI_SECRET_KEY`-derived AEAD with capture/chunk-bound AAD; only the API
  process holds that key, and the worker that produced the archive never receives it.
- **This is not a defense against the operator who runs the API.** Anyone who holds
  `UZI_SECRET_KEY` (the same key that already protects every other secret-at-rest kind in
  this codebase — forge PATs, the per-user vault's connection-level fallback) can decrypt
  any archive. The owner-only authorization above stops another *user* (including an
  admin using the ordinary viewer path) from reading someone else's archive; it does not
  hide archives from whoever administers the deployment. This mirrors the existing
  documented boundary in [docs/vault-threat-model.md](../docs/vault-threat-model.md) and
  is not a new or different guarantee.
- **A cross-generation orphaned hold is a visible, actionable cost, not a silent one.** It
  counts against the per-owner open-hold ceiling (default 8; see ADR-1751 for the continuation exemption) and
  keeps its worker undeleteable, surfaced as the `retaining_unpublished_work` worker flag
  and the `reasonCustodyLimit` queued-run reason. The cost clears when the hold's work is
  durably captured (which releases the hold via the reconciler) — there is no automatic
  eviction and no admin override. A capless orphan (one whose worker died before it
  reserved any capture) has **no owner self-serve discharge today**: `DiscardCaptureForOwner`
  discards a *capture*, not the hold, and there is no discard-hold action yet, so such a
  hold is retained until an operator clears it. Adding an owner-scoped discard-hold action
  (the D3 "owner explicitly confirms discard" leg for the capless case) is tracked in
  [#1318](https://github.com/vtmocanu/uzi/issues/1318).
- **Mixed fleets degrade honestly, not silently.** A worker or API that predates this
  contract simply never opens a hold, so a run executed there reports `unsupported`
  recovery rather than a promise that was never kept. Rolling the fleet forward protects
  runs going forward only; it does not retroactively protect a run that already executed
  on an old worker.
- **A schema or query change that reintroduces a per-run (rather than per-hold) release,
  or that adds `ON DELETE CASCADE` from `users`/`repos`/`runs`/`workers` onto any recovery
  table, silently reopens the exact data-loss class this ADR's decisions close.** Treat
  either change as a regression requiring the same generation-aware, RESTRICT-by-default
  review this PRD went through, not a routine schema edit.

---

## Amendment 2026-09-14 — PRD #1349: generation-exact custody, park/early-terminal capture, causal limit routing, and owner disposition

**Status**: Implemented (PRD #1349 M1-M6 committed on this branch); M7 (integration
proof and this documentation update) is in progress; M8 (hosted-k8s acceptance and
release) is maintainer-owned and **not shipped** in this work.
**Issue**: [vtmocanu/uzi#1349](https://github.com/vtmocanu/uzi/issues/1349)
**PRD**: [prds/done/1349-recovery-custody-hardening.md](../prds/done/1349-recovery-custody-hardening.md) —
carries the full Decision Log (D1-D12), the milestone breakdown and the verified code anchors.

The decisions above stand unchanged — the two-record model, the two-lifetime FK model, the
RESTRICT-by-default cascade rules, the AAD binding, and the owner-only-never-viewer
authorization all carry forward. PRD #1296 made a custody *release* generation-aware in the
reconciler but left the worker-facing reserve/release paths keyed on `(run_id, worker_id)`,
released a verified-empty hold only inside finalization capture, and gave an owner no way to
discharge a capless orphan hold (flagged above as [#1318](https://github.com/vtmocanu/uzi/issues/1318)).
Hosted acceptance on `v0.83.0-rc.1` exposed those gaps; this amendment records the decisions
that close them.

### Custody identity is the exact claim generation, end to end (D1)

Reserve, release, no-output disposition, park capture, and retry now key on
`(run_id, claim_generation)` — the identity this schema chose but did not thread through the
wire. The server already serialized `claim_generation`; the agent now echoes it in
`ClaimResponse` and stamps it on every recovery record and custody operation, and every
run-plus-worker release sweep is replaced by an exact evidence-bound operation.

**Invariant**: a newer same-worker generation can never drop an older generation's only copy.
The reconciler already had this property (see "Generation-aware, per-hold custody release"
above); this extends it to the worker's own reserve/release paths, where a completed
generation-2 claim could previously match and release a still-uncaptured generation-1 hold on
the same run and worker.

### Mixed fleets advertise v2 and retain on ambiguity (D11)

A recovery-capable worker advertises the additive `recovery_archive_v2` capability alongside
`recovery_archive_v1` (`api/internal/capability/capability.go`) and names its generation on
reserve/release. A new server accepting an older (v1) or ambiguous worker resolves custody
only where exactly one unambiguous current hold exists; anything ambiguous **retains** the
hold and returns `needs_action` rather than guessing which copy to drop. A v2 worker never
invokes v2 operations against an older server.

**Invariant**: a missing or ambiguous generation never widens back into run-plus-worker
matching. Retention — a visible, actionable cost (see PRD #1296's Consequences) — is always
preferred over a guess that could drop the last copy.

### Release only from per-hold durable evidence, ancestry-checked at disposition time (D2)

For legacy holds, outside the narrow server discard in the 2026-10-14 amendment above,
one exact hold may leave `open` only after one of: (1) that generation's full head was
published, (2) an available archive is bound to that hold, (3) the hold's own source-holding
worker proves no unpublished committed output against freshly fetched forge history, or (4) the
owner explicitly discards that exact held source. Outside that server-discard exception, a current generation may settle an *earlier*
generation only when a settlement-time ancestry check proves the earlier source is contained in
the durable published/archived head. Tracking/checkpoint adoption supplies a candidate source,
never lasting proof — a later rebase or amend can break ancestry before publication — so the
ancestry check runs against the *final* durable head at disposition time. Same-worker identity,
the same branch, a newer MR, a later generation, or capture absence are never proof of
continuity, and a default-branch reseed can never supply the earlier source.

**Implementation note (issue #1582).** For the publication path (a completed run's
published branch head; archive-disposition settlement is not implemented), the
settlement-time ancestry check above is server-side and forge-proven: the worker only reports candidate SHAs (the predecessor's
journaled source, its adopted tip, and its own pushed head) and the api independently reads
the branch head once, then asks the forge's own comparison primitive whether each candidate
is an ancestor of it — GitHub's compare `status`, GitLab's `merge_base`, or, on Forgejo,
a two-direction compare (`head...candidate` empty AND `candidate...head` nonempty), because
Forgejo empties the commit list whenever a merge base cannot be found, so an empty
`head...candidate` alone is not proof of ancestry. Any rate limit, error, truncation, or
inconclusive answer resolves to unknown, never to a release, and the hold is retained. The
candidates are worker-reported; the api binds them to its own record only where one exists (a
capture of the hold registered before the successor claimed, or the consumed completion permit's
head on an interlocked run), so the proof establishes containment of the reported commits.

### Graceful park and early-terminal disposition (D4)

PRD #1296 released a verified-empty hold only inside finalization capture. A graceful park
(`recovery_wait`, `limit_wait`, owner pause, shutdown/drain) and an early live-worker
`failed`/`cancelled` exit both bypassed that boundary, so a provably-empty hold could stay open
forever. The agent now dispositions custody by exact generation on those paths:

- **provably empty** (a verified fresh-forge comparison shows no unpublished committed output
  for this generation) → release the exact hold before requeue;
- **committed work** → pin the exact restore-point head in the authenticated journal, produce a
  generation-bound bundle, upload through the encrypted archive service, and release only after
  the ready acknowledgement;
- **upload or unverifiable** → RETAIN the hold and local source, record `needs_action`, and
  keep retrying model-free — never fabricate a capture or release to make parking succeed.

The unconditional local pin is credential-free; the credentialed git work (the fresh forge
fetch, the bundle produce) runs inside the reap-before-credentialed-git boundary, after the
agent tree is reaped, so a shutdown/pause stays a fast pin-and-retain and the credentialed
settle never blocks the control transition.

**Invariant**: a hard termination with no usable grace window (hard node-pressure eviction,
OOM, node loss, force delete) remains outside the guarantee — such custody is retained for
owner action, never released on a guess. The archive covers only committed Git history
represented by the verified checkpoint; it is an owner recovery artifact, **not** an automatic
cross-worker resume source (cross-worker resume still adopts the best-effort
tracking/checkpoint ref, and may redo work when that ref was unavailable).

### Causal empty-turn → limit routing (D5)

ADR-1197 (referenced above) parks every positively empty SDK turn as `recovery_wait`. That is
now narrowed: a turn that is *simultaneously* positively empty (`num_turns == 0`, no model
activity, plan, question, or completion) **and** carries that attempt's final latest-wins
rate-limit verdict `status == rejected` routes to the existing usage-limit path (`limit_wait`)
instead. The already-normalized final verdict is threaded from `HarnessTerminal` into
`TurnResult`; the server stays the authority for reset/type validation, fallback scheduling,
token selection, and `wait_on_limit` (so with `wait_on_limit=false` the newly recognized limit
fails fast rather than cycling as recovery).

**Invariant**: cause is never inferred from a token label, headroom, or timing correlation. An
earlier `rejected` followed by a later `allowed`/`allowed_warning`, a nonempty success, or any
empty turn without a final same-attempt `rejected` stays on `recovery_wait`. This adds no
lifetime cap to `recovery_wait` cycles (issue #1197's retry policy is unchanged); it only
routes the causally-corroborated case.

### Owner disposition surface (D6-D10)

PRD #1296 left a capless orphan hold with no owner self-serve discharge (#1318). This
amendment ships it. Owners can:

- **list** retained holds: `GET /api/recovery/holds` returns owner-scoped rows plus an
  aggregate (`open_holds`, `custody_hold_limit`, `decision_needed`, `blocked_runs`); each row
  carries a server-derived `attention` state, deliberately distinct from its capture state, so
  a healthy active hold is not presented as an incident (D6);
- **discard** one exact held source:
  `DELETE /api/runs/{id}/recovery-holds/{holdID}?confirm=discard` — the confirmation is
  validated **before any SQL**, ownership is enforced in the mutating SQL as well as the
  handler's owner-or-404 gate, and the operation marks the hold `discarded`, nulls its live
  worker/run references, discards its non-ready (preparing/uploading/needs-action) captures,
  and never touches an available archive or a sibling hold.

CLI: `uzi run recovery <run-id>` (list) and `uzi run discard <run-id> --hold <hold-id> --yes`
(discard). Both endpoints sit under `RequireUser` (session cookie or `uzc_`/`uza_` Bearer),
preserving the owner-only-never-viewer authorization above; the owner-wide list returns only
the caller's own holds, and the run-scoped discard resolves ownership through `GetRun`, so a
foreign owner or a read-only admin acting on someone else's run gets a 404. Archive deletion
(`DELETE /api/runs/{id}/archives/{captureID}`) stays artifact cleanup: with the parent hold
still open it does not pretend to resolve custody.

**Invariant**: a discard and a racing worker upload lock the hold and its captures in the same
order, so either discard wins and retry cannot revive it, or upload wins and its available
archive survives while the exact hold settles safely.

### One coalesced owner-level Slack episode (D10)

The redundant per-run custody Slack nudge is suppressed; the admission crossing is coalesced by
owner into at most one blocked-custody episode DM (facts only — open holds, blocked runs, a web
deep link, and the exact `uzi run discard <run-id> --hold <hold-id> --yes` command), respecting
the existing health-notification enablement and cooldown. The decision-needed breakdown is
surfaced in the web board alert rather than recomputed in the notifier loop, so the DM stays a
light aggregate read (see the Decision Log). The per-run web/CLI custody pill stays as row
context. Clearing below the limit closes the episode; a later crossing can notify again.

---

## Amendment 2026-09-16 — PRD #1392: a fifth release-evidence class for a generation that never adopted a source

**Status**: Accepted (PRD #1392 M1-M4 committed on this branch).
**Issue**: [vtmocanu/uzi#1392](https://github.com/vtmocanu/uzi/issues/1392)
**PRD**: [prds/done/1392-forge-unreachable-preclone-park.md](../prds/done/1392-forge-unreachable-preclone-park.md)

D2 above (as amended by PRD #1349) permits a hold to leave `open` only on one of four kinds of
durable evidence: publication, an available archive, a fresh-forge no-output proof, or an owner
discard. All four presume the claim reached a source to publish, archive, prove empty against,
or discard — a bare, worktree, or committed head that exists, even if empty.

That presumption does not hold for a **pre-clone** forge park (ADR-1197's 2026-09-16 amendment):
when `ensureClone` itself exhausts its retries with a transient forge error, the claim's
generation never adopted any source at all — no bare, no worktree, no committed head, nothing to
fetch against and nothing to prove empty. Evidence (3), the fresh-forge no-output proof, cannot
be produced here: there is no local source to compare against fetched forge history, and running
that comparison would need the very clone the forge is currently refusing.

This amendment adds a **fifth** release-evidence class, `no_adopted_source`, for exactly this
case: a generation whose claim opened a custody hold (fact 6 in the PRD, every recovery-capable
claim on a code-publishing profile does) but never adopted a source releases that hold on
`no_adopted_source` — no forge proof required or possible, because there is no source to prove
anything against. The release happens inside the same locked transaction that parks the run
(`SetState`, cause `forge_unreachable`), settling custody and parking atomically rather than as
a best-effort follow-up call — closing the same "park after releasing cannot promise zero holds"
gap D3 in the PRD's own Decision Log names.

**Invariant, matching the existing four:** `no_adopted_source` never substitutes for
`forge_no_output` on a generation that *did* adopt a source (even an empty one) — that generation
still needs the real fresh-forge comparison. And it settles only the exact (run, worker,
generation) hold the transaction validated; anything already on disk belongs to an earlier
generation and is inventoried separately after the next successful clone, exactly as D2 already
requires for every other evidence class. See
[adr/1392-forge-unreachable-preclone-park.md](1392-forge-unreachable-preclone-park.md) for the
full decision record.

## Amendment 2026-09-30 — issue #1742: a finalization-pinned head is bundled at restart without a forge PAT

Refines D5 (the restart-safe re-upload). Before this amendment the restart sweep could only
re-upload a bundle that was already journaled; a `pinned` record with no bundle, including one
pinned at the committed head H at finalization, was marked `needs_action` unconditionally, so a
worker restart between the finalization pin and the completed report left a source-only hold with
no archive to export even when H was sitting in the worker's local bare repository.

The restart sweep now looks at the previous process's records only (snapshotted before register, so
a live flight's record is never touched). A `pinned` record written by the finalization pin, which
now carries MAC-covered `bareDir`, `defaultBranch` and `finalizationPin` facts, whose committed
head is verified present in the local bare and is **not reachable from the bare's default ref**, is
bundled **self-contained without a forge PAT**: no forge tip is fetched, so there is no forge
prerequisite to satisfy. The bundle is journaled and uploaded at the record's exact generation, and
the legacy hold becomes `archive_ready`. Restart and quarantine capture without
an explicit guarded cached-dependency fallback option remains self-contained.
The guarded D5 exception above can produce a downloadable thin archive, but
thin availability alone never closes custody through archive-backed FINAL or
retires the source or pod. The independent completed-publication receipt in
the 2026-10-09 amendment can release the exact eligible completing generation.
Three limits carry over from the rest of D5:

- The size cap (`RECOVERY_MAX_BUNDLE_BYTES`, 64 MiB) is enforced on the bytes written while
  streaming, so an oversized bundle stops early and the hold stays `needs_action` (`oversized`).
  Without forge history to subtract, a PAT-less self-contained bundle may be oversized on a large
  repository.
- An **early pin** (the start-tip pin, or any pin that is not the finalization pin) is never
  bundled at restart and never read as "no unpublished work"; it is marked
  `early_pin_only_after_restart`. A finalization-pinned head already reachable from the default
  ref is marked `no_unpublished_work_after_restart`, and a head or bare that cannot be verified
  `source_not_verifiable_after_restart`.
- The restart-capture path leaves release evidence unchanged: it uses the five
  classes recorded above. The independent completed-publication class was
  added by the 2026-10-09 amendment.

See [adr/1742-finalize-resume-allowance.md](1742-finalize-resume-allowance.md) (D4).

## Amendment 2026-10-01 — issue #1995: a failed upload is re-driven while the worker is alive

Refines D5 (the restart-safe re-upload). A journaled recovery bundle whose upload failed while the
worker stayed up (an api outage, a timeout, a quota blip) was retried only by the boot sweep, so the
hold stayed `needs_action` until the next worker restart even after the api was reachable again.

**Live pass.** After each successful heartbeat the worker runs a bounded re-drive of journaled
bundles (`resumeLive`). It is single-flight with the boot sweep (it returns at once while either is
in flight), touches at most `liveMaxPerPass` records per pass (default 4), least recently attempted
first, and spaces passes with a capped exponential backoff: defaults 30 s base and 15 min cap, with no
lifetime cap on attempts. After the first transient pass the delay is 2x the base, doubling from
there. A pass with no transient failure resets it to the base. Candidates are only authenticated
records with a journaled bundle that are `bundled`, or `needs_action` for a transient reason or
`credential_rejected` (gated as below), of runs this worker is not executing; the upload is the
journaled-bytes upload (no forge PAT, a journaled bundle is never re-produced).

**Typed dispositions.** The api's `mapRecoveryError` now writes a stable `reason` beside `error` on
every recovery-service refusal (the handler's own request-shape 400s stay untyped and fall to the
status fallback) (additive; the `{"error"}` envelope is unchanged). The worker classifies a
failed reserve or upload by that `reason`, falling back to the HTTP status for an api without typed
bodies:

| Failure | Disposition |
|---|---|
| typed `not_authorized` | `stale_ownership` (permanent) |
| untyped or unknown 403, and other 4xx refusals (400, 404, 409; typed `ambiguous_generation`, `capture_not_found`, `not_available`, `manifest_conflict`, `bad_request`) | `upload_rejected` (permanent) |
| oversize (typed `oversize`, or 413) | `archive_constraint` (permanent) |
| 422 / typed `integrity` | retried only while the local bytes still match the journaled manifest; otherwise `local_bundle_mismatch` (permanent), never re-bundled |
| 401 | `credential_rejected`, retried only after a later successful heartbeat |
| 429, 408, 5xx (including 507 quota), transport failures | transient |

The disposition is stored in the record's existing `reason` string, not a new field: a new
MAC-covered field would make a rolled-back worker recompute a different MAC and refuse the record.
The permanent reasons are honoured at every entry point (capture, boot sweep, live pass): a record
carrying one is never uploaded again. A permanent refusal leaves the worker's record `needs_action`
with its bundle and source pin kept; that upload-refusal path releases nothing.
The independent completed-publication class in the 2026-10-09 amendment does
not change this upload-refusal decision.
A rolled-back (pre-#1995) worker still reads such a record but does not honour the disposition: its
boot sweep retries it once per boot.

**Immutability rules.** One per-capture cycle lock is the outer lock and the journal lock the inner
one; a live step takes the cycle lock in skip mode, so it never waits behind a foreground capture or sweep step on the
same record (which, in wait mode, waits for a live step that got the lock first). A bundle is installed temp-then-atomic only if the record still exists with the same pin
facts: a removed record ends the step `record_removed`, changed pin facts end it `source_advanced`,
and a bundle journaled meanwhile wins. Guarded post-pin writes never resurrect a released
record. Failure marking never downgrades `uploaded` and keeps `serverCaptureId`. A pin updates an
existing record atomically under the journal lock, with per-state field rules. Journaled bundle bytes
are dropped only when the record file is confirmed absent; an unreadable or unauthenticated record
keeps its bytes and is not written.

**Accepted residual.** A stream already in flight when cleanup releases the record cannot be recalled.
The server checks the hold once when the upload transaction starts, so it refuses the upload only if
the release committed before the upload transaction read the hold; a release that commits mid-stream lets the capture
become `available` on a released hold (an orphan archive until it expires). Either way the worker's
guarded write refuses to recreate the record. The `isExecuting` check is made at selection and again
under the cycle lock, not during the upload itself, so a run resumed mid-upload can overlap it
briefly; the bytes are immutable and a byte-identical re-upload is idempotent.

**Known gap, outside this change.** `release()`'s sibling count can treat a momentarily unreadable
sibling record as absent and remove the run dir with that sibling's bundle; it predates this change
(#1349/#1751) and was reported for separate follow-up.


### Issue #2021: transient recovery pin cleanup after durable upload

Fresh bundle installation adds the MAC-covered `recoveryPinBareDir` basename, independently of
finalization restart facts. Existing bundled records are never backfilled. After the server accepts
the bytes and the guarded authenticated `uploaded` write succeeds, one best-effort attempt resolves
the private bare and deletes only the transient run/generation recovery pin (`generation ?? 0`).
Missing metadata or capabilities, failed resolution and deletion failures retain the pin. Local
bundle removal still proceeds. A crash between the uploaded write and cleanup may leave the pin
indefinitely: no retry or cleanup scheduler is added. Settlement evidence pins and journals remain
until their exact release.

This accepts the MAC compatibility boundary noted above: an old worker refuses journals carrying
this new field, while the new worker reads old field-absent journals. Rollback restores retries,
inspection and adoption by returning to a compatible worker; refusal erases neither bytes nor pins.
No server rollout is required.

### Issue #2263: uploaded-record pin cleanup

Supersedes, for non-guarded records, the #2021 sentence "A crash between the uploaded write and cleanup may leave the pin indefinitely: no retry or cleanup scheduler is added." The coordinator now deletes a surviving transient pin (`generation ?? 0`) of an `uploaded` record, closing two leaks: bundles installed before #2021 / PR #2249 (no `recoveryPinBareDir`; when the record also has no `bareDir`, bounded discovery of up to 256 bares, ambiguity refused; a record with `bareDir` uses it directly; a record carrying both `recoveryPinBareDir` and `bareDir` passes both, and a disagreement retains the pin as `invalid_identity`) and a crash after the authenticated `uploaded` write.

**Archive proof.** Deletion requires an authenticated, non-guarded `uploaded` record with server capture id, checksum and size, `selfContained === true` and explicit empty `prerequisiteShas`; `archiveProvesPin` additionally requires no coverage digest, final request or completion receipt. A thin archive keeps its pin. Consequence: a park made with forge credentials usually bundles forge prerequisites, so its pin is now kept, where the runner deleted it unconditionally after upload. This was required at plan approval (issue #2263 plan review).

**Eligibility** is decided under the journal lock, held across a bounded, fail-closed journal read and the delete (lock order journal, attribution, bare): no inventory-guarded record in the run, every same pin-generation record uploaded, residue quarantine clear, run not executing; execution and quarantine are rechecked synchronously before the Git commit. `GitCache.cleanupArchivedRecoveryPin` makes one bounded attempt (30 s; the coordinator caps each record at 10 s, `PIN_CLEANUP_RECORD_MS`, within the 60 s boot budget): bare identity (no `.git`/`commondir`/`gitdir` redirects, unsafe config keys, symlinked reflog paths and a symlinked or hard-linked `logs/HEAD`), a bounded `cat-file -t` that must print `commit`, then `update-ref --stdin` prepare/commit compare-and-delete. Every failure retains the pin with a reason; an uncommitted transaction is aborted gracefully so no ref lock is stranded (teardown may add up to the 5 s abort grace beyond the cap).

**Paths.** Upload path: retains `executing` while the run is still executing (normal for the runner's own settle); the live pass deletes later. Boot sweep: after uploads, archive-proven records only, least recently attempted first, capped per pass, 60 s budget. Live pass: leftover upload slots only, per-record exponential backoff on refusal (the live re-drive base and cap), a 6-hour in-process settle interval after deleted or already-absent, and it runs while credentials are blocked (local work, no credential needed; it does not move the upload schedule).

**No completion marker is persisted.** A pin recreated by `anchorRecoveryHead` is caught at the next boot or settle interval; a marker would hide it.

**Runner and plain delete.** The runner's unconditional post-upload delete is removed. Its already-published path deletes with an expected-SHA compare-and-delete and honours the residue quarantine. Settlement cleanup and the owner-cancel custody release (inventory-guarded completion cleanup does not use it) keep their call sites, but the plain three-argument `deleteRecoveryPin` they call now refuses symlinked ref and reflog paths, repository redirects (including a `reftable` entry), a symlinked `packed-refs` and a symlinked or hard-linked `logs/HEAD`, so their behaviour changed: they retain the pin on unsafe metadata.

**Accepted residuals.** A symlink planted between the last check and Git's own unlink is not detectable in userspace. A v1 run (no claim generation) whose journal is removed at completed-run release before the live pass runs keeps its pin. The already-published delete gets one attempt.

## Amendment 2026-10-09 — #2507: synchronous completed-publication release

**Status**: Implemented in API and worker; this amendment records the maintainer
policy, not production acceptance or deployment evidence. It qualifies the
current D2 release classes and D5 thin/source-retention rules. Archive-backed
FINAL still requires a selected available, prerequisite-free covering archive;
thin availability supplies no cleanup authority.

### Exact completed identity and API proof

For an inventory-guarded COMPLETED `issue`, `mr_rework` or `self_improve`
run, both worker and API must support `recovery_completed_publication_v1`.
Completion commits with an immutable identity before proof runs synchronously
in the state-report path. Identity binds the exact hold, owner, originating
worker, run, completing generation, repository, forge connection/project,
branch, recorded MR and fixed reported final SHA.

The server branch is `agentIssueBranch(issue)` for issue runs, the frozen
`pipeline_ref` candidate for MR rework, and `selfImproveBranch(runID)`
(`uzi/self-improve/<run-id>`) for self-improvement. The latter is not derived
from a tracking issue or pipeline, and worker-reported branch text is not
proof authority.

`api/internal/workersvc/completed_publication.go` reads the server-recorded
MR summary in the run's own repository, requires SourceBranch equal to the
frozen branch, obtains BranchHead H, requires H equal to MR HeadSHA, and
requires `CompareAncestry(H, finalSHA)` to prove containment or equality.
This proves a matching own-repository copy; it asserts neither non-fork origin
nor branch protection. Later user deletion or rewriting is outside the proof.

Proof uses bounded forge requests without database locks. Release then locks
worker → run → exact hold, locks repository/configuration rows, and revalidates
identity with the CAS in
`api/internal/store/queries/completed_publication.sql`. Successful release
stores `completed_publication_receipt` atomically and clears the live custody
pointers. Missing, mismatched, erroring, unknown or drifting evidence retains
the hold with `completed_publication_reason`; completion remains committed.
The state ACK and owner hold DTO expose these fields. Exact receipt replay
authenticates original provenance and fixed final SHA before mutable
run/forge eligibility, so it does not depend on current forge state, a
surviving run row or a later claim.

### Independent of archive preparation

A valid receipt needs no capture, WIP commit, inventory freeze or clean-source
proof, including when available, thin or needs-action captures already exist.
Their retention policy remains unchanged, except that #2625 can expire a
proven-redundant guarded archive early (see the
[ADR-2417 amendment](2417-guarded-local-retention.md#amendment-2026-10-11--published-redundancy-early-expiry-2625)). Healthy completion custody release
adds no heartbeat prerequisite. The worker installs the fixed original
terminal outcome before preparation when its existing outbox is usable.
Completion-first ordering is negotiated; a valid receipt skips preparation.
Before protected report retirement, it must be written and read back in an
existing MAC-authenticated exact-generation recovery record. This is separate
from archive FINAL coverage authority, with no standalone receipt or tombstone.

No receipt, refusal, unknown ACK or mixed-version operation uses the original
capture fallback. An API advertising the feature but excluding a kind returns
no valid receipt and takes that fallback. If release succeeded but the ACK was
lost, a later capture refusal grants no cleanup authority and cannot fail or
reopen completion; replay of the fixed original completion recovers the stored
receipt. No-outbox and reserve-failure paths keep attempted-send guards to
prevent a false failed report; they gain no invented durable journal.

### Scope and physical retention

Only the exact completing-generation hold gains this completed-publication authority.
Older and successor holds remain independent under this path; the 2026-10-14
amendment adds a separate server-discard policy for earlier OPEN holds; unstamped older completed holds, including
earlier self-improvement runs, are not backfilled. Failed, cancelled and parked
runs and `ci_fix`, `prompt` and `task` retain their existing policy.

Physical destruction is a separate guarded decision, recorded in the
[ADR-2417 amendment](2417-guarded-local-retention.md#amendment-2026-10-09--completed-publication-receipt-authority).
Process quiescence, credential isolation, exact canonical owned paths,
expected SHA and attribution, execution/adoption exclusion through destruction
and bare locks remain required. Eligible dirty/untracked completing-source
leftovers may be discarded, while sibling generations, shared pins, metadata
consumers, foreign/unknown inventory and quarantine stay protected.

Existing archive source-boundary diagnostics, including failed/cancelled
foreground `execution_tail_present`, remain applicable to capture fallback.
This amendment adds no intent, endpoint, crash discovery, forge driver or E2E
mechanism and does not implement #2544 quota, #2545 failed publication or
#2506 failed thin release. (Superseded for #2545 only by the 2026-10-12
amendment below: failed publication is now implemented by
[ADR-2545](2545-failed-run-held-publication.md). #2544 and #2506 remain
excluded.)

## Amendment 2026-10-12 — #2545: a failed run's held work is published before custody release

**Status**: Implemented in API and worker. See
[ADR-2545](2545-failed-run-held-publication.md) for the full design.

This adds a fifth final disposition to the inventory-guarded hold:
`final_disposition = release_evidence = 'held_publication'`, bound by NULL-safe
CHECKs to a non-null `final_publication_id`. It applies only to a `failed` run
whose `fail_origin` is not `push_secret_blocked` or `worker_residue_blocked`,
for the exact claim generation, with a `created` publication row whose tip and
coverage digest match the FINAL. `recovery_inventory_hold_guard` enforces this.
The API first creates `refs/uzi-held/<run-id>/<generation>` with the owner's
stored connection credential from a worker-uploaded, scanned pack (step A). The
worker then sends a journaled FINAL `{kind:"held_publication"}` on the existing
release route (step B). Step B answers an exact replay before any mutable
check and lists the ref outside every lock. It releases under worker, run, hold
and publication locks.

Cancelled and parked runs, completed runs (still #2507's receipt policy) and
every unproved failed case keep the archive path, which is unchanged. Every
step-A outcome, and every definitive step-B refusal, short of an ACKed or
still-journaled held FINAL falls back to capturing an archive on the same frozen
record; a transient step-B answer keeps the journaled FINAL and the open hold,
with no archive, and retries. A local failure to journal the held FINAL after a
created step A (an unreadable or lost journal write) likewise keeps custody
without an archive until the worker's live re-drive or boot sweep, which replays
a FINAL that did land or otherwise captures the archive on the same record.
Retention for the held ref runs from
acknowledgement for `UZI_RECOVERY_READY_RETENTION`. #2544 quota and #2506 failed
thin release remain excluded.

## Amendment 2026-10-15 — report-only checker admission

Hold-free, nonpublishing children with exact `kind = 'cross_check'` bypass
owner custody admission for both Plan and Code cross-check, in both harness
families. This exemption is independent of [ADR-1751](1751-continuation-custody-admission.md)'s
bounded continuation: a previously claimed code-publishing run still needs
1–7 of its own owner-scoped open holds to bypass admission; at 8 it loses that
exemption. Judges and jobs gain no exemption.

The limit remains 8 and [ADR-2445](2445-custody-admission-accounting.md)'s
`fn_custody_admission_count` accounting is unchanged. Checker claims open no
custody hold; hold creation for code-publishing claims, disposition and capture
quotas are unchanged. Claim admission and queued-run health reasons exempt
checkers; `blocked_runs` already excludes them through its code-run kind list.
Owner identity, current parent/generation/round, deadline, protocol, vault,
credential, checker-slot and worker eligibility checks continue to apply.

This amendment addresses [#3011](https://github.com/vtmocanu/uzi/issues/3011).
It does not discount or release any existing hold.

## Amendment 2026-10-20 — the custody admission limit is configurable (#3150)

The "1–7 ... at 8" bound and "The limit remains 8" in the 2026-10-15 checker amendment above describe the default. The limit is now the admin setting `custody_hold_limit` (default 8, 1 to 1000), so the continuation bound is 1 through limit-1 of the configured limit. The checker exemption and accounting are unchanged. See the [ADR-2445 amendment](2445-custody-admission-accounting.md#amendment-2026-10-20--configurable-custody-admission-limit-3150).

## Amendment 2026-10-21 — #3239: a marker under a coverage wrapper is peeled at successor adopt

Coverage still covers the `wip(park):` marker: the guarded inventory, bundle and held publication key on it, and coverage construction is unchanged. At successor adopt (`prepareRecoverySuccessor`), a restore tip that is a coverage wrapper over a marker is verified and peeled: all aggregates share one fingerprint, tree == tree(source), the worker identity at the fixed 2000-01-01 date, the fingerprint recomputes, and a `refs/uzi-coverage/<runId>/…/<fp>` pin resolves to the tip (provenance). The successor then adopts the marker as it adopts a plain marker tip (`reset --soft` to its parent: content uncommitted, neither marker nor wrapper in history).

Under the original #3239 rule, leaves other than the source must be the source's ancestors or lineage-proven superseded markers. The [2026-11-02 source-selection amendment](#amendment-2026-11-02--durable-remediation-source-selection) adds a narrow authenticated remediation exception to the divergent-work refusal below. The worker-owned journal field `peeledMarkers` records markers peeled into the attempt that produced the source. Anything else (look-alike provenance, divergent committed work, a marker without lineage, lineage on another attempt) refuses before any write, keeping the source, pins and custody; the adopt attempt is recorded on the episode as `adoption_failed`. The original generic terminal failure is qualified by the #3527 amendment in [ADR-1197](1197-transient-recovery-park.md#amendment-2026-10-27-3527-owner-held-retained-adoption-retry): exact persisted evidence, API support, current ownership and proven quiescence permit an owner-held `retained_recovery_adoption_failed` park. A fresh owner grant and fresh owned claim authorize another charged attempt; without that grant it reholds without capture or model execution, and another adoption failure reholds. Unsupported or unproven cases keep the fail-closed fallback and custody. Past the episode deadline the stored blocker is instead `budget_exhausted`; the #3495 amendment in [ADR-1197](1197-transient-recovery-park.md) permits feature-negotiated bounded retries followed by an owner-held park for that confirmed persisted blocker. This reclassification does not prove the underlying adoption problem transient; renewed attempts must prove quiescence again.

With #3239 a coverage-wrapper tip that cannot be fully verified (unreadable tip, more than 8 aggregates, more than 66 leaves = 64 owed roots + the original source + the current source, or any failed git read) is refused even over a clean source, rather than adopted as before. A non-tip commit the walk cannot read within bounds (over 64 KiB or not valid UTF-8) is treated as a leaf under the leaf rule, unless its subject claims to be a coverage commit (then it refuses). The walk goes through a readable commit whose message claims the coverage prefix without the exact coverage message; once any such look-alike is walked, the adopt refuses if a marker is involved at all (any marker leaf, or a marker source), and more than 8 look-alikes refuse outright. Because a look-alike's parents are walked, a shape with no marker can also refuse where it used to be adopted (walk bounds or a failed read), always keeping the source. A peeled marker that is itself the restore tip is journaled as lineage too, and the watcher backup script accepts the new `peeledMarkers` journal field.

Limitations: lineage does not survive a retired clone (an earlier marker still owed as a leaf then refuses, custody kept); a checkpoint published before this fix whose tip is a wrapper over a marker and is adopted via the tracking or checkpoint legs is not peeled (rollout boundary); an older worker's strict journal parser rejects the new key (fails closed, journal kept). Marker vetting at capture is in the [ADR-0759 amendment](0759-protect-run-work-usage-limit-park.md#amendment-2026-10-21-issue-3239-the-wip-auto-save-refuses-core-dumps-and-oversized-files-and-vets-its-own-marker).

## Amendment 2026-10-29: reuse covered marker-free recovery sources

Repeated provider retries without model progress must not nest another recovery wrapper merely because a successor retained the preceding aggregate as its physical HEAD. New guarded inventories may reuse that current aggregate when its bounded commit shape and worker identity match and every original root is positively ancestral to it. This bounded marker-free reuse proof does not consult provenance pins; it supplies no successor source-selection authority. A coverage-looking header is shape, not provenance; marker involvement, divergent roots or unproven eligibility never gain reuse authority. The [2026-11-03 amendment](#amendment-2026-11-03--ordinary-marker-free-source-selection-3781) separately requires authenticated provenance to select the journaled content source for execution, retaining the eight-aggregate bound. The [2026-11-02 source-selection amendment](#amendment-2026-11-02--durable-remediation-source-selection) permits proven divergent adoption; it does not widen marker-free reuse.

Reuse preserves the current tree and all frozen roots, recomputes the inventory fingerprint, and writes a separate ref in the existing run/generation/fingerprint namespace. It does not share ref ownership across generations or release any source, archive or custody. Eligibility depends on immutable objects and ancestry, not on whether an earlier generation's coverage pin survives cleanup. A new inventory fingerprint need not equal the reused commit's embedded fingerprint, so the optional archive-redundancy reader may return no claim. Full frozen-source proof and ordinary archive FINAL do not depend on that optimization.

The producer defaults to legacy aggregate construction. New inventories opt into reuse; authenticated nonempty coverage records with `sourceSha === inventoryCurrentSha` require reuse during re-proof, without a fallback that could overwrite their pin with a different aggregate. Records whose source differs retain byte-identical legacy reconstruction. No journal field, database counter or protocol feature is added. A pre-fix worker cannot re-prove a reused record: rollback or mixed-version replay retains it fail-closed, and a new-code worker is needed to settle it.

An owner Resume of the affected ninth-wrapper hold can reuse the last successfully adopted eight-deep physical source when it still covers every root. An already-overbound physical source is not bypassed. Marker-involved recovery keeps legacy wrapping, and recoveries with intervening committed progress can still grow wrapper history; this amendment does not claim to solve those distinct shapes.


## Amendment 2026-10-29: fair ordinary inventory reconstruction (#3614)

The #2381 discovery FIFO is separate from the #1995 direct-replay selection above.
It rotates raw contexts (default cap four) before execution/hold checks; retained
metadata with no eligible hold can therefore consume turns. Empty candidate lists
alone do not make a context disposable.

Guarded live materialization, but not boot, reserves at most one turn for an ordinary
invalidated inventory. Complete checked records, canonical context equality and both
execution predicates determine this scheduling hint. Every frozen guarded sibling in
the generation must be replaceable; a non-replaceable successor, saved FINAL, ACK or
receipt ends priority. Selected contexts rotate behind survivors without changing
queue membership/multiplicity. Remaining turns stay FIFO; no hint or cap below two
retains the original FIFO. A failed fresh recheck wastes that already-rotated priority
turn, leaving cap-1 FIFO turns. The hint grants no authority: all fresh custody,
source, ancestry, immutable-request, quota, lifecycle and retirement proofs remain.

For P continuously eligible contexts with complete readable hints, priority progresses
within P completed materializing passes; normal survivors within ceil(N/(cap-1)).
A permanent refusal may keep the reserved turn; these are pass bounds, not a wall-clock
settlement SLA. Removed/re-added entries are new arrivals. Hint I/O failure warns and
falls back to FIFO; budget exhaustion and cancellation still end the pass. Hint reads
check cancellation between runs and do not run on non-materializing/blocked passes.

The local checked view is read once per candidate run immediately before live
materialization. #3446 D-U5-5's evidence-first order, once landed, stays first; the hint
moves with materialization in either landing order. Producer admission, #3577 retirement
proof and journal/API formats are unchanged. Pruning remains #2823's carried #2414 work
with independent fail-closed authority. Measurements and regression evidence belong in
the implementing PR.

## Amendment (2026-11-01, #3727 C2): D5 thin archive FINAL on a pinned prerequisite closure

**Status**: Implemented by C2 of [PRD #2506](../prds/2506-prerequisite-closure.md); builds on the pin lifecycle in [ADR-3591](3591-prerequisite-closure.md). The parent #2506 stays open (C3, E5 and parent acceptance).

D5's refusal of a thin archive FINAL is narrowed, not removed. For an inventory-guarded **archive** FINAL only, a capture that lists prerequisites may release its hold when the API proves, at release time, that the capture's closure is pinned: the closure row is bound to the exact capture and hold, its checksum and prerequisites match the capture, no absence is recorded, and one fresh listing of `refs/uzi-prereq/<capture-id>` on GitHub.com shows the recorded tip. The hold then records `release_evidence = prerequisite_closure` with a `final_closure_id`. Without a positive listing the hold stays open: a recorded absence is refused as `prerequisite_pin_missing`, a different tip as `prerequisite_pin_foreign` (409), and an unlistable pin, timeout or limiter refusal as `prerequisite_pin_unavailable` (503). The proof is archive-only; it adds nothing for publication, held-publication or empty-inventory dispositions, and the server still does not parse bundle bytes (T1-T5 of ADR-3591 are unchanged).

The worker mints a thin archive FINAL only under a transient grant and only against a pinned, matching closure; an unknown or transient result never grants. There are two grantors. The frozen path (`frozenOperationCycle`) can pass four grants: (1) missing v2 held metadata on a non-uncertain create; (2) proven held ineligibility; (3) an attested held-publication fallback in the same pass; (4) a feature snapshot naming the closure feature and neither held feature. The #2969 retained-claim caller (`captureAndUpload` with `closureMint: "retained_claim"`) is limited to grants 1 and 4; it runs while the run still executes, so it captures without minting and the mint comes from the later frozen pass. The wider predecessor settlement built for #2969 is disabled in production; see the [#2969 amendment](#amendment-2969-predecessor-guarded-hold-settlement-is-built-and-disabled-in-production). At the API, a committed release replays to its original ACK without forge I/O or admission. A saved thin FINAL's worker replay uses its MAC and bundle header independent of the pin, expiry or knob, subject to the fresh captured-C restriction guards in the [2026-11-02 amendment](#amendment-2026-11-02--durable-remediation-source-selection).

Mixed-version limits, all fail closed:

- C1b API with a C2 worker: the API does not advertise `recovery_prerequisite_closure_v1`, so the worker never mints a thin archive FINAL and the hold stays protected.
- C2 API rolled back to an older binary without running Down: the older code cannot observe a closure-released hold or classify it across runs, and fails closed on both. A replay of an archive FINAL still ACKs by kind, capture, source and digest.
- C1b binary against the C2 schema: it never writes the new `prerequisite_closure` evidence or `final_closure_id`.

Accepted residuals: a thin FINAL stays stuck and is retried while the pin or the feature is unavailable; available-pin audits run at about 24 hours, so an external deletion can go unnoticed for up to a day before release-time listing records the absence and refuses; an external writer deleting or rewriting the pin after release defeats restore (the external-writer residual above is unchanged). A closure-released hold remains not history-safe for forge placement (`fn_forge_source_history_safe` is unchanged).

Deploy-window cost: the schema migration (00393, renumbered at landing from draft 00389) takes `ACCESS EXCLUSIVE` on `recovery_custody_holds` for the combined `ALTER`. Measured on synthetic data it is about 0.4 s per 500k holds (about 0.8 s per million), plus up to 5 s of `lock_timeout` waiting; live row counts and a cold cache are unmeasured. Its Down refuses (SQLSTATE 55000) while closure-backed rows exist.

Admission bound: each closure-backed FINAL that survives the unlocked precheck and is admitted spends one unit of a per-worker bucket (`prereq-final-list|<worker>`) on the proposal limiter, never refunded. Replays and precheck refusals are not charged. The limiter is a fixed window per API process, so the bound is `PROPOSAL_RATE_LIMIT_MAX` per window per worker per API process, up to twice that across a window edge, multiplied by the number of API replicas. An admitted request costs at most two `GetProject` calls and one `ls-remote` on the owner's stored token; it can stop before the listing when under 3 s of the 6 s budget remain or the reservation fence is lost.

Coordination with #2884: it also rebuilds the `recovery_inventory_hold_guard` function and the release-evidence CHECK. Whichever lands second must rebuild from the other's live body rather than from its own copy; neither migration may drop the other's branch or constraint value.

## Amendment 2026-11-02 — durable remediation source selection

This amendment narrows #3239's divergent-leaf refusal for source selection. A
worker may continue from an exactly identified, remediated source S while a
coverage envelope C retains excluded history for custody. S and C are distinct:
the proof admits neither execution of C nor its publication, archive coverage,
FINAL, release or ACK. Ordinary divergent work, equal trees and convincing
commit subjects supply no authority. The 2026-10-29 marker-free reuse rule and
the 2026-11-01 C2 release amendment remain separate contracts.

The refusal also applies beyond enrolled remediation: missing or unverified
`restoreSource`, or divergent marker-free coverage without verified
source-selection evidence, refuses before successor clone creation, retaining
work, pins and custody. Source-bound ordinary ancestor-only adoption now selects
the authenticated journaled source under the full proof in the
[2026-11-03 amendment](#amendment-2026-11-03--ordinary-marker-free-source-selection-3781);
definite ordinary ineligibility alone retains the pre-existing tip fallback.
Source-less legacy coverage could previously seed C and then require
a base hold; the current refusal occurs before successor creation when source
binding is absent. Equal trees or leaf dominance cannot supply authenticated S,
and no legacy exception grants new authority. The existing feature-gated,
persisted-blocker, current-ownership and quiescence requirements for an
owner-held `retained_recovery_adoption_failed` park and fresh owned retry grant
still apply; unsupported or unproven cases keep the existing fallback and
custody. Historical journal lineage remains unknown without the journal, and
old captures remain held.

### Worker grant, import and source proof

The worker records trusted findings and remediation decisions in the attempt-bound
optional version-1 `remediationRecovery` journal. A positive grant has origin
`checkpoint-pending` or `pre-exit`, its sequence identity, immutable original
run/clone/attempt binding, scanned tip T and flagged commits Q. Persisting a
negative finding alone opens no grant. The bounded parser accepts at most 64
edges and 64 distinct flagged OIDs, with a 64 KiB serialized record cap; excluded
roots additionally have a 1524-entry ceiling within that byte cap. Identifiers
have field-specific byte and syntax limits (run 128, branch 1024, clone path
4096, attempt 256 bytes), normalized absolute current clone paths, 40-hex OIDs
and canonical context names. Identifier validation rejects control characters,
line/paragraph separators and bidi marks/embeddings/overrides/isolates through
`sanitizeForLog` equality; branch syntax is checked separately. Unknown keys or
versions, duplicate/conflicting edges, cycles and overflow refuse; evidence is
never evicted to fit. The worker and watcher backup parser enforce the bounds.
Only the existing authenticated clone-path identity is stored, with no secret
text, finding paths, commit subjects or credentials. Older strict workers reject
the new key and retain the journal; rollback must not strip it to regain use.

An authorized displacement records D → R, from preserved displaced source D
to imported replacement R. The worker proves Q → T → D by ancestry, checks
that the grant belongs to the same producing attempt, preserves D under its
owed context, and writes a `prepared` edge before the import. Ownership and
tracking receipt readback precede the `committed` edge write. A prepared edge,
missing objects, unknown ancestry, changed identity or incomplete proof refuses
source adoption. The current tracking head is custody evidence, not an
attempt identity or permission to rewrite the source journal.

Writes fsync the config and its directory and require exact readback. Consumers
repeat both syncs under the bare-repository lock and require canonical first-read/
second-read equality for the whole record; reading never promotes prepared edges.
They independently re-confirm the
live current attempt ledger, original grant bindings, current HEAD, tracking
ownership, preserved contexts and Q → T → D. A readable old record cannot
discharge newer pending negative facts. An exact current owned-tip trusted scan
closes an active grant only when its applicable history is clean and every
persisted flagged OID is positively divergent. Closure keeps completed edges;
subsequent ordinary divergence gains no authority. These checks are implemented by
`parseRemediationRecovery`, `recordRemediationScan`,
`readRemediationUnderLock` and the import path in
[git.ts](../agent/src/git.ts).

For adoption, each divergent non-marker leaf must be an authorized displaced
head or its ancestor. Each authenticated edge's replacement must reach S,
directly by ancestry or through later independently authorized displacement
hops in recorded order; an earlier replacement may have ordinary progress
before the later displacement. A later hop still needs its own edge proof,
even when one active grant authorizes several partial rewrites. A lineage-proven
marker must be in the same attempt's preserved `peeledMarkers`, and its parent
must be covered by source ancestry or an authorized displacement. Marker-free
divergent wrappers require the same full fingerprint, worker identity, tree,
coverage-pin and leaf proof. Marker-free shape alone does not bypass it.
The eight-aggregate and bounded wrapper-walk checks remain.

The non-marker source path seeds exact HEAD S with a clean index matching S
and verifies no unstaged diff, without importing C's excluded parents. The existing #3239 marker
path instead soft-resets to the marker's parent, retaining its content in the
index; the remediation path additionally proves forward reachability to that
execution parent and excludes the peeled source. Neither path infers permission
from a clean tree. See `resolveMarkerWrapperAdoption`,
`proveRemediationForwardTarget` and `prepareRecoverySuccessor` in
[git.ts](../agent/src/git.ts).

### Continuation, publication and cold consumers

Before successor admission and model launch, the enrolled path requires a
trusted merge-aware history scan and positive divergence of the restricted
roots from the execution source and resolved floors. The shared floor union
includes published candidates, range exclusion, scan floors, pack exclusions
and the default-blob tip; an empty flight-local floor list cannot hide a flagged
default-branch ancestor. Before bridge, pack creation, push and producer replay,
the worker checks the actual source, transformed candidate and floors again.
Untrusted scans, unknown ancestry or changed evidence hold the operation.
Required publication floors remain intact; a conflicting floor causes a hold.
[runner.ts](../agent/src/runner.ts) implements these boundaries in
`requireLaunchAuthority`, `remediationRangeFloors` and
`withRemediationCandidateAdmission`; Git's locked admission checks retain the
candidate/tracking binding through their writes.

A verified successor transfer preserves immutable grant origins and records the
new current clone/attempt binding, with synchronized readback before use.
Flagged OIDs, displaced heads and excluded roots survive `ready-for-model`
and episode completion; clearing the episode removes no restriction record.
Fresh runner, GitCache and recovery-coordinator consumers must authenticate
those persisted facts rather than restoring a process-local clean grant.
Enrolled-source retirement/reanchor and ordinary metadata-loss rebind
**refuse**. Protected or retired records confer custody evidence alone.
A successful enrolled-source retirement/reanchor/rebinding path is not
implemented and needs a reviewed authority design; this amendment promises no
such transfer. The relevant guards are `assertRemediationPreserved` and
`retireRunnerClone` in [git.ts](../agent/src/git.ts).

An uploaded RecoveryRecord proves retained bytes, not quiescence. Source-use
admission still requires joined execution cleanup and the existing process
proof; an unresolved tail or surviving in-scope writer supplies no adoption
authority. The focused incident, publication, quiescence and cold-restart
fixtures are in
[git-remediation-incident.test.ts](../agent/test/git-remediation-incident.test.ts),
[runner-remediation-publication.test.ts](../agent/test/runner-remediation-publication.test.ts),
[runner-remediation-quiescence.test.ts](../agent/test/runner-remediation-quiescence.test.ts)
and [runner-remediation-restart.test.ts](../agent/test/runner-remediation-restart.test.ts).
The restart fixture covers adopting, ready-before-progress and episode-complete
cuts, then fresh consumers after an actual refused enrolled retirement; it
does not demonstrate successful reanchor.

### C1b and C2 remain independent

Private capture, upload and export are unchanged. Such a capture may trigger
independent C1b T1–T5 prerequisite proof and a fresh default-tip D pin, after
that proof's own checks. D is not the recovery envelope C. Adoption supplies
no prerequisite-pin or closure authority, and an independently existing pin
does not authorize flagged or excluded C.

C2 still requires its own transient grant, verified ordinary local thin capture,
feature and pinned matching closure, plus the grantor-specific checks in the
[2026-11-01 appendix](#amendment-2026-11-01-3727-c2-d5-thin-archive-final-on-a-pinned-prerequisite-closure)
and [ADR-3591](3591-prerequisite-closure.md#amendment-2026-11-01-3727-c2-release-consumes-the-pin).
Source adoption grants no FINAL mint, replay, release, ACK or archive coverage.
The coordinator evaluates restrictions against the actual captured C, not
clean S, for new mint, saved FINAL replay, release, receipt reconciliation
and ACK. Held-publication preparation and transport likewise guard actual C.
A blocked or unavailable record, or a restricted root not proven divergent
from C, holds those effects even if C has an independent prerequisite pin.
Checked journal writes acquire the journal lock then the bare lock and retain
both through the fresh negative-proof check and write; reads around awaited
work re-confirm the record and restriction fingerprint.
See `restrictionsAllowCapturedC` and `writeRestrictionCheckedRecord` in
[recovery.ts](../agent/src/recovery.ts).
The independent #3727 closure proof remains necessary.

There is no database or accounting change here. Existing admission and custody
logic remains unchanged; worker-call spies and fixed-input fixture counters
are not SQL hold-count evidence.

### Historical incident boundary

The supplied sanitized incident chronology records `secret_found` at 21:35 and
`secret_remediation_pending` at 22:00. The 00:06 rebase/amend removed reported
text; the first displacement at 00:08 excluded `6230950e` (including `dc9c064d`,
on `a871f5ec`) while the checkpoint gate was pending, before the 04:55 pre-exit
prompt. A later rewrite displaced `91bdbe51` and produced `a338f6f5`. The earlier
`c7459622` → `dd3f4f10` marker peel was not remediation authority.
Legacy incidents lack the new attempt-bound grant/edge records and remain held.
Without the GitCache journal, marker lineage membership is unknown, not a
passing proof. The sanitized trace establishes neither adopt nor release
authority. #3583 independently owns its historical publication investigation
and any release of rewritten roots; this change neither diagnoses it nor depends
on publishing those roots.

## Amendment 2026-11-03 — ordinary marker-free source selection (#3781)

### Execution source and custody envelope

For general ordinary marker-free recovery, successor preparation selects the
content source S instead of executing the coverage wrapper W only after
authenticating the current run/clone/attempt and captured episode journal,
including its exact `restoreTip = W` and `restoreSource = S` binding and episode
pin. Neither equal trees nor a dominant leaf establishes that binding.

The complete bounded wrapper graph must verify: at most eight aggregates and
66 leaves, exact coverage messages, worker author and committer identities at
2000-01-01 UTC, every aggregate tree equal to S's tree, one common embedded
fingerprint that recomputes from the complete leaf set and S (with or without
S among the fingerprint's roots), S itself present as a leaf, and every other
leaf positively ancestral to S. The original embedded fingerprint must name a
`refs/uzi-coverage/<runId>/<generation>/<fingerprint>` ref pointing to exact W.
A later reuse pin with a different inventory fingerprint does not substitute
for that original provenance pin. Nested aggregates with the same fingerprint
can qualify; mixed fingerprints cannot establish this proof.

These checks run under the existing bare-lock/handoff boundary. Independent
source ownership, attempt/episode, reservation, object-closure, quiescence and
source-use admission checks still apply, including the final HEAD/tree recheck.

On success, execution HEAD and index tree are exactly S, with no unstaged
diff. Raw `RunnerClone.adoptedSource` can still be W for capture attribution;
`adoptedContentSource` is S. The frozen coverage envelope, original roots,
source mapping, generation context, coverage and episode refs, predecessor
clone and archive custody remain intact. Source selection releases nothing.
The aggregate's custody role does not make it execution history.

A definite negative provenance result permits keeping W only in the
pre-existing ordinary branch: no marker source or marker leaf, no divergent
leaf and no remediation enrollment. For example, a positively observed missing
original coverage pin or mismatched fingerprints can make that branch
ineligible for the S peel. This is not a new fallback for errors. Missing or
unverified source binding, missing objects, failed reads, unknown ancestry,
walk-bound exhaustion, marker verification failures and remediation refusals
still refuse before successor ledger or clone effects. The #3239 divergent
marker/lineage rules and the separately authenticated remediation exception
remain unchanged.

The 2026-10-29 aggregate reuse proof is deliberately separate: bounded,
pin-independent reuse may preserve physical W even when W lacks authority to
select S. It preserves the frozen inventory and writes its own generation pin;
it cannot mint provenance or replace the original embedded-fingerprint pin.
Owed roots are settled separately by later `reconcileOwedCandidates` against
confirmed published ancestry. Selecting S, retaining bytes or preparing a
successor does not itself settle them.

### Publication and restriction boundaries

For publication-only recovery, S must equal the exact recorded candidate C,
not merely have the same tree or contain C. Authenticated W may cover C without
becoming C or adding its parents to execution history. ACK-lost reconciliation
continues to reconcile the existing publication intent and candidate under its
own proofs; this amendment grants no new publication, release, FINAL or ACK
authority. Existing publication floors and committed work remain protected;
genuine progress beyond C still requires the existing model-required hold.

The #3816 fresh restriction-facts, lifecycle and denial-revision checks remain in force. Durable
negative evidence is read afresh and rechecked across awaited work and locked
writes; a changed restriction fingerprint, blocked or unavailable evidence
cannot be replaced by an earlier positive observation. Capture restrictions
still apply to the actual captured envelope (called C in the 2026-11-02
remediation amendment), not the clean execution source. Source selection does
not bypass denial, restrictions, prerequisite proof or custody reconciliation.
There is no database, journal schema, protocol feature or accounting change.

### Observed lifecycle and upgrade limits

Controlled real lifecycle fixtures exercise both producer/consumer orders:
original aggregate production with the new consumer, and new reuse production
with controlled original source selection. They perform actual immutable
freeze, upload, capture-journal recording and successor preparation. They are
not live deployment evidence. With eligible source selection, the Claude and
Codex publication fixtures repark on another MR-create outage and complete
after explicit owner Resume without model/helper/session execution; stable
roots reuse the same W and execution remains exact C.

The observed legacy boundary is narrower than universal upgrade repair. When
original selection first executes W and holds, a subsequent authenticated W/W
recapture can initially peel to C on owner Resume while the original matching
pin survives. Authenticated source carry-forward requires the newly captured
source to equal the previous restore tip exactly; a same-tree object cannot
supply that mapping. A second MR-create park leaves unpublished W as an owed root;
the next envelope can then mix embedded fingerprints and retain W through the
ordinary fallback, preserving the model-required hold. In the sibling fixture,
lawful generation cleanup removes the original pin while a later reuse pin
survives. Fresh GitCache/coordinator consumers after restart retain W and the
hold; that later pin does not restore original provenance authority.

These retained cases require manual export, recovery and delivery under the
existing safety and custody rules. No automatic upgrade repair, source discard
or release is promised. Old wrapper history and floors are preserved; an
already overbound graph is not bypassed. #3583's historical publication
investigation and #1324's separate maintainer acceptance remain unresolved
and outside this fix.
