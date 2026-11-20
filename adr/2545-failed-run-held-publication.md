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
  requests are in flight process-wide (429 `busy` otherwise). Hardening
  (2026-11-14): held-work and terminal-preservation uploads share an additional
  limit of one in-flight request per authenticated worker. Both reservations
  release together when request handling ends; the existing busy response is unchanged.
  Clarification (#4140): that 330 s is narrowed only by budgets, slot
  deadlines and cancellation the enclosing scope already holds. The worker's
  own frozen-preparation clock (30 s on a background recovery pass, the scan
  deadline in the foreground) bounds preparation alone and never becomes the
  attempt's budget, so it cannot cut the POST short.
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

## Amendment (2026-11-13, #4007)

Terminal preservation refs (`refs/uzi-terminal/<run-id>`) are a separate
namespace with their own tables, not held publications. The held sweep
(`SweepHeldPublications`) and `validManagedRef` never touch them, and the held
publication state machine, retention clock and removal guard above are
unchanged. Their own removal 409 is separate. See
[ADR-4007](4007-terminal-preservation.md); slice A is dark.

## Amendment (2026-11-14, #4007 slice B)

Worker-side, `prepareFrozenHeld` was split internally into `resolveFrozenRange`
and `prepareOverRange`. Legacy callers see identical behaviour: same signature,
return shape, single budget, callback order and tip-only CI check. A new
terminal-only `prepareFrozenHeldPinned` adds an opt-in per-commit CI check and
the scan and transport digests. With the terminal gate on, the held publish
closure runs the terminal preservation step after the legacy preparation and
before any held create or FINAL, and keeps the hold (no held FINAL) unless that
step is `published` or `recorded`. The gate is a
compile-time constant that is off, so this is production-dark. See
[ADR-4007](4007-terminal-preservation.md).

## Amendment (2026-11-19, #4481)

**Problem.** At the 64-head owed cap (`MAX_OWED_CANDIDATES_PER_RUN`) the worker
refuses to pin another head and stops the run. For a failed inventory-guarded
code-publishing run the terminal fetch-back that transfers the clone head into the
trusted bare was refused with it, so the freeze never ran and the custody hold
stayed open until an owner ran `uzi run recovery` or `uzi run discard`. The
existing held V2 publication and FINAL (or the archive fallback) could not
release it because nothing proved the refused head was kept.

**Mechanism.** A refused head is kept and recorded instead of dropped.

- The head stays alive under a refused ref
  `refs/uzi-incoming/<run>/<generation>/refused-<sha>`. Only the fetch paths
  (P1, P2) also leave a run-attributed stage ref
  `refs/uzi-incoming/<run>/<generation>/import-<processNonce>-<uuid>`; P3-P7 leave only the
  refused ref.
- It is recorded in a durable ledger under `<bare>/info/uzi-refusals/`.
  That directory is deliberate: older workers throw on unknown names in
  `uzi-owed` and block on unknown top-level bare directories, but ignore
  `info/`.
- The worker's terminal transfer (`transferAtOwedCap`) then proceeds only when
  the refused head is the clone head and the durable gate
  (`refusalReleaseGate`) is `eligible`; the head is anchored as on the ordinary
  path and the freeze runs with it as the current source. The existing held
  publication and FINAL (or archive fallback) release the hold unchanged.
  No new server contract, capability or endpoint is involved.

Records under `info/uzi-refusals/` (all written with fsync, rename, directory
fsync and read-back):

| Record | Role |
| --- | --- |
| `admission-<run>-<gen>.json` | Write-once provenance for one generation, carrying a per-worker-process nonce. Never overwritten. |
| `refusals-<run>.json` | The ledger of refused heads (generation, SHA, producer path), bound to run, branch and bare. |
| `final-<run>-<gen>.json` | Write-once release seal: source SHA, evidence digest, process nonce. |
| `cleanup-<run>-<gen>.json` | Write-once cleanup intent listing the exact stages and ledger entries to delete. |
| `incomplete-<run>.json` | Durable marker that refusal recording failed for the run. Never cleaned up. |
| `owedpath-<run>-<gen>.json` | Write-once marker that this generation reached or was about to fill the owed cap. Removed last by generation cleanup. |

**Producer audit.** All seven `pinOwedUnderLock` call sites record a cap
refusal at one chokepoint (`recordRefusalUnderLock`, under the bare lock,
before the refusal is thrown). The fetch stage path (P2, at the fetch site)
additionally records the incoming head directly through the same function when
no refusal for it was noted, an eighth direct recording call. A recording
failure never changes what is thrown and claims nothing. The `refused-<sha>`
ref write and the ledger append are independent steps: a failed ref write
still attempts the ledger append. If either write fails, or the seal listing
fails, or the ledger is invalid, unbound or full, or the owed-path marker write
fails, the run is latched in-process
(`refusalRecordingFailed`) and its gate and seal are ineligible ("refusal
recording failed in this process") for the worker process lifetime. The same
failure (seal-directory read, refused-ref write, ledger write or owed-path marker write) also writes a
durable `info/uzi-refusals/incomplete-<run>.json` marker
(`markRefusalIncomplete`, best effort, never throws, independent of the other
recording steps). While the marker is present the gate, and so the seal, is
ineligible for that run ("refusal history incomplete"), checked before the
`none` check and across worker restarts; cleanup never deletes the marker. The
restart rule still makes a successor ineligible while the run's owed pins stay
at the cap or any durable refusal evidence exists.

**Owed-path marker (owner decision, binding).** The restart residual is closed
fail-closed, not accepted. A write-once `owedpath-<run>-<gen>.json` in
`info/uzi-refusals` (`ensureOwedPathMarker`) is written before the pin that
fills a positive-generation context's owed cap, and on the refusal branch if
absent. A marker failure on the pin path refuses that pin as a generic
`preservation_failed` ("owed path marker write failed"), never as the cap. While
a marker at or below the releasing generation G exists, the gate never answers
`none`: it requires admissions written by the current worker process for
every generation 1..G. After a worker-process change, or with an absent
admission, the run therefore keeps its hold for the owner, even if every
refusal write failed and the pins later drop below the cap. Runs that never
filled the cap write no marker and are unchanged. Generation cleanup removes
the markers at or below its generation last, after the files. Older-worker
boundary: a generation an older worker filled to the cap has no marker; it
gets one only if a refusal happens later on the new worker. The gate's
tracking-tip rule (the run's own branch tracking ref must be covered) is an
extra check that matters for P3 (same run) and P7; it is not what protects the
other paths.

| Path | Producer |
| --- | --- |
| P1 | `fetchAgentBranch`, unobserved tracking head |
| P2 | promotion of an incoming head (fetch, `updateTrackingRef`, `updateRemediationTrackingRef`) |
| P3 | displaced historical owner |
| P4 | ancestor-branch clearing |
| P5 | remediation `ownedTip` |
| P6 | remediation marker `executionTip` |
| P7 | `retainCurrentOwedCandidate` |

Admission (provenance) is written only by the runner's authenticated flight
admission (`admitRefusalProvenance`, one attempt per flight, inventory-guarded
code-publishing kinds with a positive generation), never by these paths.

**The gate** returns `none`, `eligible` or `ineligible`. `eligible` requires:
an admission for every generation 1..G written by the current worker process;
a ledger that, if present, parses and is bound to the run and bare, with its
branch matching the admissions; no `incomplete-<run>.json` marker (valid or
not, it is final and is checked before the `none` check); no temporary residue
of this run (or unattributed) in the directory; every stage and refused tip present in the ledger; every refused head
an ancestor of, or equal to, the release source (or pinned under
`refs/uzi-owed`); and the run-owned tracking tip covered. `none` means no
refusal evidence, the pin count below the cap and no owed-path marker at or
below G: the ordinary path, unchanged.
The gate is consulted on every inventory-guarded release but is a no-op
(`none`) for a run with no refusal evidence below the cap and no owed-path marker at or below that generation; a run with earlier
recorded refusals, or exactly at the cap, needs complete admissions even if its
terminal fetch succeeded. The gate runs before the freeze, before archive capture or upload, before the
held publish, and inside the coverage proof before every guarded FINAL.

**Seal.** `sealRefusalRelease` runs the gate and writes `final-` once, before
the first FINAL. Every retry must match it (digest, source, process) or the
FINAL is not sent (`refusal_seal`, classified as an integrity refusal). An
existing seal is compared even when current evidence has disappeared (gate
`none`): empty evidence hashes to the empty digest, which differs from what the
seal fixed, so the result is a mismatch and custody is kept. With no seal
and gate `none`, nothing is written.

**Fence.** After the terminal import on the cap path, and also after a
successful terminal import of an inventory-guarded code-publishing run whose gate is not `none` (refusal evidence exists or
the run is at the cap; the path is re-proved through the cap transfer), an in-process fence
(`closeRefusalGeneration`) refuses further producing fetches and tracking
updates (P1-P6) for that run at generation at or below the closed one.
`retainCurrentOwedCandidate` (P7) is not fenced by the in-process fence; it stays
open before the freeze, and its refusals before the seal land in the seal's gate
or make it ineligible. P7 is fenced by the seal of the generation it would pin:
under the bare lock it returns `ownership_unknown`, before any candidate,
context or pin write, when a `final-` file exists at or above the producing
context's generation or the caller's generation (a failed read of the names
refuses too, as `preservation_failed`). The fence is checked before the
already-pinned and remotely-covered short cuts, so after a seal a retain that
would have written nothing new is also refused (`not_updated`; the settle path
then preserves the clone, the retirement path preserves it when the claim is
inventory-guarded): an accepted, fail-closed over-refusal. Once `final-`
exists the seal blocks recording on every path, including `retainCurrentOwedCandidate`.
On these paths a second terminal pass for the same generation re-proves the
released head instead of re-importing (this only stops the hook from
re-importing); that no later fetch adds a refused head rests on the fence.
Ordinary below-cap runs with gate `none` take the unchanged path.

**Invariants.**

- I-refusal: a custody release on the cap path happens only when every head the
  worker refused for that run, up to the releasing generation, is provably
  covered by the release source, as shown by the sealed gate evidence.
- Provenance: only a flight of the current worker process that was
  authenticated as the generation's claim holder can have written the
  admission; an admission from another process is not trusted.
- Exclusion: no producer records a refused head after the seal exists (every
  path) or after the fence closes (P1-P6; P7 is fenced by the seal of the generation it would pin, not by the in-process fence, see above), so the evidence the seal covers cannot grow.

**Failure model.** Process crash, SIGKILL and injected faults. Files rely on
fsync, rename, directory fsync and read-back. No storage or power-loss claim is
made for refs.

**Restart rule versus eventual cleanup.** A cap-stopped run whose history
spans a worker-process restart (or any other process) keeps its hold while
durable refusal evidence exists or its pins stay at the cap; the
release ordering does not rely on container teardown. Eventual cleanup of a
leaked producer is separate: tini as PID 1 exiting kills the container's PID
namespace, which a real-container test in `e2e/worker-uid/` exercises.

**Cleanup.** With no refusal evidence `cleanupRecoveryGeneration` behaves as
before. With refusal evidence it validates before any deletion (seal bound, source equal
to the acknowledged FINAL source, digest equal, ancestry, stages a subset of
the ledger), then writes `cleanup-`, deletes the excluded import stages, the
stage and refused refs, rewrites the ledger, performs the existing anchor
deletions, and finally deletes `final-` and `cleanup-` and then the owed-path
markers at or below the generation. Admissions are kept: cleanup never deletes
them (they are the seal's process-binding evidence); only the bare's removal
removes them, and owner discard does not. A crashed pass
resumes through an exact-pair subset check, and resume validates the intent
itself: the digest recomputed from the intent's own lists must equal the
seal's, every listed stage must be a ref in this run's namespace whose
generation matches its ref path, is at most the releasing generation and
whose tip a listed ledger entry covers, and no listed ledger entry may be at a
later generation. Cleanup matches exact (generation, sha) pairs, so a later
generation's row for the same head survives. Any change in the evidence retains
everything locally; this never undoes an already acknowledged release.

**Census.** `info/uzi-refusals` entries named `refusals-`, `final-`, `cleanup-`,
`incomplete-`, `owedpath-` or `.tmp-` mark a bare protected (`refusal_records`); an admission alone stays
disposable. Nested `refs/uzi-incoming/<run>/` refs count as `recovery_refs`;
a flat `refs/uzi-incoming/<uuid>` fetch stage remains unprotected.

**Limitations.** These keep today's behaviour (the owner uses `uzi run
recovery` or `uzi run discard`):

- A run claimed before this change, a run that crossed an old or another
  worker, a run whose history spans a worker-process restart (while durable refusal
  evidence exists or its pins stay at the cap), and a generation whose
  admission write failed.
- A run whose refusal writes all failed keeps its hold across a worker restart
  even when its pins later drop below the cap: the owed-path marker (or the
  `incomplete-<run>.json` marker) keeps the gate from answering `none`. A
  generation an older worker filled to the cap has no owed-path marker unless a
  refusal later happens on the new worker. Unguarded runs: a cap-filling pin
  is refused on a bare lacking `info/` (default bares have it), and the
  owed-path marker keeps the bare a refusal-records bare for the census until
  generation cleanup removes it; for a run that never reaches recovery
  generation cleanup (for example an unguarded run) it stays until the bare is
  removed.
- A refusal under a legacy or no-generation context keeps that run's hold and
  its cleanup closed permanently.
- Older workers in a mixed fleet may treat a bare holding the new records as
  disposable and leak the nested refs and records.
- Owner discard does not remove refusal artifacts (refusal refs, records,
  incomplete markers and admissions); they stay reachable. No worker-side
  discard removes them; only the bare's removal deletes them.
- A stranded temp file in `info/uzi-refusals` keeps the gate ineligible
  (see Rework below for whose temp file).
- An import refused at an already-sealed generation leaves an unanchored
  import stage, which then blocks later gates for the owner (accepted).
- A worker killed mid refusal-ref delete leaves an unresolved bare record that
  fences later writers on that bare. No in-product path resolves a dead owner's
  running bare record (the same contract as `recovery-cleanup-delete-ref`);
  operator intervention is needed (accepted).

**Owner-decided residual (2026-11-19).** Provenance gates only the automatic
cap release (the gate itself is consulted on every inventory-guarded release but
is a no-op without refusal evidence below the cap and without an owed-path marker). An old worker's refused head
that old code deleted leaves no trace; the ordinary release proceeds as today.
This is pre-existing and accepted. Releases with no refusal evidence below the
cap and no owed-path marker at or below the releasing generation are not gated on admission records. The restart
residual that earlier text left open is closed by the owed-path marker above.

**Rework (2026-11-20, #4481 review).** The code now also does the following.

- **Ledger key.** Ledger rows are keyed by (generation, sha). An older
  generation's cleanup removes only its exact pairs, so a later generation's
  refusal of the same head keeps its own row. The sha-only listed-head retain
  was removed.
- **Temp files.** Temp files in `info/uzi-refusals` are named
  `.tmp-<runId>-<uuid>`. A run's own temp keeps it ineligible and its cleanup
  retained; another run's temp is ignored; an unattributed temp fails closed for
  all runs.
- **Verified stage inventory.** The stage listing is verified. Listing stderr, a
  non-files ref backend (a reftable directory, or `extensions.refstorage` other
  than exactly `files`), or a loose ref under `refs/uzi-incoming/<run>/` that
  is a symlink, non-regular, malformed (including a symbolic `ref:`) or that
  disagrees with the listing makes the gate ineligible and cleanup retain.
- **Authority for deletes.** Sealed-cleanup deletes of refused refs and import
  stages go through the registered bare-mutation authority, op
  `refusal-cleanup-delete-ref`, with read-back settlement
  (`agent/src/git-authority-hooks.ts`, `agent/src/bare-refusal-diagnostic.ts`).
  Failed-acquisition and promotion-time stage deletes remain plain CAS
  `update-ref -d`, as before.
- **Import stages.** Import stages are named
  `refs/uzi-incoming/<run>/<gen>/import-<processNonce>-<uuid>`; evidence refs
  stay `refused-<sha>`. An import stage whose tip has a refused ref at the same
  generation plus a ledger (generation, sha) row is excluded from the refusal
  evidence and the seal digest. A live in-process import makes the gate
  ineligible, and an unanchored import stage keeps the gate ineligible and
  cleanup retained (fail-closed). Sealed cleanup lists excluded import stages in
  its intent (`importStages`, not in the digest) and deletes them first (intent,
  import stages, refused refs, ledger, pins, files, markers); a resumed cleanup
  rediscovers them from the intent. A failed acquisition CAS-deletes its own
  attributed stage at the fetched SHA, and keeps it if the ref moved, if it is
  the sole anchor of a ledgered head, or on a boundary stop. A failure in a
  producing-source import's second phase (changed source proof, refused or
  timed-out source authority, failed second lock) keeps the stage: the head is
  unproven and the stage may be its only bare copy, so the run stays held
  (gate ineligible, cleanup retained) for the owner.
- **Finished import stages (reconciliation).** An import interrupted after its
  fetch leaves its attributed stage; a later successful retry of the same head
  does not remove it, and without a refused ref and ledger row it would hold
  the gate forever. The cache therefore records, process-locally and bounded at
  4096 entries (oldest evicted, which only makes a stage unproven), the stages
  whose producing import returned, success or failure. A stage's producer is
  proven finished only when its leaf nonce equals this process's nonce, it is
  in that record and it is not live. Under the bare lock, after the latch,
  incomplete, temp and ledger checks, the later-generation check and the
  live-import check, the gate (and so the seal) removes such a stage only when
  an independent anchor sits at its exact tip and the tip is covered. Anchors:
  the owed pin `refs/uzi-owed/<run>/<tip>`; a recovery pin
  `refs/uzi-recovery-pin/<run>/<g'>` with g' at or below the releasing
  generation; or the tracking ref `refs/uzi-runner/<branch>` at the tip, with
  the branch hint described below. Covered means pinned, or an ancestor of the
  gate head. Object existence, ledger rows and the head alone never anchor.
  The removal is the registered CAS delete (op `refusal-cleanup-delete-ref`);
  a refused registration, a moved ref or any non-abort failure keeps the stage
  and the gate holds, and a boundary abort propagates. After removals the gate
  recomputes: with no ledger rows, stages, owed-path marker or full cap it is
  `none`. Any attributed import stage left at or below the generation whose
  producer is not proven (a foreign or crashed process's stage, an evicted
  record) returns `ineligible("refusal import stage producer unproven")`, even
  when a refused ref and ledger row anchor it. Cleanup planning is unchanged.
  The tracking anchor needs the caller's branch (an optional trailing argument
  of the gate and seal). Under the bare lock the branch must equal the ledger's
  branch when a ledger exists, the tracking ref must be owned by this run at a
  positive generation at or below the releasing one, and its producing context
  must parse; any failure means no tracking anchor and no admission is
  required. Because the reconciliation deletes through the authority hook, the
  runner takes the authority transaction (A) around the gate. The waits differ
  by site. `settleGuardedInventory` waits for the shutdown plus maintenance
  budget plus the boundary deadline (the terms of `cleanupAuthorityWait`). The
  transfer sites wait with `heldCapture`'s wait when held, with
  `owedStopReleaseWait(flight)` after an owed-limit stop, and otherwise with
  `owedStopReleaseWait(flight)` as a fallback; that fallback still refuses on a
  recorded cancel or claim fence. If A cannot be acquired the gate counts as
  ineligible and custody is kept. At the transfer sites an ineligible gate,
  including one caused by an A-acquisition failure, closes the refusal fence for
  that generation in this process (the settle site only preserves the clone).
  This is an accepted, fail-closed residual. The sealed cleanup
  (`refusalCleanupPlan`, unchanged) still excludes and deletes a refused-ref and
  ledger anchored import stage whatever its nonce. A stage that was proven when
  sealed (for example one a successor cleans up after a restart) is harmless; one
  that was not proven when sealed can exist only if it postdates the FINAL. In
  either case the commit stays covered by the sealed covering head,
  and the gate and seal hold such a stage as unproven. This is an accepted
  boundary. Failure
  model: process crash only. The ref guarantees make no storage or power-loss
  claim. A crash between registration and settlement leaves a `running` bare
  record that fences later same-bare writers (the ADR-1783 residual above).
- **Soft deadline.** The incoming head's refusal is recorded before the
  checkpoint soft-deadline return.
- **Recovery.** When the refusal gate is ineligible, a saved FINAL the api
  already accepted is acknowledged without sending a release: an archive through an accepted-only
  reconcile answer with an exact receipt; a settled or held release through an
  observed released hold whose receipt equals the saved request, with source
  proof and inactivity. New releases and mismatches stay gated. The residue
  quarantine is rechecked synchronously after the refusal seal, immediately
  before the release POST.
- **Runner wait.** After a mid-turn owed-cap stop, the source-authority wait of
  the terminal refusal-release fetch no longer inherits the flight signals the
  stop aborted (`owedStopReleaseWait` in `agent/src/runner.ts`). It aborts on
  shutdown, the maintenance budget, a recorded cancel or claim fence (re-checked
  at each acquisition) and the terminal lifecycle signal if still live. A
  cancel or fence recorded while an acquisition is already queued does not abort
  that acquisition once the stop has aborted the terminal lifecycle signal (while
  that signal is still live, a fence aborts it through the terminal signal); the
  import writes only this worker's bare, and the runner's local claim-fence
  checks and the server's generation fences still stop any later report or release.
- **Failure model.** Every ordering above is for a process crash. No storage or
  power-loss durability claim is made.
- **Known residual: a dead owner's refusal-cleanup record.** A worker SIGKILLed
  between the `refusal-cleanup-delete-ref` registration (or its `update-ref -d`)
  and its settlement leaves a `running` bare record. Under
  [ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md)'s accepted contract
  (no in-product path resolves a dead owner's running bare record), that record
  blocks later same-bare writes (`assertBareClear`, `bare_unresolved`): seeds,
  pins and coverage ref writes. This lasts until a manual operator action
  outside the product (ADR-1783's guarded operator discard also refuses a
  `running` pending record) and affects only that bare. The remaining refusal refs, the ledger and the seal are
  kept, and no head loses coverage. See #4066 and the recovery-design follow-up
  #4550. This change does not force-clear the record or weaken bare registration.

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
