# ADR-1416: a rewritten published branch is steered, then bridged, never force-pushed

**Status**: Accepted (PRD #1416 M1-M5 committed on this branch; M6 is this documentation
pass; M7, lead-owned hosted acceptance, is post-merge and not shipped in this work).
**Date**: 2026-09-17
**Issue**: [vtmocanu/uzi#1416](https://github.com/vtmocanu/uzi/issues/1416)
**PRD**: [prds/1416-published-branch-rewrite.md](../prds/1416-published-branch-rewrite.md) —
carries the full Decision Log (D1-D10), the milestone breakdown, the resolved facts and the
verified code anchors; this ADR restates the seams a future change must not silently break.

## Context

A run's branch is frequently **already published** on the forge before the agent starts: a
`uzi handoff` task branch, an MR being reworked, a resumed run, a self_improve branch. uzi
lands the agent's work with a **plain fast-forward push and never force-pushes** — force is
denied by the worker's guardrails and by the bot's role, by design (see
[docs/github-bot-setup.md](../docs/github-bot-setup.md)). Before this PRD, nothing told the
agent about that published tip, nothing detected a history-rewriting operation (`rebase`,
`commit --amend`, `reset --hard`, `filter-repo`/`filter-branch`) until the doomed push at
finalize, and a park after such a rewrite could silently set the rewritten commits aside in
favor of the still-published tip. A real run (`52fe0e79`) hit exactly this and failed for the
wrong reason (`finalize_base_align_conflict`) with recovery costing a human session. This ADR
records the durable shape that closes those gaps: name the floor, steer the agent, and — when
steering isn't enough — bridge the rewritten history back onto its published floor instead of
failing the run.

## Decision

### D1 — Detection is worker-local, bare-only, at the fetch-back seam

Every ancestry check runs in the worker's own bare repository, against refs that already live
there — the published floor, the checkpoint floor, and the tip written by `fetchAgentBranch`
(the one seam every checkpoint fetch-back, park, capture, and finalize path already goes
through). One `merge-base --is-ancestor` per fetch-back costs nothing and bounds detection to
`CHECKPOINT_INTERVAL`. A same-turn hook was rejected: Codex ships no hooks at all, so a
PostToolUse hook would be Claude-only, and it would still need a runner callback out of the SDK
process that the fetch-back seam already gives for free.

### D2 — Steer, never deny

No git subcommand is denied. A rewrite wholly above the published floor is valid and routine,
and command parsing cannot prove which range a `rebase` touches without replicating the
ancestry check anyway. But "steer only" is not sufficient by itself: a same-run reseed after a
park adopts the origin tip over a diverged tracking ref, and a rewrite immediately followed by
`signal_done` skips every checkpoint between the rewrite and finalize. Unresolved divergence
must never reach a release or a push — that is what the bridge (D4) guarantees on top of the
steer.

### D3 — A distinct worker-authoritative steer channel, isolated from follow-ups

The safety steer is **not** a follow-up. Follow-ups carry real server input ids, advance the
`open_followup_id` wake-guard watermark, and render as **untrusted** `<follow_up>` user text;
the `#1247` credential-switch trip aborts the turn and releases the claim outright. Neither
semantic fits a worker-originated correction. Implementation: `SteeringChannel` carries a
`safetySteer` slot (`pushSafetySteer` / `pullSafetySteer`) that sits deliberately outside
`route()`, the follow-up watermark, `hasPendingFollowUpOutcome()`, and `awaitFollowUp()`. Both
executors drain it with priority at their own loop top — the SDK executor before building the
next implement prompt and before its follow-up drain, the Codex executor at its loop top — and
render it as worker guidance outside the `<follow_up>` fence, without the "never as
instructions" framing that wraps untrusted text. A future steering addition must keep this
isolation: synthesising a steer onto the follow-up channel, or gating it behind the wake-guard
watermark, would corrupt the semantics both channels already promise their own callers.

### D4 — Bridge, do not fail, when history diverged (the bridge invariant)

When ancestry of the published floor P (or the checkpoint floor C) against the current tip H is
`divergent`, the worker builds a synthesized commit **B** with `git commit-tree`: B's tree is H's
tree **byte-for-byte unchanged**, and B's parents are H first (so `git log --first-parent` still
reads as the agent's own history) followed by each floor not already reachable from H, in order.
Because P remains an ancestor of B, the branch **fast-forwards from P without a force-push** and
loses nothing. This is applied at every publication boundary: before `captureRecoveryRestorePoint`
and `captureHoldContext`, before every release path (limit park, shutdown, owner pause), at the
mid-run checkpoint tick, at finalize before the `#377` workflow-scope guard and the align chain,
and after a successful align **rebase** fallback (which itself rewrites P). `unknown` (a
transient or unresolvable ancestry read) never bridges and never fails the run — only a
**definitive** `divergent`/mismatch triggers a bridge attempt, and only a **definitive**
malformed B (tree differs from H's, or P/H provably not an ancestor of B) counts as a bridge
failure; a transient/unresolvable validation read leaves the tracking ref at H and tries again
later.

Two invariants harden this beyond the original plan, both added in review and load-bearing for
any future change to `bridgeToFloors`:

- **Idempotent re-bridging.** An additional floor (index > 0, e.g. checkpoint floor C when C is
  itself a prior bridge) whose tree already equals H's tree contributes no new content and is
  skipped as a parent — re-bridging the *same* divergent H yields the *same* commit OID, so an
  idle checkpoint tick that finds nothing new produces no history growth. The published floor P
  (`floors[0]`) is never skipped on this basis: it must remain an ancestor of every B even when
  its tree happens to coincide with H's.
- **C never regresses.** The checkpoint floor C only ever advances to a commit at least as
  durable as its previous value: on a bridged tick it advances to B (never back down to the
  raw, unbridged H); on an unbridged confirmed publish it advances to the confirmed tip. A
  future change to the checkpoint-advance site must preserve this — regressing C back to H after
  a bridge silently reopens the exact divergence the bridge just closed.

A bridged branch's MR carries one generic sentence noting the bridge (rendered from a `bridged`
boolean computed from **pushed history**, not a flight-local flag — see D-bridge-note below), and
`git log --first-parent` on the pushed branch stays readable as the agent's intended history
regardless. `history_rewritten` (D6) is reserved for a bridge that cannot be built or validated;
a *valid* B rejected non-fast-forward is concurrent branch movement (the remote moved again under
the run) and keeps its own, pre-existing classification.

### D-bridge-note — the MR bridge-note is derived from pushed history, not run state

Whether an MR body gets the "this branch contains a history bridge" sentence is decided by
`rangeContainsBridge(bare, publishedTip, pushedTip)`, which walks `git log --first-parent
<publishedTip>..<pushedTip>` and accepts **exactly two** fully-validated commit shapes — nothing
else counts as a bridge:

1. **A worker bridge**: the commit carries `bridgeToFloors`'s deterministic marker message
   (`bridge: restore published tip <P> as an ancestor of <H> (tree unchanged)`), and *every* one
   of the following holds: the marker's `P` equals the run's actual published floor; the marker's
   `H` equals the commit's own first parent; the commit's tree equals its first parent's tree;
   between 1 and a bounded number of extra parents (a real bridge has at most two: P and C); every
   extra parent descends from the published floor; and at least one extra parent is not already an
   ancestor of the first parent. A marker present with any check failing is rejected outright —
   never re-checked against the second, markerless shape.
2. **An agent safety bridge**: no marker (the M2 steer told the agent to run
   `git merge -s ours <P>` by hand), accepted only when the commit's tree equals its first
   parent's tree *and* it has exactly one extra parent, which is exactly the published floor.

This is deliberately history-derived rather than flight-state-derived: a bridge built before a
park is invisible to a reclaimed run's in-memory state, but it is right there in the pushed
history, so a reclaim's finalize (which builds no new bridge itself) still reports the note
correctly. `--first-parent` plus full structural validation (not a bare regex over the commit
message) is load-bearing: an ordinary `git merge <default>` produces a merge whose tree *differs*
from its first parent, and its side history could otherwise be walked or spoofed into satisfying
a looser check. The parser reads `git log -z` with `%H%x00%T%x00%P%x00%B` and splits strictly on
NUL — the one byte git forbids inside a commit message — specifically so a crafted commit body
cannot inject a fake field or record separator and desynchronize the four-fields-per-record
parse. A future change to the marker format or to what counts as a valid bridge shape must update
`rangeContainsBridge` and `bridgeToFloors` together; they are two sides of one contract.

### D5 — The plan-gate nudge is a regex over `plan_md`, warn-only, false positives accepted

`gatePlan` matches the submitted plan text against `git rebase`, `--amend`,
`filter-repo`/`filter-branch`, `reset --hard`, and a forced push, and — only when the run's
branch has a published floor — emits one `status` line before the plan is posted, in both the
human-gated and auto-approve paths. On an auto-approved run there is no human to revise the
plan, so the M2 safety steer is additionally armed for the first implement turn. This is
warn-only: the gate's verdict flow is completely untouched, and a false positive (a plan that
merely says "do not rebase") is an accepted cost — a false negative is still caught downstream
by detection (D1/D2) and the bridge (D4).

### D6 — A new fail origin, not a reworded existing one (the `history_rewritten` class)

`finalize_base_align_conflict` describes a factually different situation (a moving default
branch, not a rewritten agent history) and `agent_failure` is the generic catch-all the dashboard
and the judge triage against; reusing either would misclassify this failure. `history_rewritten`
is added to both `failOrigins` and `workerReportableFailOrigins` (judge-eligible — this is an
agent defect) and widens `runs_fail_origin_check` via the established DROP+ADD migration
template. It fires **only** when a bridge cannot be built or validated at a finalize sink — never
from the generic catch, never as `finalize_base_align_conflict` — and it names the published tip,
states plainly that uzi never force-pushes, and points at
[docs/github-bot-setup.md](../docs/github-bot-setup.md).

Two precedence/security rules other code must preserve:

- **`push_secret_blocked` keeps precedence.** A bridge failure throws before any push is
  attempted, so the two can never race in practice, but the ordering is deliberate: a detected
  secret must never be reported as merely a rewritten-history failure.
- **`preserved_patch` is gated on secret-scan trust.** The pre-push secret-scan floor fails
  *open* on a divergent tip (a diverged remote is not a trustworthy floor to diff against), so an
  `history_rewritten` failure attaches a diff **only** when the scan that ran on the pushable
  range was itself trustworthy; an untrusted-range failure omits the patch outright rather than
  risk writing an unscanned secret into a stored or displayed diff. The work is not lost in that
  case — it stays recoverable from the run's branch or the durable capture (`uzi run export`).

### D7 — The fact is a worker-verified SHA, never text the agent infers

The published floor P (`RunFlight.publishedTip`, recorded once at claim from
`originBranchTip`) and the checkpoint floor C are always rendered into a prompt as a
worker-verified 40-hex OID, the same rule the existing `priorCommits`/resume-notice facts
already follow. The default branch's *name* is repo-controlled text and is never rendered; only
its worker-verified SHA (`defaultBranchCommit`) is.

### D8 — No protocol change

Every mechanism here — the floors, the tri-state ancestry check, the safety-steer channel, the
bridge, the plan-gate nudge, and the new fail origin — is worker-local, over data the worker
already holds or a field (`fail_origin`) the terminal report already carries. `agent/src/protocol.ts`
and `agent/src/client.ts` are untouched by this PRD, which keeps it clear of the concurrent
`#1247`/`#1390`/`#1393` work, all of which do edit those files.

### D9 — `--force-with-lease` scoped to `uzi/task/<id>` was rejected

Not because a correctly scoped force-with-lease would ever reach `main` — it would not — but
because it would add a genuinely destructive publication path to the worker, the
single-writer-namespace assumption it would lean on does not hold for mr_rework, resumed-issue,
or self_improve branches, and the bridge (D4) already makes force unnecessary: nothing is lost by
bridging instead. Revisit only if hosted acceptance (M7) shows a bridged, doubled-lineage history
is unacceptable to reviewers in practice.

### The P/C floors — recorded once, advanced only forward

**P** (`RunFlight.publishedTip`) is the branch's forge tip at claim time
(`originBranchTip(bare, branch)`), read **once**, from runner-level flight state that survives an
executor restart — never from `RunnerClone.baseCommit`, which can point at unpublished recovered
work. **C** (`RunFlight.checkpointFloor`) starts equal to P and advances only to a floor at least
as durable: to a confirmed checkpoint's tip on an ordinary publish, or to a bridge B when that
tick bridged (never regressing back to the raw H — see D4's "C never regresses" invariant). Both
floors key on **P being non-null**, never on run kind: any run whose branch already existed on the
forge at claim — task, mr_rework, self_improve, a resumed issue run — has a published floor and
gets the prompt fact, the ancestry checks, and the bridge; a fresh issue run has none of them.
`refs/uzi-checkpoints/<branch>` (uzi's own transport ref) is never itself a published floor.

## Consequences

- **A rewrite on a published branch no longer loses the run.** It is detected within one
  checkpoint interval, the agent is steered to self-correct, and even an ignored steer completes
  through the finalize bridge — the failure this PRD closes (`52fe0e79`'s
  `finalize_base_align_conflict`/generic-catch outcome) becomes a completed run with a one-line MR
  note instead.
- **A bridged branch's commit graph carries a visible extra parent** until a maintainer's
  squash-merge collapses it; `--first-parent` history stays clean throughout. This is a deliberate,
  documented tradeoff (D4/D9), not an artifact to "fix" by adding force capability.
- **The safety-steer channel is now a permanent third input surface** alongside follow-ups and the
  credential-switch trip. A future addition to either of those two mechanisms (a new follow-up
  shape, a new credential-switch outcome) must not accidentally widen into the safety-steer slot's
  isolation, and vice versa — a future safety-steer use case must not start consulting the
  follow-up watermark just because it is convenient.
- **The bridge-note detector is the sole authority on whether an MR mentions a bridge**, and it is
  intentionally strict: a change that makes a legitimate bridge fail its structural checks (a
  reordered marker field, a changed extra-parent bound) silently drops the MR note rather than
  erroring, so a future change to `bridgeToFloors`'s marker format must be paired with the matching
  change to `rangeContainsBridge` in the same commit.
- **A mixed-history repo is expected, not exceptional.** Any run with a published floor can be
  bridged at any publication boundary; a reviewer or tool that assumes a run's branch has a single
  linear history reaching back to its published tip must account for an extra-parent bridge commit
  instead.
- **`history_rewritten` joins the closed fail-origin vocabulary permanently**, judge-eligible like
  any other agent defect, and its `preserved_patch` is not a blanket guarantee — a reader of a
  failed run's card must not assume a diff is always attached; its absence there specifically means
  the scan floor could not be trusted, not that nothing was captured.
