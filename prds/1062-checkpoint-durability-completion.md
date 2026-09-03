# PRD #1062 — Checkpoint durability completion

Folds the three residual work-loss follow-ups left open by PRD #1030
(`prds/done/1030-worker-resume-durability.md`): **#1059** (M1), **#1036** (M2),
**#1037** (M3). One design, three independent MRs. `main` is never touched.

All code citations are pinned to `main` at `fcfd8aa` and must be re-derived at
implementation time (line numbers drift). This PRD is written to be implemented by
an offline worker: every fact it relies on is resolvable from this repo's code
alone. The one premise that is about GitHub server behaviour rather than repo code —
that a `repo`-only PAT push is gated on the pushed tip's `.github/workflows/` tree
differing from the default branch (M2) — is grounded in the recorded evidence of
`adr/0456-rebase-before-finalize-push.md` (the #422/#377 incident), not on an
open-web lookup.

## Status

- **M1 (#1059)** — DONE (2026-09-03, branch `agent/issue-1059`). Adopt-seam unified on
  the owner anchor across BOTH not-ownedHere legs; agent-side only (`agent/src/git.ts`
  + `agent/test/git-cross-worker-recovery.test.ts` + `agent/test/git.test.ts`), no
  server/schema/migration change. `adr/0628` amended, `specs/ai.md` §609 added.
- **M2 (#1036)** — approach NOT finalized: an open design fork (broker-side overlay
  synthesis vs reusing ADR-0456's `alignBranchWithDefault`) must be settled before
  implementation — see M2. Held until M1 merges regardless (edits the same `git.ts`
  adopt block).
- **M3 (#1037)** — designed, server-only (worker adoption plumbing already threads
  `checkpoint_tip`+`resume` for all kinds — see M3). Held for blast-radius isolation
  behind M2.

## Problem

PRD #1030 made the forge-checkpoint safety net reliable and observable, but its
`## Deferred` section named three residual ways a worker still loses or mis-adopts
un-pushed work:

1. **#1059 — a fresh run can adopt a prior run's checkpoint.** The owner-anchor
   guard from #1042 (`runs.checkpoint_tip`, PR #1053) is applied on only ONE of the
   two adoption legs in `agent/src/git.ts` `runnerCloneForBranch`. The other leg
   (the strict-descendant path, which covers a fresh run and a resume where
   `origin/<branch>` exists) adopts any checkpoint that strictly descends the floor
   with **no** tip check. So a fresh re-run for an issue can seed off a prior run's
   surviving `refs/uzi-checkpoints/<branch>` (possibly plan-rejected work) if the
   prior run's terminal CAS-delete failed. This re-opens the exact wrong-work class
   #1030/#1042 exist to close.

2. **#1036 — a branch behind `main` on `.github/workflows` checkpoints nothing.**
   The broker's PAT deliberately lacks `workflow` scope, so a checkpoint push whose
   tip differs from `main` under `.github/workflows/` is rejected and mapped to
   `skipped:"workflow_scope"`. On this workflow-heavy repo `main`'s workflow files
   change often, so a long-running branch is frequently behind on them and every
   checkpoint is skipped. If such a worker genuinely dies (node death, PVC reclaim),
   its work is unrecoverable — no checkpoint on the forge, no PVC. This is #1030's
   D8(b) residual.

3. **#1037 — non-issue run kinds get no checkpoints.** Forge checkpointing is
   issue-runs-only: `api/internal/workersvc/service.go` `Publish` returns
   `skipped:"unsupported"` for every other kind. So `self_improve` (long and
   valuable), `ci_fix`, `mr_rework`, `prompt`, and `chat` runs have zero
   cross-worker durability. This is #1030's G5 residual.

## The shared machinery (why these three compose, not merely coexist)

Checkpoint flow today, per branch:

- The worker builds a delta pack of `refs/uzi-runner/<branch>` vs the floor and ships
  `{tipOid, pack}` to the api — `agent/src/git.ts` `checkpointPack`,
  `agent/src/runner.ts` `publishCheckpointBestEffort` (fired at park, shutdown, and
  mid-run via the `checkpoint` tool). **`publishCheckpointBestEffort` is NOT
  kind-gated on the worker** — it fires for any run with a branch.
- Server `Publish` (`api/internal/workersvc/service.go`) is the binding gate: it
  derives repo/branch/PAT from the run row, hands the pack to the go-git broker
  (`api/internal/pushbroker/pushbroker.go`), pushes NON-FORCED to
  `refs/uzi-checkpoints/<branch>`, and on success persists
  `runs.checkpoint_tip = tipOid`.
- On (re)claim, `fetch()` mirrors origin's checkpoint refs into the worker bare;
  `runnerCloneForBranch` decides whether to adopt the checkpoint as the seed base.
- Terminal cleanup CAS-deletes the ref using `checkpoint_tip` as the expected Old.

The three issues converge on two files: the `git.ts` adopt seam (#1059, #1036) and
`service.go` `Publish` (#1036, #1037). That coupling is why they are one design and
why the milestones are sequential (see *Sequencing*).

## Milestones

### M1 — #1059: unify checkpoint adoption on the owner-anchor

**Files**: `agent/src/git.ts`, `agent/test/git-cross-worker-recovery.test.ts`
(and `agent/test/git.test.ts` if it asserts adoption). No server change.

**Current state.** In `runnerCloneForBranch`, inside the not-`ownedHere` `else`
block (floor = `originRef` if the branch is pushed, else the default branch):

- **Leg A — resume + unpushed, owner-anchored** (`checkpointExists && resume &&
  !originExists`): adopts only when `expectedCheckpointTip` is a non-empty string AND
  equals the mirrored checkpoint's SHA. This is the #1042 M4 guard; #1030 M3 relies
  on it.
- **Leg B — strict-descendant** (`else if (checkpointExists)`): covers **fresh runs**
  (`session_id == null` ⇒ `expectedCheckpointTip` is NULL) and resumes where
  `origin/<branch>` exists. Adopts when the checkpoint **strictly descends** the
  floor, with **no** owner-anchor check. On divergence it sets
  `checkpointSetAside = true`, which drives the #759 foreign-WIP cherry-pick
  recovery.

**Key finding (drives the test work).** The existing test
`git-cross-worker-recovery.test.ts` models cross-worker recovery as a fresh,
different-run-id clone adopting a strictly-descending checkpoint (e.g.
`createOrAttachRunnerClone(bareB, 628, "run-B")` with `resume=false` and no tip).
**That test encodes the #1059 bug as intended behaviour.** In production a
cross-worker requeue keeps the **same** `run_id`, and `runs.checkpoint_tip` persists
on the run row and re-arrives on the claim, so a legitimate cross-worker resume has
`expectedCheckpointTip` SET and matching. The fresh-different-run model is an
artifact of these tests predating the owner-anchor. So M1 must **rewrite** those
tests to model production, not merely add new ones.

**Target predicate.** Hoist the owner match above the leg split:
`ownerMatch = expectedCheckpointTip is a non-empty string AND === checkpointSha`.

```
# not-ownedHere block; floor / floorFrom computed as today
if checkpointExists AND ownerMatch:
    if !originExists AND resume:
        adopt (seededFrom = "checkpoint")                 # Path A: today's Leg A, unchanged
    else:
        if strictlyDescends(floor, checkpoint): adopt (seededFrom = "checkpoint")   # Path B, now owner-gated
        elif diverged(floor, checkpoint):        checkpointSetAside = true          # OWN diverged ckpt → #759
        # equal-to-floor → fall through to floor (nothing recovered), unchanged
elif checkpointExists:   # !ownerMatch — foreign / fresh (NULL tip)
    # NEVER adopt, NEVER set checkpointSetAside (that flag drives the #759 cherry-pick,
    # which would re-import the foreign work this guard exists to keep out).
    # Seed off the floor, log LOUDLY (structured warn, not a run-feed status).
```

**Why this preserves what #1059 says must not regress:**

- **#759 foreign-WIP cherry-pick recovery.** In production this is a same-`run_id`
  requeue of a parked run whose `wip(park):` marker IS the persisted
  `checkpoint_tip` (the park publishes it). So `ownerMatch` is TRUE and
  `checkpointSetAside` still fires on divergence → the cherry-pick still runs.
- **#1030 M3 same-run resume.** Path A is byte-unchanged; a same-run resume's own tip
  advances with its checkpoint on every publish, so `ownerMatch` holds (the #1042
  "trap #5" case).

**Fresh-rerun outcome.** A NULL-tip fresh run with a surviving foreign checkpoint —
whether pushed-to-origin or unpushed, descending or diverged — takes the
`!ownerMatch` branch: seed off the floor, log loudly, no inheritance, no cherry-pick.

**Test work (part of the deliverable, not optional):**

- **🔴 HIGHEST-ATTENTION — the `760`/`761`/`762` scenarios ARE the #759 cherry-pick
  regression guards, not incidental tests.** They call
  `createOrAttachRunnerClone(bareB, N, "run-B")` with `resume=false` + no tip — i.e.
  exactly the `!ownerMatch` branch M1 makes NEVER set `checkpointSetAside`. Rewrite
  them to model production (`resume=true` + the matching `expectedCheckpointTip`) so
  they still exercise `checkpointSetAside`→cherry-pick. A *botched* rewrite removes
  the #759 guard while staying green — verify each still fails on code that breaks
  the #759 path, not merely on the #1059 path. The `628` (strict-descendant committed
  adopt) scenario is rewritten the same way; `629` (equal → default) and `630`
  (no-checkpoint → default) keep their assertions.
- Add true-negative #1059 cases (new): a fresh run (`resume=false`, NULL tip) with
  (i) a strictly-descending and (ii) a diverged surviving checkpoint → `seededFrom`
  is NOT `"checkpoint"`, `checkpointSetAside` is NOT true, no files inherited, the
  loud log is present.
- Each new/changed test must fail on the unfixed code and pass with the fix (the
  repo's regression-test discipline). Watch each go red before green.
- Update the in-file invariant comment near the `checkpointSetAside` declaration
  (today it reads that the flag is only ever set on the else-floor leg): under M1 it
  is also set on `ownerMatch` Path B (diverged), where `seededFrom` stays the floor —
  still consistent, but the comment becomes misleading if left.

**ADR**: extend `adr/0628-cross-worker-resume-durability.md` with the unified adopt
predicate (correction in place; cross-link from 0759).

**Accepted residual** (already true on Leg A, now extended to Leg B): a checkpoint
pushed to origin whose `checkpoint_tip` persist failed (best-effort) will not be
adopted. The window is one line wide (persist follows push); consistent with #1042's
shipped tradeoff.

### M2 — #1036: mid-run checkpoint durability for branches behind on workflows

**Files** (overlay path): `api/internal/pushbroker/pushbroker.go` (+ tests),
`api/internal/workersvc/service.go` (persist the pushed SHA), `agent/src/git.ts`
(overlay unwrap, composing with M1's predicate). **Highest blast radius (go-git
object synthesis in the secrets-holding api, plus the #1009-reproducing seam) — its
own MR.** No schema change.

**Prior art — ADR-0456 is the authority on this gap and must be read first.**
`adr/0456-rebase-before-finalize-push.md` already: (a) confirms M2's premise (the
rejection is a tip-vs-default `.github/workflows/` tree comparison, not per-commit);
(b) explicitly names *this* problem — mid-run checkpoint coverage for a
behind-on-workflows branch — as its own **deferred** follow-up (its D5: "a
behind-on-workflows run gets no mid-run checkpoint coverage … deferred"); and (c)
already built `alignBranchWithDefault` (SHA-preserving merge first, rebase fallback)
for the *finalize* path, plus `isWorkflowScopeRejection` detection and the
`pushbroker.ErrWorkflowScopeRejected` → benign-skip mapping. M2 closes 0456's
deferred gap.

**⚠️ OPEN DESIGN FORK — settle before implementing M2 (architect follow-up).** Two
mechanisms close the gap; the PRD does not pre-commit:

- **Option A — broker-side overlay synthesis** (the original #1036 proposal, detailed
  below). Runs entirely server-side in the broker from objects it already holds; needs
  no worktree and never perturbs the live runner clone. Cost: go-git object synthesis
  in the secrets-holding api, a `checkpoint_tip`=O stored-SHA contract, a two-level
  adoption unwrap, and a pushbroker strict-descendant rework (see below) — the largest
  new surface of the three milestones.
- **Option B — reuse `alignBranchWithDefault` mid-run.** Align the tip to the
  default's workflows with the existing, proven tooling and checkpoint the *real*
  aligned tip: no synthesis, no O-vs-realTip contract, no two-level unwrap, no
  #1009-reproduction surface. Cost: `alignBranchWithDefault` needs a **worktree**, and
  ADR-0456 D5 records that the broker has **none** (and its deep-fetch was removed for
  OOM safety), so the align must run **worker-side** in a throwaway worktree at
  checkpoint time (not the live runner clone the agent is working in — that
  perturbation is the hazard Option A avoids), and a merge-align still leaves a merge
  commit / rebase-align rewrites SHAs, either of which must then compose with the
  owner-anchor `checkpoint_tip`.

The perturbation-free, server-side property is Option A's genuine advantage;
Option B's is that it reuses proven, lower-risk machinery and creates no new on-forge
object contract. This fork is a required Decision-Log entry (D7) before M2 is built.

**Whichever option is chosen, M2 MUST NOT bet on a single GitHub rule.** ADR-0456
deliberately refused to (its D2: "we cannot settle GitHub's exact rule from inside an
offline worker … robust to either mechanism"). So M2 inherits `isWorkflowScopeRejection`
and, if the aligned/overlaid push is *still* rejected, falls back to the clean skip
that is today's behaviour (`ErrWorkflowScopeRejected` → `skipped:"workflow_scope"`) —
never a 5xx, never a failed run. The claim "swapping `.github` necessarily satisfies
the check" is the expected case, not a guarantee.

---

The remainder of this section details **Option A** (the overlay), so the design is
complete if that fork is chosen. If Option B is chosen, D2/D3 below do not apply and
M2's Decision Log records Option B's align-composition instead.

**Overlay mechanics (Option A).** When the tip's `.github` tree entry differs from the
depth-1-fetched default's, synthesise an in-memory commit **O**:

- tree = the tip's root tree with the `.github` entry hash swapped to the default's;
- parents = `[realTip]` for the first overlay after a plain checkpoint (or ever), and
  `[realTip, prevO]` only when the PRIOR checkpoint was itself an overlay — the second
  parent keeps the non-forced push a fast-forward (`strictDescends(prevO, O)` holds so
  the wire CAS with Old=prevO accepts). `prevO` is **optional**, not always present.
- bot identity, subject prefixed `ckpt(overlay): …`.

Push **O** (not `realTip`) to `refs/uzi-checkpoints/<branch>`.

**No extra fetch is needed** (confirmed): `fetchBaseRefs` already fetches the default
branch depth 1, and a depth-1 fetch pulls that commit's COMPLETE tree (depth bounds
history, not tree completeness), so the default's `.github` subtree is present. The
swap needs only the **tip's root tree** (in the worker's delta pack) plus the
**default's `.github` subtree** (in the depth-1 default fetch). Note the tip's own
`.github` subtree may NOT be in the delta pack (if the run never touched `.github`,
that subtree is reachable from the excluded base and is not shipped) — but the swap
does not need it, so the "no extra fetch" conclusion holds.

**Granularity: swap at `.github`, not `.github/workflows`.** GitHub's scope check is
about `.github/workflows/**`; swapping the whole `.github` subtree necessarily makes
workflows match the default and satisfies it (subject to the no-single-bet fallback
above), using only the one subtree already in the pack. `.github/workflows`
granularity would require synthesising a new intermediate `.github` tree (default's
workflows + tip's other `.github` entries) — more synthesis, more risk, and **zero
recovery benefit because the reseed unwraps O → realTip anyway**, so the overlay's
coarse tree is never what the agent builds on. Edge cases (default has no `.github`;
tip removed workflows) all resolve correctly because recovery uses `realTip`. Cosmetic
trade-off recorded: a human fetching the checkpoint ref sees a synthetic
workflow-revert commit — informational only.

**⚠️ Load-bearing decision (D2): `runs.checkpoint_tip` stores O, the SHA actually on
the ref — NOT the worker's declared `realTip`.** The owner-anchor guard (M1) compares
`expectedCheckpointTip` (= the DB value) against the mirrored ref's current SHA (= O).
If the DB stored `realTip` while the ref holds O, `ownerMatch` would NEVER match on an
overlaid resume → own work never adopted → the #1009 total-loss incident, reproduced.
The stored value is consumed at exactly **two** sites, both needing O: the adopt guard
(compares DB against the ref) and the terminal CAS-delete (`ExpectedOldTip` must equal
origin's ref = O). The publish-time wire CAS does **not** read the DB (its Old is the
freshly-fetched ref, already O regardless), so it is not a third site — earlier drafts
double-counted it. Consequence: the broker must **return the pushed SHA** (add a `Tip`
field to `pushbroker.Result`) and `Publish` persists THAT via `SetRunCheckpointTip`,
not the worker's `tipOid`. On the no-overlay path the returned Tip == `realTip`, so
behaviour is unchanged. (Rejected alternative — store `realTip`, unwrap at every
comparison — silently breaks the CAS-delete unless every site unwraps: more seams,
more ways to reproduce #1009.)

**🔴 Pushbroker strict-descendant rework — this is core, not a wrapper (elevated from
a prose aside).** After an overlay publish, origin holds `O_prev` whose
`parent[0] = realTip_prev`. On the NEXT publish the worker declares `realTip_next`,
which descends `realTip_prev` but does NOT descend `O_prev` (`O_prev` is a sibling off
`realTip_prev`). The broker's current strict-descendant check
(`strictDescends(base = fetched checkpointTip = O_prev, declared = realTip_next)`)
returns false → `ErrNotDescendant` → **every post-overlay checkpoint would be skipped
as `not_descendant`.** So M2 must add to the broker: (a) detect that the fetched
`checkpointTip` is an overlay (subject-prefix probe), (b) unwrap it to `realTip_prev`
for the **ancestry base** while keeping the **wire-CAS Old = O_prev**, and (c)
synthesise `O_next` with `parent[1] = O_prev`. That is never-forced-invariant logic
inside the broker, which today has **zero** overlay awareness — scope it explicitly in
the Publish steps, not as a comment. Also: the "already up to date" early-return
(`checkpointTip == tipHash`) goes **dead** for overlaid branches (O never equals
realTip), so a genuine no-new-work resume — PRD #1030 M1's idempotency case — now
falls through and reports `skipped:"not_descendant"` instead of success; harmless to
correctness but a run-feed/telemetry regression to call out and handle.

**🔴 Adoption unwrap — overlay and wip-park have OPPOSITE tree semantics (do NOT
treat them as "two first-parent peels").** The existing `wip(park):` unwrap
*deliberately keeps* the marker's tree as uncommitted WIP (`checkout <marker>` then
`reset --soft markerParent`). If the overlay were peeled the same way (`reset --soft`
to `O^1`), the working tree would stay at O's tree = realTip-with-`.github`-swapped,
and `git diff --cached` would show the `.github/workflows` swap as **staged changes** —
re-polluting the branch and able to re-trigger the workflow-scope rejection at
finalize, which also breaks the #377 "agent branch never contains O" guarantee. So:

- **Overlay = discard-by-rebase.** Re-point the checkout base to `O^1` (= realTip)
  **before** the marker logic runs — `baseSha = isOverlay ? O^1 : baseSha` — discarding
  O's tree entirely.
- **Wip-park = keep-by-soft-reset**, exactly as today, running *after* the overlay
  re-point (so a `wip(park)` marker sitting under the overlay is still handled).

Detection is by commit-subject prefix (mirroring `isWipParkMarker`'s
`%s` startsWith `"wip(park):"`; overlay uses `"ckpt(overlay):"`). Order: overlay
(outermost, broker-applied) then wip-park (inner, worker-applied) — the two prefixes
can stack (`O → wipMarker → realParent`).

**Security — require FAIL-SOFT on a malformed tip.** The synthesis reads the worker's
(untrusted) root tree to enumerate entries and swap the `.github` hash. On a malformed
or edge tip (no `.github`, unreadable tree) it must fall back to pushing `realTip`
(→ the clean workflow-scope skip), never panic or 5xx. `Publish` runs on the request
goroutine so a panic is a per-request 500 (net/http recovers — not a process crash),
but a graceful non-overlay fallback is an explicit requirement, proven by the edge
fixtures (Contract B). Otherwise the surface is bounded: the synthesised tree/commit
derive purely from already-fetched trusted objects (default's `.github`) plus
already-budget-scanned worker objects, no new external input, pure in-memory
computation. Keep it isolated in `pushbroker` with unit tests against a local bare
fixture. **#377's finalize-block still covers it by construction**: O lives only on
`refs/uzi-checkpoints/*`, never `refs/heads/*`; the finalize push targets heads; the
reseed unwraps O → realTip so the agent branch never contains O.

**Raised-stakes residual (worse than #1042's, not a copy).** The best-effort persist
window (push succeeds, `SetRunCheckpointTip` fails) is more dangerous with O than
pre-M2. Pre-M2 a persist failure left DB=realTip while ref=realTip — self-healing (the
worker re-declares realTip next tick). Post-M2 a push-success/persist-fail leaves the
DB stale (prevO or NULL) while the ref holds O_current, and **the worker can never
re-derive O** (it only knows realTip). So the checkpoint is un-adoptable until the next
successful publish re-persists O_next, and permanently orphaned if the run dies
in-window. The window is one checkpoint interval, not one line — name it as a raised
residual, not #1042's tradeoff.

**ADR**: a new ADR keyed `1036` for the overlay object contract (Option A) —
`checkpoint_tip` stores O; adoption discards the overlay tree (rebase to `O^1`) then
soft-resets any wip-park marker; the owner-anchor and CAS-delete compare against O; the
broker unwraps O_prev for the ancestry base while the wire CAS keeps Old = O_prev.

### M3 — #1037: checkpoint `self_improve` runs on graceful interrupt

**Files**: `api/internal/workersvc/service.go` (kind gate + per-kind branch
derivation) and a new helper (e.g. `api/internal/workersvc/checkpoint_branch.go`).
`agent/src/sdk-executor.ts` is touched ONLY if the optional mid-run TOOL gate is
wanted (see below). Own MR. No schema change.

**M3 is server-only for the durability win (de-risked).** The worker adoption side
already works for every kind: `runnerCloneForClaim` threads
`expectedCheckpointTip = claim.checkpoint_tip` AND `resume` through `cloneBranch` for
all kinds, `self_improve` included. `publishCheckpointBestEffort` is likewise NOT
kind-gated on the worker. So the ONLY binding gate is server-side `Publish` /
`deleteCheckpointBestEffort`; no agent-side adoption change is needed.

**Scope (decided): `self_improve` only, plus a documented residual.** The milestone
title says "on graceful interrupt" on purpose: enabling server `Publish` for
`self_improve` delivers durability for GRACEFUL interruptions only — park, drain,
controller cordon-and-roll (per the hosted lane's cordon+drain, force-roll off — the
dominant hosted case), because `self_improve` has **no mid-run checkpoint cadence**
(the worker publishes at park/shutdown only; there is no milestone tool firing for it).
So a `self_improve` run that neither parks nor rolls before completing is never
checkpointed, and a HARD kill (node death, PVC reclaim) — which never reaches
park/shutdown — is not covered. Full hard-kill durability additionally needs a
**mid-run time-based auto-publish** independent of the lead's `checkpoint` tool; that
cadence is a **documented residual / follow-up**, not in M3. Do not describe M3 as
"self_improve is now durable" — it is "self_improve survives a graceful interrupt".

**Gates to change:**

1. `service.go` `Publish` (the `kind == Issue` gate) and the terminal
   `deleteCheckpointBestEffort` gate: replace the issue-only gate with a
   checkpoint-eligible-kind check plus a **per-kind server-side branch derivation**
   (the new helper). For `self_improve` the branch is `uzi/self-improve/<runId>`,
   derivable from the run row. Keep the security invariant: the branch is derived from
   validated run-row fields only, never from a worker-supplied string.
2. `agent/src/sdk-executor.ts` `checkpoint: isIssueRun`: broaden to the eligible set
   ONLY if the lead's mid-run TOOL checkpoints are wanted. For `self_improve` (no
   milestone structure) the tool is marginal; the park/shutdown/roll checkpoints are
   the main win and need only change (1).
3. Ref keying is already branch-generic everywhere (`refs/uzi-checkpoints/<branch>`) —
   no change (confirmed).

**Per-kind decisions (recorded; only `self_improve` ships in M3):**

| kind | checkpoint? | reason |
|---|---|---|
| `self_improve` | YES (M3) | long, valuable, unpushed branch — the headline durability gap |
| `ci_fix` | deferred | branches, but a `ci_fix` on an existing `agent/issue-<iid>` shares that branch and its checkpoint ref; the per-run owner-anchor discriminates, but flag before enabling |
| `mr_rework` | deferred | branches, but `origin/<branch>` already exists (an open MR), so recovery seeds off origin anyway; checkpoint adds only unpushed rework commits |
| `task` | out of scope | produces a branch (`uzi/task/<runId>`); not in #1037's list |
| `prompt` | deferred | re-runnable on the next schedule tick; low value now |
| `chat` | NO | interactive, re-askable, often repo-less |
| `judge` | NO | advice-only, writes no code, no branch |

**No ADR** — this is the existing publish machinery extended to more kinds; the
Decision Log plus the branch-derivation contract note (below) suffice.

### Docs & specs (folded into each milestone's MR, not a separate milestone)

There is no standalone M4. Each milestone ships its own docs/specs/ADR in its own MR,
matching the house "every change ships its docs and tests" discipline — a separate
docs milestone would double-book the ADR work already assigned to M1 and M2:

- **M1** ships the `adr/0628` extension (unified adopt predicate) and its `specs/ai.md`
  entry.
- **M2** ships the new ADR (`1036` for Option A, or an Option-B contract note) and its
  `specs/ai.md` entry.
- **M3** ships its `specs/ai.md` entry (no ADR) and any `docs/` update describing which
  run kinds are checkpointed.

## Sequencing

**M1 → M2 → M3**, sequential:

- M1 settles the adopt predicate in `git.ts`; M2's overlay unwrap (Option A) edits the
  same adopt block, so M2 rebases on M1. Real dependency.
- M2 → M3 is **blast-radius isolation, not a merge conflict.** M2 (the persist line)
  and M3 (the kind gate) edit `service.go` `Publish` ~70 lines apart in separate hunks,
  so they would not textually conflict; M3 sequences after M2 because M2 is the
  dangerous milestone (go-git synthesis in the secrets holder + the #1009 seam) and
  should land and bake alone.
- Genuine parallel opportunity: M3's per-kind branch-derivation helper and its
  differential test are file-disjoint from M1/M2 and can be authored in parallel; only
  wiring them into `Publish` waits for M2.

Each milestone is a separate MR. M2 MUST be its own MR for blast-radius reasons.

## Success criteria

1. **M1.** A fresh run (`resume=false`, NULL `checkpoint_tip`) does NOT adopt a
   pre-existing `refs/uzi-checkpoints/<branch>`, even one that strictly descends the
   floor; it seeds off the origin/default floor and logs loudly. Proven by a test that
   fails on the unfixed code.
2. **M1.** A legitimate same-run resume still adopts (tip matches). #759 foreign-WIP
   cherry-pick recovery and #1030 M3 resume recovery are proven intact by the rewritten
   tests.
3. **M2.** A checkpoint publish on a branch behind `main` on `.github/workflows`
   succeeds (no `skipped:"workflow_scope"`) via the chosen mechanism; if that push is
   still rejected the run skips cleanly (`skipped:"workflow_scope"`, never 5xx, never a
   failed run) — the no-single-bet fallback. On Option A additionally: `runs.checkpoint_tip`
   stores O; a resume discards the overlay tree (rebase to `O^1`) then soft-resets any
   wip-park marker, landing on `realTip` with **no staged `.github` diff**; a **second
   sequential publish over an existing overlay** is NOT skipped as `not_descendant`; the
   no-overlay path is byte-unchanged (persisted SHA == `realTip`). Proven by a pushbroker
   fixture set (behind, not-behind negative, stacked wip+overlay, second-sequential-publish,
   and the malformed/edge tips that must fail soft).
4. **M3.** A `self_improve` run's un-pushed work is checkpointed to
   `refs/uzi-checkpoints/uzi/self-improve/<runId>` on a graceful interrupt and adopted
   on resume; the server derives the branch from run-row fields only. The worker
   `RUN_KIND_PROFILES.cloneBranch` and the server derivation agree (differential test).
5. No new goose migration is required by any milestone (`runs.checkpoint_tip` from
   #1042 is reused).

## Contracts requiring differential tests

Two reimplementations this design creates; pin each with a discriminating fixture:

- **Contract A (M3): branch derivation is duplicated** — the worker's
  `RUN_KIND_PROFILES.cloneBranch` (TS) vs the new server-side per-kind derivation
  (Go). Divergence means the server pushes to `refs/uzi-checkpoints/<serverBranch>`
  while the worker reseed reads `<workerBranch>` → silent total durability loss. Pin
  with a cross-language fixture (`fixtures/api-contract` pattern):
  `{kind, claim-inputs} → expected branch`, asserted by BOTH a Go and a TS test, with
  one case per enabled kind and an assertion that each enabled kind is exercised.
- **Contract B (M2, Option A only): the overlay object shape.** Pin with a pushbroker
  fixture set — a golden snapshot from only the "behind" case would lock in the blind
  spot, so author all of:
  - **behind** ⇒ O.tree.`.github` == default's, O.parent[0] == realTip,
    returned/persisted SHA == O;
  - **not-behind** (discriminating negative) ⇒ no overlay, persisted == realTip;
  - **stacked** (a `wip(park)` tip also behind) ⇒ O over the marker ⇒ `git.ts` discards
    O's tree (rebase to `O^1`) then soft-resets the marker to its parent, landing on
    `realParent` with no staged `.github` diff;
  - **second sequential publish over an existing overlay** ⇒ `realTip_next` (which
    descends `realTip_prev`, not `O_prev`) is NOT skipped as `not_descendant`; `O_next`
    carries `parent[1] = O_prev` so the wire CAS (Old = `O_prev`) accepts. This is the
    case that catches the ancestry-base-unwrap bug — the single-publish fixtures miss it;
  - **edge / malformed tips** (default has no `.github`; tip removed
    `.github/workflows`; unreadable tree) ⇒ fail soft to pushing `realTip`, never panic.

## Deferred (documented residuals, not implemented here)

- **Hard-kill mid-run cadence for `self_improve`** — a time-based auto-publish
  independent of the lead's `checkpoint` tool, for full node-death/PVC-reclaim
  durability. M3 covers graceful interrupts (the dominant hosted case).
- **`ci_fix` / `mr_rework` / `prompt` checkpoints** — see the per-kind table; each has
  a caveat (same-branch sharing, origin-exists, low value) that warrants its own
  evaluation.

## Decision Log

- **D1 — unify both adopt legs on the owner-anchor (M1).** The strict-descendant leg
  gains the same `ownerMatch` gate as the resume+unpushed leg; a fresh/foreign run
  (NULL tip) never adopts and never sets `checkpointSetAside`. Chosen over a narrower
  fresh-run-only guard because a single predicate is auditable and cannot drift between
  legs.
- **D2 — `runs.checkpoint_tip` stores O, the overlay wrapper SHA (M2, Option A only).**
  ⚠️ Load-bearing: storing `realTip` while the ref holds O reproduces the #1009
  total-loss. The stored value is consumed at exactly **two** DB sites, both needing O:
  the adopt guard and the terminal CAS-delete's `ExpectedOldTip`. The publish-time wire
  CAS reads the freshly-fetched ref (already O), not the DB, so it is not a third site —
  earlier drafts double-counted it. The broker returns the pushed SHA; `Publish`
  persists that. **Conditional on choosing Option A** (see D7); void under Option B.
  (Gated with the user 2026-09-03.)
- **D3 — overlay swaps at `.github`, not `.github/workflows` (M2, Option A only).**
  Coarser but uses only trees already in the pack, and the reseed discards O's tree so
  the coarse tree never reaches the agent. Cosmetic cost only. Conditional on Option A.
- **D4 — M3 ships `self_improve` only; hard-kill cadence is a residual.** Server
  enablement covers graceful interrupts (park/drain/cordon-roll), the dominant hosted
  case. (Gated with the user 2026-09-03.)
- **D5 — ADRs: extend `adr/0628` for M1; new ADR `1036` for M2's overlay contract; none
  for M3.** (Gated with the user 2026-09-03.)
- **D6 — three separate MRs, sequential M1 → M2 → M3.** Blast radius (M2's go-git
  synthesis in the secrets holder) and shared-file coupling (`git.ts`, `service.go`)
  make one monolithic MR the wrong risk profile for the code that guards #1009.
- **D7 — M2 mechanism is an OPEN fork, to be settled by an architect pass before M2 is
  implemented (surfaced by PRD review 2026-09-03, not yet gated).** Option A
  (broker-side overlay synthesis) vs Option B (reuse ADR-0456's `alignBranchWithDefault`
  worker-side at checkpoint time). ADR-0456 is the authority on this gap and was NOT
  cited in the first draft; it already built the align tooling for the finalize path and
  deferred exactly this mid-run case (its D5). Option A's edge: server-side,
  worktree-free, never perturbs the live runner clone. Option B's edge: proven
  lower-risk machinery, no new on-forge object contract, no #1009-reproduction surface.
  D2/D3 apply only if Option A is chosen. Independent of the fork, M2 inherits ADR-0456's
  "do not bet on a single GitHub rule" stance (its D2): `isWorkflowScopeRejection` +
  clean-skip fallback if the aligned/overlaid push is still rejected.
