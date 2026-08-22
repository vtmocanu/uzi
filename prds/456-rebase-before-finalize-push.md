# PRD #456: Align a run's branch with the current default branch before the finalize push

**GitHub Issue**: [#456](https://github.com/vtmocanu/uzi/issues/456)
**Status**: Draft (created 2026-08-20)
**Priority**: High (data-loss: runs lose all committed work)
**Prerequisite (landed)**: [#377](https://github.com/vtmocanu/uzi/issues/377) merged to `main` 2026-08-20 (PR #454). It added the `runs.preserved_patch` column, the `workflow_scope_missing` `fail_origin`, the `runs_fail_origin_check` widening (migration `00137`), the finalize workflow-modify guard in `agent/src/runner.ts` (after the `ci_fix` guard, before `pushBranch`), and the failed-run-card render in `web/src/pages/RunView.tsx`. This PRD **reuses** that machinery. All references below are verified against `origin/main` at or after `ca096926`.
**Related code**:
- `agent/src/runner.ts` — the finalize path. `ensureClone` (claim time, the ONLY origin fetch today) → coding turns → `fetchAgentBranch` (a LOCAL `file://` fetch from the runner clone into the bare's tracking ref, no origin contact) → #377's workflow-modify guard → `pushBranch`. This PRD adds a fresh-fetch + align step between the guard and `pushBranch`.
- `agent/src/git.ts` — `changedFiles`, `pushBranch`, `fetchAgentBranch`, `runnerCloneForBranch` (the runner clone, a real worktree, still present at finalize), `defaultBranchRef` (note its frozen-mirror fallback rungs), and the `refs/uzi-runner/<branch>` tracking ref. The worker is **bare-only by design** and never runs `worktree add` (a load-bearing security invariant); the run's only worktree is the runner clone.
- `api/internal/pushbroker/pushbroker.go` — the server-side checkpoint push to `refs/uzi-checkpoints/<branch>`. **Fast-forward-only by construction**: strict-descendant check + never-forced push, over a depth-1 shallow fetch of the base (the shallow depth is deliberate — a deep fetch OOM-killed the api). This constraint shapes M4.

## Problem

A run can finish every milestone and then lose the **entire** branch at the finalize `git push`, even when it never touched any `.github/workflows/` file.

GitHub gates a PAT push of a branch whose `.github/workflows/` tree **differs from the current default branch** behind the `workflow` token scope, which uzi's bot PAT deliberately lacks (a worker that could rewrite CI is a supply-chain risk; enforced by privcheck). The difference need not be a change the branch **made**: if main's workflow files change **after** the run's clone base, the branch is merely **behind** on them, and the push is rejected atomically:

```
! [remote rejected] refs/uzi-runner/agent/issue-<n> -> agent/issue-<n>
  (refusing to allow a Personal Access Token to create or update workflow
   .github/workflows/brew.yml without workflow scope)
```

The push is atomic, so nothing lands, the worker container is torn down, and the work is unrecoverable through the normal path. The named file is whichever workflow file is **alphabetically first** among those that differ, not necessarily the only one.

**Observed 2026-08-20.** Two runs (#422 and #377's first attempt) died this way; neither touched a workflow file. A renovate batch had updated six workflow files on main (`brew.yml` `actions/checkout` v7, plus `ci.yml`, `e2e.yml`, `kind-smoke.yml`, `main-guard.yml`, `release.yml`) **after** the runs' clone base (`a7a4c4f0`). The #422 branch, recovered from the worker PVC, had 12 commits, an empty net workflow diff, and no per-commit workflow change; **rebasing it onto current main cleared the rejection** with no other change. That manual recovery is what this PRD automates.

**Checkpoints do not save the work either, and this is confirmed, not assumed.** `pushbroker` publishes mid-run checkpoints to `refs/uzi-checkpoints/<branch>` with the same forge PAT. The api error log for 2026-08-20 contains:

```
worker run publish  error: publish: pushbroker: push: command error on
refs/uzi-checkpoints/agent/issue-415: refusing to allow a Personal Access Token
to create or update workflow `.github/workflows/ci.yml` without `workflow` scope
```

So GitHub enforces the workflow-scope check on the `refs/uzi-checkpoints/*` namespace too — a behind-on-workflows branch fails its checkpoint push, which is why such runs leave no recoverable checkpoint ref (M4).

## Solution

Between #377's workflow-modify guard and the finalize `pushBranch`, **make the branch's `.github/workflows/` tree match the current default branch** before pushing, then push as today.

**The mechanism is tip-vs-default, so this is achievable without rewriting SHAs.** The observed rejection fires even though the branch has zero per-commit workflow change, which means GitHub compares the pushed tip's workflow tree against the current default branch, not per-commit. Under that mechanism, **merging the current default branch into the run's branch** (a SHA-preserving operation) makes the tip's workflow tree equal main's and should clear the rejection, while avoiding the SHA-rewrite hazards a rebase carries (tracking-ref rewrite, invalidated recorded commit SHAs, checkpoint divergence). A **rebase** onto main also clears it and is **empirically proven** (the #422 recovery), but rewrites SHAs.

Because we cannot settle GitHub's exact rule from inside an offline worker, M1 is written as a **runtime strategy that is robust to either mechanism**: try the SHA-preserving merge first, and if the push is still rejected for workflow scope, fall back to a rebase (proven to work) — see D2. On an unresolvable merge/rebase conflict, do not lose the work: fail the run with a typed reason and preserve the diff via #377's `preserved_patch` (M2).

Scope of the operation (D1): only when the branch's workflow tree actually **differs** from the freshly fetched default branch — not merely because main moved. This is the precise trigger and keeps the conflict/latency surface minimal.

**This fix modifies no `.github/workflows/` file** (worker/agent code, the `pushbroker` package, tests, docs), so it is safe for a uzi worker to implement per `.claude/rules/prds.md`.

## Milestones

### Committed in this PRD

- [x] **M1 — Align-with-default before the finalize push (the core fix).** In `agent/src/runner.ts`, after #377's workflow-modify guard and before `pushBranch`:
  1. **Fetch the current default tip from origin.** The existing finalize `fetchAgentBranch` is a local `file://` fetch; `refs/remotes/origin/<default>` still holds the **claim-time** tip. Add one fresh authenticated `git fetch origin <default>` so the align target is actually current. (This is network and non-deterministic at runtime; only the *tests* are offline/synthetic.)
  2. **Trigger only on workflow-tree divergence (D1):** if `git diff --name-only <branch-tip> FETCH_HEAD -- .github/workflows/` is empty, skip straight to push (no-op, no added latency beyond the fetch). By this point #377's guard has already handled a branch that *modifies* workflows, so any divergence here is main-ahead-only.
  3. **Align in the runner clone, never the bare (B3).** Operate in `runnerClone.path` (a real worktree, still present at finalize): try `git merge <fresh-default>` first (SHA-preserving). On success, `fetchAgentBranch` the merged tip back into `refs/uzi-runner/<branch>` and push. On a **push rejected for workflow scope**, fall back to `git rebase <fresh-default>` (`--empty=keep`, no autosquash; assert commit count preserved — S3), re-`fetchAgentBranch`, push. Make the fresh default objects reachable in the runner clone (its `--shared` alternate / a targeted fetch) so merge/rebase can resolve.
- [x] **M2 — Graceful conflict handling with diff preservation.** If the merge or the rebase conflicts, abort it, end the run `failed` with a new typed `fail_origin` (`finalize_base_align_conflict`) and a human-readable `failure_reason`, and **preserve the pre-align diff** in `runs.preserved_patch` (the column #377 added), rendered on the failed-run card. **Migration coordination (S5):** the new `fail_origin` migration (number assigned at merge time; live head is `00138` after #422, so `00139+`) must widen the CHECK that **already contains** #377's `workflow_scope_missing` — include both values, add `finalize_base_align_conflict` to `failOrigins` and `workerReportableFailOrigins`, and reconcile the `serverOnly` partition so `TestFailOriginVocabularyMatchesCheck` and `TestCoerceFailOrigin` stay green. Both lockstep tests move together.
- [x] **M4 — Checkpoint pushes: detect and skip gracefully (NOT a base-refresh).** The checkpoint premise is confirmed (api log above). But `pushbroker` is fast-forward-only over a depth-1 shallow base and has no worktree, so it **cannot** rebase or otherwise rewrite the checkpoint tip server-side — a base-refresh there is infeasible. Scope M4 to: recognize the workflow-scope rejection on the checkpoint push and **skip it cleanly** (log once, never fail the run — checkpoints are already best-effort), so a behind-on-workflows run degrades gracefully instead of erroring every checkpoint. The **finalize** fix (M1) is the real safety net for such a run's work; mid-run checkpoint coverage for it is an accepted gap (see the optional periodic-align follow-up).
- [x] **M5 — Tests.** Agent tests (need a **real on-disk git repo fixture** to produce a genuine merge/rebase conflict, plus stubbed origin state — not pure stubs): behind-on-workflows → merge → push proceeds; merge push rejected for workflow scope → rebase fallback → push proceeds; merge/rebase conflict → run fails `finalize_base_align_conflict` with `preserved_patch` set, push skipped; branch already aligned → no fetch-align work, push as before. A **live-DB** round-trip (throwaway Postgres, like #377's) for the new `fail_origin` + `preserved_patch` on this path. A `pushbroker` unit test that a workflow-scope rejection is caught and the run is not failed. No test writes a real file under `.github/workflows/`.
- [x] **M6 — Docs.** ADR (number = 456) recording the tip-vs-default mechanism, the merge-first/rebase-fallback decision, and why a server-side checkpoint base-refresh is infeasible; ARCHITECTURE run-lifecycle note; extend `docs/github-bot-setup.md`'s workflow-scope section with the behind-on-workflows case and the automatic align; cross-reference from `.claude/rules/prds.md` that behind-on-workflows is now handled automatically. Keep doc frontmatter valid (`web/scripts/check-docs.mjs`).

### Optional / follow-up (only if the owner wants them)

- [ ] **Periodic / early align** so mid-run checkpoints also survive: align the branch with main at run start and/or on a cadence, not only at finalize. Closes M4's accepted mid-run gap at the cost of more frequent align work and conflict exposure. Deferred because the finalize fix already prevents the data loss.
- [ ] **Post-align re-validation by re-invoking the agent.** The worker cannot run an arbitrary user repo's gates itself (gates are agent/LLM-driven; the worker even appends a "Quality gates unverified" banner for that reason, and `runSelfImproveChecks` is uzi's own suite, meaningless against a user repo). Genuinely re-validating a merged/rebased tree therefore means an extra LLM turn (token cost on the user's key, non-deterministic, possible re-approval). Deferred: for a behind-on-workflows-only branch the only tree change from the align is workflow files plus independent main changes, so the interaction risk is low; if it proves to matter, add this as its own milestone with a cost/scope analysis. (This is why M3 from the first draft was cut.)
- [ ] A metric/log counter for how often the finalize align fires, how often the merge suffices vs the rebase fallback, and how often it conflicts.

## Success criteria

1. A run whose clone base predates a workflow-file change on main, and which touches no workflow file, **pushes successfully** at finalize (reproduces the #422 condition; would fail before this PRD).
2. A run whose align conflicts **loses no work**: it ends `failed` with `finalize_base_align_conflict` and its diff is visible in `runs.preserved_patch` on the failed-run card.
3. A run already aligned with the current default is unaffected: no merge/rebase, push behavior identical apart from one fetch.
4. A behind-on-workflows run no longer **errors** on every checkpoint push; it skips cleanly and its final work is saved by M1 (M4).
5. The whole change touches no `.github/workflows/` file and passes `task gate:*` for every component it modifies.

## Decision Log

- **D1 — Trigger on workflow-tree divergence, not on "behind the default branch."** "Not based on the default tip" is true almost every run; "the branch's `.github/workflows/` tree differs from the fresh default" is the precise condition and is cheap to check. Rationale: minimize the align/conflict surface to the runs that actually need it.
- **D2 — Merge first (SHA-preserving), rebase as the proven fallback.** The rejection is tip-vs-default (a branch with zero per-commit workflow change is still rejected), so a merge that brings the tip's workflow tree up to main should clear it — and it avoids every SHA-rewrite hazard. Only the rebase was empirically confirmed (in the #422 recovery); a merge was **not** tested, so we do not assert it works — instead the runtime tries merge, and on a persisting workflow-scope rejection falls back to the proven rebase. This is robust to whichever rule GitHub actually applies without needing to settle it offline. (Corrects the first draft, which asserted "rebase, never merge" on an inconsistent per-commit argument.)
- **D3 — Reuse #377's preservation.** `preserved_patch` and the failed-card render exist on main (#377). A conflicting align routes its pre-align diff through the same path; no new storage surface.
- **D4 — No automatic post-align re-validation (moved to optional).** The worker has no machinery to run a user repo's gates; real re-validation needs an LLM turn. The interaction risk for a workflow-only divergence is low, so accept it and document it rather than pay that cost by default.
- **D5 — Checkpoints stay best-effort; M4 is detect-and-skip, not base-refresh.** `pushbroker`'s fast-forward-only, shallow, worktree-less design cannot replay a rebase server-side. So the checkpoint path can only skip the rejected push cleanly; the finalize fix carries the safety.

## Risks & mitigations

- **An align can conflict and the run cannot auto-resolve.** Mitigated by M2: abort, fail typed, preserve the diff — the exact manual recovery #422 got, now automatic.
- **The rebase fallback rewrites SHAs**, invalidating the worker tracking ref and any recorded commit SHAs, and diverging from any already-published checkpoint (a later resume seeding off that checkpoint would get pre-rebase commits). Mitigations: re-`fetchAgentBranch` to the rewritten tip (M1); treat recorded SHAs as informational (the run id is identity); prefer the merge path (D2) which avoids this entirely. The merge-first design means the rebase (and these hazards) only occur when merge does not clear the rejection.
- **Rebase can silently drop empty commits.** Mitigated by `--empty=keep`, no autosquash, and a pre/post commit-count assertion (S3).
- **TOCTOU: main's workflow files can change again between the align and the push** (or during any re-validation). Mitigated by doing the fetch+align immediately before the push, and a bounded retry of the whole align-and-push on a workflow-scope rejection (S4).
- **This run could itself hit the bug it fixes** if main's workflow files change during its own execution. Mitigation: recover it from the worker PVC as #422 was (bundle `refs/uzi-runner/agent/issue-456`, align onto main, push). One-time exposure that the fix then closes.
- **The align targets a stale ref.** The rebase/merge must target the freshly fetched origin default (`FETCH_HEAD` / updated `refs/remotes/origin/<default>`), not `defaultBranchRef`'s frozen-mirror fallback rungs (N2).

## Out of scope

- Widening the bot PAT to include `workflow` scope (the `repo`-only boundary is a deliberate supply-chain guardrail; privcheck enforces it).
- The branch that legitimately **modifies** a workflow file — that is #377's job and stays as-is; #377's guard fires before M1's code.
- A general "keep every branch continuously rebased on main" policy (the optional periodic-align follow-up aside).
- Cross-forge generalization beyond GitHub's workflow-scope rule; GitLab/Forgejo do not impose it.
- A server-side checkpoint base-refresh (infeasible against `pushbroker`'s fast-forward-only, shallow, worktree-less design — D5).
