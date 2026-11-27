# Landing the whole open-PR set

Use it when the user asks to merge the open PRs, land the batch, or clear the queue
before a release. Each PR still goes through SKILL.md's loop; this file adds the cross-PR
order and checks. `uzi-release` starts only after this is done. `S` is `.agents/skills/uzi-lander/scripts/`.

## 1. Survey

```sh
gh pr list --state open
gh pr view <n> --json number,title,headRefName,mergeable,mergeStateStatus,files \
  --jq '"branch=\(.headRefName)  \(.mergeable)/\(.mergeStateStatus)\n" + ([.files[]|"  \(.path)"]|join("\n"))'
S/claims.sh list          # PRs another lander already holds: leave them to their owner
```

`BEHIND` = out of date with `main` (see merge-mechanics.md, *Merging behind main*); `CONFLICTING` = a real
conflict (SKILL.md step 5). Classify every PR up front: **green / red-repo-wide /
red-defect / stale-base / Renovate-class** (references/renovate.md).

- **In-flight uzi runs.** Auto-CI-fix can open a `ci_fix` run and a PR mid-batch.
  Follow SKILL.md's *CI-fix branch ownership*: refresh, decide wait/cancel with the
  buddy, record the decision and review the pushed delta before merging.
- **Migration collisions.** Two PRs adding the same `NNNNN_*.sql` under
  `api/internal/store/migrations/` both land as distinct files and brick strict-goose boot.
  Note which PRs add a migration at or below the live head
  (`git ls-tree --name-only origin/main api/internal/store/migrations/ | sort | tail -1`);
  `S/land-prep.sh` renumbers them. A PR editing only sqlc query files adds no migration.

## 2. Reds first

A red PR is often red for a **repo-wide** reason that also reddens `main` and every sibling,
so one diagnosis can unblock the batch; a red-defect PR cannot merge until fixed anyway.

- **Is it the PR's fault?** Often not. `vulncheck:{api,controller,web}` (inside the
  `lint-*`/`validate-web` jobs) re-evaluate against the LIVE advisory DB, so a branch green
  when cut goes red when a new CVE lands, and `main` fails the same way. Read the failing
  job (references/merge-mechanics.md, *Post-merge CI*, for the live-log commands).
- **Repo-wide** (new CVE, toolchain currency): the fix is a dependency bump on its own
  branch, landed first; then refresh each other PR only when it is next in merge order
  (SKILL.md step 5), so its checks re-run on the fixed base.
- **Defect**: the PR's own loop (SKILL.md step 4). **Stale base**: defer the refresh until
  the PR is next (SKILL.md step 5).

Watch several PRs' CI at once with `S/watch-prs-ci.sh <PR> [<PR>...]` in the background
(exit 1 the moment any PR has a confirmed failing job, 0 when all are terminal and green).

## 3. Order the green batch

**Our PRs first, the routine Renovate minor/patch batch last** (most green patches
automerge in the weekly window; the ones still open are landed here, batched into one PR when CI-sufficient: references/renovate.md). Renovate rebases its own PRs
only in its Saturday-morning window, so landing ours first keeps them fresh for the next window (between
windows they stay stale or conflicting: references/renovate.md); the reverse re-stales
ours against the new deps. It also isolates dependency breakage (features settled, then deps)
and confines lockfile stacking (references/merge-mechanics.md) to one end phase. The one
carve-out is §2: a dependency PR that FIXES a repo-wide red goes first.

**Probe cross-PR conflicts** when several PRs edit a shared hand-edited file
(ARCHITECTURE.md, a shared handler): in a throwaway worktree off `origin/main`,
`git merge --no-ff origin/<branch>` each in the intended landing order, letting clean merges
accumulate so each probe sees its predecessors. On a conflict, record it and
`git merge --abort` (the earlier clean merges stay), then continue with the next. Remove the
worktree after. Declare an ordering with
`S/claims.sh claim '#B' --depends-on '#A'`.

## 4. Per-PR review, then merge each as it is ready

Each PR takes SKILL.md's review lane (step 2) and *Always yours* checks. For a PR closing a
PRD, the scope match is per milestone: read the PRD and mark each **IMPLEMENTED / PARTIAL /
SKIPPED** with backing files; a milestone reframed and documented is not skipped. For a PR
adding a migration, note whether it is safe to renumber filename-only. Surface any PR you
would not merge to the user instead of merging it.

Merge each PR the moment it is ready (`S/merge.sh`, squash; the subject carries `(#PR)`,
which the release CHANGELOG cites); do not hold the batch to the end. Review in parallel,
but rebase serially: land-prep one PR, merge it, then the next (SKILL.md step 5); never
re-push the whole held batch, since every merge stales the rest again. Watch post-merge CI per
SKILL.md step 7; a `cancelled` run under a newer merge is supersession, not red.

## 4a. Fast merge mode (backlog clearing, on the user's word)

Use only when the user asks to clear a backlog fast; it replaces per-PR fresh CI on the
current `main` with one `main` CI per batch, fixing forward. Releases still gate on a green tip.

- **Per PR**: review lane done; one green PR CI run on the reviewed pushed head (its base may be
  stale); `MERGEABLE` (no conflict); migrations renumbered and `task check:migration-numbering`
  run locally. A PR adding a migration or touching data integrity also needs the
  `LiveDB: N passed, 0 skipped` line from both `test-livedb-workersvc` and `test-codex-refresh-lostreply-e2e` on that head (SKILL.md step 5).
- **Batch**: 4-5 PRs, one per area. Two `agent/**` PRs may pair when their tests and jobs
  are separable. If both touch the worker image or packaged runtime, dispatch
  `gh workflow run codex-packaged-proof.yml --ref main` after both merge, confirm the run's
  `headSha` is the combined batch SHA (or a descendant), and watch it with `main` CI; a red counts below.
- **Prepare migrations early**: pin one live main SHA. Assign non-overlapping ranges
  above its migration head in landing order. Preflight the cumulative merge tree in a
  throwaway worktree. Compose any replacement SQL function on its actual predecessor,
  with Down restoring that definition. Finish renumbering and reference edits, then
  push each final head once so CI overlaps. Merge individually in order after the
  per-PR proofs above. If the base or order invalidates a range, replan before pushing
  or merging. Never fill a lower gap after a higher version lands.
- **Readiness report**: run `S/fast-batch.sh [PR ...]` with the proposed landing order.
  Pass `--evidence FILE` with a JSON array of `{pr,head,review,trust_boundary}` records
  from the reviewed heads. Use the full SHA and an approval receipt for `review`.
  Use null for unknown evidence. Pass the observed account-wide count with
  `--active-jobs N` and a conservative incremental peak, including companion workflows,
  with `--peak-jobs N`. Treat the result as an estimate against the 20-job cap. Include
  pending demand when planning the push wave. A missing proof stays unknown.
- **CI slots**: let the lane owner (tower) grant at most two concurrent PR CI runs
  alongside main CI with `S/ci-lane.sh grant PR FULL_SHA --owner NAME`, then
  `S/ci-lane.sh release PR` after the slot is no longer needed. Use `list` to see grants.
  Keep the local slot state outside the checkout.
  Size grants in jobs using the readiness report, including queued demand and
  companion workflows. A push without a slot is allowed. Identify the CI and KinD Smoke
  runs created for its exact head. For a queued run, use
  `gh api -X POST repos/vtmocanu/uzi/actions/runs/<id>/force-cancel` and verify
  `completed cancelled`. Cancel an in-progress run normally. After a slot is granted,
  use `gh run rerun <id>` on the cancelled CI and report its run ID. Leave CodeQL and
  Codex packaged proof alone. LiveDB jobs and near-completion do not protect ungranted
  PR runs. Keep exact-head grants and PR runs an owner relies on, recorded with
  `relied_on: true` in the exact-head evidence.
- **Low-risk main-CI option**: use only on the user's explicit request. Confirm an
  approved, mergeable PR has no migration, trust-boundary change or exact-head CI
  obligation. It needs no PR CI slot. Use `merge.sh ... --expect-head FULL_SHA
  --main-ci-gate`. The flag refuses detected migration and trust markers or known red
  checks. All other guards run. Gate the batch on main CI and retain exact-tip release
  coverage.
- **Batch grant**: have the lane owner (tower) authorize the ordered PR and full-SHA list for a
  bounded window. Run `merge.sh` per PR and report each merge SHA. Return to the owner if
  a head or the order changes, a hold arrives, or the grant expires. The report alone
  grants no merge authority.
- **Order for merges and CI slots**: take red-main or release fixes first, then PRs
  that unblock other PRs or runs (most dependents first), then large or conflict-prone
  PRs, then the rest. Use non-infra changes first as a tie-breaker. Give a large PR at
  most one separable small companion so a red stays attributable. Include this line in
  every `MERGE?` or slot request:
  `size: <files/+lines>; blocks: <PRs/runs/issues waiting on this merge>; obligation: <exact-head CI yes/no>`.
- **After each batch**: one `main` CI run; the next batch may merge while it runs. Red: rerun a
  known flake, or fix forward / revert the culprit in parallel. A second unresolved red pauses merges.
- **Single, never batched**: trust-boundary PRs (own CI plus `e2e.yml` on the exact head).
- **Main CI**: keep the newest main CI, KinD and CodeQL runs. Let running main-push
  CI and KinD finish, as their workflow concurrency policy does. After a newer main
  push, use `S/ci-lane.sh plan` to inspect older runs, then `apply --log FILE` to force-cancel
  eligible runs and verify the outcome. An older main run is eligible only if
  the descendant at the current tip has a replacement for the same workflow
  that is active or completed with success, failure, or a timeout. A skip-CI
  push can leave the older run as the latest check. The same plan covers
  ungranted PR runs and proposes the next slots. Use `plan --json --evidence FILE` with head-pinned
  `{pr,head,blocks,red_main_or_release_fix,conflict_prone,relied_on}` records for
  the proposal. Keep unpublished `chore(release)` runs and main runs more than 85%
  complete. Keep the exact or two-hop metadata CI evidence needed for publication.
  The planner retries torn run counts and leaves unknowns untouched. Runs queued
  more than 24 hours with no started jobs appear separately as STALE. Exclude them
  from cancellation and active CI occupancy.
- **CI hygiene**: cancel runs for already-merged PRs when they no longer supply evidence.
  Preserve required packaged proof, trust-boundary CI or `e2e.yml`, the exact-head
  LiveDB proofs above, and runs another owner relies on.

## 5. Report-only runs whose issue could now be closed

Not every delivery is a PR: a run can finish `completed` with `report_only=true` and no PR
(an investigation, a fix-in-place, or an empty-week scheduled job). Their issue often sits
open with the answer delivered, so the end of a batch is the moment to sweep them:

```sh
uzi run list --json | jq -r '
  [ .[] | select(.status=="completed" and .report_only==true and .issue_iid!=null and .mr_iid==null) ]
  | sort_by(.finished_at) | reverse
  | .[0:30][] | "run=\(.id[0:8])  #\(.issue_iid)  judge=\(.judge_verdict // "-")  \(.issue_title[0:70])"'
```

**Report-only is not resolved.** Read each run's conclusion (`uzi run get RUN_ID --field
report_md`, `judge_verdict`) and confirm the issue is still open. Present the ones that look
done as a close-candidate list; close only those the user confirms
(`gh issue close N --comment "WHAT THE RUN DELIVERED, run RUN_ID"`).

## 6. Batch report

One table: every PR merged (PR | issue | what it delivered | review lane), every PR closed
or held unmerged with the reason (a hold also names what unblocks it and the buddy's view), and every issue closed from a report-only run. Then: milestones
reframed or deferred (never silently skipped), behaviour changes, merge mechanics handled
(renumbers, conflict resolutions, lockfile regen, `--admin`), and follow-ups: review gaps,
SDK adoption findings and deferred majors (references/renovate.md). Offer to file issues.
