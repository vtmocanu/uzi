# Landing the whole open-PR set

Use it when the user asks to merge the open PRs, land the batch, or clear the queue
before a release. Each PR still goes through SKILL.md's loop; this file adds the cross-PR order and checks.
`uzi-release` starts only after this is done. `S` is `.agents/skills/uzi-lander/scripts/`.

## 1. Survey

```sh
gh pr list --state open
gh pr view <n> --json number,title,headRefName,mergeable,mergeStateStatus,files \
  --jq '"branch=\(.headRefName)  \(.mergeable)/\(.mergeStateStatus)\n" + ([.files[]|"  \(.path)"]|join("\n"))'
S/claims.sh list          # PRs another lander already holds: leave them to their owner
```

`BEHIND` = out of date with `main` (fine under an admin merge); `CONFLICTING` = a real
conflict (SKILL.md step 5). Classify every PR up front: **green / red-repo-wide /
red-defect / stale-base / Renovate-class** (references/renovate.md).

- **In-flight uzi runs.** `uzi run list`: auto-CI-fix can open a `ci_fix` run (often parked
  at `awaiting_approval`) and a PR mid-batch. Read its plan before re-diagnosing
  (`uzi run get <id>`, `uzi run logs <id>`); `uzi run reject <id> -m "<why>"` one you
  supersede.
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
  branch, landed first; then `gh pr update-branch <n>` the rest so their checks re-run.
- **Defect**: the PR's own loop (SKILL.md step 4). **Stale base**: `gh pr update-branch`.

Watch several PRs' CI at once with `S/watch-prs-ci.sh <PR> [<PR>...]` in the background
(exit 1 the moment any PR has a confirmed failing job, 0 when all are terminal and green).

## 3. Order the green batch

**Our PRs first, the routine Renovate minor/patch batch last.** Renovate auto-rebases its own
PRs when `main` moves, so landing ours first keeps them fresh for free; the reverse re-stales
ours against the new deps. It also isolates dependency breakage (features settled, then deps)
and confines lockfile stacking (references/merge-mechanics.md) to one end phase. The one
carve-out is §2: a dependency PR that FIXES a repo-wide red goes first.

**Probe cross-PR conflicts** when several PRs edit a shared hand-edited file
(ARCHITECTURE.md, a shared handler): in a throwaway worktree off `origin/main`,
`git merge --no-ff origin/<branch>` each in the intended order, record which conflict,
`git merge --abort` between probes, remove the worktree after. Declare an ordering with
`S/claims.sh claim '#B' --depends-on '#A'`.

## 4. Per-PR review, then merge each as it is ready

Each PR takes SKILL.md's review lane (step 2) and *Always yours* checks. For a PR closing a
PRD, the scope match is per milestone: read the PRD and mark each **IMPLEMENTED / PARTIAL /
SKIPPED** with backing files; a milestone reframed and documented is not skipped. For a PR
adding a migration, note whether it is safe to renumber filename-only. Surface any PR you
would not merge to the user instead of merging it.

Merge each PR the moment it is ready (`S/merge.sh`, squash; the subject carries `(#PR)`,
which the release CHANGELOG cites); do not hold the batch to the end. Watch post-merge CI per
SKILL.md step 7; a `cancelled` run under a newer merge is supersession, not red.

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
unmerged with the reason, and every issue closed from a report-only run. Then: milestones
reframed or deferred (never silently skipped), behaviour changes, merge mechanics handled
(renumbers, conflict resolutions, lockfile regen, `--admin`), and follow-ups: review gaps,
SDK adoption findings and deferred majors (references/renovate.md). Offer to file issues.
