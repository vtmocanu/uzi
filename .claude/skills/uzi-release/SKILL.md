---
name: uzi-release
disable-model-invocation: true
description: "Reviews, merges, and releases uzi PRs end to end for this repo (hosted on GitHub). Surveys open PRs, reviews each against its PRD/issue for skipped milestones and correctness, merges them while handling migration-number collisions and cross-PR conflicts, watches GitHub Actions CI at job level, cuts a Model-B version tag by following deploy/README.md, and finishes with a per-PR summary. Use when the user runs /uzi-release, or asks to review-and-merge the uzi PRs, cut or ship a uzi release, bump the uzi version, or deploy uzi. Triggers include uzi-release, release uzi, merge the uzi PRs, cut a uzi version, ship uzi, deploy uzi."
---

# uzi-release

Drive uzi's open pull requests through review, merge, release, and deploy verification, then summarize. This repo is hosted on **GitHub**: use `gh` (which infers the repo from the checkout, so a fork under any owner works unedited; never `glab`/`tea`). CI is **GitHub Actions** (`.github/workflows/`), images and the Helm chart publish to **GHCR** on a `v*` tag.

Do NOT bake step details that live in the repo into your head. Read the source of truth each run: `deploy/README.md` (release + deploy runbook), the workflow files under `.github/workflows/` (`ci.yml`, `release.yml`, `e2e.yml`, `brew.yml`), the destructive-ops rules in `CLAUDE.md`, and `.claude/rules/*.md` for the component you touch.

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
**Watch EVERY workflow run your push or tag generated, not just one — and check each for both failed jobs AND a pending approval.** A push to `main` (and each PR) triggers **CI** (`ci.yml`) and, on `main`/tags, **E2E** (`e2e.yml`); a **`v*` tag additionally triggers `release.yml` AND `brew.yml`**, so one `git push origin vX.Y.Z` fans out to several runs, each of which can fail or stall on an approval independently. Enumerate them by ref (below) and watch all of them to terminal. Poll each run's **jobs** every ~2 min and surface any failed one the moment a tick sees it. A job is a terminal unit: once it is `failure`, the rest of the run still going tells you nothing new about it, and waiting for the whole run to conclude just wastes minutes.
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

**🔴 EVERY TICK, ALSO CHECK FOR A PENDING APPROVAL — a run can STALL on an environment gate, and that is neither a failure nor progress.** Both `release.yml`'s publish jobs and `brew.yml`'s publish job declare `environment: release`, which carries a required-reviewer protection rule — so on a tag there can be MORE THAN ONE run waiting at once. Such a run sits at `status=waiting` with those jobs `waiting/` and **nothing advances until a human approves** — a poll that only looks for failed jobs will wait forever and report "still running". Detect it and surface it the moment a tick sees `status=waiting`:
```sh
R=$(gh repo view --json nameWithOwner -q .nameWithOwner)     # owner/repo of the checkout
gh run view <run-id> --json status --jq .status              # "waiting" => a gate is blocking
gh api "repos/$R/actions/runs/<run-id>/pending_deployments" \
  --jq '.[]|"env=\(.environment.name) can_approve=\(.current_user_can_approve) reviewers=\([.reviewers[].reviewer.login]|join(","))"'
```
Approving is a **publish gate** (it releases the images, chart and formula to GHCR / the tap), so it is the owner's call. Approve a pending deployment when all three hold: the run is one **you** generated (your release push or tag), the approval is what's needed to finish that release, and publishing it **makes sense** (a green build of the thing you just cut) — that covers both the `release.yml` and `brew.yml` gates a tag raises. In a `/uzi-release` run the user initiated, cutting the release IS that authorization. Do NOT rubber-stamp a `waiting` run you did not create — surface it instead. If a session's permission classifier blocks the POST, ask the user to approve in the forge UI. The environment id comes from the same call:
```sh
envid=$(gh api "repos/$R/actions/runs/<run-id>/pending_deployments" --jq '.[0].environment.id')
gh api --method POST "repos/$R/actions/runs/<run-id>/pending_deployments" \
  -f state=approved -f "comment=<why it is safe to publish>" -F "environment_ids[]=$envid"
```
The POST's `--jq` post-filter may print `expected an object but got: string` — that is cosmetic; the approval still lands (the `waiting/` jobs flip to `in_progress`).

**🔴 THE GATE FIRES MORE THAN ONCE PER RUN — approving the image jobs does NOT approve `publish-chart`.** `publish-chart` `needs:` every image job, so it is not even *pending* when you approve the first wave; it enters `waiting` only after the images publish, and then needs its OWN approval. So the run goes `waiting` → (approve) → images build → `waiting` AGAIN → (approve) → chart pushes. A watcher that stops after the first approval, or an operator who walks away once the images go green, leaves the chart unpublished with every image already up. Keep the pending-approval check running until the whole run is `completed`, and expect to approve twice.

**A note on SKIPPED checks.** `ci.yml` has a `changes` job that path-filters the image `build-*` jobs, so a PR that doesn't touch a component shows those builds as SKIPPED. `gh pr checks` renders those as skipped/neutral, not failing — do not read them as red.

**Real failure vs flake.** A timing/resource-sensitive test failing under CI CPU contention is a flake, not a defect — confirm it (assertion is timing-based, failing test unrelated to the diff, same job passed on another run off the same base), then **rerun the single job** instead of re-pushing. File the flake as a `bug` issue. Shared runners can also hit transient infra failures (disk pressure, a registry hiccup) on a SUBSET of runners, so some jobs pass while others fail on the same run — that is the rerun-not-repush case par excellence; keep polling until the run is terminal, because jobs still `in_progress` when you first looked can fail later.

## 5. Cut the release — follow deploy/README.md, do not improvise
Read `deploy/README.md` "Release procedure" and follow it. In brief (verify against the doc each time, it is authoritative):

- **Bump + changelog on one commit.** Edit `deploy/chart/Chart.yaml` `version` AND `appVersion` to the new `X.Y.Z` (must be equal), and fold `CHANGELOG.md`'s `[Unreleased]` into a new `## [X.Y.Z] - <date>` section (keep an empty `[Unreleased]` on top). Match the recent CHANGELOG style (Keep a Changelog: `### Added`/`### Changed`/`### Fixed`). **No em dashes in your own new entries** (user preference for user-facing content).
- **Every SHIPPING merge since the previous tag MUST be cited in the new section.** The oracle is `scripts/assert-changelog-covers-release.sh`; it walks first-parent merges in `<prev-tag>..<ref>` and requires each that touched a *shipping* path (`api/**`, `agent/src/**`, `controller/**`, `web/src/**`, `deploy/chart/**`, `docs/**`, excluding tests) to cite an issue/PR number OR its short SHA in the section. Merges touching only non-shipping paths (`specs/`, `scripts/`, `.github/`, `ideas/`, `*_test.go`, top-level `*.md`) are not required (but cite them anyway if notable). A merge with genuinely nothing to announce is exempt only via a `Changelog: none` line in its own commit (impossible to add retroactively — use the short SHA).
  - **The oracle reads from git objects (`git show <ref>:CHANGELOG.md`), NOT the working tree.** So **commit the release first, then run it against HEAD**: `bash scripts/assert-changelog-covers-release.sh HEAD <prev-tag> <ver>`. Its output tells you exactly what to cite; fix and `git commit --amend` until it prints `OK`.
- **Commit `chore(release): vX.Y.Z`** on `main` (no AI co-author trailer), then **push `main`**.
- **🔴 ORDER: push main → wait for `ci.yml` GREEN → THEN tag.** This is the key GitHub difference from the old GitLab flow. GitHub does **not** run `ci.yml` on tags and cannot `needs:` another workflow, so `release.yml` re-runs ONLY the two release-specific gates (`assert-version`, `assert-changelog`) — the heavy lint/test/build gates are ASSUMED green on the tagged `main` commit. If you tag before `ci.yml` is green on that commit, a real failure publishes anyway. So watch the `main` `ci.yml` run to completion (step 4) before tagging. (`e2e.yml` does run on tags; a green `main` e2e is a bonus.)
- **Tag that commit and push the tag:**
  ```sh
  git checkout main && git pull
  git tag -a vX.Y.Z -m "uzi X.Y.Z"        # == Chart.yaml version/appVersion
  git push origin vX.Y.Z
  ```
  The tag push triggers `release.yml` AND `brew.yml` (plus `e2e.yml`) — watch and approve all of them (step 4), not just `release.yml`. Never tag a commit whose message carries a `[skip ci]`/`[ci skip]` marker — GitHub skips the tag's workflows too, so nothing publishes.
- **Watch `release.yml` (step 4, `--branch vX.Y.Z` or by run id) — and expect it to PAUSE for approval.** The jobs are `prep`, `assert-version`, `assert-changelog`, then the gated `publish-{api,web,controller}` + `publish-agent` (matrix: base, jvm), and `publish-chart` (LAST, after every image, so a chart never references a not-yet-pushed image tag). The three prep/assert jobs run immediately, then the run sits at `status=waiting`: the publish jobs are behind the `release` environment's required-reviewer gate and **do not start until you approve** (step 4's pending-approval check + POST). This is the normal release flow, not a hang. Images land at `ghcr.io/<owner>/uzi/{api,web,controller,agent-<t>}:X.Y.Z` (+ `:<short-sha>`); the chart at `oci://ghcr.io/<owner>/uzi/uzi:X.Y.Z`. Confirm publish by the jobs going green and, if you can, that the tags exist in GHCR.
  - **Homebrew formula publishing is a SEPARATE workflow (`brew.yml`) that ALSO triggers on the tag and ALSO waits on the `release` environment gate — approve it too** (step 4). A failure there does not unpublish the images+chart, but the release is not fully done until it is green; watch it as one of the tag's runs, not an afterthought.

## 6. Verify the deploy
The deploy layer is mid-migration (see the note at the top). Where ArgoCD tracks the published chart, a `0.*`-range app deploys the new chart on its next reconcile. Verification needs cluster access; if you don't have the relevant `kubectl` contexts, say so and stop here rather than guessing a rollout happened. When you do have access, follow `deploy/README.md` "Verify a live deploy" — it is authoritative on which cluster holds the ArgoCD `Application` vs the workloads, and on the hard-refresh to force an immediate reconcile. Pass literal `--context <name>` flags (zsh does not word-split an unquoted `$var`). Done = the api/web/controller deployments on the new tag, pods Running + Ready.

## 7. Final summary (always end with this)
Report to the user, concisely:
- **One line up top**: version X.Y.Z tagged and published to GHCR (images + chart), CI green; deploy state (or "deploy verification needs cluster access I don't have").
- **Per-PR table**: PR | issue | what it implemented (1-2 sentences) | review verdict.
- **Milestone check**: state plainly that nothing was silently skipped, and call out any milestone reframed/deferred with documentation rather than dropped.
- **Behavior changes to flag**: anything that changes existing behavior (surface it even though it shipped), plus post-deploy caveats.
- **Follow-ups**: adjacent-scope gaps the reviews surfaced; offer to file issues.
- **Merge mechanics handled**: migration renumbers, conflict resolutions, sqlc regen, any `--admin` merges — one line.
