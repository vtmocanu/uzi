# ADR-4476: Codex claim provenance

**Status**: Implemented (M1 schema and claim gate, M2 health, M6 docs); exact-head CI and live-fleet acceptance pending
**Date**: 2026-11-19
**Related**: Issue #4476; [ADR-628](0628-cross-worker-resume-durability.md), [ADR-1392](1392-forge-unreachable-preclone-park.md), [ADR-1590](1590-codex-binding-same-identity-readmission.md), [ADR-4201](4201-in-place-credential-switch.md)

## Decision

A Codex-indicating run that has been claimed at least once (`claim_generation >= 1`)
is admitted by `ClaimRun` only for the worker recorded as the claimant of its
**current** generation. Any other worker, including a new ephemeral worker, is
refused even when the old affinity rules would have released the run.

"Codex-indicating" means `harness = 'codex'`, or `codex_material_revision IS NOT
NULL`, or `codex_secret_id IS NOT NULL`. Claude runs are unchanged. A Codex run
at generation 0 (never claimed) is unchanged: any eligible worker may take its
first claim.

The rule exists because a Codex run's session and local source live on the
worker that last ran it, and the older fall-open arms could hand the run to a
peer while the predecessor's flight might still be using that source. Holding
the run for its last worker closes that window for Codex; it does not prove the
predecessor has stopped (see Residuals).

## Scope restriction and consequences

None of the affinity fall-open arms overrides the gate:

- the stale-requeue grace lapsing (`WORKER_STALE_REQUEUE_GRACE`, ADR-628 #2705 amendment);
- the affinity ceiling (`WORKER_AFFINITY_CEILING`, ADR-628 D3a);
- `worker_id IS NULL`;
- `fn_forge_origin_owner_pins` (ADR-1392 immediate escape);
- the `kind = 'cross_check'` bypass (a Codex checker child at generation >= 1 is held as well).

Consequences:

- A Codex run whose last worker is gone (row deleted, offline for good) stays
  `queued` until the owner cancels it (`uzi run cancel <id>`). Nothing releases it
  automatically.
- **Custody stays with the held run.** Custody holds opened by earlier claims
  (PRD #1296) stay open while the run is held, and they keep counting toward the
  owner's custody-admission limit (`fn_custody_admission_count`). Cancelling the run
  does not release them: the cancel statements do not touch
  `recovery_custody_holds`. Release still comes only from the existing custody
  paths, for example the custody-release reconciler, the holder worker's own
  release, the merged-predecessor reconciler, or an explicit discard. Until then, deleting a worker that is the live
  holder of such a hold is refused (`CountOpenCustodyHoldsForWorker`), even after the
  run is cancelled. Waiting or cancelling settles the run, not its custody cleanup.
- A held **judge** run has no queued expiry, and the one-active-judge index blocks
  later judges of the same target while it exists. This change adds no new terminal
  state for it (per the issue); the owner cancels it or waits for the worker.
- A held **cross-check** checker child fails at its deadline as today, through
  `ExpirePlanCrossCheck` / `ExpireCodeCrossCheck`, so the lead is not stranded.
- **No backfill.** Every Codex run at generation >= 1 when this deploys has no
  provenance pair, so it is held at its next fresh claim, even on its own worker.
  Readoption (#1390) still continues such a run in its current flight; otherwise
  the owner cancels it.

## The provenance pair

Two nullable columns on `runs`, with no default, no foreign key and no backfill
(`api/internal/store/migrations/00493_run_claim_provenance.sql`; the number is a
draft and is renumbered at landing):

- `claim_provenance_worker_id uuid`
- `claim_provenance_generation bigint`

The pair is the worker and generation of the last admitted Codex claim. It is
current only while `claim_provenance_generation = claim_generation`; after any
later generation bump that did not restamp it, the pair is stale and admits
nobody. There is no foreign key so that deleting a worker row leaves the name
lookup to degrade (health prints the id text) instead of cascading or blocking.

`fn_codex_claim_provenance_admits(r runs, claimant uuid)` is the single
admission predicate. It admits a non-Codex run, a run below generation 1, or a
claimant equal to the recorded worker whose recorded generation equals the run's
current generation. A NULL claimant (a worker that does not exist yet) is never
admitted for a held run.

`ClaimRun` (`api/internal/store/queries/runtime.sql`) gates on the predicate in
the target and in the fleet-spread peer block, and stamps the pair, in the same
UPDATE that bumps `claim_generation`, on every Codex-indicating claim.

## The write guard

`trg_runs_claim_provenance_guard` (BEFORE UPDATE, firing only when either
provenance column changes) raises unless all four conditions hold. An insert
trigger rejects any provenance on insert.

1. **Exactly +1 generation** (`NEW.claim_generation = OLD.claim_generation + 1`).
   Provenance can change only inside a claim; a park, Resume, promoter,
   account-unavailable write or manual update that does not bump the generation
   by one cannot write it.
2. **OLD-row admission for `NEW.worker_id`**: either `OLD.claim_generation < 1`
   with no existing pair (first claim), or `OLD.claim_generation >= 1` and the
   OLD pair is complete and matches `NEW.worker_id` at `OLD.claim_generation`.
   This re-checks the gate in the database, so a claim
   path that forgot the predicate still cannot re-stamp a held run.
3. **Complete pairs** on OLD and NEW (both columns NULL or both set; NEW must be
   set). A half-written pair is never stored.
4. **NEW pair = (`NEW.worker_id`, `NEW.claim_generation`)**. Only the claimant
   can be recorded, at the generation it just took.

The condition is wrapped as `IS NOT TRUE`, so a NULL conjunct (for example a NULL
`NEW.worker_id`) raises rather than passing.

The trigger rejects other provenance-changing write shapes, including a legacy
backfill or a foreign re-stamp after a generation rewind. It does not inspect
updates that leave both provenance columns untouched, and is not a privilege
boundary against an SQL administrator who can disable it.

## Mirrors

Every query that answers "could a worker claim this queued run?" mirrors the
gate so that provisioning and health do not count a held run as demand:

- `CountOnlineWorkersClaimableForRun`
- `ListUnplaceableQueuedRunsForEphemeral`
- `ListSaturationQueuedRunsForEphemeral`
- `ListHibernationWakeDemand`

Wake candidates are rechecked against the same predicate after taking the run's
share lock, before recording a wake. A recorded wake also becomes stale when its
worker is no longer admitted, so a claim/requeue between passes cannot leave an
unclaimable worker's wake blocking the recorded claimant.

`ListIsolatedQueuedRunsForEphemeral` keeps excluding Codex runs as before.
Readoption (#1390) is untouched: it continues a run in its current flight and
does not claim.

Down drops both triggers, both functions and both columns. The provenance is
lost; a re-upgrade starts from NULL and holds every Codex run at generation >= 1
again.

## Rollback scope

There is no in-database rollback fence. While an api from before #4476 serves
claims there is no gate: any worker may claim a Codex run, the old `ClaimRun`
does not stamp the pair, and the interval is unprotected. After re-upgrade, a
Codex run claimed by the old api has a stale pair (its recorded generation is
behind `claim_generation`), is held, and the trigger forbids re-stamping it. The
health reason for that run is the unavailable one below, and the owner cancels it.

## Health

The run-health reason (`api/internal/workersvc/health.go`) is one of:

- `held for worker <name>: wait for it or cancel the run; only the worker that
  last ran this Codex run may resume it`. `<name>` comes from the recorded
  provenance, never from `runs.worker_id`; if the worker row was deleted the
  worker id text is shown.
- `held for worker <name>: restart it or cancel the run; its previous process was
  released; only the worker that last ran this Codex run may resume it` when the
  released-worker pair still matches that recorded worker's registration nonce.
  The comparison includes NULL nonces, like ClaimRun. It does not follow the
  mutable `runs.worker_id` hint. After the nonce changes, ordinary waiting advice returns.
- `held: predecessor provenance unavailable; cancel the run; no worker may resume
  this Codex run` for NULL or stale provenance (a legacy row at generation >= 1,
  including a paused or parked one, or a run claimed by an old api during a
  rollback window).

All three are `waiting_worker`. Precedence: after the egress rung and before the
manual-cordon, owner-hold and stale-pin rungs. The prerequisite rungs in
`queuedPrerequisiteReason` (`api/internal/workersvc/health.go`) still outrank it,
among them the instance intake pause (maintenance), the weekly usage reserve, the
handoff setup, vault, custody and Codex account blockers, and the Codex admission
wait. The held reason is shown from the first tick (the young-run gate does not
delay it).

Nudges follow PRD #3454 D12 for the named reason and deliberately depart from it
for the unavailable reason:

- The **named** reason is quiet while the stale-requeue pin or a draining or
  maintenance owner-hold window is in force by its clock, and nudges once when
  that window ends. If the condition that made the run pinnable ends early (the
  drain is cleared, or the worker row is deleted or replaced), the single nudge
  comes at `updated_at + WORKER_AFFINITY_CEILING`. (Amended by #4507: when the
  health detector observes the owner hold in force during the episode, it persists
  a per-episode marker on the run, `runs.health_owner_hold_since`, valid only for
  the current episode, and the single nudge then waits for that ceiling even if the
  stale-requeue grace expired earlier and the drain then cleared. An episode held
  only by the stale pin still nudges once at grace expiry. Only owner holds the
  detector observed and committed are remembered: a hold that starts and clears
  between two health ticks, or whose marker write fails, falls back to the earlier
  timing, the stale-grace end when the stale pin covered the episode, otherwise the
  affinity ceiling. Notification timing only; admission, custody, credentials and
  run state are unaffected.) If the hold began through
  another quiet transition (leaving a maintenance intake pause, a Codex admission
  wait, or a reserve), it also nudges once at that point, and never if that ceiling
  had already passed when the episode began (the same as other `waiting_worker`
  episodes after a D12 quiet exit). A held run outside any window (for example a
  normal Resume onto a live worker) nudges on the first health tick, bypassing the
  young-run grace, and counts in `queue.waiting`.
- The **unavailable** reason never resolves on its own, so it nudges on entry from
  any state (the nudge cooldown still applies). This is a deliberate exception to
  D12.

Alarm effect: `queue.waiting` and `fleet.capacity` exclude only reasons `LIKE
'waiting until %'`, and their code is unchanged. Held runs therefore now count as
genuine waits, so a routine worker drain can trip `queue.waiting` while the Slack
nudge stays quiet. Known residual, deferred: the Slack nudge head for
`waiting_worker` reads "still waiting for a worker to pick it up", which
contradicts the unavailable text.

## Residuals: Claude runs and the forge PAT

uzi does not prove that a predecessor's flight has stopped before another worker
resumes the same run, and this change closes that gap only for Codex runs. Claude
runs have the same predecessor-quiescence gap: when the stale-requeue grace or the
affinity ceiling lapses, or the owner row is gone, a peer worker can claim a run
while its previous worker may not have ended its flight. The forge personal access
token has the same gap: nothing in this change revokes or narrows the predecessor's
forge access when a run changes hands. Neither is fixed here. Claude runs keep the
existing fall-open behaviour and carry no provenance pair. Closing these needs
positive takeover proof and provider-side revocation, which are out of scope for
this change.

## Not covered by this change

- Positive takeover proof (the predecessor is confirmed stopped).
- Provider-side revocation of the predecessor's credentials.
- Same-worker new-incarnation proof (PRD #1860, #3430, #3431): the pair names a
  worker id, not an incarnation, so a restarted worker is admitted for a run its
  earlier incarnation was running.
- Claude runs and the forge PAT (previous section).

## ADR-4201 note

ADR-4201 records a nonconformance in the #4202 work. This change does not edit
that text, which lives only on the #4202 branch. When #4202 merges a `main`
containing this change, it narrows its own nonconformance text to the
same-worker new-incarnation residual above.

## Amendments elsewhere

Dated 2026-11-19 notes in ADR-628 (D3a, the #2705 amendment and Consequences),
ADR-1590 D4, ADR-1392 and PRD #1030 M2 record that a Codex run at generation >= 1
is no longer offered to another worker by those fall-open arms.
