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

- **M1 (#1059)** — **MERGED** 2026-09-03 (PR #1068 → main `92fba2e1`). Adopt-seam unified
  on the owner anchor across BOTH not-ownedHere legs; agent-side only (`agent/src/git.ts`
  + `agent/test/git-cross-worker-recovery.test.ts` + `agent/test/git.test.ts`), no
  server/schema/migration change. `adr/0628` amended, `specs/ai.md` §609 added.
- **M2 (#1036)** — design FINALIZED (D7 resolved: Option B, worker-side temp-index
  `.github/workflows` overlay; `agent/src/git.ts`-only, broker untouched). Ready to
  implement now that M1 merged. Rebases on M1's adopt block.
- **M3 (#1037)** — designed, server-only (`service.go`; worker adoption plumbing already
  threads `checkpoint_tip`+`resume` for all kinds — see M3). File-disjoint from M2 under
  Option B, so **M2 ‖ M3 can run in parallel** after M1.

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

**Files**: **`agent/src/git.ts` only** (overlay synthesis in `checkpointPack` + overlay
peel in `runnerCloneForBranch`) + agent tests. **No api/broker/schema change** —
`pushbroker.go` and `service.go` `Publish` are byte-untouched. Own MR (edits the
just-settled M1 adopt seam). No schema change.

**Prior art — ADR-0456 is the authority on this gap and must be read first.**
`adr/0456-rebase-before-finalize-push.md` already: (a) confirms M2's premise (the
rejection is a tip-vs-default `.github/workflows/` tree comparison, not per-commit);
(b) explicitly names *this* problem — mid-run checkpoint coverage for a
behind-on-workflows branch — as its own **deferred** follow-up (its D5); and (c) its
**#627 amendment already ships a `.github/workflows` subtree overlay** for the
*finalize* path: `alignBranchWithDefault(…, "workflow-subtree")` (`git.ts:1498/1516`,
the narrow primary strategy described at `git.ts:1493`), gated by `workflowTreeDiffers`
(`git.ts:1443`) with `fetchDefaultTip` (`git.ts:1394`), plus `isWorkflowScopeRejection`
and the `pushbroker.ErrWorkflowScopeRejected` → benign-skip mapping. M2 is the mid-run
sibling of that finalize overlay, and closes 0456's deferred D5 gap.

**Decision (D7, resolved 2026-09-03): Option B — a worker-side `.github/workflows`
subtree overlay built via a TEMP INDEX (no worktree), producing a genuine fast-forward
commit the UNCHANGED broker pushes and adoption peels as a transport wrapper.** Rejected:
Option A (broker-side go-git synthesis) — it added object synthesis AND a
strict-descendant/never-forced rework to the secrets-holding `pushbroker`, the exact
#1009 guard. Also rejected: the throwaway-*worktree* form of Option B — `alignBranchWithDefault`
runs working-tree ops (`checkout`/`reset --hard`/`clean`/`checkout <ref> -- …`) which,
worker-side, would fire attacker-chosen `.gitattributes` filter/smudge drivers **as the
PAT holder** (the code-exec vector the bare-only invariant at `git.ts:36-50` closes) and,
in the live clone, `reset --hard` away the agent's in-flight work. The temp-index
plumbing (`read-tree`/`rm --cached`/`read-tree --prefix`/`write-tree`/`commit-tree`) is
pure object-DB work: it never populates a working tree, fires no filter drivers, and
never touches the live runner clone — so it matches Option A's perturbation-freedom while
leaving the broker and its never-forced invariant byte-untouched. See D7 for the full
rationale (blast radius / #1009 surface / reuse / perturbation).

**No merge or rebase mid-run.** Rebase rewrites SHAs → diverges the checkpoint from
`refs/uzi-runner/<branch>` and the live session → fails M1's owner-anchor and orphans the
tracking ref (ADR-0456 D2's SHA-rewrite hazards). Merge FF-composes but adds a needless
merge commit to a transport wrapper. Only the **conflict-free overlay** is used: it is a
fast-forward, so `checkpoint_tip` = the declared overlay tip (a real commit) and M1's
`ownerMatch` (`git.ts:592-596`) matches it trivially on resume; adoption peels the
wrapper.

**No-single-bet fallback (inherited from ADR-0456 D2).** A branch that itself MODIFIED
`.github/workflows/**` fails the overlay gate and is doomed at finalize regardless (#377),
so mid-run it **skips cleanly** (`skipped:"workflow_scope"`, today's behaviour) — never a
merge/rebase, never a 5xx, never a failed run. "Swapping the workflows subtree satisfies
the check" is the expected case, not a guarantee; a still-rejected push falls back to the
clean skip.

**Overlay mechanics (Option B, all in `agent/src/git.ts`).** Unused legacy paragraph
retained below only for contrast; the authoritative mechanics are §2a/§2b here:

*2a — build the overlay in `checkpointPack` (`git.ts:1144`), worker bare, temp index.*
Before packing `<branch>`: resolve `realTip` (tracking tip, `git.ts:1113`) and `defaultTip`
(`fetchDefaultTip`, `git.ts:1394`). **Gate (reuse #627's exactly):** overlay only when
`workflowTreeDiffers(realTip, defaultTip)` (`git.ts:1443`) is TRUE **and** the branch did
NOT itself modify a workflow file (`changedFiles` filtered to `.github/workflows/**`,
mirroring `runner.ts:1240`); gate fails ⇒ ship `realTip` unchanged (→ clean skip; #377
owns a workflow-modifying branch at finalize). Synthesize via temp index (no worktree, no
filter drivers): `GIT_INDEX_FILE=<tmp> git read-tree <realTip>`; `git rm --cached -r
--ignore-unmatch .github/workflows` (**`--ignore-unmatch` is load-bearing, not cosmetic**:
against a temp index with no worktree, `git rm --cached` exits non-zero when no path
matches, so without it the no-`.github` / already-deleted-workflows edges would ERROR
instead of being no-ops — same flag `alignBranchWithDefault` carries at `git.ts:1530`); if
default has a workflows tree, `git read-tree --prefix=.github/workflows
<defaultTip>:.github/workflows` (empty ⇒ rm-only, the #627 deleted-workflows edge); `newRoot = git write-tree`; if `newRoot == realTip^{tree}` ⇒ not
behind ⇒ ship `realTip`; else `O_ov = git commit-tree newRoot -p realTip [-p prevOverlay]
-m "ckpt(overlay): …"` with **DETERMINISTIC identity + committer date** (= realTip's,
fixed identity, `gpgsign=false`) so a no-new-work rebuild yields the same OID and the
broker's "already up to date" early-return still fires. **Parent chaining:** read the
current `refs/uzi-checkpoints/<branch>` tip; if it exists and is NOT an ancestor of
`realTip` (a prior overlay sibling), add it as `parent[1]`, so `O_ov` strictly descends
the current ref and the broker's existing `strictDescends`/wire-CAS accept it as a FF with
**zero broker changes**. Return `{ tipOid: O_ov, pack }` (pack = `O_ov ^floor`; default's
workflow blobs excluded, already at origin).

*2b — peel the overlay at adoption (`runnerCloneForBranch`, composes with M1).* After
`baseSha` resolves and BEFORE `adoptedMarker` (`git.ts:669`): if `baseSha`'s subject
starts `ckpt(overlay):` (a new `isOverlayMarker` mirroring `isWipParkMarker` `git.ts:1832`),
re-point `baseSha = baseSha^1` (its underlying real tip — **DISCARD** the overlay tree; it
is a transport wrapper, never branch content). Then the EXISTING `adoptedMarker` / wip-park
`reset --soft` logic runs on the peeled `baseSha`, so a `wip(park)` marker under an overlay
is still handled. **Order: overlay (outermost) → wip-park (inner); and the tree semantics
DIFFER and that is the whole point — overlay = discard (re-point base), wip-park = keep
(soft-reset).** A naive "two identical peels" keeps `O_ov`'s swapped `.github` staged and
re-triggers workflow-scope at finalize (breaking #377's "agent branch never contains the
wrapper"). `checkpoint_tip` = `O_ov` matches the mirrored ref so M1's `ownerMatch` adopts
it; the peel lands the agent on `realTip` exactly where it parked.

*2c — what does NOT change:* `pushbroker.go` (O_ov is a genuine FF descendant; all
invariants hold), `service.go` `Publish` (persists the declared `tipOid` = O_ov via
`SetRunCheckpointTip` as today — no `Result.Tip`), CAS-delete (`ExpectedOldTip` =
`checkpoint_tip` = O_ov = origin's ref).

<details><summary>Superseded Option-A mechanics (kept for contrast; NOT the chosen design)</summary>

**Overlay mechanics (Option A — REJECTED).** When the tip's `.github` tree entry differs from the
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

**ADR (Option A — REJECTED)**: would have keyed `1036` for a broker-side overlay object
contract. Superseded by the Option-B ADR below.

</details>

**ADR**: a new ADR keyed `1036` for the **worker-side transport-wrapper contract** — the
checkpoint ref may carry `ckpt(overlay):` commits; the worker builds them via a temp index
in `checkpointPack`; adoption peels them (discard the overlay tree, re-point to `O^1`)
before the wip-park soft-reset; the broker is deliberately NOT involved. Cross-links
ADR-0456 (its #627 finalize sibling), ADR-0122, and ADR-0628. Written by the M2
implementation run in its own MR.

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

**M1 first; then M2 and M3 in PARALLEL** (Option B made them file-disjoint):

- **M1 → M2** is a real dependency: M2's overlay peel edits the same `git.ts` adopt block
  M1 just settled, so M2 rebases on M1 (merged).
- **M2 ‖ M3** — under Option B, M2 is `git.ts`-only (overlay synthesis in `checkpointPack`
  + peel in `runnerCloneForBranch`) and M3 is `service.go`-only (kind gate + branch
  derivation, plus optional `sdk-executor.ts`). No shared file, no shared surface, so they
  can run concurrently after M1. The old "both edit `service.go` `Publish`" coupling is
  gone (M2 no longer touches the broker or `Publish`), and M2 is no longer "the dangerous
  milestone" — its blast radius is now the just-settled adopt seam, not the secrets holder.
- M3's per-kind branch-derivation helper and its differential test are independently
  authorable at any time (file-disjoint from M1/M2).

Each milestone is a separate MR. M2 is still its own MR because it edits the delicate adopt
seam, not because of broker blast radius (which no longer applies under Option B).

## Success criteria

1. **M1.** A fresh run (`resume=false`, NULL `checkpoint_tip`) does NOT adopt a
   pre-existing `refs/uzi-checkpoints/<branch>`, even one that strictly descends the
   floor; it seeds off the origin/default floor and logs loudly. Proven by a test that
   fails on the unfixed code.
2. **M1.** A legitimate same-run resume still adopts (tip matches). #759 foreign-WIP
   cherry-pick recovery and #1030 M3 resume recovery are proven intact by the rewritten
   tests.
3. **M2 (Option B).** On a branch behind `main` on `.github/workflows` (and that did NOT
   itself modify a workflow file), the worker builds `O_ov` — a FF commit whose
   `.github/workflows` tree equals the default's — via a temp index, and the **unchanged
   broker** pushes it (no `skipped:"workflow_scope"`). `runs.checkpoint_tip` = the declared
   `O_ov` (a real, mirrored, deterministically re-derivable commit), so M1's owner-anchor
   matches on resume; adoption **discards** the overlay tree (re-point to `O_ov^1`) then
   soft-resets any wip-park marker, landing on `realTip` with **no staged `.github` diff**.
   A **second sequential overlay** carries `parent[1] = O_ov_prev`, so it is a genuine FF
   descendant the broker accepts (NOT skipped `not_descendant`). A branch that MODIFIED
   workflows, or a still-rejected push, **skips cleanly** (`skipped:"workflow_scope"`, never
   5xx/failed) — the no-single-bet fallback. `pushbroker.go` and `service.go` are proven
   byte-unchanged. Proven by the agent fixture set (Contract B).
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
- **Contract B (M2, Option B): the overlay object shape.** An **agent** differential/fixture
  test (real git, the existing cross-worker harness — NOT a pushbroker fixture, since the
  broker is untouched). A golden snapshot from only the "behind" case would lock in the
  blind spot, so author all of, asserting each exercises its branch:
  - **behind, not workflow-modified** ⇒ `O_ov` built, its `.github/workflows` tree ==
    default's, `parent[0]` == realTip; adoption peels to `realTip` with no staged diff;
  - **not-behind** (discriminating negative) ⇒ no overlay, ships `realTip`;
  - **workflow-MODIFIED** ⇒ no overlay, clean skip (the gate holds);
  - **stacked** (a `wip(park)` tip also behind) ⇒ peel overlay (discard) then soft-reset
    the marker, landing on `realParent` with the WIP uncommitted;
  - **second sequential overlay** ⇒ `O_ov2` with `parent[1] = O_ov_prev` is a FF descendant
    the (unchanged) broker accepts — NOT skipped `not_descendant`; the single-publish
    fixtures miss this;
  - **edge** (default deleted its workflows; realTip has no `.github`) ⇒ rm-only / ship
    `realTip`, never throw.

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
- **D2 — VOID under Option B (superseded by D7).** Under the chosen Option B the
  checkpoint ref carries a real, worker-declared `O_ov` commit, so `runs.checkpoint_tip`
  stores the declared tip exactly as today; the owner-anchor and CAS-delete compare against
  it unchanged, and adoption peels it. There is no broker `Result.Tip`/persist change and
  no realTip-vs-O contract, so the #1009-reproduction risk D2 guarded against does not
  arise (the broker is untouched). *(D2 was gated with the user 2026-09-03 assuming the
  Option-A overlay; the better Option-B design makes it moot.)*
- **D3 — overlay swaps at `.github/workflows` (M2, Option B).** Matches #627's finalize
  overlay exactly, via a temp-index `read-tree --prefix`, worker-side (not the coarser
  `.github` the rejected Option-A draft chose, and not broker-side). Uses only trees the
  worker/default fetch already hold.
- **D4 — M3 ships `self_improve` only; hard-kill cadence is a residual.** Server
  enablement covers graceful interrupts (park/drain/cordon-roll), the dominant hosted
  case. (Gated with the user 2026-09-03.)
- **D5 — ADRs: extend `adr/0628` for M1; new ADR `1036` for M2's worker-side
  transport-wrapper contract; none for M3.** (Gated with the user 2026-09-03; the `1036`
  content changed from a broker-overlay contract to the worker-side one per D7.)
- **D6 — separate MRs; M1 first, then M2 ‖ M3.** Under Option B M2 (`git.ts`-only) and M3
  (`service.go`-only) are file-disjoint and run in parallel after M1. Each is its own MR;
  M2's is isolated because it edits the delicate adopt seam, no longer because of broker
  blast radius (which Option B removes).
- **D7 — RESOLVED 2026-09-03: Option B (worker-side temp-index `.github/workflows`
  overlay).** Decided by an architect pass after the initial fork. The worker builds a
  legitimate FF overlay commit via a temp index (`read-tree`/`rm --cached`/`read-tree
  --prefix`/`write-tree`/`commit-tree` — no worktree, no filter drivers, never touches the
  live runner clone), the UNCHANGED broker pushes it, and adoption peels it as a transport
  wrapper. Chosen over **Option A** (broker-side go-git synthesis) — which added synthesis
  AND a strict-descendant/never-forced rework to the secrets-holding `pushbroker`, the exact
  #1009 guard — on all four axes (blast radius, #1009 surface, reuse of #627's shipped
  `workflow-subtree` overlay, perturbation-neutral). Also rejected: the throwaway-*worktree*
  form of Option B — working-tree ops fire attacker-chosen `.gitattributes` filter drivers
  as the PAT holder (the `git.ts:36-50` bare-only invariant) and would `reset --hard` the
  live clone. Composition: NO merge/rebase mid-run (rebase rewrites SHAs → fails the M1
  owner-anchor; merge is a needless commit) — only the conflict-free FF overlay; a
  workflow-MODIFIED branch or a still-rejected push skips cleanly (ADR-0456 D2's
  no-single-bet stance). This makes D2 void, D3 `.github/workflows`, and M2 ‖ M3 parallel.
