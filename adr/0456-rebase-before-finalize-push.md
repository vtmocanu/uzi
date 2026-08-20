# ADR-456: Align a behind-on-workflows GitHub branch before the finalize push, merge-first with a rebase fallback

**Status**: Accepted (PRD #456 M1/M2/M4 landed)
**Date**: 2026-08-20
**Deciders**: Vlad (maintainer), agent team (architect, coders, reviewers)
**PRD**: [prds/456-rebase-before-finalize-push.md](../prds/456-rebase-before-finalize-push.md) (GitHub issue [vtmocanu/uzi#456](https://github.com/vtmocanu/uzi/issues/456)) — the PRD carries the milestones, the full evidence base (including the #422 PVC recovery), and the Decision Log (D1–D5); this ADR carries the mechanism and the two decisions (D2, D5) most likely to look wrong to a future reader who has not re-derived them.
**Prerequisite**: [PRD #377](../prds/377-early-fail-unpushable-workflow.md) (merged as PR #454), which added `runs.preserved_patch`, the `workflow_scope_missing` `fail_origin`, and the finalize workflow-**modify** guard this fix sits immediately after. This PRD reuses that machinery for a different trigger — see Context.

## Decision (summary)

A run can lose its **entire** branch at the finalize push even when it never touched `.github/workflows/`, because GitHub gates a `repo`-only PAT push on whether the pushed tip's `.github/workflows/` tree **differs from the current default branch** — not on whether the branch itself edited a workflow file. Between #377's workflow-modify guard and the finalize push, for **GitHub runs only**, uzi now:

1. Fetches the default branch's **current** tip from origin (`fetchDefaultTip` — the existing finalize `fetchAgentBranch` is a local `file://` fetch and would otherwise compare against a stale claim-time mirror).
2. Triggers only when the branch's `.github/workflows/` tree actually **differs** from that fresh tip (`workflowTreeDiffers`) — a precise, cheap predicate that keeps the align/conflict surface to the runs that need it.
3. Aligns the branch in the runner clone (never the bare — B3): tries a **SHA-preserving merge** first; if the merged push is **still** rejected for workflow scope, falls back to a **rebase** (the mechanism proven in the #422 manual recovery). See D2 for why merge is tried first.
4. On an unresolvable conflict (merge and rebase both fail to auto-resolve), aborts, fails the run with a new typed `fail_origin = finalize_base_align_conflict`, and preserves the pre-align diff in `runs.preserved_patch` (the column #377 added) for a human to rebase-and-land.
5. Server-side, the checkpoint broker (`pushbroker`) now recognizes the same rejection on `refs/uzi-checkpoints/<branch>` and **skips the push cleanly** (`ErrWorkflowScopeRejected` → a benign skip, like `ErrNotDescendant`) instead of erroring every checkpoint. See D5 for why it cannot do more than skip.

## Context

uzi's GitHub bot PAT is deliberately scoped to exactly `repo`, without `workflow` (privcheck forbids the pair as over-privilege). GitHub enforces the `workflow` scope requirement **on the pushed tip**, atomically: if `.github/workflows/**` differs between the pushed branch and the current default, the whole push is rejected —

```
! [remote rejected] refs/uzi-runner/agent/issue-<n> -> agent/issue-<n>
  (refusing to allow a Personal Access Token to create or update workflow
   .github/workflows/<name>.yml without workflow scope)
```

— **even when the branch's own commits never touched a workflow file.** #377 already handles the case where the branch *itself* modifies `.github/workflows/**` (fail early, typed, diff preserved). It does not fire here, because the diff this PRD's runs hit is zero on a per-commit basis: the branch is merely **behind** main's workflow files, because main advanced them after the run's clone base.

Observed 2026-08-20: two runs (#422 and #377's own first attempt) died this way, neither having touched a workflow file. A renovate batch had updated six workflow files on main after the runs' clone base. The #422 branch, recovered from the worker PVC, had an empty net workflow diff and no per-commit workflow change — **rebasing it onto current main cleared the rejection**, with no other change. This ADR's fix automates that manual recovery.

Checkpoints hit the identical rejection on `refs/uzi-checkpoints/<branch>` (confirmed in the api error log for the same date), so a behind-on-workflows run got no mid-run safety net either, before this fix.

## The decisions

### D2 — Merge first (SHA-preserving), rebase as the empirically-proven fallback

The rejection is a **tip-vs-default** comparison, not a per-commit one: a branch with zero per-commit workflow change is still rejected purely because its tip's `.github/workflows/` tree differs from main's. Under that mechanism, a SHA-preserving **merge** of the fresh default into the branch — which makes the tip's workflow tree equal main's while leaving every original commit SHA intact — should clear the rejection just as well as a rebase, and it avoids every SHA-rewrite hazard a rebase carries (an invalidated worker tracking ref, invalidated recorded commit SHAs, divergence from an already-published checkpoint).

**Only the rebase was actually proven to work** — the #422 manual recovery rebased, not merged, onto current main. A merge was never tested against GitHub's real behavior. An early draft of this fix reasoned from a **per-commit** view of the rejection ("the branch's own commits never touched a workflow file, so only a history rewrite can clear it") and concluded a merge would not help — but that reasoning is inconsistent with the observed evidence: the rejection already fires against a branch with a genuinely empty per-commit workflow diff, which only makes sense as a tip-vs-default rule, not a per-commit one. The PRD's Decision Log (D2) corrects that earlier "rebase, never merge" conclusion.

Because we cannot settle GitHub's exact rule from inside an offline worker, the implementation is a **runtime strategy robust to either mechanism**, not a bet on one: try the SHA-preserving merge first (`alignBranchWithDefault(..., "merge")`); if the merged push is *still* rejected for workflow scope (`isWorkflowScopeRejection`), fall back to the rebase that the #422 recovery proved clears it (`alignBranchWithDefault(..., "rebase")`, rewinding to the original agent tip first so the rebase replays the original commits rather than the merge commit). Either strategy landing satisfies the fix; neither landing routes to the conflict-and-preserve path (below).

The rebase fallback asserts its own commit count is preserved across the replay (`--empty=keep --no-autosquash --reapply-cherry-picks`, S3 in the PRD) — a rebase that silently drops a commit must throw rather than push truncated work.

### On an unresolvable conflict: fail typed, preserve the diff, never guess

If neither the merge nor the rebase can auto-resolve, the run must not silently push a half-aligned tree or discard the agent's work. It aborts the in-progress git operation, fails the run with `fail_origin = finalize_base_align_conflict` (widened onto `runs_fail_origin_check` by migration `00139`, alongside #377's `workflow_scope_missing`), and preserves the **pre-align** diff via `runs.preserved_patch` — the exact column and failed-run-card render #377 already shipped. This is deliberately the same shape as #377's outcome (D3 in the PRD): a conflicting align is routed through machinery that already exists rather than adding a second preservation path.

### D5 — Checkpoints stay best-effort; no server-side base-refresh

`pushbroker` (ADR-0122) is fast-forward-only by construction (a strict-descendant check, never a forced push), fetches its base at depth 1 (a deep fetch previously OOM-killed the api), and has **no worktree at all** — it operates purely on an in-memory `go-git` repository reconstructed from the worker's delta pack. None of a merge, a rebase, or any other history rewrite is available to it: there is no working tree to run one in, and a deep fetch of the base to attempt one is the exact operation that was removed for memory safety.

So a checkpoint push that hits the workflow-scope rejection can only be **detected and skipped cleanly** — `pushbroker.ErrWorkflowScopeRejected` is mapped to a benign `Skipped: "workflow_scope"` result in `workersvc.Service`'s publish switch, the same shape as `ErrNotDescendant`, never surfaced as a 5xx and never failing the run. This closes the *error-every-checkpoint* symptom but does not close the underlying gap: a behind-on-workflows run gets no mid-run checkpoint coverage, and its work is protected only by the finalize align (M1) landing at the end. That gap is accepted, not solved, here — a periodic/early-align follow-up that keeps a run's branch aligned with main throughout its life (closing the gap at the cost of more frequent align work and conflict exposure) is deferred in the PRD as optional, because the finalize fix already prevents the data loss that motivated this work.

## Consequences

- A run whose clone base predates a workflow-file change on main, and which never itself touches a workflow file, now survives the finalize push — the exact condition that lost runs #422 and #377's first attempt.
- A run whose align genuinely cannot resolve loses no work: it fails typed with its diff on the card, the same manual-recovery shape a human got by hand for #422, now automatic.
- A run already aligned with the current default pays for exactly one extra fetch and no merge/rebase work.
- Checkpoint pushes on a behind-on-workflows run no longer error; they skip silently, and the run's real safety net is the finalize align, not the checkpoint.
- GitHub-only: GitLab and Forgejo impose no equivalent workflow-scope push rule, so this whole path is skipped for those forges.
- This fix, like #377, modifies no file under `.github/workflows/`, so it was safe to hand to a uzi worker under `.claude/rules/prds.md`'s guardrail.

## Alternatives considered

- **Rebase only, no merge attempt.** Rejected per D2: it forgoes the SHA-preserving option for a mechanism only the tip-vs-default evidence, not a per-commit one, actually requires — and it pays the SHA-rewrite hazards (tracking-ref invalidation, checkpoint divergence) on every align, not only the runs where a merge genuinely fails.
- **Widen the bot PAT to include `workflow` scope.** Out of scope by design: the `repo`-only boundary is a deliberate supply-chain guardrail (a worker that could rewrite CI is a bigger risk than a lost run), and privcheck already refuses a `{repo, workflow}` token as over-privilege.
- **A server-side checkpoint base-refresh (rebase/merge inside `pushbroker`).** Rejected per D5: infeasible against the broker's fast-forward-only, shallow-fetch, worktree-less design without reintroducing the deep-fetch OOM risk that shaped it in the first place.
- **A general "keep every branch continuously rebased on main" policy.** Deferred as optional follow-up in the PRD, not adopted here: it would close M4's accepted mid-run gap but adds align/conflict exposure on every run, not only the ones that actually diverge on workflow files, for no benefit beyond what the finalize fix already delivers.
