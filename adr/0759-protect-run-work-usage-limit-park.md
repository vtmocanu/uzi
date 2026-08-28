# ADR-759: Protect uncommitted run work across an Anthropic usage-limit park

**Status**: Accepted (PRD #759 M1–M5 implemented on branch `agent/issue-759`; M6 lands the tests, docs, and this ADR)
**Date**: 2026-08-28
**Deciders**: Vlad (maintainer — explicitly requested revisiting #218 D6); agent team (architect, coders, reviewers); PRD #759 Decision Log (D1–D5)
**PRD**: [prds/done/759-protect-run-work-usage-limit-park.md](../prds/done/759-protect-run-work-usage-limit-park.md) (GitHub issue [vtmocanu/uzi#759](https://github.com/vtmocanu/uzi/issues/759)) — the PRD carries the milestones, the #685 incident forensics, the two-axis root cause, and the Decision Log; this ADR records the durable invariants a future change would silently break.
**Related**: extends [ADR-628](0628-cross-worker-resume-durability.md) (same incident class — M1 affinity liveness, M2 checkpoint-on-park; this PRD reuses that shipped machinery and raises the ceiling it introduced). Consumes the api-brokered checkpoint push of [ADR-122](0122-checkpoint-push-broker.md) (join token, no PAT). Bounded by [ADR-456](0456-rebase-before-finalize-push.md) (why the WIP marker is stripped at adopt, never at finalize). Narrows PRD #209 D4 row 3 via the `plan_source` provenance discriminator #209 D8 introduced.

## Decision (summary)

A gated, human-approved run that is actively implementing can lose **all** in-progress work when it parks on an Anthropic usage limit: every existing durability layer (checkpoint-on-park, mid-run checkpoints, reseed) captures **committed** commits only, and the reseed `fs.rm`s the clone on the next claim (#685: ~4h of uncommitted work discarded). This PRD closes that gap along both axes the incident exposed, and takes four decisions whose invariants must survive future edits:

- **D3 — a durable survivor is a MARKED throwaway commit, restored to uncommitted at adopt time.** On a usage-limit park, uncommitted work is committed to a `wip(park):`-prefixed throwaway commit (M1, `agent/src/git.ts` `commitWipMarker` / `WIP_PARK_COMMIT_PREFIX`); on resume the reseed `git reset --soft`s that marker back to **uncommitted** at adopt time (M2, `runnerCloneForBranch`). This reconciles #218 D6 ("no auto-commit on park — a half-applied edit that survives is worse than one that does not"): the survivor is clearly marked and restored to uncommitted, so it never masquerades as reviewed work and never lands in the MR.
- **Cross-worker diverged recovery is best-effort with a safe failure** (M2).
- **D4 — M4 narrows, not reverses, #209 D4 row 3** (a thrice-reviewed *safety* decision): the dropped-session re-gate is skipped **only** for a provably-reviewed plan and **only** when recovery succeeded.
- **D5 — the affinity ceiling is raised flat 30m→2h** (M3), not made park-duration-aware.

Entry points: park commit — `agent/src/runner.ts` park path, before `fetchBackBestEffort`; adopt-time recovery — `agent/src/git.ts` `runnerCloneForBranch` (tracking-ref leg, checkpoint leg); M4 gate — `agent/src/runner.ts` (`m4ResumeReviewedPlan`, `planApproved`) + `agent/src/sdk-executor.ts` (`preApproved`, `embedSeededPlan`); ceiling — `api/internal/config/config.go` `WORKER_AFFINITY_CEILING`.

## Context

Two independent loss axes fail a mid-implementation run that parks (verified against the #685 forensics: no remote branch, empty tracking ref, no checkpoint ref, clean clone):

- **Axis 1 — total tree loss because nothing was committed.** With zero commits the tracking ref equals the clone base, the checkpoint pack is empty (the reseed's strict-descendant test rejects it), and the working-tree edits are `fs.rm`'d on the next claim. Uncommitted mid-milestone work was unprotected by construction.
- **Axis 2 — re-plan + re-gate because the session was lost cross-worker.** The ~87-minute park exceeded the then-30m `WORKER_AFFINITY_CEILING`, so a different worker claimed it; its per-worker PVC had no SDK transcript, so `planApproved` collapsed to false and the run re-planned and re-gated onto an empty tree.

The decisive coupling (found in review): a **same-worker** resume avoids both axes for free — the `ownedHere` tracking-ref recovery leg uses **no ancestry test** (it recovers a WIP commit even when `main` advanced during the park), and the session on that worker's PVC is still resolvable. A **cross-worker** resume hits the strict-descendant checkpoint guard (which rejects a WIP that diverged because `main` advanced — the exact #685 condition) and loses the session. So the robust fix is (a) make a durable WIP commit exist, and (b) keep long parks on their original worker; cross-worker recovery is a harder best-effort fallback.

## The decisions

### D3 — the WIP marker: durable, marked, and stripped to uncommitted at ADOPT time (M1 + M2)

On a usage-limit park, `commitWipMarker` commits an otherwise-dirty clone to a single throwaway commit whose subject is prefixed `WIP_PARK_COMMIT_PREFIX` (`"wip(park):"`), run as the **runner uid** in the clone (`runGitAsRunner`, after `killAgentTree`, with the clone's existing `AGENT_GIT_IDENTITY`), inserted **before** the park fetch-back. The shipped #628 machinery then carries it: the fetch-back writes the local tracking ref, and the #628 broker best-effort publishes it to `refs/uzi-checkpoints/<branch>`. On resume, when the adopted tip is a `wip(park):` marker, the reseed `git reset --soft <parent>` returns the WIP content to the working tree **uncommitted** and leaves the branch tip at the last real commit.

**The durable invariant: the `wip(park):` marker must never enter the history that reaches finalize.** The guarantee is the **adopt-time** `reset --soft`, NOT a finalize-time strip. This is deliberate and load-bearing: a finalize-time history rewrite would collide with ADR-456's base-align, which preserves the agent's commit SHAs and touches only `.github/workflows`. Burying a marker-strip there would fight that machinery. Because the marker is dissolved to uncommitted before the agent builds a single commit on top of it, it is structurally impossible for it to appear in the MR — no reviewed commit, no history entry, nothing for finalize to clean up.

**Why revisit #218 D6 at all.** #218 D6 chose no auto-commit because a half-applied edit that survives can be mistaken for reviewed work. The maintainer explicitly asked to revisit it after #685 lost ~4h. The reconciliation is not "D6 was wrong" but "a *marked, uncommitted-on-resume* survivor satisfies D6's concern": it is never a reviewed commit and never in the MR, so it cannot masquerade as finished work, while still surviving the reseed. R1's residual risk — a cold resumed agent building on a half-applied tree — is handled by telling the resumed lead (via `wipRecovered`) to treat the dirty tree as mid-edit and reconcile it against the plan.

### Cross-worker diverged recovery is best-effort, with two invariants that must hold (M2)

Three recovery legs, by how the adopted tip relates to the new floor:

- **Same-worker** (tracking-ref leg): no ancestry test — the marker is recovered and `reset --soft` even when `main` advanced. Robust.
- **Cross-worker strict-descendant** (checkpoint leg, marker strictly ahead of floor): recovered by `reset --soft`.
- **Cross-worker diverged** (`main` advanced during the park, so the marker is not a descendant of the new floor and the strict-descendant guard rejects it): the marker's WIP delta is **cherry-picked `--no-commit`** onto the new floor **only when** the marker's parent is an ancestor of the floor (so no committed milestone is dropped) **and** the pick applies cleanly. Otherwise recovery **reports failure** — the checkpoint is set aside (`seededFrom` stays the fallback floor, `default` for an in-flight park), not force-applied and not silently dropped.

**The two durable invariants: never silently drop committed work, and never force a conflicted tree.** A marker whose parent carries committed divergence below it, or whose delta does not apply cleanly, is left set aside for a human rather than partially applied. A failed recovery is a first-class outcome that M4 then handles safely, not an error to paper over.

### D4 — M4 narrows #209 D4 row 3: skip the re-gate only for a PROVABLY-reviewed plan, only when recovery succeeded

A dropped-session cross-worker resume today re-plans and re-gates (#209 D4 row 3). #209 escalated that on its **third** review from a liveness bug to a **safety** bug: `plan_approved` can be true while `plan_md` is worker-authored and unreviewed, and re-planning is the defense against implementing a plan no human ever gated. M4 removes the wasteful re-gate for the common reviewed-plan case **without** weakening that safety property. `m4ResumeReviewedPlan` (`runner.ts`) holds only when:

- `plan_approved` is true, **and**
- `plan_source === "agent"` — a **POSITIVE allowlist**, not `!== "seeded"`, and
- `plan_md` is present, and the session is genuinely dropped (`!sessionId && !seeded`), and
- **NOT** (`humanApproved && recoveryFailed`).

**The durable safety invariant, and WHY the positive allowlist.** `plan_source === "agent"` is the #209 D8 provenance value meaning "worker-authored but human-gated". Writing the condition as `!== "seeded"` would fail **OPEN** on any future provenance value (an unreviewed source added later would silently qualify). The positive allowlist fails **SAFE**: an unrecognized provenance value re-gates. Second half of the invariant: a **human-approved** run whose recovery **failed** (empty tree) still re-gates — the gate is the human's one chance to notice the tree was lost, kept exactly where #209 put it. An **autopilot** recovery-failed run resumes: there is no human at the gate to protect. `humanApproved` is computed **fresh** from `claim.auto_approve` (the server clears it when a run parks at the plan gate), not reused from the mutable `ciFixHumanApproved`. And `preApproved` (`sdk-executor.ts`) plus the `embedSeededPlan` guard are both extended, so the plan body actually reaches the implement turn — a bare `planApproved` flip would leave the model without its plan (the #209 M2 gap).

### D5 — the affinity ceiling is raised FLAT 30m→2h, not made park-duration-aware (M3)

`WORKER_AFFINITY_CEILING` (`config.go`) is raised from 30m to **2h**, flat. This is the primary recovery path: a park shorter than the ceiling resumes on the original, still-live worker, where the session is intact (no re-plan) and the tracking-ref leg recovers the WIP with no ancestry test.

**The honest, non-obvious mechanism — record it, because it is easy to overstate what the ceiling does.** The affinity clock is stamped at **promotion (park-END)** in `PromoteLimitWaitRuns` (`updated_at = now()`), and the `ClaimRun` affinity leg (verified in `runtime.sql`) frees the pin to a peer **immediately** when the owner is heartbeat-stale or draining (the liveness leg from ADR-628 D3a). Therefore the ceiling bounds **only post-promotion queue-dwell for an alive-but-BUSY original worker**. It does **not** bound park duration — an alive worker re-claims its own promoted run within one poll regardless of the ceiling — and it does **nothing** for a worker recycled during the park (the liveness leg already freed that run; that case is covered by the now-robust cross-worker recovery above). 2h exceeds the observed #685 park (~87 min) and covers a typical multi-hour run's busy window.

**The accepted cost (ADR-628 D3a).** A wedged-but-heartbeating owner now strands its run from an idle peer for up to 2h (was 30m). This is bounded by the ceiling and by the liveness leg (the moment the owner goes stale or drains, the run frees at once).

**Why it stays FLAT, not park-duration-aware.** ADR-628 D3a rejected duration-awareness for a concrete reason: there is **no crisp claim-time signal** that isolates a park promotion from an ordinary requeue — the `limit_*` history fields (`limit_resets_at`, etc.) are deliberately left in place across later requeues as display history, so none of them mean "this requeue is a park promotion". M3 raises the flat value rather than re-opening that decision. The row lock still serializes claimants, so this changes *which* worker wins and *when* a peer becomes eligible, not claim atomicity.

## Consequences

- A gated, human-approved run that parks mid-implementation and resumes **same-worker** (the common case once the ceiling is 2h) loses **no** committed or uncommitted work and does **not** re-gate.
- The same run resuming **cross-worker** recovers when the WIP applies cleanly (strict-descendant, or a clean cherry-pick onto an advanced floor); when it cannot (diverged, unrecoverable), a human-approved run **re-gates** rather than re-implementing onto an empty tree.
- A surviving WIP snapshot is always `wip(park):`-marked and **never** appears in the final MR — guaranteed by the adopt-time `reset --soft`, so no finalize-time rewrite and no collision with ADR-456.
- `WORKER_AFFINITY_CEILING` default is now 2h (was 30m); the wedged-but-heartbeating-owner hold widens correspondingly, bounded by the ceiling and short-circuited by the liveness leg.
- The checkpoint publish is unchanged: still api-brokered on the join token (ADR-122), no PAT on the worker, no forge protected-branch write. The `main`-is-never-touched primary directive and its four guardrail layers are untouched.
- **Anyone later tempted to "simplify" the M4 gate to `plan_source !== "seeded"` must re-read D4:** the positive allowlist is the safety property, not a stylistic choice — it fails safe on unrecognized provenance where `!==` fails open.
- **Anyone tempted to strip the marker at finalize instead of adopt must re-read D3:** the adopt-time `reset --soft` is what keeps this fix from colliding with ADR-456.

## Out of scope (recorded so a future reader does not assume it landed)

- **Full cross-worker SDK session durability** — PRD #556's domain; M4 settles for a cold re-implement from a provably-reviewed plan.
- **Non-`limit_wait` interruptions (crash/eviction/shutdown).** #218 M1 covered the shutdown path for *committed* work; extending the WIP commit there is deferred (the network publish must not be forced into the ~30s termination grace), flagged for a fast-follow.

## Linked from ARCHITECTURE.md

Linked from ARCHITECTURE.md's Run lifecycle section, alongside the `limit_wait` / checkpoint / affinity references, per the repo convention.
