---
name: uzi-release
disable-model-invocation: true
description: "Reviews, merges, and releases uzi MRs end to end for this repo (vtmocanu/uzi on gitlab.example.com). Surveys open MRs, reviews each against its PRD/issue for skipped milestones and correctness, merges them while handling migration-number collisions and cross-MR conflicts, watches CI at job level, cuts a Model-B version tag by following deploy/README.md, verifies the ArgoCD rollout on dev-cluster, and finishes with a per-MR summary. Use when the user runs /uzi-release, or asks to review-and-merge the uzi MRs, cut or ship a uzi release, bump the uzi version, or deploy uzi to k8s. Triggers include uzi-release, release uzi, merge the uzi MRs, cut a uzi version, ship uzi, deploy uzi to k8s."
---

# uzi-release

Drive uzi's open merge requests through review, merge, release, and k8s-deploy verification, then summarize. This repo is `vtmocanu/uzi` on `gitlab.example.com` (GitLab): use `env -u GITLAB_TOKEN glab ...` (an exported `GITLAB_TOKEN` 401s on this host). Never use `gh`/`tea`.

Do NOT bake step details that live in the repo into your head. Read the source of truth each run: `deploy/README.md` (release + deploy runbook), the destructive-ops rules in `CLAUDE.md`, and `.claude/rules/*.md` for the component you touch.

## 0. Guardrails first
- NEVER `docker compose -p uzi down -v` and never glob `uzi-` containers (see `CLAUDE.md` "Destructive operations"). This skill touches git, glab, and kubectl only.
- Work on `main` in the repo root worktree; make a sibling worktree for any branch integration (`git worktree add ../uzi-issue-NNN -B agent/issue-NNN origin/agent/issue-NNN`). Never check the root worktree out onto another branch.
- `main` is protected against agents but the maintainer (repo owner) may push it directly, which is how a release lands. If a push is refused, stop and tell the user.

## 1. Survey the open MRs
```sh
env -u GITLAB_TOKEN glab mr list --repo vtmocanu/uzi
```
For each MR pull metadata, the changed-file list, the closing issue/PRD, and its head-pipeline status:
```sh
enc=vtmocanu%2Fuzi
env -u GITLAB_TOKEN glab api "projects/$enc/merge_requests/<iid>/changes" \
  | python3 -c "import sys,json;[print(('NEW ' if c['new_file'] else 'DEL ' if c['deleted_file'] else 'MOD ')+c['new_path']) for c in json.load(sys.stdin)['changes']]"
```
Fetch every MR branch so review agents can read whole files: `git fetch origin 'refs/heads/agent/issue-N:refs/remotes/origin/agent/issue-N' ...`.

**Detect migration collisions now.** Two MRs adding the same `NNNNN_*.sql` under `api/internal/store/migrations/` will both land as distinct files (git sees no conflict) and brick strict-goose boot. Find the live head (`ls api/internal/store/migrations/ | sort | tail -1`) and note which MRs add migrations at or below it. They get renumbered at merge (step 3).

**Check in-flight uzi runs before you start.** `uzi run list`. uzi's auto-CI-fix opens a `ci_fix` run for every failed MR pipeline (often parked at the plan-approval gate, `awaiting_approval`) and can auto-open an MR mid-release (e.g. a flake fix on a re-created branch). Read a run's plan first (`uzi run get <id>`, `uzi run logs <id>`) so you don't re-diagnose what it already found; `uzi run reject <id> -m "<why>"` any whose fix you supersede.

**Triage a red head-pipeline: is it the MR's fault?** Often it is NOT — the absolute vuln gates (`vulncheck:api`/`vulncheck:controller` govulncheck on *called* Go stdlib/dep CVEs, `vulncheck:web` npm audit) re-evaluate against the LIVE advisory DB, so a branch green when cut goes red when a new CVE lands, with no code change, and `main` fails the same way. Read the failing job trace (step 4). If it's repo-wide (new CVE, toolchain-currency), the fix is a dependency bump, not an MR change: land it on `main` directly (`chore(deps): ...` — e.g. re-pin the CI `golang:1.26@sha256:...` digest to the current go1.26.x found via `crane config golang:1.26 --platform linux/amd64`, and bump the npm lockfile), push `main`, then `git merge origin/main` into each MR branch and push so their pipelines re-run green. If reviews are already dispatched the branch head will advance — reviewers should re-derive at the new head.

## 2. Review each MR (gate: no objections)
Spawn one `reviewer` agent per MR, in parallel (one message, multiple Agent calls). Each agent must answer, concisely, ending with a one-line verdict `MERGE` / `MERGE-WITH-NOTES` / `OBJECT`:
1. **Milestones.** Read the PRD (`git show origin/agent/issue-N:prds/done/<file>.md`); enumerate milestones and mark each IMPLEMENTED / PARTIAL / SKIPPED with backing files. Flag any in-scope milestone with no code. A milestone deliberately *reframed* and documented is not "skipped" — say which it is.
2. **Correctness.** Red flags that block merge (read code, do not re-run tests). If the pipeline is red for a reason unrelated to the diff (step-1 triage: a vuln-gate CVE, a known flake), tell the reviewer so explicitly and have them review the code only, not the red CI.
3. **Migration** (if it adds one): is it standalone/safe to renumber filename-only, or does it depend on ordering?

Do not merge anything until every verdict is MERGE or MERGE-WITH-NOTES. Surface any OBJECT to the user.

## 3. Merge orchestration
**Probe sequential conflicts first** in a throwaway worktree (no push): `git worktree add -b probe/batch <tmp> main`, then `git merge --no-ff origin/agent/issue-N` for each in your intended order, recording which merge clean and which conflict; `git merge --abort` between probes. Remove the worktree after.

- **Clean MRs**: merge via the API with the FULL 40-char head SHA (short SHA silently no-ops; plain `glab mr merge` can 422 on a mergeable MR, per `CLAUDE.md` tech-stack notes):
  ```sh
  full=$(git rev-parse origin/agent/issue-N)
  env -u GITLAB_TOKEN glab api --method PUT "projects/$enc/merge_requests/<iid>/merge" \
    -f "sha=$full" -f "should_remove_source_branch=true"
  ```
  Merge the migration-adding MRs before any MR that must renumber against them.
- **Conflicting / colliding MRs**: integrate locally in a sibling worktree.
  1. `git merge origin/main`; resolve conflicts (semantic merge, keep both intents).
  2. Renumber any colliding migration to the next free number above the live head: `git mv .../00120_x.sql .../00122_x.sql` (goose reads the version from the filename; grep confirms no code cites the number).
  3. Regenerate sqlc with the PINNED version, not the PATH one (find it in `.gitlab-ci.yml` / `api/sqlc.yaml`, currently a `go run github.com/sqlc-dev/sqlc/cmd/sqlc@<pin> generate` from `api/`). CI enforces zero drift via `git diff --exit-code`; a wrong local sqlc version reddens it. Zero diff after regen means git's auto-merge of the generated files was already canonical.
  4. `go build ./...` + `go vet` + the touched package's unit tests; `task gate:web` for web changes. NOTE local Node: `.nvmrc` pins the CI version; a newer local Node can fail unrelated tests (jsdom localStorage) that pass on CI — reproduce any failure on clean `main` before blaming the merge.
  5. Push the branch (non-force merge on top of its own history), let its MR pipeline go green (step 4), then merge with the full-SHA API call.

After all merges, `git pull --ff-only origin main` and confirm migrations are distinct (`ls api/internal/store/migrations/ | sort | tail`).

## 4. Watch CI at JOB level (~2-min cadence) — act on a failed job immediately, do NOT wait for the pipeline
Poll the pipeline's **jobs** every ~2 min and surface any failed one the moment a tick sees it. A job is a terminal unit: once it is `failed`, the rest of the pipeline still running tells you nothing new about that job, and waiting for the whole pipeline to reach `success`/`failed` before reacting just wastes minutes on a slow, contended runner. So **diagnose and fix/retry a failed job as soon as it appears**, in parallel with the pipeline's other jobs finishing — that is the whole reason this poll is job-level, not pipeline-level.
```sh
env -u GITLAB_TOKEN glab api "projects/$enc/pipelines/<id>/jobs?per_page=100" \
  | jq -r '.[]|select(.status=="failed")|"FAILED \(.stage)/\(.name) job=\(.id)"'
# and a one-line status summary so you see progress without the full list:
env -u GITLAB_TOKEN glab api "projects/$enc/pipelines/<id>/jobs?per_page=100" \
  | jq -r 'group_by(.status)|map("\(.[0].status)=\(length)")|join("  ")'
```
Run it as a background command that loops on a **120s** sleep, prints the failed-job lines at each tick, and exits when the pipeline status is terminal. Do not block a foreground turn on it. When the user asks to "check CI every 2m for failed jobs", this is the loop they mean. If a job fails, read its log (`env -u GITLAB_TOKEN glab ci trace <job-id>` or the job web_url) right then; if it's a real defect, fix and re-push; if it's a flake/infra failure, retry the single job (below) without touching the branch. **A pipeline showing `running` with one job already `failed` is a pipeline you should already be acting on** — the merge gate needs every job green, so that one failure is going to block regardless of what the others do.

> **Infra note seen live (2026-08-17 release):** shared runners can run out of disk (`No space left on device` in kaniko snapshot, nix install, or Postgres `could not extend file … SQLSTATE 53100`). This hits a SUBSET of runners, so some jobs on the same pipeline pass while others fail, and it is **not** the MR's fault — it is the job-level, retry-not-repush case par excellence. Retry each disk-failed job (`.../jobs/<id>/retry`); they land on a healthy runner and pass. Because it is per-runner, a pipeline can show more failed jobs on a second look than the first (jobs that were still `running`/`created` fail later), so keep polling until terminal even after you have retried the ones you saw.

**Real failure vs flake.** A timing/resource-sensitive test failing under CI CPU contention is a flake, not a defect — confirm it (assertion is timing-based, failing test unrelated to the diff, same job passed on another pipeline off the same base), then **retry the single job** instead of re-pushing: `env -u GITLAB_TOKEN glab api --method POST "projects/$enc/jobs/<job-id>/retry"`. File the flake as a `bug` issue. The **tag pipeline re-runs the full gate**, so a flake can block the `publish:*` stage even after every MR merged green — retry the job, do not re-tag. Cancel a redundant `main` pipeline to free runners for the tag: `... --method POST "projects/$enc/pipelines/<id>/cancel"`.

## 5. Cut the release — follow deploy/README.md, do not improvise
Read `deploy/README.md` section **"Release procedure"** and follow it. In brief (verify against the doc each time, it is authoritative):
- Bump `deploy/chart/Chart.yaml` `version` AND `appVersion` to the new `X.Y.Z` (must be equal), and fold `CHANGELOG.md` `[Unreleased]` into a new `## [X.Y.Z] - <date>` section. Match the recent CHANGELOG style (no em dashes in your own new entries).
- Every shipping merge since the previous tag MUST be cited in the new section. Run the oracle before tagging: `bash scripts/assert-changelog-covers-release.sh main` (or `... HEAD <prev-tag> <ver>`). A merge with no issue number is cited by its short SHA; a merge with nothing to announce is exempt only via a `Changelog: none` line in its own commit (impossible to add retroactively — use the SHA).
- Commit `chore(release): X.Y.Z` on `main` (no AI co-author trailer), push `main`, then annotated tag `git tag -a vX.Y.Z -m "uzi X.Y.Z"` and `env -u GITLAB_TOKEN git push origin vX.Y.Z`. Never tag a `[skip ci]` commit (the tag pipeline gets skipped too — see the doc's trap).
- Watch the tag pipeline (step 4). The critical publish jobs are `publish:assert-version`, `publish:assert-changelog`, the `publish:{api,web,controller,agent}` images, and `publish:chart`.

## 6. Verify the k8s deploy
ArgoCD's control plane runs on the **argo-cluster** cluster; the workload runs on **dev-cluster**. The app auto-tracks `0.*`, so a published chart deploys on the next reconcile.
```sh
# app state (Synced + revisions should show the new version + the release SHA; Healthy)
kubectl --context argo-cluster -n argocd get application uzi -o json \
  | python3 -c "import sys,json;d=json.load(sys.stdin)['status'];print(d['sync']['status'],d['sync'].get('revisions'),d['health']['status'])"
# force it immediately (hard refresh; automated+selfHeal then syncs)
kubectl --context argo-cluster -n argocd annotate application uzi argocd.argoproj.io/refresh=hard --overwrite
# confirm the rollout on the workload cluster
kubectl --context dev-cluster -n uzi get deploy -o custom-columns='NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image'
```
Pass literal `--context <name>` flags — zsh does NOT word-split an unquoted `$var`, so a `CTX="--context x"` string becomes one malformed token. Done = api/web/controller deployments on the new tag, pods Running + Ready.

## 7. Final summary (always end with this)
Report to the user, concisely:
- **One line up top**: version X.Y.Z is live on dev-cluster (Healthy), CI all green.
- **Per-MR table**: MR | issue | what it implemented (1-2 sentences) | review verdict.
- **Milestone check**: state plainly that nothing was silently skipped, and call out any milestone that was reframed/deferred with documentation rather than dropped.
- **Behavior changes to flag**: anything that changes existing behavior (surface it even though it shipped), plus post-deploy caveats.
- **Follow-ups**: adjacent-scope gaps the reviews surfaced; offer to file issues.
- **Merge mechanics handled**: migration renumbers, conflict resolutions, sqlc regen — one line.
