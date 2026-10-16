# ADR-2545: terminal held work is published to a forge ref before custody release

**Status**: Accepted (implemented in API, worker, CLI and web; live and deployment acceptance not claimed here)
**Date**: 2026-10-12
**Issue**: [vtmocanu/uzi#2545](https://github.com/vtmocanu/uzi/issues/2545)
**Refs**: #2545, #3030, #2507, #1867, #1810, #1964 (and the #1296 custody model it extends)
**Amended**: 2026-10-15 (#3030 terminal held publication V2)

## Context

An inventory-guarded custody hold ([ADR-1296](1296-durable-run-recovery.md))
keeps a run's unpublished local work on the worker until a verified final
disposition. Before this change, a failed run's only final disposition for
work after its last checkpoint was an archive capture uploaded to the API
database. #2507 added a forge-proven release for COMPLETED runs only and
listed #2545 (failed-run publication) as out of scope.

A failed run's held work is usually ordinary commits on the run branch. Putting
them on the run's own forge, under a namespace no CI watches, gives the owner a
plain `git fetch` and keeps the bytes out of the API database. The archive stays the fallback when publication cannot be proved; release still
requires its existing coverage and verification guards.

## Decision

### Scope

V1 remains failed-current-generation publication. Widening requires both the
server feature and worker capability `recovery_held_publication_v2`. Under V2,
the server run must be terminal (`failed`, `cancelled` or `completed`), with
neither `push_secret_blocked` nor `worker_residue_blocked` as its failure origin.
Parked runs are outside this extension. Hold-opening kinds remain issue,
MR-rework, self-improvement, `ci_fix`, prompt and task.

The exact positive generation must have ended and be no later than the current
claim generation. Its inventory-guarded hold must still be open and live on the
calling original custodian, with matching run, user and repository. The run's
current worker need not be that original worker. This grants no adoption or
impersonation authority and does not widen live-predecessor settlement.

Four sites enforce the boundary:

- worker terminal selection and ordering (`heldEligibleTerminal` in
  `agent/src/runner.ts`, and `agent/src/recovery.ts`);
- step A's scope check (`heldScope` in `api/internal/workersvc/held_publish.go`);
- step B's locked eligibility (`heldStateEligible` in
  `api/internal/recovery/held_final.go`);
- the `held_publication` branch of `recovery_inventory_hold_guard`, replaced by
  migration `00340_terminal_held_publication.sql` from migration 338, preserving
  its non-held disposition branches. The database guard binds the publication
  to the exact generation, original worker, live custody, tip and coverage.

Failed and cancelled runs try held publication before deferred capture, with
archive fallback on the same frozen record. Completed runs without an applicable
completed-publication receipt stay archive-first: a self-contained archive FINAL
or fully verified empty inventory remains sufficient, including for `ci_fix`,
prompt and task. Only a prerequisite-bearing thin archive or structured typed
oversize/quota refusal permits the held alternative on that same record. A
success report or unverified head is not publication proof. A receipt covers its
own completing generation; it cannot cover or suppress an earlier generation.
Earlier-generation work is preserved normally, with no new discard policy.

### Namespace: `refs/uzi-held/<run-id>/<generation>`

`pushbroker.HeldRef` names a new managed namespace, outside `refs/heads/` so
no CI runs on it, and outside `refs/uzi-checkpoints/` so worker mirrors and new
run seeds never fetch it. The existing namespaces do not fit:

- `refs/uzi-recovery/<run-id>` ([ADR-1810](1810-checkpoint-retention-follows-custody.md))
  preserves a superseded checkpoint tip *while* custody is open, and is owed a
  CAS delete once the run's last hold is released or discarded. Releasing the
  hold against it would make that release delete the only copy at once.
- `refs/uzi-salvage/<run-id>` ([ADR-1867](1867-failed-run-salvage-ref.md)) is
  one copy per run of the last *published* checkpoint. A sweep creates it only
  while that tip is still live under the checkpoint or recovery ref, and only
  on forges listed in `UZI_SALVAGE_FORGES` (default off). Held work is
  exactly what was never published, so salvage cannot carry it. Its
  single-per-run shape would also collide across claim generations.
- The generation in the path means each claim generation's held work has its
  own ref. A later generation never collides with an earlier one.

A held ref is created only by a create-only push (receive-pack with old = zero
hash, so the remote refuses an existing ref). It is removed only by a CAS delete
on the recorded tip. It is never moved or forced.

### Credential boundary

The worker builds and uploads a pack over its worker bearer token. It names only
the tip, the generation and the coverage digest. The API derives the
repository, user, worker, ref and connection from the hold and the run, applies
the SSRF gate, and pushes with the owner's stored forge connection
credential. The forge PAT never enters the runner clone's Git config or the
worker. The held step requires server-proven terminal custody and never reopens agent
execution.

### Worker preparation (merge-aware scan, #1964 gate, same range packed)

The tip is the frozen inventory record's `sourceSha`, which already covers every
root and the WIP marker commit. The worker:

1. resolves the checkpoint range against forge-present bases: the confirmed
   checkpoint tip, the published branch tips and the default branch;
2. runs the merge-aware checkpoint secret scanner (`secretScanCheckpointRange`,
   [ADR-1597](1597-midturn-checkpoint-durability.md)) over that range. Merge
   contributions are scanned too, and gitleaks' silencers are off;
3. applies the #1964 remembered-finding remediation hold to the candidate;
4. refuses a range that authors a `.github/workflows` change, because the
   stored credential may lack workflow scope;
5. packs that same range object (capped at the route's 64 MiB) and uploads it.

A finding, untrusted or timed-out scan, unresolved range, pending remediation,
workflow refusal, pack-tip mismatch, oversize pack or definitive create refusal
can use archive fallback on the same frozen record. V2 unknown create/upload
outcomes retain the invocation and journal for read-back instead of starting an
archive. Release requires a qualifying complete archive or the source remains
protected; committed-only coverage cannot discharge unproven residue.

### Step A: create the ref (`POST /api/worker/runs/{id}/held-publication`)

- **Route-specific deadlines.** The base listeners time out at 15 s. This one
  route extends its own deadlines via `http.NewResponseController`: a 120 s
  body-read deadline, one 150 s service context (`workersvc.HeldServiceBudget`)
  for the pack pre-verify, the marker transaction, the push and its read-back,
  and a 30 s answer grace. The write deadline is their 300 s sum. The worker's
  per-request timeout for this route alone is 330 s
  (`HELD_PUBLICATION_TIMEOUT_MS`, `agent/src/client.ts`). At most four
  requests are in flight process-wide (429 `busy` otherwise).
- **Pre-body eligibility.** Ownership, switch, worker capability and scope are
  checked before the body is read. If a create was already invoked for this
  identity, the answer is reconcile-only and no body is read.
- **At most one create per publication identity.** The pack is pre-verified
  read-only (budget, base fetch, in-memory apply, tip present). Then one
  transaction locks worker, run, hold and publication in that order,
  re-checks scope, upserts the `run_held_publications` row for
  `(run, generation)` and commits a monotonic `create_invoked_at` marker
  *before* the network create. Only the request that sets the marker sends.
  Every other request after that marker only lists the ref. If the worker's
  first request never arrived, an empty reconciliation probe can return
  `pack_invalid`. That response is not proof of absence: the worker may
  re-verify and resend the same frozen identity's pack through this same
  locked marker. A late original request and the retry still have only one
  create winner. Preparation failure or another ambiguous answer keeps custody
  and the uncertainty marker; it does not authorize archive fallback.
- **Unknown outcomes only reconcile.** The broker reports `created` only on a
  read-back at the tip, and `refused` only on an explicit `ng` with no ref of
  ours. Everything else, including a deadline the forge outlives, is
  `create_unknown`. It is reconciled by listing and is never made terminal by
  an absent ref. Outcomes are recorded on a context detached from the expired
  service deadline.
- Database triggers pin the row's identity and the one-way state machine.
  Rows are born `prepared`, and a row that is or may be the only record of a
  remote ref cannot be deleted.

### Negotiation and switch

Three independent facts must hold:

- the worker reports the matching protocol capability: `recovery_held_publication_v1`
  for V1, or `recovery_held_publication_v2` for the widened paths
  (`api/internal/capability/capability.go`);
- the API advertises the matching server feature in `protocol_features`
  (`api/internal/handler/worker_protocol.go`). Failed/cancelled V2 paths defer
  capture for held publication; completed inventory remains archive-first;
- the switch `UZI_HELD_PUBLICATION` (default on) is set. Off removes both
  advertisements and refuses new publications and new held FINALs. Exact-hold
  reconciliation, immutable FINAL replay, cleanup and owner routes remain
  available, so existing refs stay visible and deletable.

### Step B: journaled FINAL `{kind:"held_publication"}`

After step A answers `created`, the worker journals a held FINAL
(publication id, source SHA, coverage digest) in its MAC-authenticated
generation recovery record. It sends the FINAL once the run no longer executes,
on the existing archive-release route. A crash or lost ACK replays the exact
journaled identity.

The API answer order is binding:

1. stateless shape;
2. immutable ownership, then the receipt compare with no mutable read, so an
   exact replay of a released receipt gets the original ACK even after the
   switch, capabilities or forge went away (replay-first);
3. mutable gates (capabilities and the switch only);
4. one forge listing of the held ref, outside every database lock and behind a
   per-worker limiter;
5. one transaction locking worker, run, hold and publication. It re-compares
   the receipt first, then re-checks eligibility, the switch and capabilities
   again, and `UZI_RECOVERY_READY_RETENTION` ≥ 1 s (the retention check exists
   only here, after the listing, so with retention below 1 s and the forge down
   the answer is `held_publication_unavailable`), then releases the hold with
   `release_evidence = final_disposition = 'held_publication'` and
   `final_publication_id`, and acknowledges the publication in the same
   commit.

NULL-safe CHECKs tie `final_disposition = 'held_publication'` to a non-null
`final_publication_id` and to the matching evidence class in both directions. A
RESTRICT FK keeps the receipt from dangling. The retention clock
(`expires_at = acknowledged_at + UZI_RECOVERY_READY_RETENTION`) starts at
acknowledgement, not at ref creation. The persisted `expires_at` is not renewed
by reuse or replay; the default retention is 168 hours (7 days). Retention below
one second refuses new held FINAL; without a qualifying fallback, custody stays
protected.

Retained answers are HTTP 200 with a bounded reason. `held_publication_unavailable`
is transient: the worker keeps the journaled request and the open hold.
`held_publication_ref_missing`, `held_publication_ref_mismatch`,
`held_publication_not_eligible` and `held_publication_unsupported`, as well as a
400 from an API that does not know the kind, clear the held request. Provided the exact hold is still open and the attempt budget permits, the worker
then attempts an archive on the same record; a known concurrent settlement or
policy discard permits neither a new FINAL nor fallback. A held ACK runs the same local cleanup as an
archive FINAL ACK without needing a self-contained bundle, because the forge ref
is the retained copy.

### Exact reconciliation and bounded cancellation (#3030)

`GET /api/worker/runs/{id}/recovery-holds?generation=G` adds an optional
exact-generation V2 original-custodian read, scoped to the user, repository and
original worker. Open evidence must match guarded, live custody and an ended
generation of a terminal run before new held publication. Closed persisted
evidence and receipt observation are not a FINAL ACK. Missing, foreign,
malformed or unknown evidence retains protection. Disabling new publication
keeps this exact read available.

An immutable journaled FINAL is replayed before mutable checks, including after
a lost ACK. Cleanup requires the actual ACK from posting that original receipt.
A known settled or policy-discarded concurrent close authorizes neither a new
FINAL nor archive fallback. Executing-flight guards may defer FINAL until the
tail closes; existing boot and live recovery redrive saved FINALs.

Cancellation observation starts one 600,000 ms attempt budget measured by
`performance.now()`. Lock waits, freeze, scans, packing, create/read-back, FINAL
and cleanup share it; retries and fallback do not reset it, and operation caps
are limited by the remaining time. Late outputs confer no new authority. At
expiry the model stays stopped and custody/source stay protected. A later
existing recovery pass may start a fresh bounded attempt.

The create-marker transaction serializes admission with hold settlement. After
settlement, no new create is authorized. A forge mutation admitted before that
settlement may finish later; an unknown outcome is reconciled, not resent. An
unlocked read cannot revoke an already admitted request.

### Accepted residual: terminal late writers (#3030)

The maintainer accepted on 2026-10-14 that an undetected terminal late writer can
lose bytes written after immutable capture under existing cleanup, matching the
failed-run held-publication boundary. Detected or unproven writers fail closed;
this is not proof of writer exclusion. There is no new post-release retention
of cancelled source or pins across hosted GC, controller teardown or recycling.
A reviewing peer preferred durable post-release retention across that lifecycle;
that alternative was rejected because of its size and because it would recreate
custody after release.

Held-ref visibility follows the repository. Copies fetched from a public
repository before ref deletion cannot be revoked.

### Cleanup and deletion authority

`SweepHeldPublications` (`api/internal/workersvc/held_cleanup.go`) is always
registered and has three lanes:

- **reconcile**: `invoked` past a 10-minute grace, `create_unknown` and
  `delete_unknown`. It only lists and records, so it may run while a hold is
  open. An absent ref leaves a create unknown under capped backoff. For
  `delete_unknown`, an absent ref is `deleted` and a foreign tip is
  `abandoned`.
- **expiry**: `acknowledged` rows past `expires_at`. The owner's early expiry
  (`POST /api/runs/{id}/held-publications/expire`, `uzi run held expire`) only
  moves `expires_at` for acknowledged rows.
- **orphan**: only `created` rows that were never acknowledged and whose run
  has no open hold.

Deletion is the only destructive path. The forge connection is derived before
the transaction. The run row is locked `FOR NO KEY UPDATE NOWAIT`, which
conflicts with `ClaimRun` and step B, and the lock is held through
revalidation (lane predicate, repo identity, no open hold of any generation),
the listing, the CAS delete, the confirming listing and the outcome write on the
same transaction. The open-hold exclusion gates deletion only, not
reconciliation. A foreign tip is never touched.

While a row may own a remote ref, its `live_run_id` is a RESTRICT FK
(`run_held_publications_live_run_id_fkey`). Repo and forge-connection removal
count live held publications first and refuse with a 409 naming them
(`api/internal/handler/forge.go`). A race that reaches the FK is mapped to the
same 409. Deleting the connection would also destroy the credential the
retention delete needs.

### Owner surface

`GET /api/runs/{id}/held-publications` and `uzi run held <run-id>` list a
run's publications (ref, tip, state, timestamps, sanitized last error). The run
page shows a held-publication panel. Forge-originated text is sanitized and
secret-scrubbed before it is stored or shown.

## Consequences

- Eligible terminal held work can land on its own forge, where `git fetch
  origin refs/uzi-held/<run-id>/<generation>` restores it. The archive remains
  the fallback within the same attempt and coverage guards; an unproved
  disposition keeps source custody protected.
- The acknowledged ref becomes due for deletion when retention expires (or
  after an owner early expiry), subject to open-hold exclusion and forge
  reconciliation. Failed deletion does not extend the persisted expiry.
- **Accepted limit: `create_unknown` has no terminal exit on absence.** It
  stays reconcile-only until a listing finds the ref at the tip; no code path
  records a refusal from `create_unknown`, so that listing is the only exit. Until then its `live_run_id` keeps blocking removal of the repo
  and the forge connection, and the owner has no abandon path for it.
- **Accepted limit: the worker treats `held_publication_unsupported` as
  definitive** and falls back to the archive, although the server considers it
  non-definitive (the switch or capability can return). A qualifying complete archive FINAL remains
  a safe disposition, and waiting on a withdrawn feature would strand
  the hold. A briefly disabled switch can therefore leave a created,
  never-acknowledged ref. Once the run's holds close, the orphan lane deletes
  it.
- **Accepted limit, inherited:** the push broker reads the forge's whole ref
  advertisement with no cap (`pushbroker.ListRefTips`). This is the shared
  broker's existing behavior for checkpoint retention and salvage and is not
  fixed here.
- **Accepted limit, inherited:** the uploaded pack excludes only its base. The
  scan treats tips this flight already published to the forge as public
  floors, so those commits can be carried in the held pack without being
  rescanned. This is the same invariant as the #1597 checkpoint path: each
  commit in the pack is either scanned now or already reachable from a
  published checkpoint or the default branch.
- Completed runs keep #2507's exact-generation receipt policy. V2 adds the
  bounded archive-first alternative above, not a general thin-archive release.
  It adds no model-free blocked/orphan producer (#3031), live-predecessor
  widening (#2969), writer design (#2852), plan-rejection producer/discard policy
  (#2884), zero-write fast path or workflow-file editing authority.
