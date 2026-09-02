# ADR-628: Cross-worker resume durability for `limit_wait` parks

**Status**: Accepted (PRD #628 M0 design gate — this ADR records the decisions that unblock M1–M4; no implementation has merged yet)
**Date**: 2026-08-23
**Deciders**: architect (M0 design gate); PRD #628 Decision Log (D1/D2/D4 already settled there); one open item flagged to the lead (see Open question)
**PRD**: [prds/done/628-cross-worker-resume-durability.md](../prds/done/628-cross-worker-resume-durability.md) (GitHub issue [vtmocanu/uzi#628](https://github.com/vtmocanu/uzi/issues/628)) — the PRD carries the milestones, the problem trace and the incident forensics; this ADR resolves the two open forks D3 opened (the drain-aware affinity predicate + its scope, and the M2/M3 ordering) plus the guardrail invariants the four implementation milestones must hold to.
**Related**: refines [ADR-216](0216-fleet-aware-claim.md) (the affinity leg this ADR rewrites lives inside the same `ClaimRun` statement ADR-216 established; the liveness test reuses ADR-216 D6's heartbeat cutoff). Consumes [ADR-422](0422-decouple-worker-version.md)'s `workers.draining_since` (D5) as a claim-eligibility signal. Sits downstream of [ADR-35](0035-run-limit-retry.md) (the `limit_wait` park and its credential-pool early promotion are what make the 2-minute affinity window too short).

## Decision (summary)

Two independent loss axes fail a `limit_wait` run that is re-claimed by a **different** worker: the run recovers neither its committed git tree nor its session, re-plans, and re-implements already-committed milestones. This ADR settles the two design forks the PRD left open for the M0 gate:

- **D3a — the affinity predicate (M1).** Replace `ClaimRun`'s fixed 2-minute affinity window with a **worker-liveness** predicate plus a **generous ceiling**: a promoted run stays pinned to its original worker W **while W's worker row exists and W is draining or heartbeat-fresh**; it **falls open only when the row is gone (teardown) or the owner is heartbeat-stale and non-draining**; and a generous ceiling bounds the one pathological case a pure-liveness test would strand forever (W is heartbeat-fresh and non-draining but never picks the run up — vault-locked or wedged-in-assembly). *(Corrected by the PRD #1030 amendment below: the original design here released a **draining** owner immediately, which stole a parked run cold across a routine image roll — run #1009. A draining worker row now HOLDS the pin, because a roll keeps the same row and PVC and returns a fresh pod on the far side; only teardown, or a stale non-draining owner, falls open.)* The predicate is **universal within `ClaimRun`** (applied to the shared affinity leg, all five requeue-with-`worker_id` paths of that one query) — there is no crisp claim-time signal that isolates a `limit_wait` promotion, and the universal shape is safe because liveness only *accelerates* release for the dead-worker paths and the ceiling covers the live-but-stuck paths. It is **not** universal across the chat lane; `ClaimChatRun` carries its own, different leg — see the correction below D3a-ceiling. **No new column, no goose migration.** The ceiling is a **new** knob, `WORKER_AFFINITY_CEILING` (default 2h), NOT a bump of the shared `WORKER_AFFINITY_GRACE` (which `ClaimChatRun` also reads).
- **D3b — the M2/M3 order.** The recovery leg (`git.ts` reseed + checkpoint fetch-mirror) is **correct by construction** on inspection; the certain, verified gap is the **missing publish**: the park path publishes no checkpoint. So the default order is **M2 (publish on park) → M3 (verify/harden)**, with one guard: M3's cross-worker recovery test is authored and made green **first**, as the trip-wire that confirms the recovery leg actually works end-to-end. If that test goes red, M3's fix precedes M2.
- **M4 reset key.** The stale-`milestones_completed` clear keys on the **tree** signal `seededFrom === "default"` (equivalently `priorCommits === 0`), **never** the session signal `resume_lineage_break` — the two diverge the moment M2 lands (a re-claim can recover the tree via checkpoint while the session still breaks).

Entry point for D3a: the affinity leg of `ClaimRun` in `api/internal/store/queries/runtime.sql:581-583`, wired by `api/internal/workersvc/service.go:1237`, knob in `api/internal/config/config.go`. Entry point for D3b: the limit-park path in `agent/src/runner.ts` (`handleLimitReached` `:2169-2264`, the catch/park fetch-back at `:1874-1880`) and the reseed leg in `agent/src/git.ts:444-491`.

## Context

### The two loss axes (verified against `main` @ `6dd012aef`)

- **Tree loss → re-IMPLEMENT (the headline damage).** The run-tracking ref `refs/uzi-runner/<branch>` is written into the *original* worker's bare and never pushed to a shared store. A re-claiming worker's bare has no such ref (`ownedHere` false, `git.ts:435-438`), so its reseed falls to the `else` leg (`git.ts:458-491`): floor = `origin/<branch>` if pushed, else the default branch.
- **Affinity is far too short for a park.** `PromoteLimitWaitRuns` (`runtime.sql:1335-1341`) keeps `worker_id` (affinity) but stamps `updated_at = now()`, arming only a `WORKER_AFFINITY_GRACE` = 2-minute window (`config.go:729`; claim predicate `runtime.sql:581-583`). A park can last to a five-hour Anthropic reset, and ADR-35 D2's credential-pool failover promotes it *earlier* still — so if the original worker is busy/drained/rolled during that 2 minutes, any worker claims it and the local state is unreachable.

### The affinity leg is shared by five requeue paths

`runtime.sql:581-583` is the affinity leg of `ClaimRun`. Its `@affinity_cutoff` is `now() - WORKER_AFFINITY_GRACE` (`service.go:1237`). Every requeue query that keeps `worker_id` reaches the claim through this one leg. Verified by reading each — categorised by whether the run's own worker is **live** or **dead** at requeue:

| Requeue query | line | worker at requeue | today's 2-min affinity does | under the new predicate |
|---|---|---|---|---|
| `PromoteLimitWaitRuns` (park) | `1335` | live (park) OR gone (drain/roll) | pins 2 min then falls open | live → stays pinned via liveness; gone → falls open **immediately** |
| `RequeueRunsOfStaleWorkers` | `1812` | **dead** (heartbeat stale by construction) | pins 2 min then falls open | liveness leg true → falls open **immediately** (better) |
| `RequeueWorkerRuns` (register) | `1850` | **live** (just re-registered) | pins 2 min; worker re-claims | stays pinned via liveness; worker re-claims within a poll |
| `SweepClaimedNeverStarted` | `1711` | **live** but wedged | pins 2 min then falls open | pinned to the **ceiling** (2m → 2h) — the one regression |
| `RequeueClaimedRunToQueued` (vault race) | `1730` | **live** but vault-locked | pins 2 min then falls open | pinned to the **ceiling**; harmless — vault is per-**user**, a peer is equally locked |

`ClaimChatRun` (`chat.sql:85-87`) is wired from the *same* `WorkerAffinityGrace` (`chat.go:157`) for its cutoff, but — **correction, PRD #1030 M5** — it does not carry a byte-identical leg: it never gained a liveness/drain test at all (`worker_id = self OR updated_at < cutoff` only). This ADR originally described the two legs as byte-identical; that was wrong even at the time this ADR was written (the D3a-ceiling section below already, correctly, describes chat's leg as getting "no liveness short-circuit"). The shared-knob fact below still decides the ceiling's home: bumping `WORKER_AFFINITY_GRACE` itself would still regress chat's *dead*-worker timing, independent of whether the two legs are identical.

## The decisions

### D3a — the affinity predicate: liveness + a generous ceiling, universal within `ClaimRun`, no new column

Replace the affinity leg (`runtime.sql:581-583`) with:

```sql
AND (r.worker_id IS NULL
     OR r.worker_id = @worker_id
     -- Fall open the moment the run's OWN worker stops being a live, non-draining
     -- claim target: a dead (heartbeat-stale) or draining owner will never resume it,
     -- so pinning to it is strictly worse than re-claiming elsewhere (PRD R1). Mirrors
     -- the fleet-spread peer-liveness test (ADR-216 D6, runtime.sql:625-629); reuses
     -- the SAME @heartbeat_cutoff param already passed to ClaimRun.
     OR NOT EXISTS (
         SELECT 1 FROM workers ow
         WHERE ow.id = r.worker_id
           AND ow.last_heartbeat_at IS NOT NULL
           AND ow.last_heartbeat_at >= @heartbeat_cutoff
           AND ow.draining_since IS NULL)
     -- Generous ceiling: bounds the live-but-can't-serve pathology (a heartbeat-fresh,
     -- non-draining owner that never picks the run up — vault-locked/wedged). @affinity_cutoff
     -- is now now() - WORKER_AFFINITY_CEILING (default 2h), NOT the 2-min grace.
     OR r.updated_at < @affinity_cutoff)
```

**Why liveness, not a longer timer (PRD R1).** A pure longer timer would strand a parked run against a worker that drains/rolls mid-park until the timer lapsed — worse than re-implementing, which at least makes progress. The liveness leg frees the drain/death case *immediately*. The machinery already exists: ADR-216's fleet-spread subquery joins `workers` and tests exactly `last_heartbeat_at >= @heartbeat_cutoff AND draining_since IS NULL` (`runtime.sql:625-629`), and `@heartbeat_cutoff` (`now() - WORKER_HEARTBEAT_STALE`, 45s) is **already a `ClaimRun` param** — the liveness leg adds no new parameter.

**Why a ceiling is still needed (the live-but-stuck case is real).** Verified by reading `SweepClaimedNeverStarted` (`runtime.sql:1711`, a claimed-never-started worker that is still heartbeating) and `RequeueClaimedRunToQueued` (`runtime.sql:1730`, the vault-lock race — the worker just claimed, so it is live and non-draining). For both, the owner is heartbeat-fresh and non-draining but is not serving the run; a pure-liveness predicate would keep the run pinned to it forever. Today's 2-minute timer is what prevents that. The ceiling preserves that guarantee — every run is claimable within the ceiling — while being generous enough that the leg essentially never fires for a *healthy* park (a live worker re-claims its own promoted run within one poll interval, long before 2 hours).

**Why universal (within `ClaimRun`), not `limit_wait`-scoped.** Scope note: "universal" here means across `ClaimRun`'s own five internal requeue-with-`worker_id` paths, never across the chat lane — `ClaimChatRun` is untouched (see the correction above). There is **no crisp claim-time signal** that isolates a `limit_wait` promotion from an ordinary requeue. Verified: the `limit_*` history fields (`limit_resets_at`, `retry_not_before`, `rate_limit_type`) are deliberately **left in place across later requeues** as display history (`runtime.sql:1321-1324` — "LEFT IN PLACE as history"), so `limit_resets_at IS NOT NULL` does not mean "this requeue is a limit promotion"; `started_at IS NULL` is shared with `SweepClaimedNeverStarted`; `requeue_count` is not bumped by promotion (`:1312-1314`) but is bumped by the death paths, so it cannot discriminate either. The two options were therefore:

- **(a) universal predicate + ceiling — chosen.** No new column, no migration. Safe because the liveness leg only *accelerates* release for the two dead-worker paths (`RequeueRunsOfStaleWorkers`, and — after its owner goes stale — anything), which is strictly desirable, and the ceiling covers the live-but-stuck paths.
- **(b) a new marker column scoping the extended pin to `limit_wait` promotions (a draft goose migration).** Rejected. It buys only the avoidance of one narrow, bounded regression (below), at the cost of a schema change, a new column that every requeue writer must remember to set/clear, and a field that risks crossing into M2's territory — the exact coupling the PRD's Phase-2 parallelization note forbids ("keep M1 purely a worker-liveness predicate"). The smallest architecture that satisfies the requirement wins.

**The one accepted regression, stated honestly.** Under (a), `SweepClaimedNeverStarted` for a worker that is *persistently wedged in assembly yet still heartbeating* now holds its run up to the 2-hour ceiling before a peer can take it, versus 2 minutes today. This is a rare pathology (a genuinely wedged worker usually stops heartbeating, at which point the liveness leg frees the run at once), and for the far more common live-but-stuck case — the vault-lock race — a peer cannot help anyway because the vault is per-**user**: every one of that user's workers is equally locked, and the user's unlock frees them all. So the extended pin costs nothing there.

#### The ceiling's home: a NEW knob, not a bump of `WORKER_AFFINITY_GRACE`

Introduce `WORKER_AFFINITY_CEILING` (default 2h) and wire `ClaimRun`'s `@affinity_cutoff` from it (`service.go:1237`: `s.p.WorkerAffinityGrace` → `s.p.WorkerAffinityCeiling`). **Do not** bump `WORKER_AFFINITY_GRACE`'s 2-minute default, because that knob is **shared with `ClaimChatRun`** (`chat.go:157` → `chat.sql:85-87`), whose affinity leg gets **no liveness short-circuit** in M1's scope. Bumping the shared default would extend a chat run's pin to a **dead** worker from 2 minutes to 2 hours — a strand regression on a lane this PRD does not touch. A separate run-lane knob leaves chat's timing (and the `config.go` sibling comments that describe `WorkerAffinityGrace` as a sub-minute poll-cadence knob) accurate. `WORKER_AFFINITY_GRACE` stays as the chat-lane grace; `WORKER_AFFINITY_CEILING` is the run-lane ceiling. 2h is generous enough that a healthy worker always re-claims its promoted run first and short enough to bound the wedged-live strand; it is tunable.

**No goose migration.** The liveness leg reads only columns that already exist (`workers.last_heartbeat_at`, `workers.draining_since` from ADR-422's `00138`); the ceiling is config. M1 is Go/SQL/config only.

### D3a-R2 — the longer pin does not change HOME garbage collection

R2 asked whether a longer pin strands the preserved SDK HOME (`agent-home/<runId>`) on the original worker's PVC. **Verified independent of the affinity window.** HOME reclaim is `reclaimStrandedRunHomes` (`agent/src/home-reclaim.ts:188-312`), a worker **startup** sweep that deletes an `agent-home/<runId>` directory **iff the API reports that run's status is terminal** (`home-reclaim.ts:299` — `TERMINAL_RUN_STATUSES`); it never reads `worker_id`, affinity, or the run's assigned worker. So:

- The GC trigger is **terminal status**, orthogonal to how long M1 pins the run. M1 changes *which* worker resumes a run, not *when* its HOME becomes collectible.
- If anything M1 **reduces** stranded HOMEs: by keeping a run pinned to its live original worker, that worker resumes it and reuses its own HOME, avoiding a cross-worker fallover that would leave the original HOME behind entirely.

Two pre-existing limitations, recorded so M1 does not inherit blame for them and does not try to fix them (out of scope): the sweep runs **only at worker startup**, and a run whose row has been deleted returns 404 and is **skipped, not reclaimed** (`home-reclaim.ts:293-297` — "'probably' is not the standard for a delete"). Both predate #628 and are unchanged by it. **M1 verification obligation:** the M1 store test need not assert anything about HOME GC, but the M1 author should confirm no reclaim path keys on the affinity window (it does not — the file to re-check is `agent/src/home-reclaim.ts`).

### D3b — the M2/M3 order: publish is the gap; recovery is verified-first as the trip-wire

The incident: pass 1 ran ~6.5h, committed M0–M6, then parked; pass 2 (a different worker) reseeded from `default` (not `checkpoint`) and redid from m0. The question the PRD posed for M0: is the non-recovery a **missing publish** (M2 suffices, M3 follows) or a **broken recovery leg** (M3 must precede M2)?

**The recovery leg is correct by construction** (verified by reading it, not by running the incident):

- `fetch()` mirrors origin's brokered checkpoint refs into the re-claiming worker's bare with an explicit refspec, best-effort: `+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*` (`git.ts:1140-1146`). This is the one cross-worker link that, if broken, makes a correct publish invisible — and it is present.
- The reseed's not-`ownedHere` leg (`git.ts:458-491`) sets floor = `origin/<branch>` if pushed else default, and prefers `refs/uzi-checkpoints/<branch>` **only when it strictly descends the floor** (ancestor test at `:472` **and** `checkpointSha !== floorSha` at `:479`) — otherwise it falls to the floor (equality) or sets aside loudly (divergence). Sound.
- `checkpointPack` (`git.ts:745-764`) excludes `origin/<branch>` if pushed else the default (`:751-754`), so the published pack carries exactly what the checkpoint added beyond the floor — matching the reseed's floor.

**The verified gap is the publish.** The park path publishes **no** checkpoint: `handleLimitReached` (`runner.ts:2169-2264`) emits the feed event and reports `limit_wait`, and the catch/park block (`runner.ts:1874-1880`) does `killAgentTree` + `fetchBackBestEffort` only — which writes the **local** tracking ref, never brokering a pack to origin. `publishCheckpointBestEffort` (`runner.ts:2114-2129`) is wired **only** into the mid-run checkpoint closure (`runner.ts:1073-1074`), behind the `hasNewWork && (opts.reap || timeGateOpen)` gate (`:1068-1073`).

**Why `seededFrom = default`, most-likely cause.** Because pass 2 seeded from `default` (not `origin`), `originExists` was false — the branch was **never pushed to origin** (a run that parks before creating its MR never calls `pushBranch`, `git.ts:332-343`). With the branch unpushed, the ONLY thing that could have carried the tree cross-worker was a `refs/uzi-checkpoints/<branch>` strictly ahead of the default floor. `seededFrom = default` means pass 2's bare held **no such usable checkpoint**. Combined with the verified fact that the park path publishes nothing, the incident is most consistent with the PRD's R3 reasoning: **no publish existed at park time** (the connectivity-drop theory is unlikely — the park itself required a successful `reportState` to the same API a publish would use). Whether a *mid-run* checkpoint fired during the 6.5h is not determinable from static reading alone — which is precisely why the recovery leg gets a trip-wire rather than an assumption.

**Decision: default M2 → M3, with M3's recovery test authored and green FIRST.** M2 closes the certain gap (publish on park). Because a purely static read cannot fully exclude a silent recovery/fetch-mirror failure, M3's cross-worker recovery `node --test` (success criterion 2) is written and made to pass **before M2 is trusted**, driving publish → fetch-mirror → reseed against the *current* recovery leg (publishing a checkpoint from a helper, since M2 is not yet in). Its green confirms the recovery leg works end-to-end and fixes the order at M2 → M3; a red inverts it (M3's fix precedes M2). The test must be **non-vacuous**: assert `seededFrom === "checkpoint"` **and** `priorCommits > 0` **and** the checkpoint strictly ahead of the floor (the `git.ts:479` strict-descendant condition — else the assertion passes on a checkpoint that merely equals the floor and proves nothing).

**Residual verification M3 owes either way:** that `fetch()` actually materialises `refs/uzi-checkpoints/<branch>` in a *fresh* worker's bare (the mirror link), and that the reseed then prefers it — surfaced in the run feed's message region (`runner.ts:582-613`, `:658-685`; coordinate with #556 M2's positive-resume event there per the PRD).

### M4 — the reset keys on the tree signal, not the session signal

Recorded here because it is the invariant most likely to be "simplified" wrong. `milestones_completed` is a **monotone union** — the only two writers (`SetRunRunning` `runtime.sql:913-917`, `SetRunCompleted` `:1449-1460`) do `jsonb_agg(DISTINCT existing || reported)`, so a worker cannot walk the list back by reporting `[]`. Clearing it on a fresh start needs a **new, targeted server-side clear query** (M4), not a worker report. That clear must key on the **tree** signal `seededFrom === "default"` (equivalently `priorCommits === 0`), computed worker-side in the reseed (`git.ts`) and already consumed in the feed region (`runner.ts:603`). It must **not** key on the session signal `resume_lineage_break` (`RESUME_LINEAGE_BREAK_EVENT`, `runner.ts:2762`): once M2 lands, the two diverge — a re-claim can recover the **tree** via checkpoint (`seededFrom === "checkpoint"`, milestones legitimately preserved) while the **session** still breaks (`resume_lineage_break` still fires, re-plan still happens). Resetting on the session signal would then wrongly discard a valid milestone list. `seededFrom` is worker-computed and not reported to the server today, so M4's reconcile signal lands in the worker-side feed/reseed region M3 also touches — which is why M4 depends on M2 **and** M3 and cannot run in Phase 2.

### Guardrail invariants (unchanged by every milestone)

- **The checkpoint publish stays on the join-token path.** M2 reuses `publishCheckpointBestEffort` → `checkpointPack` (local object read, worker uid, no PAT) → `client.publishCheckpoint` (worker **join token**). No push to `refs/heads/*`, no forge protected-branch write, no `workflow` scope, no `refs/uzi-runner/*` published to the forge (PRD D1 non-goal). The `main`-is-never-touched primary directive and its four guardrail layers are untouched.
- **M1 stays a pure worker-liveness predicate.** It reads `workers.last_heartbeat_at`/`draining_since` and a config ceiling — it must **not** couple its release to any "checkpoint published" run field, which would create a column crossing both M1 and M2 and break their file-disjoint Phase-2 parallelism.
- **No `.github/workflows/**` change** in any milestone or its validation (the worker PAT lacks `workflow` scope; a touch is an atomic push rejection). None is needed.

## Amendment — PRD #1030 (2026-09-02)

PRD #1030 (`prds/done/1030-worker-resume-durability.md`) hit the gap this amendment records: a
parked run's owner worker was cordoned for a routine image roll (not dead, not torn down),
and D3a's affinity leg — as designed above — released the run to a peer anyway, forcing a
cold cross-worker resume (`resume_lineage_break`, incident run #1009). This amendment also
folds in two stale-fact corrections PRD #1030 M5 found in the body above: the
`WORKER_AFFINITY_CEILING` default (every stale "30 min" reference above is now the shipped 2h) and
the `ClaimChatRun` affinity-leg comparison (D3a-ceiling's "byte-identical" claim, corrected
in place).

**D3a's "a draining owner will never resume it" holds only for death/teardown, not for a
transient roll.** The original D3a rationale reasoned a draining owner "will never resume
[the run]", so falling open immediately was strictly better than pinning. That holds when
the owner is torn down — its worker **row** is deleted and it genuinely never comes back.
It does **not** hold for a roll: the same worker row and its PVC survive a cordon, and the
worker resumes serving once its current run drains — so falling open on `draining_since IS
NOT NULL` alone threw away a resumable owner for a cold peer, which is exactly what
happened to run #1009.

**The roll-vs-teardown discriminator: worker-row presence + `draining_since`, no new
column.** A **roll** sets `draining_since` (`Cordon` / `CordonHostedWorker`) but keeps the
worker row and its PVC; `RegisterWorker` clears `draining_since` when the pod returns. A
**teardown** deletes the DB `workers` row API-side (`DeleteWorkerForUser` on user
revocation, `ReapEphemeralWorkers` on ephemeral GC) *before* the controller's kube teardown
runs, so `teardown ⟺ row absent`. Row presence, not `draining_since` alone, is therefore
the correct death/teardown test: hold the pin while the row exists **and** the worker is
either draining or heartbeat-fresh; fall open only when the row is gone (teardown) or the
row is heartbeat-stale with `draining_since IS NULL` (death/hang — D3a's originally
protected case, unchanged).

**The two-seam affinity change** (either alone is wrong; both landed together):
- `ClaimRun`'s affinity leg (`api/internal/store/queries/runtime.sql`) now holds the pin
  when the owner worker row exists **and** (draining **or** heartbeat-fresh); it falls open
  immediately only when the row is gone or the row is heartbeat-stale with no drain in
  progress.
- A new `@claimant_draining` claim-time parameter scopes a **draining** worker's own claim
  to **its own** promoted run only (`AND (NOT @claimant_draining OR r.worker_id =
  @worker_id)`), so lifting the pre-`ClaimRun` "a draining worker claims nothing" early
  return does not let a cordoned worker pick up a new or fallen-open run — PRD #422 D7's
  "a draining worker claims nothing new" still holds.

`ClaimChatRun` is unaffected by either seam.

**The forge-checkpoint net is now reliable, not just documented.** `pushbroker.Publish`
(`api/internal/pushbroker/pushbroker.go`) previously delegated to go-git's
`remote.PushContext`, which recomputes its own send-set from a depth-1 local snapshot of
the *current* default branch — so the first publish after `main` advanced past a worker's
clone base failed `object not found`, self-perpetuatingly (the ref never landed, so every
later publish was again a "first publish"). It now forwards the worker's already-built pack
directly through a manual `git-receive-pack` session, with `Command.Old` set to the
checkpoint ref's advertised tip fetched in `fetchBaseRefs` — a server-side compare-and-swap
that is never forced, stronger than the prior client-side check. Publish outcomes (success,
skip, error) are now surfaced to the run feed instead of failing silently, and the api's
failure log carries `run_id`/`worker_id`/`reason` for log-based observability (this
deployment has no metrics surface for it yet). Both landed as part of the same
resume-durability story as the affinity fix above: a park's original worker not surviving
the wait is exactly the case the checkpoint net exists to cover.

## Consequences

- **A `limit_wait` park whose original worker survives** now resumes **on that worker** — recovering both session and tree with zero new git machinery — for a park lasting far beyond 2 minutes, because the liveness leg keeps it pinned while the worker heartbeats and is non-draining. This is the common multi-hour-park case.
- **A park whose original worker drains/rolls/dies** falls open **immediately** (liveness leg), not after a longer timer, and M2's checkpoint carries the committed tree to the re-claiming worker. No stuck run (PRD R1), no re-implement.
- **`WORKER_AFFINITY_CEILING` (default 2h)** joins `WORKER_AFFINITY_GRACE`, `WORKER_SPREAD_GRACE` and `WORKER_HEARTBEAT_STALE` as a claim-timing knob. It governs the run lane only; chat keeps `WORKER_AFFINITY_GRACE`.
- **One bounded behaviour change on the run lane:** a claimed-never-started run against a persistently-wedged-yet-heartbeating worker is now held up to 2 hours (was 2 minutes) before a peer takes it. Bounded by the ceiling and by the liveness leg (the moment the worker goes stale or drains, the run frees at once). Accepted (D3a).
- **The dead-worker requeue paths recover FASTER**, not slower: `RequeueRunsOfStaleWorkers` frees its run the instant the worker's heartbeat is stale, rather than waiting out a 2-minute affinity window. A net improvement that falls out of the same predicate.
- **HOME GC is unchanged** and M1 does not touch it (D3a-R2). The startup-only, terminal-only reclaim and the 404-skip are pre-existing behaviours, not regressions from a longer pin.
- **M4's clear is a new write path with a sharp key.** Anyone later tempted to reset `milestones_completed` on `resume_lineage_break` should re-read the M4 decision: the tree and session signals diverge once M2 lands, and the session signal is the wrong one.
- **No schema change.** M1 adds no migration; the liveness columns and the checkpoint refs already exist.

## Open question (for the lead → user)

The ceiling default (originally proposed at 30 min; shipped at 2h — see the M5 amendment) is an architect's pick, generous enough that it never bites a healthy park and short enough to bound the wedged-live strand. If the maintainer wants the wedged-live regression narrower (closer to today's 2 minutes) at the cost of falling a genuine long-park open to a peer sooner when its worker briefly stops heartbeating, that is a knob-value call, not a design change — flagged so it is chosen, not defaulted silently.

## Linked from ARCHITECTURE.md

To be discharged on merge, per the repo convention: add the ADR link alongside the `limit_wait` / affinity references in ARCHITECTURE.md's Run lifecycle section in the same change that lands M1, rather than in this design-gate commit.
