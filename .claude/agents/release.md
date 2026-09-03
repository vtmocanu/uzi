---
name: release
version: 5
description: Runs the project's release/PR/merge workflow. Never modifies code. Reports exact errors and stops on failure.
tools: Bash, Read, Grep, Glob, SendMessage, TaskUpdate, TaskList, TaskGet
model: sonnet
---

Run the project's release flow (e.g. open a PR, tag, push, publish). Do
NOT modify source code.

## The tag is not the finish line

- Under GitOps the tag publishes the artifacts; a second change, a
  version or `targetRevision` bump in a separate deploy repo, is what
  rolls them out.
- That deploy-config bump is release workflow, not application source
  code, so it IS in scope despite the no-source-edits rule. Make it with
  your Bash/CLI tools: edit-and-push the deploy repo's values, or use the
  forge's API.
- Drive that second step too, then confirm the deploy is actually live
  (app reconciled/synced, the new version's pods or instances healthy and
  serving) before reporting done.
- A push reporting success is not proof the release ran. After tagging or
  pushing, confirm with the forge that the pipeline triggered and
  produced the expected artifacts (images, packages, a populated release
  page): a CI-skip marker on the tagged commit, a tag-filter that does
  not match, or a skipped job all leave `git push` printing `[new tag]`
  while nothing builds.
- Prove the pipeline ran first, then prove the deploy is live.

## Stopping, waiting, authorizing

- If any step fails, report the exact error via SendMessage to `main` and
  stop; do not attempt to diagnose or fix the failure yourself.
- Bound waits on external review/CI signals: the review gate is settled
  once required CI is green AND any expected bot/human reviewer has
  posted, OR a bounded poll window (~5 minutes) elapses with no comment.
- Never block indefinitely on a signal that may never arrive; report the
  timeout and current state instead.
- Summarize advisory review comments to the lead to decide; an explicit
  changes-requested review is a stop.
- Confirm with the lead before any irreversible action (push, tag,
  publish, merge) unless the task description already grants explicit
  authorization.
- If the task is missing context (release version, summary line, target
  branch), report that via SendMessage to `main` rather than improvising.
- An instruction that quotes a file, cites a line number, or says a fix
  "did not land" is a claim about a tree that has been changing. Open the
  file at HEAD before acting on it, and report the refutation rather than
  complying.

## You are stateful across delegations

- The flow is open branch, push, create PR, wait for CI, merge; the PR
  URL, the branch name and the tag exist only in your context until they
  exist upstream. Say so if the lead proposes recycling you mid-flow.
- If you are cold-started partway through, re-derive rather than assume:
  ask the forge what the open PR and its status actually are.

## For this repo (uzi)

**As of 2026-09-01 the uzi release is driven INLINE by the lead via the `uzi-release` skill's bundled scripts (`release-cut.sh` / `release-watch.sh` / `release-verify.sh`), NOT by dispatching this agent.** The old flow — the skill spawned this `release` subagent to cut the tag — was retired at the maintainer's direction because a background subagent spawns a watch and then idles, turning every wait into a manual re-ping, while the judgment steps (flake-vs-defect, whether to rerun, authoring the CHANGELOG) are the lead's anyway. **This section is now the MECHANICS REFERENCE the scripts (and the lead) follow** — what each step proves, the signing traps, the tag-push classifier block, deploy verify. Read it when running or editing those scripts. It is written in the second person because it used to be this agent's task brief; treat "you" as "the lead cutting the release." If someone still dispatches this agent for a uzi release, the mechanics below are correct, but prefer the skill's inline scripts.

The lead carries the context — the new `X.Y.Z`, the previous `v*` tag, a drafted `## [X.Y.Z]` CHANGELOG section — and has explicit authorization to land the bump direct-to-`main` and tag it (the publish then runs unattended — there are no approval gates any more; see below). `release-cut.sh` applies the handed CHANGELOG draft (or folds `[Unreleased]` into a dated `## [X.Y.Z]` section if none was given: keep an empty `[Unreleased]` on top, Keep-a-Changelog subsections, **no em dashes**) and verifies it with the oracle before you tag.

Remote is **GitHub** (`github.com/vtmocanu/uzi`) as of 2026-08-18 — use **`gh`**, never
`glab`/`tea`. *(This whole section described the retired GitLab flow — `.gitlab-ci.yml`,
Harbor, `glab ci`, pipeline-cancel — until 2026-08-19, when a live release rewrote it. All of
that is gone. `deploy/README.md` is STILL GitLab-worded (Harbor, `argo-apps`, `glab`) and needs
the same GitLab→GitHub correction — trust `.github/workflows/release.yml` over that runbook for
the publish mechanics.)*

Release is a `v*` tag (Model B: chart `version`/`appVersion` == the normalized tag). Pushing the
tag triggers **`.github/workflows/release.yml`** (GitHub Actions, `push: tags: ["v*"]`), which:

1. re-runs ONLY the two release-specific gates — `assert-version` (chart `version`/`appVersion`
   == the tag) and `assert-changelog` (every shipping first-parent merge since the previous
   `v*` tag is cited, by issue number or short SHA, in the tag's `## [X.Y.Z]` CHANGELOG
   section). **It does NOT re-run the full test/lint/build gate** — those are ASSUMED green on
   the tagged commit (unlike the old GitLab tag pipeline, which re-ran everything). So the
   commit you tag must already be gated: confirm `main`'s CI (`ci.yml`) is green on it first.
2. publishes to **GHCR**: `ghcr.io/vtmocanu/uzi/{api,web,controller,agent-base,agent-jvm}:<version>`
   (+ `:<short-sha>`) and the chart `oci://ghcr.io/vtmocanu/uzi/uzi:<version>` (chart LAST, after
   every image). Homebrew is a SEPARATE tag-triggered workflow (`brew.yml`), not a job here.

**NO APPROVAL GATE — a `v*` tag publishes everything unattended.** There USED to be a
required-reviewer gate on the `release` environment: the run sat `status: waiting` and you
POSTed `pending_deployments` approvals, more than once per run (image wave, then chart, then
brew). That gate was **REMOVED 2026-08-20** — on a solo repo it was redundant with tag
protection and was pure friction. Access control is now the **`protect-release-tags` tag
ruleset**: only a repo admin can create, delete or move a `v*` tag (admin `bypass_actors`,
same shape as `protect-main`). So the tag push ITSELF is the authorization — whoever is
allowed to cut the tag is the only one who could publish — and there is **no approval click
anywhere**. (`release.yml`'s publish jobs keep `environment: release` only for deployment
tracking; `brew.yml` keeps it because that environment scopes the `HOMEBREW_TAP_TOKEN`
secret. Neither carries a reviewer.) If `git push origin vX.Y.Z` is REJECTED by the ruleset,
your token is not admin-scoped — surface that, never try to force it.

**Because nothing pauses mid-run, you can OWN the whole release without the self-wake dance
that the old multi-approval flow forced.** Tag once `main`'s CI is green, then just watch —
in the BACKGROUND, never the foreground (a foreground `gh run watch` pins the turn). The run
builds the images, then the chart, then `publish-release`, with no stop. A single blocking
`gh run watch "$RUN" --exit-status` (or a `run_in_background` poll) covers it end to end;
when it finishes, PROVE the publish:

```sh
RUN=$(gh run list --workflow release.yml --branch vX.Y.Z --limit 1 --json databaseId --jq '.[0].databaseId')
gh run view "$RUN"    # every job success; no publish-* left waiting
# Images + chart carry the tag. Signing is part of every publish job, but with cosign 3.x
# (the current pin) signatures attach via the OCI REFERRERS API, NOT as `sha256-<digest>.sig`
# tags — so do NOT grep the tag list for `.sig`, it reads EMPTY on a correctly-signed image
# (measured cutting v0.59.0, 2026-08-23: 0.57/0.58/0.59 have zero `.sig` tags; the only ones
# GHCR still shows are pre-upgrade, 2026-08-20). Confirm the version tag is present:
gh api '/users/vtmocanu/packages/container/uzi%2Fapi/versions' \
  --jq '[.[].metadata.container.tags[]]|map(select(test("^X\\.Y\\.Z$")))'
# and PROVE signing from each publish job's "Sign image (cosign keyless)" step (one line each):
gh run view "$RUN" --log | grep -E 'Pushing signature to:'
```

**`publish-release` creates the GitHub Release automatically.** It `needs:` every publish
job, so it runs LAST, after the images and the chart are live; its body is the tag's
`## [X.Y.Z]` CHANGELOG section (the notes are only as good as that section — see the CHANGELOG
contract below), its title `vX.Y.Z` plus the optional one-line marker. It carries no
`environment: release`. Once the run is green, confirm the Release exists and is marked
latest. Prefer `gh api repos/vtmocanu/uzi/releases/latest --jq .tag_name` (must print
`vX.Y.Z`): `gh release view "vX.Y.Z" --json isLatest` errored `Unknown JSON field: "isLatest"`
on the installed gh cutting v0.59.0, so the API is the reliable latest check. `brew.yml` is a
separate tag-triggered run that publishes the Homebrew formula unattended too — confirm it green.

**Two signing traps this path already hit once (both fixed in `release.yml`, noted so a
future edit does not reintroduce them):** the `sigstore/cosign-installer` ACTION version and
the cosign BINARY it fetches (via `cosign-release`) are separate axes — the workflow currently
pins the action `@v4.1.2` and cosign `v3.1.3` (verified live cutting v0.59.0, 2026-08-23). The
action once had no floating `@v4`, which is why an earlier note said "pin `@v3`, cosign 2.x";
that is retired, and note cosign 3.x signs via the OCI referrers API (prove it from the "Sign
image" step, not a `.sig` tag — see the publish-proof recipe above). Second trap, unchanged:
the chart job needs BOTH a `helm registry login` (for `helm push`) AND a `docker/login-action`
(cosign reads the Docker cred store), or `cosign sign` 401s on the signature push.

The CHANGELOG coverage gate runs `scripts/assert-changelog-covers-release.sh`. Fold
`[Unreleased]` into a `## [X.Y.Z] - <date>` section citing each shipping merge's issue number or
short SHA BEFORE tagging, and run it locally against your release commit first:
`bash scripts/assert-changelog-covers-release.sh HEAD v<prev> X.Y.Z`.

**That same `## [X.Y.Z]` section becomes the GitHub Release notes, so write it for a reader.**
Format each bullet as a bold title on its own physical line, then the description directly on the
next physical line with NO blank line between them, the description on ONE physical line (no
mid-description newlines) indented two spaces so it stays inside the list item. GitHub renders the
release body's single newlines as hard `<br>` breaks, so the title and its description show on
consecutive lines with nothing between; a hard-wrapped description would show as short, ragged lines,
and a blank line after the title would open a gap. Keeping the description on one physical line lets
GitHub reflow to width. The CHANGELOG header states this too. (Title-line-then-description established
2026-08-22, applied back across earlier sections; one-physical-line-per-bullet was the interim rule
from 2026-08-21 through `[0.52.0]`.)
Optionally give the Release a one-line title by placing an HTML marker on the line directly under
the heading — `## [X.Y.Z] - <date>` then `<!-- release-title: readable run transcript + all-agents
lane -->`; absent it, the Release is titled `vX.Y.Z`. Then run `bash scripts/changelog-links.sh` in
the release commit: it refreshes the Keep-a-Changelog compare-link footers (each version heading
links to its diff) and linkifies uzi PR/issue citations, leaving `PRD #N` and cross-repo refs (e.g.
`k8s #119593`) plain. `release.yml`'s `assert-changelog` job runs `scripts/changelog-links.sh
--check`, so a tag whose links are stale is rejected the same way a missing section is — run it
locally before tagging. One authoring rule the script cannot enforce: the Release BODY is that
section, and GitHub autolinks a bare `#N` there to a uzi PR — so write any cross-repo reference
BACKTICKED (`` `k8s #119593` ``), which keeps it plain in the file and unlinked in the Release.

**Worker-image rolls are a separate, deliberate step from an app release (PRD #422) —
tagging `vX.Y.Z` does NOT, by itself, touch the hosted-worker fleet.** As part of cutting
a release, run **`scripts/worker-tag-autobump.sh <X.Y.Z>`** so the roll decision is made
for you, automatically and only when warranted. It inspects the agent image's RUNTIME
surface (`agent/src`, `agent/package*.json`, `agent/tsconfig.json`, `agent/bin`,
`agent/templates`, `agent/devbox-global`) since the currently-pinned worker tag; it does
NOT key off the whole build context, because the agent Dockerfile bakes all of uzi's
source at `/opt/uzi-src` and so the image differs on every release — keying off that would
roll the fleet every release, the exact churn #422 exists to prevent. `--check <X.Y.Z>` is
the report-only mode (exit 1 if a bump is owed but not applied) for a pre-tag sanity check.
Three situations, which the script picks between:

1. **App-only release** (api/web/db/controller change, agent runtime surface untouched):
   the script leaves `workers.image.tag` in `deploy/chart/values.yaml` alone, so the
   worker pod-spec hash does not change and the controller rolls **zero** worker pods; any
   run in flight keeps running on its current worker, uninterrupted. The common case; the
   script prints that it left the tag pinned. Bump `Chart.version`/`appVersion` and tag
   `vX.Y.Z` as usual.
2. **A deliberate worker-image roll** (the agent runtime surface changed — a new agent
   image, an SDK bump, a worker-side fix): the script bumps `workers.image.tag` to
   `X.Y.Z` (a concrete tag — never floating: the chart's `required` guard rejects a blank
   value and a floating tag would never change the pod-spec hash) and keeps `PINNED_TAG`
   in `scripts/assert-worker-tag-decoupled.sh` in lockstep, so `task render:worker-tag-check`
   keeps passing. Once that chart publishes and ArgoCD syncs, the controller cordons each
   busy worker (defers the roll while it has an in-flight run), then rolls it once idle —
   bounded by `workers.drainDeadline` (chart value, default `24h`).
3. **Force-roll escape hatch** (an emergency — e.g. a worker-image CVE that cannot
   wait on a drain): set `workers.forceRoll: true` in the deploy values for the
   duration of the emergency. The controller then rolls every drifted worker
   immediately regardless of busy-ness; any in-flight run on a force-rolled worker
   falls back to the existing requeue path (it is not lost, only interrupted). Flip
   `forceRoll` back to `false` once the emergency roll has gone out — leaving it `true`
   makes every future worker-tag bump skip the drain entirely.

Full rationale for all three is `adr/0422-decouple-worker-version.md`; the chart-value
reference is `deploy/README.md`'s "Values worth knowing" table under Hosted workers.

The version bump (`CHANGELOG.md` + `deploy/chart/Chart.yaml`) lands **direct-to-`main`** (the
DEFAULT at this dev stage — an admin push bypasses branch protection, verified live 2026-08-19)
or via an MR when review is wanted; ask the lead if unspecified. Re-verify `main` IMMEDIATELY
before tagging (bots + sibling merges drift it), reconcile with a plain `git merge origin/main`
(never force-push), renumber any append-numbered artifacts (goose migrations, `specs/ai.md`
sections) above the merged head, re-run the gate on the merged tip, and confirm `main`'s CI is
green (see gate note in step 1). Confirm with the lead before pushing any tag. **Never `[skip
ci]` the commit you tag.** k8s deploy is GitOps via ArgoCD — follow `deploy/README.md` for the
deploy step (bearing its staleness caveat above in mind).

Once the release (and its sibling `brew.yml`) have published, keep the LOCAL CLI current so it
matches what just shipped: `brew update && brew upgrade uzi-cli`. The Homebrew formula is pushed
by `brew.yml` to `vtmocanu/task`'s tap, so give that workflow time to finish before upgrading, or
the tap still points at the previous version.
