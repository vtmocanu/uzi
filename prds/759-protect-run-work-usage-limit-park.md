# PRD #759 — Protect run work across an Anthropic usage-limit park

**Issue**: #759
**Priority**: High
**Status**: Draft

## Problem

A gated PRD run that was already approved and actively implementing can lose **all** of its
in-progress work when it hits an Anthropic usage limit and parks (`limit_wait`). Nothing is
committed or pushed, and recovery is only possible by hand from the worker PVC — if at all.

### The incident (run #685, 2026-08-28)

- The run was approved at its plan gate and implemented ~4 hours of real code across `api/` and
  `web/`: the branding settings keys, a goose migration, `handler/branding.go` + routes + live-DB
  tests, `AdminBranding.tsx`, `AppShell.tsx` chrome, and ~10 test files.
- At 10:43 it hit a **5-hour** usage limit and parked (`limit_wait`, `resets_at` 12:10). The park
  lasted ~87 minutes.
- On resume a **different** worker claimed it, re-created the runner clone onto the **new** `main`
  tip (`main` had advanced during the park), and the run **re-planned from scratch** and returned to
  `awaiting_approval` — its feed message said "waiting for human approval before implementing", as if
  it had never started.
- Forensics confirmed total loss: **no** `agent/issue-685` branch on the remote, **no**
  `refs/uzi-checkpoints/agent/issue-685` ref, an **empty** worker tracking ref
  `refs/uzi-runner/agent/issue-685`, and a **clean** working clone (0 ahead, 0 uncommitted). The ~4h
  of work existed only in the working tree and was discarded by the reseed.

### Why the existing protections did not help

The runtime has three durability layers, all landed, and **every one captures committed commits
only — never the working tree**:

- **Checkpoint-on-park** (PRD #628 M2, `agent/src/runner.ts:2088-2104`): on a park it fetches the
  branch back (`git.fetchAgentBranch`, `git.ts:692-707`) and publishes a checkpoint pack via the
  api broker. `checkpointPack` returns `null` when the tracking ref does not exist
  (`git.ts:749-750`), and an at-base tip yields an empty delta the reseed's strict-descendant test
  rejects.
- **Mid-run checkpoints** (PRD #122 M8 / #267): fetch committed work to the tracking ref and broker
  a pack to origin `refs/uzi-checkpoints/<branch>` at milestone/interval boundaries — committed-only.
- **Reseed** (`git.ts:414-503`, `runnerCloneForBranch`): **unconditionally `fs.rm`s the clone dir on
  every claim** (`git.ts:418`); the working tree is never preserved (PRD #218 M6 dropped
  clone-preservation, `runner.ts:2247-2253`).

There is **no auto-commit anywhere on the park path** — this is a *deliberate* prior decision (PRD
#218 D6/R3: "a half-applied edit that survives is worse than one that does not"; PRD #267
Out-of-scope). The only protection is the lead *choosing* to commit at each self-contained step,
driven by prompt text. #685's lead batched ~4h without a durable commit, so all three layers were
no-ops.

## Root cause — two independent axes

**Axis 1 — the tree loss was total because the run never committed.** Every durability layer
captures committed commits; with zero commits the tracking ref equalled the clone base, the
checkpoint pack was empty, and the working-tree edits were `fs.rm`'d on the next claim. Mid-milestone
uncommitted work is unprotected by construction.

**Axis 2 — the re-plan + re-gate happened because the session was lost cross-worker.** The park (~87
min) exceeded `WORKER_AFFINITY_CEILING` = **30m** (`config.go:755`, `runtime.sql:587-597`,
`service.go:1273`), so a different worker claimed it. The SDK transcript lives on the original
worker's per-worker PVC (`agent/src/main.ts:115`), so on the new worker `sessionTranscriptResolvable`
is false → `sessionId` is cleared (`runner.ts:696`) →
`planApproved = (claim.plan_approved ?? false) && (!!sessionId || seeded)` is false (`runner.ts:924`)
→ the executor's `preApproved` gate is false (`sdk-executor.ts:981-984`) → the run re-plans and
re-gates. This is by design for a dropped-session non-seeded run (PRD #209 D4 row 3, escalated to a
**safety** decision on its third review — see Prior art); cross-worker session durability is
explicitly out of scope (PRD #556).

**The decisive coupling (found in review):** a **same-worker** resume avoids *both* axes for free.
Its recovery leg (`ownedHere`, `git.ts:453-457`) uses **no ancestry test**, so it recovers a WIP
commit even when `main` advanced during the park; and the SDK session on that worker's PVC is still
resolvable, so no re-plan happens. A **cross-worker** resume hits the strict-descendant checkpoint
guard (`git.ts:472-489`), which **rejects a WIP commit that diverged because `main` advanced — the
exact #685 condition** — and loses the session. Therefore the robust fix is to (a) make a durable
WIP commit exist at all, and (b) keep long parks on their original worker; cross-worker recovery is a
harder best-effort fallback.

## Prior art (in-repo — read before implementing)

- `prds/done/628-cross-worker-resume-durability.md` + `adr/0628-cross-worker-resume-durability.md` —
  same incident class; M1 affinity, M2 checkpoint-on-park. **ADR #628 D3a explicitly rejected a
  `limit_wait`-duration-aware affinity ceiling** (the `limit_*` fields are left in place as history
  across later requeues, so there is no crisp claim-time signal isolating a park promotion). M3 below
  respects that.
- `prds/done/209-*.md` — **the decision M4 reverses.** Its D4 row 3 (re-plan a dropped-session
  non-seeded run) was escalated on its **third** review from a liveness bug to a **safety** bug:
  `plan_approved` can be true while `plan_md` is worker-authored and unreviewed, and row-3
  re-planning is the defense against implementing a plan no human ever gated. D8 introduced
  `plan_source`/provenance as the discriminator. M4 must consult provenance, not just presence.
- `prds/556-session-preserving-worker-resume.md` — session resume; cross-worker session durability
  out of scope (Axis 2's deeper fix).
- `prds/done/267-time-based-checkpoints.md` / `prds/done/218-park-resume-work-loss.md` — the
  committed-only checkpoint model; **#218 D6 = no auto-commit on park** (the decision this PRD
  deliberately revisits, with the maintainer's explicit request, see Decision Log). #218 M1 also
  covered the **shutdown/eviction** path, "where the larger loss actually is" (see M6 note).
- `prds/done/110-checkpoint-agent-work.md` + `adr/0122-checkpoint-push-broker.md` — the checkpoint
  push is api-brokered (join token, no PAT); reuse this seam, never push with a PAT.
- `adr/0456-rebase-before-finalize-push.md` — the finalize-time align touches only
  `.github/workflows` and preserves the agent's SHAs; do NOT bury the WIP-marker strip here (see M2).

## Recovery strategy (the framing the milestones follow)

1. **A durable WIP commit must exist** (M1) — the runtime commits uncommitted work to a clearly
   marked throwaway commit before the tree is wiped on park. This is the one thing #685 lacked.
2. **Same-worker resume is the robust primary path** (M3) — keep long parks pinned to their original,
   still-live worker by raising the affinity ceiling. There the session is intact (no re-plan) and
   the tracking-ref recovery leg has no ancestry test (the WIP commit is recovered even when `main`
   moved). This alone resolves the #685 incident.
3. **Cross-worker resume is best-effort** — the brokered checkpoint push (already shipped, #628 M2)
   carries the WIP commit off-worker, but adopting it when `main` diverged needs a rebase/merge at
   adopt time (M2); when that is not clean, recovery fails safely and M4 re-gates a human-approved run
   rather than silently re-implementing onto an empty tree.

## Milestones

- [ ] **M1 — Marked WIP auto-commit of uncommitted work on the park path.** When a run parks on a
  usage limit and its runner clone has uncommitted changes, commit them to a **clearly-marked
  throwaway commit** (subject-prefixed `wip(park):`) run as the **runner uid** in the clone
  (`runGitAsRunner`, after `killAgentTree`, using the clone's existing `AGENT_GIT_IDENTITY`), inserted
  **before** `fetchBackBestEffort` (`runner.ts:2087`). The existing fetch-back then carries it to the
  local tracking ref, and the existing #628 broker best-effort publishes it to
  `refs/uzi-checkpoints/<branch>`. Reuse the shipped machinery; the only new code is the marked
  commit and its recognition downstream. *Success*: a run that parks mid-milestone with uncommitted
  edits leaves a `wip(park):` commit on its local tracking ref, and (best-effort) on the remote
  checkpoint ref.

- [ ] **M2 — Recover the WIP tree on resume via `reset --soft` at adopt time.** On reseed, when the
  adopted tip (tracking-ref leg for same-worker, checkpoint leg for cross-worker) is a `wip(park):`
  marker, `git reset --soft <parent>` so the WIP content returns to the working tree **uncommitted**
  and the branch tip is the last real commit — the marker **never enters the history the agent builds
  on and never reaches finalize** (this is what keeps it out of the MR; do NOT try to rebase it away
  later at finalize, which collides with ADR #456). For the **cross-worker diverged** case (`main`
  advanced during the park, so `isAncestor(newFloor, wip)` is false and the strict-descendant guard
  at `git.ts:472-489` rejects the checkpoint), attempt a rebase/merge of the WIP tree onto the new
  floor at adopt time; if it does not apply cleanly, treat recovery as **failed** (`seededFrom` stays
  `default`) rather than force it. *Success*: after a mid-milestone park + same-worker resume, the
  recovered clone contains the pre-park edits as uncommitted changes and the final branch carries no
  `wip(park):` commit; the cross-worker clean-rebase case likewise recovers, and the diverged
  non-clean case reports failure instead of silently dropping work.

- [ ] **M3 — Keep long parks on their original worker (the primary recovery path).** Raise the
  **flat** `WORKER_AFFINITY_CEILING` (`config.go:755`) so a multi-hour park resumes on the original,
  still-live worker, where the SDK session is intact (no re-plan) and the tracking-ref leg recovers
  the WIP commit with no ancestry test. Do **NOT** make it park-duration-aware — ADR #628 D3a rejected
  that for a concrete reason (no crisp claim-time signal isolates a park promotion; `limit_*` history
  persists across requeues). Name the trade-off a larger flat ceiling incurs (a wedged-but-heartbeating
  worker holds its run longer, ADR #628 D3a) and pick a value reasoned against the observed 5-hour
  limit window. *Success*: a park shorter than the new ceiling resumes same-worker with the session
  preserved and the WIP recovered.

- [ ] **M4 — Resume an approved run without re-gating — safely.** When a dropped-session run would
  today re-plan (cross-worker resume), skip the re-plan/re-gate and implement from the persisted plan
  **only when the plan is provably reviewed**: extend `planApproved` (`runner.ts:924`) /`preApproved`
  (`sdk-executor.ts:981-984`) to hold for `plan_approved && plan_md` **gated on `plan_source`
  provenance** (the frozen human-reviewed or autopilot-intended plan — the #209 D8 discriminator), and
  extend the `seeded` guard that gates `embedSeededPlan` (`sdk-executor.ts:2549-2555`) so the plan
  body actually reaches the implement turn for this case (a bare `planApproved` flip leaves
  `seededPlanBody` undefined and the model silently falls back to the issue — the exact #209 M2 gap).
  **Condition it on recovery success**: on a **human-approved** run whose recovery failed
  (`seededFrom == "default"`, empty tree), fall back to **re-gate** rather than re-implement cold — the
  gate is the human's one chance to notice the tree was lost. *Success*: a cross-worker resume of a
  provably-reviewed approved run with a recovered tree continues implementing (plan body present, no
  re-gate); a recovery-failed human-approved run still re-gates.

- [ ] **M5 — Make recovery and loss transparent in the run feed.** Emit a feed event that
  **distinguishes** recovering an *uncommitted WIP snapshot* from recovering a *committed milestone*
  (the existing "recovered N commit(s)" wording, `git.ts:170`, would otherwise mislead — and would
  **over-count by one** while the WIP marker is a commit, so compute the count after the `reset
  --soft`). Keep the existing #218 M3 loss notice ("no earlier work could be recovered", `runner.ts:616`)
  for the residual unrecoverable case; add only the genuinely-new WIP-recovery signal. *Success*: the
  #685 shape produces a feed event that correctly says whether uncommitted work was recovered or lost —
  no PVC forensics.

- [ ] **M6 — Tests, docs, ADR, close-out.** A cross-worker + same-worker recovery test that is
  **calibrated to fail against pre-fix code**: the fixture writes files into the worktree **without
  `git add`/`git commit`** (a dirty tree — the single deviation from the existing park factories, all
  of which commit) then throws `LimitReachedError`; the discriminating assertion is **file content
  present in the resumed clone** (not merely "a checkpoint ref appeared"), and the finalized branch
  shows **no `wip(park):` subject**. Pre-fix, the resumed clone's `seededFrom` is `default` and the
  file is absent — assert that failure explicitly. Add a test that a provably-reviewed approved
  dropped-session run implements from its plan (plan body reaches the turn) without re-gating, and that
  a recovery-failed human-approved run still re-gates. `task gate:api` + `task gate:agent` green.
  Docs: the resume/worker docs, `ARCHITECTURE.md`, `specs/ai.md`, CHANGELOG; a new ADR recording the
  #218 D6 revisit and the #209 D4-row-3 narrowing. Move this PRD to `prds/done/`.

## Out of scope

- **Full cross-worker SDK session durability** (sharing the transcript so any worker resumes the same
  agent session) — PRD #556's domain; M4 settles for a cold re-implement from a provably-reviewed
  plan.
- **Non-`limit_wait` interruptions (crash/eviction/shutdown).** #218 M1 covered the shutdown path for
  *committed* work and called it "where the larger loss actually is". A one-line WIP commit +
  local fetch-back on the shutdown branch (`runner.ts:2116`) would extend M1's protection to
  same-worker requeue after an eviction; it is deferred here only because the network *publish* must
  not be forced into the ~30s termination grace. Flagged for a fast-follow rather than silently
  dropped.
- Changing how the checkpoint push authenticates — it stays api-brokered (ADR #122); no PAT on the
  worker.

## Success criteria (whole PRD)

1. A gated, human-approved run that implements partially, parks on a usage limit, and resumes: (a) on
   the **same** worker (the common case once M3 raises the ceiling) loses **no** committed *or*
   uncommitted work and does **not** re-gate; (b) on a **different** worker recovers when the WIP
   applies cleanly, and when it cannot (diverged, unrecoverable), **re-gates** rather than
   re-implementing onto an empty tree.
2. A survivor WIP snapshot is always clearly marked and **never** appears in the final MR (guaranteed
   by the `reset --soft` at adopt time, not a finalize-time rewrite).
3. Recovery or loss is reported in the run feed, distinguishing uncommitted-WIP recovery from
   committed-milestone recovery — no PVC forensics.
4. `task gate:api` and `task gate:agent` green; the new behavior is covered by a test that **fails
   against the pre-fix code** on a genuinely-uncommitted, cross-worker fixture.

## Risks

- **R1 — Reversing #218 D6 (no auto-commit).** A surviving half-applied edit could be mistaken for
  reviewed work, and a **cold** resumed agent (M4) building on a half-applied tree has no session
  memory of which plan steps are done. *Mitigation*: the WIP is a marked throwaway stripped to
  *uncommitted* at adopt time (never a reviewed commit, never in the MR); and the resumed agent must
  reconcile the working tree against the plan before continuing (state this in the resume prompt so a
  cold agent treats a dirty recovered tree as "mid-edit, verify what is done", not as finished work).
- **R2 — M4 reverses #209 D4 row 3, a thrice-reviewed safety decision.** *Mitigation*: skip the
  re-gate **only** for a provably-reviewed plan (via `plan_source`), never on bare
  `plan_approved && plan_md`; fall back to re-gate on recovery failure for human-approved runs (this
  keeps the human's loss-detection gate exactly where #209 put it).
- **R3 — Plan body not delivered (the #209 M2 gap).** A bare `planApproved` flip leaves the model
  without the plan. *Mitigation*: M4 explicitly extends the `embedSeededPlan` guard and M6 tests the
  plan text reaches the implement turn (the stub executor cannot catch this — assert on the real
  path).
- **R4 — Marker leaking into the final branch.** *Mitigation*: `reset --soft` at adopt time means the
  marker is never in the history that reaches finalize; M6 asserts a marker-free final branch.
- **R5 — Larger affinity ceiling widens the wedged-worker hold (ADR #628 D3a).** *Mitigation*: a
  reasoned flat value, the cost named in the ADR; concurrency review confirmed the row lock still
  serializes claimants, so this changes *which* worker wins and *when* a peer becomes eligible, not
  atomicity.

## Decision Log

- **D1 — Fix both axes.** The re-plan/re-gate (Axis 2) is the visible symptom; the data loss is Axis 1
  (nothing committed). Fixing only Axis 2 resumes onto an empty tree. Both are in scope.
- **D2 — Same-worker resume is the primary, robust recovery path; cross-worker is best-effort.** The
  `ownedHere` tracking-ref leg has no ancestry test and the session survives, so a raised affinity
  ceiling (M3) plus the WIP commit (M1) resolves the #685 incident directly. The cross-worker
  checkpoint leg's strict-descendant guard rejects a diverged WIP (the incident's own condition), so
  it is treated as best-effort with a rebase-at-adopt attempt and a safe failure.
- **D3 — Durable = a marked commit, restored to uncommitted at adopt time.** Reconciles #218 D6: the
  survivor is a clearly-marked throwaway that is `reset --soft` back to uncommitted on recovery, so it
  never masquerades as reviewed work and never lands in the MR — without the finalize-time history
  rewrite that would collide with ADR #456.
- **D4 — M4 narrows, not reverses, #209 D4 row 3.** The re-gate is skipped only for a provably-reviewed
  plan (consulting `plan_source`, the D8 discriminator) and only when recovery succeeded; a
  recovery-failed human-approved run still re-gates. This keeps #209's safety property (no
  implementing an unreviewed plan with no gate) while removing the wasteful re-gate for the common
  reviewed-plan case.
- **D5 — Flat affinity ceiling, not park-duration-aware.** ADR #628 D3a rejected duration-awareness
  for a concrete reason (no crisp claim-time signal; `limit_*` history persists across requeues); M3
  raises the flat value and accepts the named wedged-hold cost rather than re-open that decision.
