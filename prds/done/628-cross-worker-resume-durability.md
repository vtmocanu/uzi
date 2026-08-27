# PRD: Cross-worker resume durability for `limit_wait` parks

> **Status: complete — all milestones (M0-M4) implemented and reviewed** on branch
> `agent/issue-628`. M0 (ADR, `adr/0628-cross-worker-resume-durability.md`) resolved
> the two design forks; M1 shipped drain-aware resume affinity in `ClaimRun` plus the
> new `WORKER_AFFINITY_CEILING` knob (default 30m); M2 shipped a checkpoint publish on
> the limit-park path; M3 shipped a non-vacuous cross-worker recovery test
> (`seededFrom = "checkpoint"` trip-wire); M4 shipped the `milestones_completed` clear
> keyed on `seededFrom = "default"`.

Anchors below verified against `main` @ `6dd012aef` (2026-08-23).

## Problem

When a run parks on an Anthropic usage limit (`status = limit_wait`) and is later
re-claimed by a **different** worker, it recovers neither its SDK session nor its
committed git work, emits `resume_lineage_break`, starts from the default branch, and
**re-implements already-committed milestones from scratch**. This wastes budget and
duplicates work, and it can recur on every subsequent park.

### Observed incident (2026-08-23, run `0a0ea841-a46f-4b38-a286-3d69c52c343b`, issue #602)

- Pass 1 ran ~6.5h (20:41-03:21 UTC), committed the **entire** PRD M0-M6 (M6 = `50bc91f5`), then entered final review.
- 03:21:47 hit the **five-hour** Anthropic session limit and parked (`limit_wait`, `resets_at` 03:50).
- 03:26:42 re-claimed by a **different** worker -> `resume_lineage_break` -> reseeded from the default branch -> re-planned -> Pass 2 re-implemented from m0.
- The re-claim at 03:26:42 (vs. `retry_not_before` 03:24:30, `resets_at` 03:50) is **not** a bug: it is the Decision-6e credential-pool failover promoting the run early once another Anthropic token had headroom (`autoselect.NextAvailable` lowering the base, `api/internal/workersvc/limitwait.go:371-374`). Early promotion is the **normal** multi-credential path — which is exactly what makes the 2-minute affinity window too short.
- Stale progress: `milestones_completed` stayed `[m0..m5]` (from pass 1) while pass 2 re-implemented from m0 — the user-visible "marked done but still working on them" symptom (addressed by M4 below).
- Cost undercount as a side effect (tracked separately in #332): ledger `usage.cost_usd` = $64.74 vs. real result-frame total $321.66 (pass-1 $192.11 + pass-2 in progress $129.55).

## Root cause (verified against `main` @ `6dd012aef`)

There are **two independent loss axes**, and conflating them hides why one fix is not enough:

- **Session-loss -> re-PLAN.** The SDK session HOME is preserved on a park (`agent/src/runner.ts:1965-2051`, guarded `if (runHome && !parked)` at `:2036`), but only on the **original worker's PVC**. A re-claim elsewhere finds no HOME, so `sessionTranscriptResolvable` (`agent/src/sdk-session.ts:134-174`) returns false; the guard call is at `runner.ts:661` and the tagged `resume_lineage_break` is emitted at `:668` (log) and `:682` (feed).
- **Tree-loss -> re-IMPLEMENT (the headline damage, and the $321-vs-$64 cost).** The run-tracking ref `refs/uzi-runner/<branch>` is written locally into the worker's own bare (`agent/src/git.ts:692-707`) and **never pushed to the forge or any shared store** (`pushBranch` at `git.ts:341` uses that ref only as the *source* and publishes objects to `refs/heads/<branch>`; the `refs/uzi-runner/*` namespace is never created remotely — the sole `git push` in `agent/src`). A re-claiming worker's bare has no such ref (`ownedHere` false, `git.ts:430-438`), so the untagged "starting from the default branch" message fires (`runner.ts:603-613`, `seededFrom === "default"`).

**Affinity is far too short for a park.** `PromoteLimitWaitRuns` (`api/internal/store/queries/runtime.sql:1292-1341`) keeps `worker_id` (affinity preserved, comment `:1316-1319`) but sets `updated_at = now()` (`:1339`), arming only a **`WORKER_AFFINITY_GRACE` = 2 min** window (`api/internal/config/config.go:305,729`; claim predicate `runtime.sql:581-583`; wiring `workersvc/service.go:1237`). A `limit_wait` park can last until a 5-hour reset (and credential-pool failover promotes it *earlier* still); if the original worker is busy/drained/rolled during that 2 min, any worker claims it and the local state is unreachable.

**A cross-worker durability mechanism already exists but does not cover this path.** `refs/uzi-checkpoints/<branch>` (PRD #122 M8) is published to origin best-effort via the worker **join token** (no PAT) — pack builder `agent/src/git.ts:745-764`, publish `runner.ts:1074` (call) / `:2114-2129` (def), reseed pickup `git.ts:439-488` (`seededFrom = "checkpoint"` only on a **strict-descendant** checkpoint, `:479`). But it fires **only** from the mid-run checkpoint callback (`runner.ts:1074`, ~20-min time-gate `checkpointIntervalMs`); **`handleLimitReached` does NOT publish a checkpoint** (`runner.ts:2169-2263`). The park path's only durability is `fetchBackBestEffort` (`runner.ts:1879` call, `:2084-2096` def), which writes the **local** tracking ref only. In the incident, Pass 2 reseeded from `default` (not `checkpoint`) — **M0 must determine why** (see M0).

**Separately, the completed-milestone list cannot decrease.** `milestones_completed` is a **monotone union**: `SetRunRunning` does `jsonb_agg(DISTINCT COALESCE(milestones_completed,'[]') || reported)` (`api/internal/store/queries/runtime.sql:913-917`), copied verbatim into `SetRunCompleted` and marked "MUST stay a UNION" (`:1449-1460`, PRD #265 M1) — these are the *only* two writers of the column. So a worker re-reporting `[m0..m3]` unions with pass-1's `[m0..m5]` and stays `[m0..m5]`; a worker cannot walk the list back by reporting `[]` (the union ignores it). The staleness is therefore not "never reconciled" but "structurally cannot decrease", and clearing it needs a **new, targeted server-side clear path** (new store query + service plumbing + a worker-driven signal) — a write-semantics change, addressed by M4. (`milestones_in_progress` *is* overwritten wholesale at `:918-921`, so it alone is resettable via a report.)

## Proposal

Both fixes are required; they cover different loss axes and neither is deferrable.

- **M1 (api/Go/SQL) — drain-aware resume affinity.** Keep a promoted `limit_wait` run pinned to its original worker **only while that worker is a live, non-draining claim target** (heartbeat fresh AND `draining_since IS NULL`), else fall open to any worker. This is a *liveness* predicate, **not a longer timer** — a long timer would strand a parked run against a worker that drains/rolls mid-park (worse than re-implementing, which at least makes progress). The needed machinery already exists: the fleet-spread subquery in `ClaimRun` already joins `workers` and checks heartbeat/drain (`runtime.sql:625-629`). When the original worker survives the park, same-worker resume recovers **both** session and tree with zero new git machinery; when it does not, the claim falls open and M2 carries the tree.
- **M2 (agent/TS) — checkpoint on the park path.** Wire the limit-park path to `publishCheckpointBestEffort` before parking, so a re-claiming worker recovers committed work from `refs/uzi-checkpoints/<branch>` instead of restarting from default. This is the *only* fix for the tree axis when the original worker is genuinely gone (the drain/roll case a correct M1 deliberately routes cross-worker). Reuses the existing join-token publish seam (no PAT, no forge protected-branch touch, no workflow scope). Edits the catch/limit path (`runner.ts` ~`:1874-1880` + `handleLimitReached` `:2169+`), not the finally carve-out.
- **M3 (agent/TS) — verify + harden cross-worker checkpoint recovery.** Confirm the `seededFrom = "checkpoint"` reseed leg (`git.ts:439-488`) recovers correctly on a cross-worker re-claim, add a regression test, and surface the recovery outcome in the run feed (message region `runner.ts:582-613`, `:658-685`). **Ordering is M0-conditional:** if M0 finds the *recovery* leg (not just the publish) is broken, M3 becomes a prerequisite for M2 rather than a follow-on.

Explicit non-goals: publishing the `refs/uzi-runner/*` namespace to the forge (pollutes the forge, widens the guardrail surface); cross-worker **session/transcript** durability (that is #556's deferred item — this PRD deliberately fixes the **code/tree** axis + affinity, and leaves re-planning-on-session-loss as-is).

## Milestones

| ID | Title | Files (primary) | Depends on |
|----|-------|-----------------|------------|
| M0 | ADR: cross-worker resume durability + **resolve the incident recovery gap** | `adr/0628-cross-worker-resume-durability.md`, PRD Decision Log | - | <!-- check-docs:ignore-path: forward reference, M0 creates this ADR -->
| M1 | Drain-aware resume affinity for promoted `limit_wait` runs | `api/internal/store/queries/runtime.sql`, `api/internal/workersvc/service.go`, `api/internal/config/config.go`, live-DB store test | M0 |
| M2 | Publish a checkpoint on the limit-park path | `agent/src/runner.ts` (limit path + `handleLimitReached`), `agent/src/git.ts`, `node --test` | M0 (order vs M3 M0-conditional) |
| M3 | Verify + harden cross-worker checkpoint recovery | `agent/src/git.ts` reseed leg, `agent/src/runner.ts` feed region, `node --test` | M2 (or precedes M2 if M0 finds recovery broken) |
| M4 | Reconcile `milestones_completed` on a no-recovery fresh start | new store clear-query in `api/internal/store/queries/runtime.sql`, `api/internal/workersvc` plumbing, worker signal in `agent/src/runner.ts`/`git.ts`, live-DB store test + `node --test` | M2, M3 |

**Status: all of M0-M4 implemented and reviewed** on `agent/issue-628`.

### Parallelization

- **Phase 1 (sequential prerequisite):** M0 (ADR) — settles the affinity liveness predicate, and **critically** determines whether the checkpoint *publish* or the *recovery* leg is the gap (which fixes the M2/M3 order).
- **Phase 2 (parallel):** M1 (Go/SQL) and M2 (agent/TS) touch disjoint modules and can run as concurrent agents — **provided** M1 does not couple its affinity-release to any "checkpoint published" run field (that would create a field crossing both milestones; keep M1 purely a worker-liveness predicate).
- **Phase 3 (sequential):** M3 after M2 (unless M0 inverts), then M4 after M3 — M4 both keys on the `seededFrom` "default vs checkpoint" split M2 introduces and edits the same worker-side feed/reseed region as M3, so it must follow them and cannot run in Phase 2.

## Success criteria

1. A `limit_wait` run whose original worker is a live, non-draining claim target resumes **on that worker** (no `resume_lineage_break`, no re-plan) for a park lasting well beyond 2 minutes; a run whose original worker is draining/rolled/dead **falls open** and is claimable elsewhere (no stuck run) — both asserted in a live-DB store test.
2. A `limit_wait` run whose original worker is gone recovers committed work from a checkpoint on the re-claiming worker: `seededFrom = "checkpoint"` (not `"default"`) with `priorCommits > 0`, and the checkpoint **strictly ahead of** `origin`/default (the `git.ts:479` strict-descendant condition, else the test passes vacuously) — asserted in a `node --test`.
3. No new duplicated-milestone re-implementation on a park in either case.
4. After a cross-worker re-claim that reseeds from `default` (no tree recovery), `milestones_completed` is cleared to reflect the fresh start; after one that reseeds from `checkpoint` (tree recovered), it is **preserved** — asserted in a live-DB store test (the new clear-query) plus a `node --test` (the worker keys the signal on `seededFrom`, not on `resume_lineage_break`).
5. No guardrail weakened: checkpoint publish stays on the join-token path; `main`/protected branches untouched; no `.github/workflows/**` change in the diff.

## Risks

- **R1 — stuck run (M1).** A time-based re-pin against a worker that drains/rolls during a multi-hour park would block every other worker until the 24h drain deadline. Mitigation: M1 is a *liveness* predicate (heartbeat fresh AND `draining_since IS NULL`), never a timer; a non-eligible original worker falls open immediately.
- **R2 — ephemeral-storage pressure (M1).** Keeping a run pinned longer keeps its preserved HOME (`agent-home/<runId>`) on the original worker's PVC longer (cf. #225, #556 R3). Mitigation: the liveness predicate bounds the pin to a live worker; ensure HOME GC still fires when the run ultimately resumes elsewhere or goes terminal.
- **R3 — false confidence in M2 without M0's recovery check (F4).** "Connectivity drop" is an unlikely explanation: the park itself required a successful `reportState` to the api, so the api was reachable and a checkpoint publish to the same api would have been too. More likely the branch was never pushed (`originExists=false` -> floor `default` -> `seededFrom=default`) and no mid-run checkpoint existed. M0 must confirm the *recovery* leg works before M2 is trusted.

## Constraints / notes for the implementer

- **No `.github/workflows/**` changes** in implementation or validation (the worker PAT lacks `workflow` scope; a workflow touch is an atomic push rejection that loses the branch). This PRD needs none.
- Goose migration numbers (if M1 adds one) are **drafts**; rename to the next free number above the live head at landing time.
- `-race`, `-count=1` and the live-DB store-IT runner are mandatory for the M1 store test (see `.claude/rules/go.md`); positive controls required (`--- PASS`, `RUN>0`, zero SKIP).
- **#556 coordination:** M2 edits the catch/limit path, not the finally carve-out #556 M1 touches — already disjoint. The real overlap is **M3's feed-region edits (`runner.ts:582-613`, `:658-685`) vs. #556 M2's positive-resume event (~`:661`/`:675`)**; coordinate there.

## Decision Log

- **D1:** Chose the existing join-token checkpoint channel (`refs/uzi-checkpoints`) for cross-worker code durability over pushing `refs/uzi-runner/*` to the forge. Rationale: the channel already exists, needs no PAT, and does not pollute the forge or widen the guardrail surface.
- **D2:** M1 and M2 are **co-required**, covering different loss axes: M1 (affinity) recovers both session and tree but only when the original worker survives the park; M2 (checkpoint-on-park) recovers the tree cross-worker regardless of worker survival, and is what a correct drain-aware M1 relies on for the drain/roll case. Neither is deferrable for the stated symptom.
- **D3 (open, for M0 ADR):** the exact liveness predicate for the re-pin (heartbeat freshness threshold + `draining_since IS NULL`, reusing the `runtime.sql:625-629` join vs. a new predicate); and **whether the incident's non-recovery was a missing publish (M2 suffices) or a broken recovery leg (M3 must precede M2)**.
- **D4:** the stale-`milestones_completed` symptom (originally scoped as a separate issue) is folded here as M4, because it is not separable: `milestones_completed` is a monotone union (can only grow, `runtime.sql:913-917`), so the fix is a new server-side clear path, not a worker `[]` report; and the reset must key on the **tree** signal (`seededFrom = "default"` / `priorCommits = 0`), **not** the session signal (`resume_lineage_break`) — the two diverge once M2 lands (a re-claim can recover the tree via checkpoint while the session still breaks, and resetting there would wrongly discard a valid list). `seededFrom` is worker-computed and not reported to the server today, so the reconcile lands worker-side in M3's region — hence M4 depends on M2+M3 and cannot run in Phase 2.
- **D3 resolved (M0 ADR [adr/0628-cross-worker-resume-durability.md](../../adr/0628-cross-worker-resume-durability.md), 2026-08-23):**
  - **D3a — affinity predicate + scope.** Replace `ClaimRun`'s 2-min affinity leg (`runtime.sql:581-583`) with a **worker-liveness** leg (`NOT EXISTS` over `workers ow` testing `last_heartbeat_at >= @heartbeat_cutoff AND draining_since IS NULL`, mirroring ADR-216 D6 `:625-629` and reusing the existing `@heartbeat_cutoff` param) **OR a generous ceiling** (`updated_at < @affinity_cutoff`). Chose **universal (option a)** — no crisp signal isolates a `limit_wait` promotion (the `limit_*` fields persist across requeues by design, `:1321-1324`; `started_at`/`requeue_count` don't discriminate), and universal is safe because liveness only *accelerates* release for dead-worker paths while the ceiling covers the live-but-stuck paths (vault-lock is per-user so a peer can't help anyway). **No new column, no goose migration.** Ceiling = a **new** knob `WORKER_AFFINITY_CEILING` (default 30m), NOT a bump of `WORKER_AFFINITY_GRACE` — that knob is shared with `ClaimChatRun` (`chat.sql:85-87`, `chat.go:157`), which gets no liveness short-circuit in M1's scope, so bumping it would strand a chat run on a dead worker for 30m. One accepted regression: a claimed-never-started run against a wedged-but-heartbeating worker is held to the ceiling (2m→30m) before a peer takes it.
  - **R2 (HOME GC) verified independent of the affinity window.** `reclaimStrandedRunHomes` (`agent/src/home-reclaim.ts:188-312`) is a worker-**startup** sweep keyed on **terminal run status** (`:299`), never on `worker_id`/affinity. M1's longer pin cannot strand a HOME; if anything it reduces stranding. Pre-existing limits (startup-only, 404-skips) are out of scope.
  - **D3b — M2/M3 order.** The recovery leg is **correct by construction** (verified: `fetch()` mirrors `+refs/uzi-checkpoints/*` `git.ts:1140-1146`; reseed prefers a strict-descendant checkpoint `git.ts:458-491`; `checkpointPack` floor matches `git.ts:751-754`). The **verified** gap is the missing **publish** — the park path (`handleLimitReached` `runner.ts:2169-2264`, catch/park `:1874-1880`) fetch-backs the local tracking ref only and publishes no checkpoint. `seededFrom=default` in the incident ⇒ branch never pushed **and** no usable checkpoint existed, consistent with the publish gap (R3). Decision: **default M2 → M3**, with M3's cross-worker recovery `node --test` (SC#2, non-vacuous: `seededFrom="checkpoint"` ∧ `priorCommits>0` ∧ strict-descendant) authored and green **first** as the trip-wire — a red inverts to M3-before-M2.
  - **M4 reset key** confirmed = `seededFrom` (tree), not `resume_lineage_break` (session), per D4.
- **Implementation landed (2026-08-23):** M1-M4 built in ADR order with no inversion — the M2->M3 order held (D3b's recovery leg was correct by construction; M3's trip-wire test went green against M2's publish on the first pass, no red requiring the M3-before-M2 fallback). M1's ceiling landed exactly as decided: a new `WORKER_AFFINITY_CEILING` config knob (default 30m), separate from the shared `WORKER_AFFINITY_GRACE`.
