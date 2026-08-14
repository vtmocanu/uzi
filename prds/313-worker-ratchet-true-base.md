# PRD #313: Worker lint ratchet clamps to the true branch base

**Issue**: https://gitlab.example.com/vtmocanu/uzi/-/issues/313
**Priority**: Medium
**Status**: Draft

> Self-contained for an offline worker. This change lives entirely inside uzi's own
> `agent/` package plus local `git` behaviour. It needs **no** open-web access: no external
> API, no docs site, no `WebFetch`/`WebSearch`. Every fact below was established locally and
> is stated as resolved; every success criterion is verifiable offline with a scratch git repo.

## Problem

On **resume legs**, the worker's `task lint:api` ratchet computes its base from a **stale
frozen mirror** instead of the branch's real base, so pre-existing lint findings in files the
run never touched surface as brand-new "regressions". The lead then wastes iterations
re-diagnosing the false red and re-linting by hand against the true base, and the judge
re-flags it every run.

The ratchet is `.golangci.yml`'s `issues: {new-from-merge-base: origin/main, whole-files: true}`.
golangci-lint reports findings introduced in `merge-base(origin/main, HEAD)..HEAD`. If the
clone's `origin/main` sits as a **strict ancestor of the branch's real base**, that merge-base
regresses to an older commit and pulls in **other people's** work between the stale commit and
the real fork point, so their pre-existing findings are reported as new.

### Measured evidence (run `9ad22852`, PRD #308, 2026-08-12)

The worker for this run already carried the issue #262 fix (shipped v0.23.0, 2026-08-09; the
factory was on ~v0.30 by 2026-08-12), yet the false red still happened:

- Branch base `baseSha` = `5e0f1f0c` (a **fresh** `main` commit, 2026-08-12).
- The clone's `origin/main` = `5712a0d4` (a **3-week-stale** commit, 2026-07-21).
- `5712a0d4` is a plain ancestor of `5e0f1f0c` (verified: `git merge-base --is-ancestor 5712a0d4 5e0f1f0c` = yes; `--is-ancestor 5e0f1f0c 5712a0d4` = no).
- So `task lint:api` reddened on `api/internal/handler/ws.go`, `api/internal/poller/autopilot.go`,
  `api/internal/store/revise_cap_*_test.go` and others: **none touched by PRD #308**.
- The lead's manual workaround `golangci-lint run --new-from-merge-base=5e0f1f0c ./...` returned
  **0 issues**, proving the findings are pre-existing backlog, not the branch's work.

### Root cause (structural)

In `agent/src/git.ts`, `runnerCloneForBranch` resolves two commits:

- `baseSha` (git.ts:481) = the commit the agent branch was checked out at. On a fresh run this is
  the default-branch tip; on a **resume** it is the branch's own prior origin/tracking tip (see
  the caveat under "Invariant" below).
- `defaultBranchCommit` (git.ts:491) = on a fresh leg it is exactly `baseSha`; on a **resume leg**
  (`seededFrom !== "default"`, git.ts:487) it is `defaultBranchSha(barePath)`, which flows through
  `defaultBranchRef`'s fallback chain (git.ts:801-817).

`defaultBranchRef`'s chain prefers the fresh remote-tracking ref
(`refs/remotes/origin/HEAD`, `refs/remotes/origin/main`) but **falls through to the frozen mirror
`refs/heads/main`** when the fresh ref is absent. `cloneBare` rewrites the fetch refspec to
`+refs/heads/*:refs/remotes/origin/*`, so the bare's own `refs/heads/main` never moves after the
first clone: it is fixed at first-clone time (here `5712a0d4`, 3 weeks old).

Issue #262 then `update-ref`s the runner clone's `refs/remotes/origin/<default>` (what the ratchet
reads as `origin/main`) to `defaultBranchCommit` (git.ts:544-545). On a resume leg that hits the
frozen rung, so #262 advances the ratchet base to a **stale** commit that is an ancestor of the
real base: a no-op improvement, and the false red persists. Meanwhile `baseSha` was resolved
fresh, so the worker already holds a base that is never behind the fork point.

Why the fresh `refs/remotes/origin/main` was absent in that specific run was not pinned down (the
code comment at git.ts:178-185 already flags this as a "narrow window": a bare whose first fetch
died can persist carrying `refs/heads/*` only). The fix below is deliberately robust to the cause:
it never lets the ratchet base be a strict ancestor of the branch's base, whatever the fetch state.

## Solution

**Invariant to enforce:** the clone's `origin/main` (the ratchet base) must **never be a strict
ancestor of `baseSha`**. That is precisely the condition that pulls other people's intervening work
into the diff, which is the false red. Note `baseSha` is "the commit the clone is checked out at",
which equals the fork-point-from-main only on a fresh run; on a resume it is the branch's own prior
tip. Both readings satisfy the invariant we need, because anything reported must then be reachable
from `baseSha` (the branch's own line), never from a third party's commits before it.

`runnerCloneForBranch` already has both `baseSha` and `defaultBranchCommit` in scope at the issue
#262 `update-ref` site, and the private `isAncestor(barePath, ancestor, descendant)` helper
(git.ts:838-839) is available.

**Recommended fix (clamp):** when `defaultBranchCommit` is an ancestor of `baseSha`, point the
clone's `refs/remotes/origin/<default>` at `baseSha` instead; otherwise keep `defaultBranchCommit`.
At the #262 update-ref site:

```
// Read ancestry against the BARE (worker-owned; both commits are reachable there, since
// baseSha and defaultBranchCommit are both resolved from bare refs at git.ts:481/491).
// Use the existing worker-uid isAncestor helper — do NOT introduce a runner-uid variant.
let ratchetBase = defaultBranchCommit;
if (ratchetBase && await this.isAncestor(barePath, ratchetBase, baseSha)) {
  ratchetBase = baseSha; // never let the ratchet base be a strict ancestor of the branch base
}
if (defaultBranch && ratchetBase) {
  // The WRITE stays runGitAsRunner (runner-owned clone), exactly as #262 already is.
  await this.runGitAsRunner(clonePath, ["update-ref", `refs/remotes/origin/${defaultBranch}`, ratchetBase]);
  if (ratchetBase !== defaultBranchCommit) {
    this.log.info("runner clone: clamped ratchet base to branch base (stale default ref)", {
      branch, baseSha, defaultBranchCommit, clamped_to: ratchetBase,
    });
  }
}
```

This is the exact base the lead uses by hand (`--new-from-merge-base=<baseSha>` gave 0 issues).

**Where the clamp fires, stated accurately** (this matters, and an earlier draft got it wrong):
`isAncestor` is true at equality and on any descent, so the clamp branch is entered whenever
`defaultBranchCommit` is an ancestor-or-equal of `baseSha`. That covers three cases, only the last
of which is a behaviour change from #262:

1. **Fresh run** — `defaultBranchCommit === baseSha`, so `ratchetBase === defaultBranchCommit`; the
   write is byte-for-byte identical to #262 and the `log.info` does **not** fire (guarded on an
   actual change). No behaviour change.
2. **Resume, main has NOT advanced** — `defaultBranchCommit` = the true fork point, `baseSha` = the
   branch's prior tip which descends from it, so the clamp sets `origin/main = baseSha`. This equals
   the "always-`baseSha`" behaviour below and gates only this leg's new commits. It is **not** the
   frozen-mirror bug, but the outcome is still correct (never a false red; the branch's earlier legs
   were already gated when they ran).
3. **Frozen-mirror bug (the target)** — `defaultBranchCommit` is a *stale* ancestor of `baseSha`
   (as in run `9ad22852`), so the clamp corrects `origin/main` to `baseSha` and the false red
   disappears. Behaviour change vs #262, in the intended direction.

The only case where `defaultBranchCommit` is kept is when it is **not** an ancestor of `baseSha`
(a resume where main genuinely moved forward on a divergent line): there `merge-base` finds the
true fork and vs-main semantics are preserved.

**Alternative considered (always set `origin/main = baseSha`):** simpler, and the clamp already
collapses into it in cases 1 and 2. The two differ **only** in the resume-where-main-moved-forward
case: the clamp keeps gating the whole branch vs the fork point, while always-`baseSha` gates only
the current leg. Neither ever hides a newly-introduced finding. Choosing between them is a small
semantic call (re-gate the branch's own earlier legs, or not) and is a **decision to settle at the
plan gate**; the clamp is written above as the default because it is the minimal change from #262.

## Out of scope (do not touch)

- **`.golangci.yml`'s `origin/main`** must stay a **static literal** (`.golangci.yml:203-230`).
  golangci-lint does not expand env vars in config, and the value must resolve in both the local
  bare-clone layout and GitLab's detached-HEAD CI. The fix is entirely in how the **worker prepares
  the clone's ref**, never in the config.
- **The Taskfile `lint:api` / `lint:controller` merge-base pre-flight** is unaffected: after the
  clamp, `origin/main` = `baseSha` is still an ancestor of HEAD, so `git merge-base "origin/main"
  HEAD` still resolves.
- **`changedFiles` / MR-diff base** (`git diff --name-only origin/main...ref` against the **bare**)
  is a different concern from the runner clone's lint ratchet ref; leave it alone.
- **Real CI** already ratchets against current `main` and is unaffected. This is a worker-gate-only
  ergonomics fix; it never hides a real finding (it only ever removes false positives).
- **Ownership split:** only the `update-ref` **write** into the runner-owned clone uses
  `runGitAsRunner` (as #262 already does). The ancestry **read** uses the existing worker-uid
  `isAncestor` against the worker-owned **bare** (`barePath`); do **not** add a runner-uid variant
  or run the read against `clonePath` — both are unnecessary and cross no ownership boundary.
- **The frozen local `refs/heads/main`** in the clone stays untouched (only the remote-tracking
  `refs/remotes/origin/<default>` is rewritten, as #262 already does).
- **The no-resolvable-default-branch edge** stays as today: when `defaultBranch`/`defaultBranchCommit`
  is undefined the update-ref is skipped and the clone's `origin/main` keeps whatever the plain
  clone copied (which can be the frozen mirror), so the ratchet could still be stale in that
  pathological case. It is governed by the Taskfile pre-flight and is out of scope here; note it in
  the code comment rather than trying to fix it.

## Success criteria

1. In a resume-shaped topology where `defaultBranchCommit` is a **stale ancestor** of `baseSha`, the
   runner clone's `origin/main` resolves to `baseSha`, so `merge-base(origin/main, HEAD) == baseSha`
   and the ratchet gates only branch-reachable findings (no findings in files unreachable from
   `baseSha`).
2. On a fresh run (`defaultBranchCommit === baseSha`) the write is byte-for-byte identical to #262
   and the clamp log does **not** fire.
3. On a resume where main moved forward on a divergent line (`defaultBranchCommit` is **not** an
   ancestor of `baseSha`), the clamp is a no-op and `origin/main` keeps `defaultBranchCommit`.
4. The clamp `log.info` fires **only** when `ratchetBase !== defaultBranchCommit` (an actual change).
5. Both Go gates (`task gate:api`, `task gate:controller`) and `task gate:agent` stay green, and the
   change is covered by an automated test that is red before the fix and green after.

## Milestones

- [ ] **M1: Offline reproduction (red before the fix).** In `agent/test/git.test.ts` (the existing
  `#262` frozen-mirror test at ~208-241 and the resume test at ~158-197 are the pattern; scratch
  bare origins come from `agent/test/fixture-repo.ts`, no network/auth), build the residual-bug
  topology: a branch whose real base `baseSha` is fresh, while the bare's **fresh tracking refs are
  removed/staled** (`refs/remotes/origin/HEAD` and `refs/remotes/origin/main`) so `defaultBranchRef`
  falls through to the frozen `refs/heads/main` rung and `defaultBranchCommit` resolves to a commit
  that is a strict ancestor of `baseSha`. As a **precondition** assert the topology is genuinely
  stale (`defaultBranchCommit` is an ancestor of `baseSha`). The **failing assertion** is the
  desired post-fix state — the clone's `origin/main == baseSha` and `merge-base(origin/main, HEAD)
  == baseSha` — so the test is red before M2 and green after (do **not** assert the pre-fix stale
  value, which would lock in the bug).

- [ ] **M2: Clamp the ratchet base.** In `runnerCloneForBranch`, at the issue #262 update-ref site,
  add the clamp exactly as in Solution: read `isAncestor(barePath, defaultBranchCommit, baseSha)`
  with the existing **worker-uid** helper; when true, `update-ref` the clone's
  `refs/remotes/origin/<default>` to `baseSha` (write stays `runGitAsRunner`); `log.info` only when
  the value actually changed. Preserve the existing skip for an unresolvable default branch and note
  that edge in the comment.

- [ ] **M3: Prove the fix and both no-op paths.** Make the M1 test pass after M2, and add the two
  no-op cases from Success Criteria 2 and 3: a fresh run (`defaultBranchCommit === baseSha`, write
  unchanged, no clamp log) and a resume where main moved forward on a divergent line
  (`defaultBranchCommit` not an ancestor of `baseSha`, `origin/main` keeps `defaultBranchCommit`).
  Also assert the clamp log fires only on the actual-change path.

- [ ] **M4: Update the docs and specs that describe this behaviour.** Correct the `git.ts`
  `RunnerClone.baseCommit` / issue-#262 comment block (git.ts:167-206, 529-542) to state the clamp,
  the invariant it enforces, and the no-default-branch edge; update `docs/dev-conventions.md`'s
  frozen-mirror ratchet note (docs/dev-conventions.md:154-172); add a `specs/ai.md` decision entry.
  State that `.golangci.yml`'s `origin/main` config is intentionally unchanged. No open-web lookups.

## Risks and mitigations

- **Risk: the clamp changes behaviour on an ordinary resume, not only the bug.** Reality (not a
  risk to mitigate away): it does, in case 2 above, where it collapses into always-`baseSha`. The
  outcome is still correct (never a false red), and M3 tests it. The clamp-vs-always-`baseSha`
  choice is surfaced for the plan gate rather than hidden.
- **Risk: `isAncestor` run against a repo where a commit is unreachable.** Mitigation: run it
  against `barePath`, where both `baseSha` and `defaultBranchCommit` are resolved from refs, so both
  are reachable; `isAncestor` already answers `false` on a missing ref, so a bad candidate never
  wins the clamp.
- **Risk: the clamp log misleads by firing on no-op fresh runs.** Mitigation: guard the `log.info`
  on `ratchetBase !== defaultBranchCommit` (Success Criterion 4).
- **Risk: ownership-invariant regression.** Mitigation: only the `update-ref` write uses
  `runGitAsRunner` (as #262 already does); the ancestry read uses the existing worker-uid helper
  against the worker-owned bare, crossing no ownership boundary.

## Dependencies

None. Self-contained in `agent/` plus its tests and docs. Builds on issue #262 (already merged),
which this extends rather than reverts.

## Validation strategy

- Automated: the M1/M3 `node:test` cases (red before M2, green after) plus `task gate:agent`.
- Regression: `task gate:api` and `task gate:controller` stay green (the fix does not touch either
  Go module's sources, only the worker's clone preparation).
- The worker can validate everything offline; no external network is required. `runGitAsRunner` is a
  passthrough to plain `git` outside a real worker, so the existing #262 test already exercises the
  `update-ref` path this change extends.

## Notes for the maintainer (not worker scope)

The stale cross-run agent **memory [12]** ("golangci-lint in uzi is v2 ... whole-files ratchet",
id `3ca75b1a`) reinforces the manual workaround and will keep nudging the lead even after this
lands. Purge or update it with `uzi memory rm <id>` once the fix is verified; it is uzi DB state,
not repo content, so it is intentionally outside this PRD's scope.
