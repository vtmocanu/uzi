---
name: uzi-release
disable-model-invocation: true
description: "Reviews, merges, and releases uzi PRs end to end for this repo (hosted on GitHub). Surveys open PRs, reviews each against its PRD/issue for skipped milestones and correctness, merges them while handling migration-number collisions and cross-PR conflicts, watches GitHub Actions CI at job level, then hands the actual release (version bump, tag, GHCR publish, release-environment approvals, deploy verify) to the `release` agent and presents the summary while it runs. Use when the user runs /uzi-release, or asks to review-and-merge the uzi PRs, cut or ship a uzi release, bump the uzi version, or deploy uzi. Triggers include uzi-release, release uzi, merge the uzi PRs, cut a uzi version, ship uzi, deploy uzi."
---

# uzi-release

Drive uzi's open pull requests through review, merge, release, and deploy verification, then summarize. This repo is hosted on **GitHub**: use `gh` (which infers the repo from the checkout, so a fork under any owner works unedited; never `glab`/`tea`). CI is **GitHub Actions** (`.github/workflows/`), images and the Helm chart publish to **GHCR** on a `v*` tag.

Do NOT bake step details that live in the repo into your head. Read the source of truth each run: `deploy/README.md` (release + deploy runbook), the workflow files under `.github/workflows/` (`ci.yml`, `release.yml`, `kind-smoke.yml`, `e2e.yml`, `brew.yml`), the destructive-ops rules in `CLAUDE.md`, and `.claude/rules/*.md` for the component you touch.

> **Note on the deploy layer.** `deploy/README.md` still describes the previous GitLab/ArgoCD-on-Harbor deploy path and is mid-migration to GitHub/GHCR; treat its deploy specifics as in-flux and verify against the maintainer rather than this skill. The release itself (tag → GHCR publish) is fully on GitHub and is what this skill drives end to end. Cluster/deploy verification needs cluster access you may not have; if so, say so plainly rather than guessing.

## 0. Guardrails first
- NEVER `docker compose -p uzi down -v` and never glob `uzi-` containers (see `CLAUDE.md` "Destructive operations"). This skill touches git, `gh`, and (if you have deploy access) `kubectl` only.
- Work on `main` in the repo root worktree; make a sibling worktree for any branch integration (`git worktree add ../uzi-issue-NNN -B agent/issue-NNN origin/agent/issue-NNN`). Never check the root worktree out onto another branch.
- `main` is guarded by a **ruleset** (required status checks + up-to-date branch), not classic branch protection — so `gh api repos/OWNER/REPO/branches/main/protection` 404s even though rules are enforced. The maintainer (repo owner) can push `main` directly, which is how a release lands. If a push or merge is refused, read the refusal and stop rather than forcing.

## 1. Survey the open PRs
```sh
gh pr list --state open
```
For each PR pull metadata, the changed-file list, the closing issue/PRD, and its check status:
```sh
gh pr view <n> --json number,title,headRefName,mergeable,mergeStateStatus,files \
  --jq '"branch=\(.headRefName)  \(.mergeable)/\(.mergeStateStatus)\n" + ([.files[]|"  \(.path)"]|join("\n"))'
gh pr diff <n>            # full diff for review
```
Branches are fetched by `gh` on demand; `git fetch origin <branch>` if a review agent needs whole files locally. `mergeStateStatus=BEHIND` means the branch is out of date with `main` (the ruleset requires up-to-date before a normal merge — see step 3); `CONFLICTING` means a real conflict.

**Detect migration collisions now.** Two PRs adding the same `NNNNN_*.sql` under `api/internal/store/migrations/` will both land as distinct files (git sees no conflict) and brick strict-goose boot. Find the live head (`ls api/internal/store/migrations/ | sort | tail -1`) and note which PRs add migrations at or below it. They get renumbered at merge (step 3). Note: PRs that only edit sqlc-generated query files (`*.sql.go`, `queries/*.sql`) add **no** migration and pose no collision risk — check for a genuinely new file under `migrations/`.

**Check in-flight uzi runs before you start.** `uzi run list` (if the `uzi` CLI is on PATH). uzi's auto-CI-fix opens a `ci_fix` run for a failed pipeline (often parked at the plan-approval gate, `awaiting_approval`) and can auto-open a PR mid-release. Read a run's plan first (`uzi run get <id>`, `uzi run logs <id>`) so you don't re-diagnose what it already found; `uzi run reject <id> -m "<why>"` any whose fix you supersede.

**Also surface report-only runs whose issue could now be closed — not every delivery is a PR.** A run can finish `completed` with `report_only=true` and **no MR/PR** (the `report_only` boolean and `report_md` are first-class fields on the run DTO). These come from two places: an `issue`/`prompt` run that investigated, validated, or fixed-in-place and had nothing to open an MR for; and the **time-driven schedules** (`uzi schedule list --json` — e.g. weekly test pass, docs-hygiene, bug-hunt, feature-bingo) which are *designed* to finish report-only on an empty week. Their backing issue often sits open with the answer already delivered in the report, so a release is the natural moment to sweep them. Enumerate the candidates:

```sh
uzi schedule list --json | jq -r '.[]|"\(.id[0:8])  \(.status)  next=\(.next_fire_at // "-")  \((.title // .prompt // "-")[0:60])"'
uzi run list --json | jq -r '
  [ .[] | select(.status=="completed" and .report_only==true and .issue_iid!=null and .mr_iid==null) ]
  | sort_by(.finished_at) | reverse
  | .[0:30][] | "run=\(.id[0:8])  #\(.issue_iid)  judge=\(.judge_verdict // "-")  \(.issue_title[0:70])"'
```

Then, per distinct still-open issue, decide before recommending a close: **report-only ≠ resolved.** Many report-only runs are "surveyed, found nothing" (an empty-week schedule fire) or a partial investigation, and closing their issue would be wrong. Read the run's conclusion (`uzi run get <id> --field report_md`, and `judge_verdict`) and confirm the issue is still open (`gh issue view <n> --json state,title -q '.state'`). Present the ones that genuinely look done as a **close-candidate list for the user to confirm** — do NOT auto-close: closing an issue is a mutating, owner's-call action (the permission classifier may block it, like `--admin`), so close only the ones the user green-lights (`gh issue close <n> --comment "<what the run delivered; run <id>>"`), then report them in step 6.

**When a PR bumps `@anthropic-ai/claude-agent-sdk`, read the SDK changelog for adoption opportunities — this is not just a lockfile bump.** That package is uzi's agent-runtime core (the `agent/` worker builds on it, incl. the `PreToolUse` guardrail hooks and `settingSources`), so a new version can ship capabilities uzi should adopt or refactor onto, not merely a patched dependency. Read the release notes: renovate usually embeds an upstream "Release Notes" section in the PR body (`gh pr view <n> --json body -q .body`); if that's thin, go to the source changelog (`anthropics/claude-agent-sdk-typescript` `CHANGELOG.md`, or `npm view @anthropic-ai/claude-agent-sdk`). Scan for new hooks, tool-runner/permission APIs, session/streaming features, or options uzi could use — and cross-check against the four guardrail layers and the run lifecycle in `ARCHITECTURE.md` for anything that lets uzi simplify or harden them. Merging the bump does NOT implement any of that; it only makes the new SDK available. When you spot something uzi could benefit from, **propose it to the user and offer to file an `enhancement` issue** (`gh issue create`), keeping the improvement issue separate from the dep-bump PR — never implement it inside the release. Report what you found (or "nothing actionable in this bump") in the step 6 follow-ups.

**Triage a red check: is it the PR's fault?** Often it is NOT. The `vulncheck:{api,controller,web}` steps (inside the `lint-*`/`validate-web` jobs — govulncheck on *called* Go stdlib/dep CVEs, npm audit for web) re-evaluate against the LIVE advisory DB, so a branch green when cut goes red when a new CVE lands, with no code change, and `main` fails the same way. Read the failing job's log (step 4). If it's repo-wide (new CVE, toolchain currency), the fix is a dependency bump, not a PR change: land it on `main` directly (`chore(deps): ...`, e.g. re-pin the toolchain digest and bump the lockfile), then `git merge origin/main` into each PR branch (or `gh pr update-branch <n>`) so their checks re-run green.

## 2. Review each PR (gate: no objections)
Spawn one `reviewer` agent per PR, in parallel (one message, multiple Agent calls) for a real batch; for a few small, green, single-issue PRs an inline diff review is fine. Each review must answer, concisely, ending with a one-line verdict `MERGE` / `MERGE-WITH-NOTES` / `OBJECT`:
1. **Milestones.** Read the PRD (`git show origin/agent/issue-N:prds/done/<file>.md` or the merged file); enumerate milestones and mark each IMPLEMENTED / PARTIAL / SKIPPED with backing files. Flag any in-scope milestone with no code. A milestone deliberately *reframed* and documented is not "skipped" — say which it is.
2. **Correctness.** Red flags that block merge (read code, do not re-run tests). If a check is red for a reason unrelated to the diff (step-1 triage: a vuln-gate CVE, a known flake, a path-skipped build showing as non-success), tell the reviewer so explicitly and have them review the code only.
3. **Migration** (if it adds one): is it standalone/safe to renumber filename-only, or does it depend on ordering?

Do not merge anything until every verdict is MERGE or MERGE-WITH-NOTES. Surface any OBJECT to the user.

## 3. Merge orchestration
**Probe cross-PR conflicts first** when several PRs touch a shared, hand-edited file (specs/ai.md, ARCHITECTURE.md, a shared handler). Two PRs editing *different regions* of the same big file three-way-merge cleanly; only overlapping hunks conflict. A throwaway worktree settles it without pushing: `git worktree add -b probe/batch <tmp> origin/main`, then `git merge --no-ff origin/agent/issue-N` for each in your intended order, recording which merge clean and which conflict; `git merge --abort` between probes; remove the worktree after.

**Merge method.** Merge commits (`gh pr merge <n> --merge`) keep both the PR number (`Merge pull request #<pr>`) and the source branch (`agent/issue-<M>`) in the merge subject, which is what lets the CHANGELOG cite the **issue** number and still satisfy the changelog oracle (step 5). Squash puts only the PR number in the subject. Add `--delete-branch`. There is no GitLab-style full-SHA requirement here.

- **Clean, up-to-date PRs**: `gh pr merge <n> --merge --delete-branch`.
- **`BEHIND` PRs (ruleset requires up-to-date, no merge queue)**: a normal merge is refused with "head branch is not up to date with the base branch". Two ways forward, in order of preference:
  1. **Update the branch, let checks re-run, then merge** — `gh pr update-branch <n>` (or `git merge origin/main` on the branch and push). This is the no-bypass path, but it is **sequential**: each merge advances `main`, making the other open PRs stale again, so a batch costs one CI cycle per PR.
  2. **`--admin`** — `gh pr merge <n> --merge --admin --delete-branch` bypasses the up-to-date requirement and merges immediately. Use it ONLY when you have reviewed the PR (verdict MERGE), its checks are green, and you have **confirmed no conflict and no migration collision** (a probe merge or a different-region check). `ci.yml` re-runs on the resulting `main` push as the safety net. `--admin` is a privileged action: an agent session's permission classifier may block it, and it is genuinely the owner's call — if blocked, ask the user to approve it or run it themselves rather than forcing.
- **Conflicting / migration-colliding PRs**: integrate locally in a sibling worktree.
  1. `git merge origin/main`; resolve conflicts (semantic merge, keep both intents).
  2. Renumber any colliding migration to the next free number above the live head: `git mv .../00120_x.sql .../00122_x.sql` (goose reads the version from the filename; grep confirms no code cites the number).
  3. Regenerate sqlc with the PINNED version, not the PATH one (find the pin in `Taskfile.yml` / `api/sqlc.yaml`). CI enforces zero drift via `git diff --exit-code`; a wrong local sqlc version reddens it. Zero diff after regen means git's auto-merge of the generated files was already canonical.
  4. `go build ./...` + `go vet` + the touched package's unit tests; `task gate:web` for web changes. NOTE local Node: `.nvmrc` pins the CI version; a newer local Node can fail unrelated tests (jsdom localStorage) that pass on CI — reproduce any failure on clean `main` before blaming the merge.
  5. Push the branch, let its checks go green (step 4), then merge.

After all merges, `git checkout main && git pull --ff-only origin main` and confirm migrations are distinct (`ls api/internal/store/migrations/ | sort | tail`).

## 4. Watch CI at JOB level (~2-min cadence) — act on a failed job immediately, do NOT wait for the whole run
This step is the skill's own CI watch: after the step-3 merges, and again after the release commit lands, watch **`main`'s `ci.yml`** (and the `kind-smoke.yml` it triggers) to green. **The tag's own runs — `release.yml`, `brew.yml`, their `release`-environment approval gates, and the "gate fires more than once" dance — are NOT this step; they are the `release` agent's job (step 5), documented in `.claude/agents/release.md`.** Poll the run's **jobs** every ~2 min and surface any failed one the moment a tick sees it. A job is a terminal unit: once it is `failure`, the rest of the run still going tells you nothing new about it, and waiting for the whole run to conclude just wastes minutes. **Beware a stale first-tick read:** `gh run view --json jobs` can briefly report `in_progress` jobs with a non-null (often `failure`/`cancelled`) conclusion right after a run starts, so a poller's first tick may print bogus `FAILED` lines that clear on the next tick — re-query live before believing a failure, and confirm the final state at run end.
```sh
# ALL runs for the ref you pushed (main OR the tag) — iterate every one, don't pick just one
gh run list --branch <ref> --limit 10 \
  --json databaseId,workflowName,headSha,status,conclusion \
  --jq '.[]|"\(.workflowName)  \(.headSha[0:7])  \(.status)/\(.conclusion // "-")  run=\(.databaseId)"'
# failed jobs in a run (ignore SKIPPED path-filtered build jobs — a docs-only change skips image builds)
gh run view <run-id> --json jobs \
  --jq '.jobs[]|select(.conclusion!=null and .conclusion!="success" and .conclusion!="skipped" and .conclusion!="neutral")|"FAILED \(.name)"'
```
Run it as a background command that loops on a **120s** sleep, prints failed-job lines each tick, and exits when the run's `status` is `completed`. Do not block a foreground turn on it. Read a failed job's log right then: `gh run view --job <job-id> --log-failed` (or open the job `web_url`). If it's a real defect, fix and re-push; if it's a flake/infra failure, **rerun just the failed jobs** without touching the branch: `gh run rerun <run-id> --failed` (or `gh run rerun --job <job-id>`). `ci.yml`'s `concurrency` cancels older `main` runs when a newer commit lands, so a `cancelled` older run is expected, not a failure.

**The tag's `release.yml`/`brew.yml` runs and their `release`-environment approval gates are the release agent's (step 5).** They stall on a manual approval that `git push` never surfaces, and the gate fires more than once per run — all of that lives in `.claude/agents/release.md` now, so the agent owns it end to end. This step only needs to get `main`'s `ci.yml` green.

**A note on SKIPPED checks.** `ci.yml` has a `changes` job that path-filters the image `build-*` jobs, so a PR that doesn't touch a component shows those builds as SKIPPED. `gh pr checks` renders those as skipped/neutral, not failing — do not read them as red.

**Real failure vs flake.** A timing/resource-sensitive test failing under CI CPU contention is a flake, not a defect — confirm it (assertion is timing-based, failing test unrelated to the diff, same job passed on another run off the same base), then **rerun the single job** instead of re-pushing. File the flake as a `bug` issue. Shared runners can also hit transient infra failures (disk pressure, a registry hiccup) on a SUBSET of runners, so some jobs pass while others fail on the same run — that is the rerun-not-repush case par excellence; keep polling until the run is terminal, because jobs still `in_progress` when you first looked can fail later.

## 5. Hand the release to the `release` agent — do NOT cut it inline
Once **every PR is merged** and **`main`'s `ci.yml` is green** on the tip (steps 3-4), the actual release — version bump + CHANGELOG, the `chore(release)` commit, the tag, the GHCR publish, the `release`-environment approvals (there is more than one), the Homebrew formula, and deploy verification — is the **`release` agent's job**. Dispatch it; do not run the bump/tag/publish/approve steps yourself. **`.claude/agents/release.md` is the authority for those mechanics** (tag order, the fires-more-than-once approval gate, GHCR/chart verification, deploy verify, local CLI refresh) — read it rather than duplicating it here.

**What the skill decides before handing off** (it has the review context the agent lacks):

- **The version `X.Y.Z`.** Patch (`0.46.0` → `0.46.1`) for a maintenance/deps/tooling batch with no user-facing feature or behavior change; minor (`0.46.0` → `0.47.0`) when a product feature ships. Patch tags are in this repo's history (`v0.42.1`, `v0.38.1`), so a small batch does not force a minor. Model B: chart `version` == `appVersion` == the tag.
- **A draft `## [X.Y.Z] - <date>` CHANGELOG section**, folded from `[Unreleased]` (keep an empty `[Unreleased]` on top), Keep-a-Changelog subsections (`### Added`/`### Changed`/`### Fixed`), **no em dashes** (user preference for user-facing content). **Every shipping merge since the previous `v*` tag MUST be cited** by its issue/PR number or short SHA — the oracle `scripts/assert-changelog-covers-release.sh` enforces it (shipping = `api/**`, `agent/src/**`, `controller/**`, `web/src/**`, `deploy/chart/**`, `docs/**`, excluding tests; `docs(...)`-typed and non-shipping-only merges are exempt). Enumerate them: `git log --first-parent --oneline <prev-tag>..HEAD` and check each merge's paths.

**Dispatch** the `release` agent (Agent tool, `subagent_type: release`) in the **background**, with a handoff that carries everything it needs (it cold-starts without your context): the new `X.Y.Z`, the previous `v*` tag, the drafted CHANGELOG section, and **explicit authorization** — this is a user-initiated `/uzi-release`, so the agent may land the bump direct-to-`main`, tag, and **approve the `release`-environment publish gates for the run it creates** (both the image wave and the second `publish-chart` gate, plus `brew.yml`'s). Tell it to report back the published image/chart tags, or to stop and surface the exact error if a push/tag/approval is refused (a permission classifier can block the `--admin` push or the approval POST, in which case the user approves in the forge UI).

**While the agent runs, present the step-6 summary to the user** — that is the point of the handoff: the release publishes in the background while you report what shipped. When the agent reports back, fold its outcome (published tags, or a block) into the summary rather than leaving it open.

## 6. Final summary (always end with this)
Report to the user, concisely:
- **One line up top**: version X.Y.Z and the `release` agent's status — dispatched / publishing / **published to GHCR (images + chart)** / blocked on `<what>`; CI green on the tagged commit; deploy state (or "deploy verification needs cluster access I don't have"). Since the agent runs in the background (step 5), the summary may go out while it is still publishing — say so, and update once it reports back.
- **What shipped, by issue/PR**: one table covering every PR **merged** (PR | issue | what it delivered in 1-2 sentences | review verdict) **and** every issue **closed** from a report-only run (issue | run id | what the run delivered | why it's closeable). Include PRs closed-not-merged (e.g. superseded) with the reason. The point is a single overview of what this release resolved and a terse description of each delivery.
- **Milestone check**: state plainly that nothing was silently skipped, and call out any milestone reframed/deferred with documentation rather than dropped.
- **Behavior changes to flag**: anything that changes existing behavior (surface it even though it shipped), plus post-deploy caveats.
- **Report-only / schedule sweep**: the close-candidate issues you surfaced in step 1 — which you closed (with the user's go-ahead), and which you left open and why (found-nothing survey, partial investigation, still-live). Never list one as closed unless it actually is.
- **Follow-ups**: adjacent-scope gaps the reviews surfaced; SDK-bump adoption opportunities (step 1); offer to file issues.
- **Merge mechanics handled**: migration renumbers, conflict resolutions, sqlc regen, any `--admin` merges — one line.
