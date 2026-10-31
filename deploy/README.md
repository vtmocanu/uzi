# Releasing and deploying uzi

> **⚠ Migration note (updated 2026-08-20).** uzi moved from GitLab + Harbor to **GitHub** on
> 2026-08-18. The release now publishes via **GitHub Actions** (`.github/workflows/release.yml`)
> to **GHCR** (`ghcr.io/vtmocanu/uzi/…`), driven with `gh`, and its publish jobs run UNATTENDED on a
> `v*` tag (no approval gate; access control is the `protect-release-tags` tag ruleset, see the
> Release procedure below). **`.claude/agents/release.md` is the current, authoritative release
> procedure; trust it and `release.yml` over any GitLab/`glab` wording still in this file.**
>
> **The registry the cluster pulls from is now GHCR, and the Harbor references below were
> corrected on 2026-08-20** (verified live: the deployed pods run `ghcr.io/vtmocanu/uzi/*`, and
> the ArgoCD `uzi` Application's chart source is `ghcr.io/vtmocanu/uzi` with `targetRevision: 0.*`;
> the per-cluster values relocated into the private `argo-apps` GitOps repo). The remaining
> cluster-topology values further down (CIDRs, Infisical folder scopes, the CNPG image, DNS) are
> deliberate **public-repo placeholders** (`example.com`, `dev-cluster`, RFC 5737 CIDRs) and some
> have not been re-verified since the migration; confirm against the live cluster and the private
> `argo-apps` values before relying on them.
>
> **RC-first release train (ADR-1265).** Releases are now cut as candidates
> (`vX.Y.Z-rc.N`) by default and promoted to stable only when the maintainer chooses — see the
> Release procedure section below and [ADR-1265](../adr/1265-rc-release-train.md). A deployment
> that wants to run candidates tracks the Helm `targetRevision` range `0.*-0` instead of the
> `0.*` shown above.

The release + deploy runbook for uzi (PRD #52). Two deploy topologies:

- **compose (laptop)** — the MVP path, unchanged. `docker compose up` on
  `127.0.0.1:8080`, real `./.env` secrets. See the repo `README.md` /
  `ARCHITECTURE.md`. Workers still run here (`docker compose --profile agent`).
- **k8s (dev-cluster)** — uzi deployed to the **dev-cluster** Kubernetes cluster
  via **ArgoCD**, GitOps, the way the rest of the platform deploys. This file is about
  that path: cutting a release and getting it live.

The chart is `chart/` (an umbrella chart: web + api + the CloudNativePG `cluster`
subchart). Per-cluster values live in the private GitOps repo, not this repo:
`argo-apps:apps/uzi/values/dev-cluster.yaml`. The in-repo `values/` holds
only `ci-render.yaml` (sanitized CI render stand-in) and `kind-smoke.yaml` (KinD
smoke). CI is **GitHub Actions**: `ci.yml` validates every PR + `main`, and the `v*`-tag
`release.yml` publishes the images + OCI chart to GHCR; ArgoCD (`argo-apps`, `apps/uzi/`)
deploys. uzi follows the Model-B release shape.

## What deploys where

| | compose (laptop) | k8s (dev-cluster) |
| --- | --- | --- |
| Brought up by | `docker compose up` | ArgoCD (auto-sync) from `argo-apps` `apps/uzi/` |
| web / api images | built locally | `ghcr.io/vtmocanu/uzi/{web,api}:<tag>` |
| controller / agent images | n/a (compose runs no controller) | `ghcr.io/vtmocanu/uzi/controller:<tag>`, `ghcr.io/vtmocanu/uzi/agent-{base,jvm}:<tag>` (PRD #58 M6) |
| Postgres | `postgres:17` container | CNPG `Cluster` (`postgres-uzi-cluster`, 1 instance, `standard`) |
| Secrets | `./.env` | Infisical (`/uzi` folder) + CNPG-generated `-app` creds |
| Public URL | `http://127.0.0.1:8080` | `https://uzi.example.com` (ingress-nginx, `*.example.com` wildcard TLS) |
| Worker | `docker compose --profile agent` | laptop workers unchanged; **hosted workers ship but are OFF** — see [Hosted workers](#hosted-workers-prd-58) |

The ArgoCD `Application` is **multi-source** (example-app precedent): the released
chart from the public GHCR OCI registry + the per-cluster values from the argo-apps GitOps
repo, so operational config (`apps/uzi/values/dev-cluster.yaml` there) can change
without cutting a new chart release — that path is proven: the M6 Infisical-scope fix
reached the running deploy through it, with no chart release. Wiring lives in
`argo-apps` `apps/uzi/{prj.uzi.yaml,app.uzi.yaml}`, delivered by MR !294
(**merged 2026-07-16**; first live deploy = uzi `0.2.0`).

**dev-cluster auto-tracks the chart version** (changed 2026-08-11). The OCI-chart
source's `targetRevision` is a **bounded semver range `0.*`**, not an exact pin, so
ArgoCD resolves the newest published `0.x` chart from GHCR on each reconcile: a
`v0.Y.Z` release deploys with **no `targetRevision` bump** — step 3 below is gone
for this cluster. The `0.*` bound keeps a future major (`1.0.0`) a deliberate human
change, and uzi's plain `vX.Y.Z` tags carry no prereleases (a `-0` suffix in the
range would be needed to include any). This is a **dev-cluster** choice: a future
stage/prod cluster should keep the exact-version pin so its deploy stays an
explicit, reviewable git change (PRD #52 Decision 3). The
api is a **hard singleton** (`replicas: 1` + `Recreate`) — see `ARCHITECTURE.md`
for why (poller/sweeper/Slack in-memory state + goose boot migration with no
advisory lock).

## Versioning: Model B

`chart/Chart.yaml` `version` == `appVersion` == the release git tag (`vX.Y.Z`).
Both images follow `appVersion` (the chart leaves `api.image.tag`/`web.image.tag`
unset), so one version is the whole release coordinate: images at
`ghcr.io/vtmocanu/uzi/{api,web}:<version>` and the chart at
`oci://ghcr.io/vtmocanu/uzi/uzi:<version>`. The tag workflow
**asserts** `version == appVersion == tag` (`release.yml`'s `assert-version` job) and fails
the whole publish on a mismatch — a lagging `Chart.yaml` blocks the release
atomically, it does not ship a half-version.

## Release procedure

Standard releases use `.github/workflows/release-auto.yml` on main, with manual
dispatch and opt-in nightly RC attempts. The shared scripts below
remain the local fallback. [ADR-3512](../adr/3512-release-automation.md) records the
accepted authorization boundary and staged recovery work.

Before the first live dispatch, the maintainer configures and verifies:

| Control | Required configuration |
|---|---|
| RC App | Repository-only installation, contents write, no workflows permission |
| Stable App | Separate repository-only installation, contents write, no main bypass |
| `release-rc` | Main-only Environment; `RELEASE_APP_ID` variable, `RELEASE_APP_KEY` secret |
| `release-stable` | Main-only; same variable/secret names, required maintainer reviewer, prevent self-review off for the solo-maintainer case |
| `RELEASE_DISPATCHERS` | Repository variable, comma/space-separated exact GitHub logins |
| `RELEASE_NIGHTLY_ENABLED` | Repository variable; only exact `true` enables nightly RC attempts, unset/false pauses them |
| Main review rules | Pull requests/checks; admin and RC App bypass |
| Main integrity rules | Deletion/non-fast-forward; admin bypass only |
| Stable-tag creation | `v*` excluding `v*-rc.*`; admin and stable App bypass |
| RC-tag creation | `v*-rc.*`; admin and RC App bypass |
| Tag integrity | All `v*` updates/deletion/non-fast-forward; admin bypass only |

The existing `release` publisher Environment is unchanged. The workflow never
creates or modifies these settings. Verify that the RC App cannot create stable
tags, either App cannot update/delete tags or force/delete main, the stable App
cannot write main, and rejecting one ref rejects the whole atomic push. A dry-run
or source review alone is not evidence that server rules are active.

For an explicitly authorized RC, dispatch `release-auto.yml` with `kind=rc`.
Optional `version` chooses a base; `abandon_train=true` needs a higher version.
`supersede` must name the latest failed/missing-publication candidate. It waives
only that publication refusal, never pin, coverage, oracle, metadata or adoption
checks. Manual RC dispatch preserves empty-delta respins; the cutter still decides
releasability. Review the run summary and its exact parent/release/tag identifiers.

Only the human dispatches `kind=stable`, supplying the newest `rc_tag` and its full
peeled `rc_sha`, then approves `release-stable`. Agents never perform either act.
The request expires 24 hours after the original run creation, including approval
wait. Promotion uses the candidate and ignores current-main CI and
advancement. The summary links first-parent main changes observed during preparation.

Codex lock changes in the planned range require the adoption read and a local cut.
On abandon the range starts at the highest stable, so a previously adopted change
can still block automated abandon until a local cut. There is no CI attestation
parser or model access. Before any local fallback, finish/stop CI release work,
inspect remote main/tags and resolve interrupted publication on the same tag.

The prepared bundle is validated in a separate job without repository checkout
before the App token is minted. RC metadata and tag push atomically, without force.
A lost push receipt is investigated against the prepared tag/SHA, never by cutting
a replacement. Prepare and push refuse when the run attempt is greater than one: re-dispatch
instead, after inspecting remote refs when a push receipt is uncertain. Never
rerun prepare or push in the same run. Watch and report may rerun against the
exact existing tag/SHA. The inspection bundle is retained for one day; watch
and report recovery use the existing tag and stored outputs, not a new bundle
or a second push. Each watch rerun starts its own bounded recovery operation;
retry budgets cannot replenish within one operation. The local watch/verify scripts remain available for
manual same-tag recovery. Only a confirmed publication and `release-verify.sh` success mean
done; deployment health is a separate check. Problems use one `Release automation
failures` tracking issue; expected no-ops remain in the summary.

Prepare streams the cutter's output and logs tip SHA, CI run/attempt/state changes,
elapsed time and its deadline. Failure summaries name the phase and planned tag,
with bounded uncited commit/PR identities; unavailable fields are `null`.
A green `report` job confirms incident delivery, not a successful release: the
failed prepare or watch job keeps the run red. Receipt/publication uncertainty
fails watch; an issue-delivery failure fails report.
An unconfirmed push receipt stops watch before publication recovery and
verification, so automatic publication retries do not run in that branch.
Inspect remote refs, then use local watch/verify against the same tag/SHA.

Keep a tip intended for release eligible for CI. When a recognized skip
instruction leaves exact main-push proof absent, manual preparation and the
local cutter's point check refuse immediately with `skip-ci-tip`. Scheduled
attempts stay unheld and keep waiting for a newer tip within the existing
budget; progress logs show `skip_ci_tip=true`. A newer green CI-bearing tip
can be cut in that same operation. If the budget expires while the selected
tip still lacks proof because of a skip instruction, the scheduled attempt
defers with `skip-ci-tip`. Planning no-ops retain priority. There is no
parent-proof exemption for docs or PRDs: their changed text has not passed its
own checks. Automation does not dispatch CI, create a repair commit or select
an older tip.

Nightly attempts are scheduled for 04:47 Europe/Bucharest using GitHub's native
timezone support, after the local maintenance window, aiming to cut near 05:00
when on time. They are
off until the user decides to activate and the maintainer sets
`RELEASE_NIGHTLY_ENABLED=true`. Unset it or set it to `false` to pause nightly
attempts; manual dispatch still works. Enablement is rechecked at job boundaries,
so a not-yet-pushed attempt may stop there, possibly red with `nightly-disabled`.
Cancel an in-flight run explicitly for an immediate stop. Disabling the whole workflow
also disables manual dispatch. Scheduled authorization pins repository, main ref
and event/run metadata, without treating the scheduler as an allowlisted human.

The shared cutter's read-only plan decides nightly eligibility: at least one
non-metadata shipping first-parent change since the newest open-train RC or
previous stable, with releasable notes. Notes-only and exempt-only ranges can be
reported no-ops; they do not wait for CI or create a bundle. Failed previous
publication and other blocked plans still refuse. Manual RC respins preserve
the cutter's existing empty-delta behavior.

The provisional push cutoff is strictly before 13:00 on the original run's
Bucharest calendar date. A queued run does not gain a fresh window tomorrow.
Scheduled tip waiting uses the lesser of three hours and the remaining time
until 12:50, reserving a provisional 600 seconds from the existing cutter timeout,
not a measured cut duration. An on-time 04:47 attempt can wait until 07:47; a late
10:47 attempt can wait until 12:50. Waiting holds a runner under the 20-job cap.
The prepare ceiling stays 210 minutes (180 waiting, ten cutting, 20 overhead).
Manual waits remain 7200 seconds and their cutter timeout remains 600 seconds;
the job ceiling stays 210 minutes. Push/watch ceilings remain 15/125 minutes.
The key job checks the window before
minting and independently again immediately before the atomic update, validating
numeric epochs and the Bucharest offset. Creation timestamps tolerate 60 seconds
of clock skew; the 13:00 push cutoff has no grace period. A missed window stops
with `missed-target` and no push; a tip not green in its budget is reported
separately. Main races never cause automatic rebasing or an older-tip fallback.
Expected no-ops stay in the summary; misses and failures update the tracking
issue. Once a tag is pushed, recovery ignores the cutoff and pause flag.
Verification recorded after the 14:00 local target produces a late warning,
retaining verified success without opening an extra incident. These times are
best-effort targets, subject to Actions delays and the shared non-cancelling queue.

The wider window accommodates observed schedule delays of roughly six hours;
[ADR-3512](../adr/3512-release-automation.md) cites the public E2E and release
run timestamps. This is not a guaranteed delay. Late attempts may run during
daytime merges, so exact-tip CI may take longer to settle within the same cap.

Manual tip selection is bounded at 120 minutes plus 10 minutes for cutting.
Publication watching retains its 60-minute bound within the 120-minute recovery
allowance. One publication watcher owns the existing per-workflow retry budgets
and records a bounded attempt journal, including failing jobs/steps and retry
reservations/results. The first failed job's detail is retained. The journal
keeps every observed attempt and explicitly counts omitted detail when it reaches
32,768 source bytes; unavailable evidence is not proof of recovery.
Release-commit main CI is report-only, with one observation and no automatic reruns. Automatic
recovery was deliberately excluded: publication uses strict inherited coverage,
the next RC needs a newer CI-bearing tip, and rerun isolation would change main
cancellation behavior. Protected CI opts into two infrastructure-only tagged
KinD retries and one targeted `assert-tag-smoke` rerun after real smoke succeeds.
Build/install/readiness/product checks, mixed/unknown failures and cancellation
are not retried. Existing publication budgets stay separate and never reset.
First-week contention measurements remain a rollout check.
Read-only journal lookup failures do not stop publication observation; retries
still require fresh complete evidence. Incident audits are capped at 64 rows and
8,192 encoded bytes with explicit counts, while the prepare summary stays full.
Main-CI observation errors cannot replace the publication result, but an in-flight
API request can still consume the existing watcher job ceiling; no new timeout
or transport isolation is added.
Terminal journal write failures preserve the publication result and label any
surviving attempt records incomplete. Retry requests still require a saved reservation.
KinD retries require matching tagged/controller KinD workflow definitions;
assertion recovery requires both KinD and release definitions to match.
Older candidates after workflow updates, or RC tips whose workflow changed during
the wait, need manual exceptional recovery; normal watching and verification stay
available. All writes in recovery mode, including ordinary publication retries,
require unchanged remote tag identity and no superseding stable. A watcher-start
monotonic 120-minute backstop forbids new writes only,
with no remote cancellation or extra wait window. The shared 60-tick publication
window and 125-minute job ceiling remain; transport hangs use that job ceiling.
For deliberate local same-tag exceptional recovery, opt in with
`--recover-tag-smoke` plus `--journal`, `--operation`, `--repository`, `--sha`,
`--kind` and the exact remote `--tag-oid`. Finish or stop the CI operation first.
The default local watcher remains publication-only. No recovery path cuts a tag.
The procedures below are the local path.

**Releases are cut RC-first** (ADR-1265, PRD #1265). `release-cut X.Y.Z`
(`.agents/skills/uzi-release/`) cuts a release **candidate** `vX.Y.Z-rc.1` by default: it
publishes the five images and the chart at that version, opens a GitHub Release flagged
pre-release (never latest), and does not publish the stable Homebrew formula (since PRD #1378 an
RC publishes only the separate, opt-in `uzi-cli-rc` formula). A stable `vX.Y.Z` is a
**promotion** of the in-flight candidate — built from the candidate's own commit, never from
whatever `main` has moved on to since — cut in lockstep with the next candidate by
`release-cut Y.Z.W --promote`. The verbs, terse (`.agents/skills/uzi-release/SKILL.md` owns the
full operational detail): no flag cuts the next candidate, or refuses with the facts if a lower
base's candidate is still in flight; `--promote` promotes the in-flight candidate to stable,
then cuts the next candidate; `--promote-only`, given the in-flight base, promotes it and stops
(no next candidate, `main` untouched, one push: the stable tag); `--skip-promote` abandons the
in-flight base and cuts a new
`vX.Y.Z-rc.1` candidate (renaming the open section and assembling legacy `[Unreleased]` plus pending fragments), never a
stable tag; `--stable` is the old one-step model, refused while a candidate is in
flight. **Push order on a promote: the stable tag, then `main`, then the next candidate's
tag** — the candidate's changelog-coverage gate needs the new stable tag visible first. A
hotfix onto an already-promoted stable is a separate, manual procedure — see **Hotfix**, below.
Full design: [ADR-1265](../adr/1265-rc-release-train.md).

Within a single cut — whether a candidate or a promotion — it is still **two steps** on
dev-cluster (a `--promote-only` promotion is the exception: nothing lands on `main`, so it is step 2
alone, the tag push) (the third — the ArgoCD `targetRevision` bump — is gone now that this cluster
auto-tracks `0.*`, or `0.*-0` for a deployment that opts into candidates instead (ADR-1265
D8); it survives below only as the rollback/other-cluster note). Only the tag push publishes
anything. Step 1 (the chart-version
bump) can land **two ways** — a **direct-to-`main` commit** or an **MR**. **Direct-to-`main`
is the default and preferred at this early dev stage**; the MR way is there for when you want
the change reviewed. Step 2 (the tag) is identical either way. The repository admin role can bypass the
main ruleset for the release metadata push.

**Auto-tracking does not mean instant.** ArgoCD only notices the new chart on its next
reconcile poll (default ~3 min) — and a **normal** refresh reuses the cached resolved
version, so to deploy the moment the tag workflow finishes publishing, force a **hard**
refresh (it invalidates the manifest cache, re-resolving `0.*` against GHCR):

```sh
kubectl -n argocd annotate application uzi \
  argocd.argoproj.io/refresh=hard --overwrite --context argo-cluster
# equivalently, if you have the argocd CLI logged in: argocd app get uzi --hard-refresh
```

The Application is already `automated: {prune, selfHeal}`, so the hard refresh both
re-resolves the version and triggers the sync; no separate `argocd app sync` is needed.

Why direct-to-`main` is preferred here: it skips the MR gate cycle, so `ci.yml` runs the full
suite **once** on `main` instead of twice (MR, then merge), which matters because every gate run
is an independent roll against flaky tests. The trade is that the release commit gets no
independent pre-land review; its scope is release metadata on code every feature PR already
gated: chart/worker metadata, assembled CHANGELOG notes and incorporated fragment deletions.

**Release publication proves CI coverage without repeating the full gate.**
`assert-ci-coverage` requires successful main-push CI and smoke on the tagged SHA or its
ancestors across at most two strict metadata-only hops. It rejects known failures on the
tagged SHA. Tagged-tree metadata checks validate the chart and bundled changelog; the
real tagged KinD smoke must succeed before chart publication. Agent runtime bases and
registry layers are reused when their inputs match; release image identity checks still run.

1. **Wait for the exact main tip, then cut and review the release metadata.**
   Work in the existing main worktree. Fetch `origin/main` and fast-forward with
   `git merge --ff-only origin/main`; wait for that exact tip's CI and KinD smoke to succeed
   before running `.agents/skills/uzi-release/scripts/release-cut.sh X.Y.Z`.
   Pushing a release commit while parent CI is running cancels the parent's run and
   destroys its inheritance evidence. The cutter refuses a non-green exact tip.
   Follow the [release skill](../.agents/skills/uzi-release/SKILL.md) for candidate and
   promotion options, review `git show HEAD`, then push main. Full main CI still runs
   on the release commit, but the tag can follow immediately using the enforced parent
   coverage. A manually prepared metadata bump can also land through an MR; publication
   requires the same exact-SHA or strict-parent coverage proof.

   **Pending notes come from independent fragments plus legacy `[Unreleased]`.** Ordinary
   PRs create a new lowercase UUID-v4 fragment before the PR exists, initially citing the
   relevant issue; see [fragment guidance](../changelog.d/README.md). Keep existing legacy
   bullets until the next cut. The cutter groups categories, preserves legacy order within
   each, appends fragments in C filename order, and consumes incorporated paths in the
   release commit. Later RCs append new blocks without changing frozen shipped blocks;
   promotion-only promotes the candidate's own commit, leaving main's pending fragments.
   `--changelog-file` refuses pending fragments before mutation. Preflight failures preserve
   inputs, but preparation failure or interruption can leave notes/deletions staged or a
   completed local commit. Inspect HEAD, index and worktree; use the printed starting SHA
   and exact owned paths for deliberate recovery, with no automatic destructive rollback.
   Linkification and previous-stable date reconciliation still apply. See
   [ADR-2957](../adr/2957-changelog-fragments.md) for the classifier and safety boundaries.

   **Re-check the CHANGELOG just before tagging, whichever way you landed it.**
   `release.yml`'s `assert-changelog` job fails the publish if any merge since the previous stable tag
   changed shipping code without being cited in the new version's section. Run it yourself
   first — `bash scripts/assert-changelog-covers-release.sh main` — because the failure mode
   it guards is invisible in the release commit's own diff (MR or direct): **anything that
   merges while you are preparing the release rides into it without ever passing through
   it.** That has happened three times (0.11.7 and 0.11.8 shipped with entries still under
   `[Unreleased]`; issue #60 landed inside 0.11.10's window with no entry at all). A merge
   with genuinely nothing to announce is exempted with a `Changelog: none` line in its
   commit message. Fragment touches do not provide this exemption; the oracle still
   requires the shipping commit's identity in the assembled release section.

2. **Tag the reviewed release commit and push the tag.**

   ```sh
   git tag -a v0.1.0-rc.1 -m "uzi 0.1.0-rc.1" HEAD  # use the tag printed by release-cut
   git push origin v0.1.0-rc.1
   ```

   The push triggers `release.yml`: it asserts the version equality (`assert-version`) and
   CHANGELOG coverage (`assert-changelog`), CI coverage and tagged metadata, then builds + pushes each image
   (`ghcr.io/vtmocanu/uzi/{api,web,controller,agent-base,agent-jvm}:<tag>` + `:<short-sha>`) and
   the OCI chart (`oci://ghcr.io/vtmocanu/uzi/uzi:<tag>`, published LAST). Homebrew is a separate
   `v*`-triggered run (`brew.yml`).

   > **Tag publication remains unattended after authorization.** The release-auto
   > stable review happens before its App creates the tag. Configured tag rulesets
   > admit the corresponding App and admin local fallback; the existing `release`
   > publisher Environment has no required reviewer. (`publish-*` jobs keep `environment: release` only for
   > deployment tracking; `brew.yml` keeps it to scope the `HOMEBREW_TAP_TOKEN` secret. Neither
   > carries a reviewer.) After `git push origin v0.1.0`, the run builds the images, then the
   > chart, then the GitHub Release, with no pause:
   >
   > ```sh
   > RUN=$(gh run list --workflow release.yml --branch v0.1.0 --limit 1 --json databaseId --jq '.[0].databaseId')
   > gh run view "$RUN"    # every job success; no publish-* left waiting
   > ```
   >
   > Watch the run in the BACKGROUND (the multi-arch builds take a few minutes), then PROVE it
   > published: `gh run view "$RUN"` all-green AND the GHCR packages carry the new tag plus a
   > cosign `.sig`. **Never `[skip ci]` the commit you tag** — GitHub Actions honours the marker
   > on tag pushes too, so `git push origin v0.1.0` prints `* [new tag]` and nothing runs.
   > `.claude/agents/release.md` carries the full, current procedure.

3. **~~Point ArgoCD at the new version~~ — no longer needed on dev-cluster.**
   This cluster auto-tracks (`targetRevision: 0.*` in `apps/uzi/app.uzi.yaml`), so a
   published `0.Y.Z` chart deploys on ArgoCD's next reconcile with no argo-repo change.
   Force it immediately with the hard refresh shown under "Release procedure" above. The
   manual bump survives only in two cases:

   - **Rollback** (below) — pin `targetRevision` back to a known-good exact version.
   - **A future stage/prod cluster**, if one keeps the exact-version pin (PRD #52 Decision
     3: an explicit, reviewable deploy). There, the old flow applies verbatim — bump
     `targetRevision` to the new chart version, direct-to-`main` (default) or via an MR,
     and ArgoCD auto-syncs once the bump is on that repo's `main`.

**Rollback** (auto-tracking changes this — a plain git revert no longer moves the deploy):
pin `targetRevision` in `apps/uzi/app.uzi.yaml` from `0.*` **back to the last-good exact
version** (e.g. `0.28.0`) and push; ArgoCD re-syncs to that older, still-published chart (no
image rebuild; old versions stay in GHCR). Because `0.*` would otherwise re-advance to the
bad version, resuming auto-tracking means cutting a **fix release** (`0.Y.Z+1`) first, then
setting `targetRevision` back to `0.*`.

**Hotfix** remains a manual procedure. Start a worktree at the previous stable patch
(for example `v0.85.2` for `v0.85.3`), cherry-pick the already-merged fixes, add a cited
CHANGELOG section and bump both chart version fields. This manual off-main preparation
must incorporate the cherry-picked fixes' notes and deliberately consume their fragment
paths in the same release commit; a pending fragment alone is not release coverage. Run
`scripts/worker-tag-autobump.sh 0.85.3` before committing the metadata so an agent runtime
fix also advances the worker pin. Run the affected component gates
and `task check:release-metadata` locally. Since this off-main tree has no main-push CI,
use an annotated stable patch tag with an explicit exception:

```sh
git tag -a v0.85.3 -m "v0.85.3" -m "CI-Coverage: hotfix-uncovered" HEAD
```

The gate reports **UNVERIFIED hotfix: no full CI evidence**. This preserves the existing
hotfix path; it does not claim those local checks are CI proof. The tag must target an
off-main descendant of the immediately previous stable patch on its first-parent chain.
A lightweight tag or missing marker cannot use this exception. In general, a failed
main-push run on the tagged SHA or any visited coverage-chain commit vetoes publication;
tagged metadata, version/changelog, worker-image checks and real tagged
KinD smoke before chart publication remain required. Push under the usual tag authority.
See [ADR-1265](../adr/1265-rc-release-train.md) (D4) for the separate coverage window.

## Platform-admin prerequisites (one-time)

These are **not** MRs to this repo — they are platform config an admin runs once
before the first deploy. Each mirrors an existing example-app step.

1. **GHCR publish uses the workflow's own token — no stored registry credential.**
   `release.yml` pushes images + the OCI chart to `ghcr.io/vtmocanu/uzi/*` with the
   run's `GITHUB_TOKEN` (`packages: write`, scoped to this repo's GHCR namespace) plus
   GitHub OIDC for cosign keyless signing, so there is no Harbor robot, no
   GitLab↔Harbor integration, and no `HARBOR_USERNAME`/`HARBOR_PASSWORD` to provision.
   The credential-isolation property Decision 2 wanted still holds: PR builds run
   `--no-push` and never touch the registry, and the push-capable token exists only on
   the `v*`-tag run, whose creation is restricted by the split rulesets above. So an
   agent-authored PR that edits a workflow file cannot reach a push credential.

2. **Protected release refs.** Apply the split rulesets above before live CI use.
   RC and stable Apps have distinct creation authority, while only admin retains
   tag update/deletion and force/delete-main bypass. The stable App has no main
   bypass. The worker PAT still lacks workflow scope and release authority. The
   existing publisher Environment has no second approval gate; stable review is
   before the separate stable App creates its tag.

3. **GHCR packages are anonymous-pullable, so the cluster needs no image pull secret.**
   The `ghcr.io/vtmocanu/uzi/*` images and chart are public. The only pull secret still
   needed is for the CNPG Postgres image, which stays on the internal mirror (see
   item 6). (Replaces the old push-only Harbor robot on `uzi/*`.)

4. **ArgoCD Helm OCI repo for `ghcr.io` — registered WITHOUT credentials.** Because the
   chart package is anonymous-pullable, register the `ghcr.io` Helm OCI repo on the
   ArgoCD instance with `type: helm`, `enableOCI: true`, and **no** secret
   (`argocd repo add ghcr.io/vtmocanu/uzi --type helm --enable-oci=true`, or the
   equivalent `repo` Secret). This is an instance-level step, not part of this repo.
   Without some covering repo entry ArgoCD cannot resolve the chart. (Replaces the old
   Harbor `oci-helm-creds` prefix credential.)

5. **ArgoCD git access to the `argo-apps` GitOps repo** (for the `$values` source):
   already covered, because the per-cluster values now live in `argo-apps` itself (the
   uzi-side file was removed so the uzi repo could go public without exposing cluster
   topology), and ArgoCD already reads `argo-apps` as its app-of-apps source. The uzi
   repo is no longer an ArgoCD source at all, so no token for it is needed.

6. **Infisical `/uzi` folder + an operator grant on the app-secrets project.**
   uzi's own runtime secrets live at `/uzi` in the **app-secrets** project (slug
   **`app-secrets`**, envSlug **`dev`**) — the convention every app in that project
   follows (`example-app` → `app-secrets`/`dev`:`/example-app`, `sibling-app` →
   `app-secrets`/`dev`:`/sibling-app`). Populate `JWT_SECRET`, `UZI_SECRET_KEY`
   (required; the api refuses to boot without them) plus any optional
   `UZI_SEED_*` / Slack / OIDC keys the chart maps (`chart/values.yaml`
   `api.secretEnv`). With the app/worker images now anonymous-pullable from GHCR (item 3),
   the only image pull secret still sourced from the k8s-clusters project
   (`example-project`, envSlug `prod`, `/registry-robot`) is the one for the internal-mirror
   CNPG Postgres image, which is why `chart/values.yaml` `infisicalList` intentionally has two
   different scopes.

   > **The cluster's operator identity needs membership on the `app-secrets` project.**
   > Infisical MI permissions are **per-project**: without the grant, auth succeeds
   > and the sync then 403s. The cluster's machine identity on dev-cluster already
   > had `example-project` (its ingress-nginx/cnpg values use it) but **not**
   > the app-secrets project — granted 2026-07-16 (example-app/sibling-app run on argo-cluster, whose
   > MI already had it). Add it under **app-secrets → Access Control → Identities**.

   > **History (M5→M6, 2026-07-16).** The M5 draft of this step asserted `/uzi`
   > lived in `example-project`/`prod` and that "no new operator grant is needed".
   > **Both were wrong.** First sync 404'd (`Folder with path '/uzi' in
   > environment 'prod' was not found`), so `uzi-secrets` was never created and the
   > api CrashLooped on an empty `JWT_SECRET`. Fixed by pointing `infsec-uzi` at
   > `app-secrets`/`dev`. Verify the project slug + folder before a first sync on
   > any new cluster — a wrong scope 403/404s the InfisicalSecret.

7. **DNS `uzi.example.com`.** dev-cluster's ingress-nginx serves a
   `*.example.com` default wildcard cert, so no per-host TLS block /
   cert-manager annotation is needed. Only residual action: confirm/add the DNS
   record if it is not already wildcard-covered.

## Confirm before the first live deploy (pre-M6 gates)

The chart renders and lints today (CI `helm_chart` job). A live deploy to
dev-cluster also rests on cluster facts the render cannot check; these were
**confirmed on-cluster 2026-07-15** and the resulting values are already baked
into `argo-apps:apps/uzi/values/dev-cluster.yaml` (re-verify if the cluster changes):

- **`api.networkPolicy.probeCIDRs` = `192.0.2.0/24`** — the dev-cluster node
  InternalIP CIDR (all nodes observed on `192.0.2.x`). The api NetworkPolicy is
  default-deny ingress; kubelet health probes originate from the **node IP** (host
  network), which no `podSelector` can match. Antrea (the dev-cluster CNI) subjects
  probe traffic to NetworkPolicy, so an unset `probeCIDRs` would drop the api
  **startup probe → the pod never goes Ready → CrashLoop**. The node CIDR is kept
  **out of `TRUSTED_PROXIES`** so a host-network probe source can never be used to
  spoof XFF.

- **Pod CIDR = `10.244.0.0/16`** (`kube-controller-manager --cluster-cidr`), so
  `TRUSTED_PROXIES` is set to it. The api's rate-limit `ClientIP` walk trusts XFF
  only from a `TRUSTED_PROXIES` CIDR; both real in-cluster hops — the web pod and
  the pod-networked ingress-nginx controller (observed `198.51.100.153`) — fall
  inside `10.244.0.0/16`, so the api resolves the real browser IP and per-IP
  rate-limit buckets do not collapse. (An earlier draft used `100.64.0.0/10`, a
  wrong guess at the pod net — corrected here from the on-cluster CIDR.)

- **Antrea enforces NetworkPolicy** — confirmed (`antrea-agent` running on every
  node; Antrea enforces by default). The in-cluster XFF-spoofing closure (a rogue
  pod on the shared cluster — `coder.example.com` runs arbitrary dev code —
  cannot reach the api directly) rests on this. If enforcement were ever disabled,
  only `TRUSTED_PROXIES` (pod net) would remain between a rogue pod and a forged
  `X-Forwarded-For`.

- **`postgresql:16.4` present in `cloudnative-pg`** — confirmed via `crane ls`
  (`16.10` also mirrored; `16.3` is **not**). The CNPG cluster pins
  `registry.example.com/cloudnative-pg/postgresql:16.4` (also what a sibling app
  uses on the cluster). PG16 is a fleet-consistency choice, not an operator ceiling — the
  dev-cluster CNPG operator 1.23.5 supported up to PG17 (the fleet now runs 1.30.1).

- **Forge egress.** The api dials `gitlab.example.com` from the cluster (issue
  sync, MR creation); `FORGE_ALLOWED_BASE_URLS` is set to it. Confirm dev-cluster
  egress to `gitlab.example.com` is open. Anthropic egress is **not** needed
  in-cluster this release — runs execute on laptop workers.

## Worker onboarding (laptop workers)

**Corrected 2026-07-17 (PRD #58 M6): there IS a published worker image now.** This
section used to open "per Decision 5 there is no published worker image and no worker
deployed to dev-cluster". The first half stopped being true when M6's CI began
publishing `agent-base` / `agent-jvm` on `v*` tags; the second is still true, because
hosted workers are shipped but disabled (see [Hosted workers](#hosted-workers-prd-58)).
PRD #52's Decision 5 was about *that* release's scope, not a standing rule.

Laptop users run their own worker against the deployed api, exactly as before —
hosted workers are an addition, never a replacement:

```sh
# from a repo checkout on the laptop
docker compose --profile agent build      # build the worker image locally
# point the worker at the deployed api (Settings → Workers issues the join token)
UZI_API_URL=https://uzi.example.com docker compose --profile agent up
```

The worker is outbound-only and authenticates with a **Bearer join token** (no
cookies, no CSRF), so it works through the TLS ingress unchanged. The token is
shown once at issuance. See `docs/worker-setup.md` for the full procedure and
`ARCHITECTURE.md` for the trust model.

## Hosted workers (PRD #58)

**Shipped and OFF.** The chart carries the whole feature — the worker namespace and
its RBAC/quotas/policies, the controller Deployment, the api's hosting switch — and
`argo-apps:apps/uzi/values/dev-cluster.yaml` still sets `workers.enabled: false`. Nothing about
hosted workers renders until that flips. Compose is untouched either way.

**What runs.** A `uzi-controller` Deployment in the release namespace: the only
component in uzi holding a kube credential (the api holds none and must never hold
one). It polls the api outbound, listens on nothing, and materializes each hosted
worker's Deployment + Secret + PVCs into the `uzi-workers` namespace, which contains
nothing else by design.

### Expanding existing worker data volumes

New persistent L workers request 50Gi of `/data` (#2013). Existing PVCs keep their
capacity; the controller creates missing claims but does not resize existing
`/data` claims or revert an operator's expansion. M remains 25Gi, and run-bound
workers use `workers.ephemeralWorkerDataSize` (40Gi by default). Expand an existing
run-bound data PVC to 40Gi using the same procedure while retaining its contents.

Before upgrading, raise any per-cluster overrides of both worker tiers'
`limitRange.maxPVCStorage` to at least 50Gi. The controller refuses to start if a
configured ceiling cannot admit the largest preset. Recalculate each tier's
`quota.requestsStorage` for its worker count, maximum data size, 20Gi `/nix`, and
the effective Docker `dindDataSize`. Defaults cover 20 restricted workers at
`20 x (50 + 20) = 1400Gi` and 10 Docker workers at
`10 x (50 + 20 + 20) = 900Gi`. For example, a 30Gi DinD override needs 1000Gi for
10 Docker workers. The isolated lane needs 600Gi for ten run-bound workers
(`10 x (40 + 20)`), with a PVC ceiling of at least 40Gi. Its chart guard also checks
run-bound data overrides. Allow budget for existing claims and their requested growth.

The `l-highmem` worker size (#3383) requests 24Gi and is limited to 32Gi of memory.
Before using it, raise any per-cluster override of `limitRange.max.memory` on all
three tiers (`workers.limitRange`, `workers.docker.limitRange`,
`workers.isolatedLane.limitRange`) to at least 32Gi; the chart default is now 32Gi
(was 20Gi). CPU max, `maxPVCStorage` and all quotas are unchanged, and the
controller has no boot-time memory check, so a lower ceiling leaves `l-highmem`
pods inadmissible. Quotas are shared budgets sized on `l`; each `l-highmem` worker
consumes more of them, and a fleet is not guaranteed to fit. Check node
allocatable memory (24Gi plus sidecars) and namespace quota headroom first. A quota
hit shows as the worker Deployment's `ReplicaFailure`. Existing workers are not
resized.

For each existing L or run-bound worker's data PVC, inspect its request and StorageClass:

```sh
kubectl -n '<worker-namespace>' get pvc '<worker>-data' -o yaml
kubectl get storageclass '<storage-class>' -o yaml
```

The StorageClass must have `allowVolumeExpansion: true`, and its CSI driver must
support expansion. If the request is below the desired size and quota and storage
capacity permit the increase, expand the PVC in place. This example targets a persistent L worker;
use `40Gi` for a run-bound worker and never shrink a larger existing claim:

```sh
kubectl -n '<worker-namespace>' patch pvc '<worker>-data' --type=merge \
  -p '{"spec":{"resources":{"requests":{"storage":"50Gi"}}}}'
kubectl -n '<worker-namespace>' get pvc '<worker>-data' -o yaml
kubectl -n '<worker-namespace>' describe pvc '<worker>-data'
```

Check the reported capacity, events and conditions, then verify the mounted
filesystem with `df -h /data` in the worker container. Filesystem expansion can
complete online or require a pod restart, depending on the driver. If a restart
is required, wait for the worker to be idle and follow the normal controlled pod
roll; retain its data PVC. See [Kubernetes volume expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims).

If expansion is unsupported, recreate a worker only when it is idle, has no
custody hold and holds no unpublished work. Deleting its data PVC destroys the
bare clone, tracking refs and recovery data; an idle worker can still hold
retained work. Do not delete or recreate a custody-held data volume.

The larger volume adds headroom; it does not bound build-cache growth. Existing
cache-cap and disk-pressure safeguards remain enabled. Long-run acceptance for
#2013 still requires a post-release measurement.

### Cross-check capacity

`workers.crossCheckSlots` defaults to 1 for persistent and ephemeral hosted
workers. The chart renders the controller's `UZI_WORKER_CROSS_CHECK_SLOTS`,
which is passed through `RenderConfig` into pod `WORKER_CROSS_CHECK_SLOTS`.
The controller accepts integers 0–16 and refuses invalid input. Explicit
`workers.crossCheckSlots: 0` renders quoted `"0"` unconditionally, disabling
the lane; it does not use the run-cap zero-to-default behavior. Compose
instead exposes `WORKER_CROSS_CHECK_SLOTS` directly (default 1).

Changing the slot count changes the desired spec hash and uses the existing
worker roll process. It does not increase preset CPU or memory. A compatible
worker can check its own Claude lead's plan on Codex without an additional
pod when its lane has space; otherwise existing capability-gap/saturation
provisioning and caps apply. A cordoned worker may finish its pinned checker,
subject to maintenance fences. Active children block ephemeral lease entry,
teardown and reaping, and keep their parent's lease binding.

Hosted acceptance is pending: default-one memory headroom with a lead and
Codex checker has not been measured. See
[worker sizing and isolation](../docs/worker-setup.md#cross-check-slots) and
[PRD #2169's acceptance checklist](../prds/2169-cross-check-lane.md#acceptance-hosted-k8s-maintainer-owned).
An offline Helm render is not a hosted deployment test.

### Turning it on

**Order matters, and step 1 is not optional.** Enabling hosting without the
controller token does not merely break hosted workers — the api refuses to boot
(`WORKER_HOSTING_ENABLED=true requires WORKER_HOSTING_CONTROLLER_TOKEN_SHA256`),
which takes uzi down. It fails closed, but the blast radius is the whole product.

1. **Generate the controller's bearer token and put BOTH halves in Infisical**
   (`app-secrets` / `dev` / `/uzi`, the same folder `JWT_SECRET` lives in):

   ```sh
   TOKEN=$(openssl rand -base64 32)
   printf '%s' "$TOKEN" | shasum -a 256 | cut -d' ' -f1   # the hex hash
   ```

   | Infisical key | Who reads it | How |
   | --- | --- | --- |
   | `UZI_CONTROLLER_TOKEN` | the controller | file-mounted (`UZI_CONTROLLER_TOKEN_FILE`) |
   | `WORKER_HOSTING_CONTROLLER_TOKEN_SHA256` | the api | env var (a hash, not a credential) |

   The plaintext is file-mounted rather than env-injected because an env-borne secret
   is readable through `/proc/<pid>/environ` — the leak class `docs/proc-hardening.md`
   closed for the worker, and the reason the worker's join token is a file too.

   **The chart deliberately does not generate this.** A `randAlphaNum` default would
   re-render on every `helm upgrade` and silently rotate the credential — the one
   rotation an operator cannot see coming.

   > **Rotating: change both halves, then restart the controller** (it reads the file
   > at boot). Updating only one fails **closed** in either direction: the
   > controller's polls 401 and it stops reconciling. Hosted workers are then not
   > created, rolled or torn down — existing ones keep running on their own join
   > tokens — so the cost is an outage of the feature, never a bypass.

2. **Flip both flags in `argo-apps:apps/uzi/values/dev-cluster.yaml`:**

   ```yaml
   api:
     tls:
       enabled: true          # REQUIRED with workers.enabled
   workers:
     enabled: true
   ```

   The chart **refuses to render** with one without the other (`uzi.workerAPIPort`),
   so this is enforced, not remembered: with TLS off, hosted workers would be admitted
   to the api's plaintext `:8080` — the full router including `/api/auth/*`, no XFF
   stripping, and claim traffic carrying the user's *decrypted* forge PAT and
   Anthropic token in the clear across a shared cluster's pod network. **Never set
   `workers.allowPlaintextAPI` here**; it exists for KinD (no cert-manager), and
   wanting it on a real cluster means something else is wrong.

3. **Release, then point ArgoCD at it** (the ordinary procedure above). The agent and
   controller images publish at the same version as api/web.

### Values worth knowing

| Value | Default | Notes |
|---|---|---|
| `workers.enabled` | `false` | The envelope + (with the controller) the whole feature. |
| `workers.controller.enabled` | `true` | `false` = M3's envelope-only shape: namespace/RBAC/quotas/policies with nothing running. Also turns the api's hosting switch off — the two are one flag (`uzi.apiHostingEnabled`), because hosting with no controller means users provision workers nothing ever materializes. Only `kind-smoke.yaml` sets it. |
| `workers.image.repository` | `ghcr.io/vtmocanu/uzi` | The **prefix**; the controller appends `/agent-<template>:<tag>`. Must match the prefix `release.yml` publishes the agent images under (`ghcr.io/vtmocanu/uzi/agent-base`, `.../agent-jvm`). |
| `workers.image.tag` | concrete published pin (see `chart/values.yaml`) | **Decoupled from `Chart.AppVersion`** (PRD #422; reverses the old `""` → appVersion default). An app-only release leaves this tag alone and rolls zero worker pods when worker rendering is also unchanged. Controller template changes can still cause a paced same-image roll. Bump it to a new concrete version to deliberately advance the worker fleet, with the matching exact `workers.rollTargetVersion` below. The singleton controller admits a canary per owner/lane/capability pool and cordons busy workers when their turn is admitted; eligible uncordoned retained workers keep claiming, with a separate custody-held migration-drain exception. **Must be a concrete version string**: the chart wraps it in `required`, so a blank tag fails the render rather than silently reverting to appVersion — an out-of-tree per-cluster values file that relied on the old empty-tag default must now set one explicitly. See [adr/0422-decouple-worker-version.md](../adr/0422-decouple-worker-version.md). |
| `workers.controller.replicaCount` | `1` | Enabled controller rendering accepts `0` or `1`, with Recreate strategy. `0` stops reconciliation. Extra controllers outside the chart are unsupported; there is no multi-controller coordination. |
| `workers.rollConcurrency` | `1` | Positive integer: concurrent admitted replacements per coverage pool after canary promotion. Busy draining reservations and unconfirmed writes consume slots. |
| `workers.rollTargetVersion` | verified image stamp in published charts (`""` in source) | Exact verified baked version of the pinned worker image, including `+g<commit>`. Release packaging verifies each pinned template image signature and matching config at one resolved digest, then injects the common stamp. Per-cluster overrides still win; custom worker pins/source charts need a matching verified override. Remove a hand-set per-cluster `workers.rollTargetVersion` when using the published chart's worker pin: otherwise it becomes stale at the next pin bump and holds the roll again. ADR-1720's full stamp cannot be derived from those tags. Missing mapping holds ordinary paced rolls before their admission cordon; the separate migration drain below does not authorize replacement. Never infer the mapping from the replacement or strip metadata. |
| `workers.drainDeadline` | `"24h"` | Compatible Go duration (`"1h"`, `"30m"`), with unchanged positive-only parsing; exposed as `UZI_WORKER_DRAIN_DEADLINE`. Now bounds only independent **nondrifted** pressure/nix-size maintenance, which can interrupt busy runs and delete /data unless custody-held. No elapsed timer permits busy spec-hash/generation replacement or associated recycle, even with no pacing entry/plain drain; shortening this value no longer forces image upgrades. No new knob. |
| `workers.forceRoll` | `false` | **Global** emergency bypass of pacing, holds, incarnation proof and busy deferral for drifted workers, plus busy deferral for independent maintenance. May interrupt runs and lose all capacity; neither targeted nor a recovery guarantee. Custody-held data and independent DinD readiness/identity/pod-absence guards remain authoritative; DinD has no deadline or ForceRoll bypass. No automatic rollback. Restore `false` after the emergency. |
| `workers.storageClass` | `""` (cluster default) | `standard` on dev-cluster. |

### Deploying paced hosted rolls (#1470)

Under #3103, actual image-pin/template/size spec-hash or generation drift and
associated recycle wait indefinitely for genuine idle. Run-bound workers are
outside pacing but follow the same busy-image policy. Any assigned nonterminal
run blocks, including queued, paused and all waits/gates. Own pinned
continuations/checkers may finish; unrelated new roots, unassigned children and
judge intake are refused. An admitted busy drainer consumes pool concurrency
(default one) without replacement coverage, canary proof or post-promotion proof and can
hold the pool indefinitely. Unadmitted peers normally stay uncordoned and claimable,
except for eligible custody-held migration drains (see below), without a
legacy-recycle escape; separately fenced DinD maintenance remains
independent. Idle guards and custody still apply even with no assigned blockers.
No timer automatically uncordons workers. Indefinite waits can delay capacity
turnover and security fixes; choose global ForceRoll only as an explicit emergency
trade-off. Unchanged retained zero-replica Deployment restore while busy keeps
its existing guard.

Custody-held old/unknown-layout Docker workers remain protected from replacement,
including ForceRoll/recycle/maintenance. For persistent drifted workers, ordinary
admission is planned first; then the controller may reuse `image_roll` to drain
one eligible held worker with distinct live eligible uncordoned same-owner/lane
coverage of its full frozen capabilities. Busy coverage is allowed; free slots
are not required. This grants no admission, reservation, canary, roll slot or
pod-template patch; run-bound workers are excluded. Full-poll observed held drains
of any reason, including unrenderable targets, and selected/possibly committed
drains block additional selection. Retained drains can delay ordinary rolls and
remain after coverage loss. This is not a physical one-drain guarantee.

No API/schema/protocol change or server-side fence is added; this controller-only
exception reuses the existing reason and is deployment-order independent.
Registration/restart and explicit undrained polls trigger eligible reconciliation
without a fixed time bound. Claims can enter during registration or poll/write
failure gaps; process-local uncertainty is lost on restart, and delayed timed-out
cordons can commit after an undrained poll. Multiple held workers may physically
drain. Review `blocking_reason`/health and preserve custody before an operator
decision; no automatic discard. Custody-clear ordinary release can admit fresh
work before later re-drain; worker-pin rollback retains custody refusal before ordinary
release. Unknown-target multiple-drain diagnostics leave desired image/version
empty, grant no mutation authority and may stamp the existing first
`upgrading_since` through rolling fallback. Independent DinD, pressure and
maintenance fences remain. Eviction, node drain and manual pod deletion are
outside controller protection. See
[operational limits](../docs/hosted-worker-roll-verification.md#migration-drain-policy-and-operational-limits)
for idle terminal holds and chat continuation.

Deploy the new API first, then the new singleton controller and matching chart
RBAC, keeping `workers.image.tag` steady. Verify poll evidence and report support
before advancing the worker pin. Published charts carry its verified exact
`workers.rollTargetVersion`; set a matching override only for a custom worker pin
or a source-chart deployment.
Adding `UZI_POD_UID` and the existing registration-name transport changes the
worker template, so the controller upgrade itself causes a paced template roll
at the same image pin. Worker images and user-chosen names do not change.

A new controller against an old API holds when promotion evidence is missing;
it does not silently fall back to unpaced replacement. An old controller against
a new API still rolls without pacing. Downgrading during an active paced roll is
unsafe. Use [Hosted roll verification](../docs/hosted-worker-roll-verification.md)
for prerequisites, recovery and the maintainer's **open**, unexecuted hosted
checks. The existing [KinD smoke values](values/kind-smoke.yaml) disable the
controller and prove the envelope, not paced availability.

### Not proven until the first real rollout

Recorded because a green render and a green KinD run both say nothing about these —
each was checked rather than assumed:

- **NetworkPolicy enforcement: nothing at all.** kindnet implements no NetworkPolicy
  — not the Antrea ANNP, not even the default-deny floor — so both policies are
  created on KinD and silently never enforced. The YAML is the only artifact.
- **FQDN egress: capability proven, enforcement not.** Antrea v1.13.3 accepts the
  namespaced ANNP through the live `annpvalidator` webhook, and the behaviour was read
  out of the v1.13 source. **No packet has crossed.** The first real hosted worker
  either reaches `gitlab.example.com` or does not.
- **The `fsGroup: 10001` pin.** KinD's local-path PV is hostPath-backed and ignores
  fsGroup (`/data` and `/nix` arrive `0777 root:root`), so uid 10001 writes them there
  for a reason that does not exist here — dev-cluster's RWO ext4 volumes mount
  `root:root 0755` and fsGroup is the only thing making them writable.
- **The controller has never run.** KinD runs no controller (see above), so its first
  start is the rollout: watch that it reads its token, verifies the api's cert against
  the projected CA, and that its polls are not dropped by the api's default-deny.

### Verifying restricted-tier egress enforcement (operator packet test)

A repeatable procedure for discharging the "FQDN egress: capability proven,
enforcement not" caveat above, on dev-cluster. **This is operator reconnaissance
run from outside the sandbox — a hosted worker agent will (correctly) refuse to
run it.** Until an operator runs it and records the result, "no packet has
crossed" still stands; this section gives the steps, not the evidence.

**0. Confirm the tier first.** `uzi admin workers` → the target worker's
`docker:` flag must be `false`. The docker tier (`uzi-workers-docker`) allows
broad egress by design (see below) and will pass every check here for the wrong
reason. If you have no live worker to test against, a throwaway pod in the
`uzi-workers` namespace labelled `app.kubernetes.io/name: uzi-hosted-worker`
is selected by the same ANNP `appliedTo` and is an equally valid target.

1. **Allowlist behaviour.** From that worker/pod, assert the allowlisted hosts
   connect on 443 and the rest do not:

   ```sh
   # allowlisted — expect a completed HTTP status (any code)
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://cache.nixos.org
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://gitlab.example.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://api.anthropic.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://api.openai.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://chatgpt.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://auth.openai.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://ghcr.io
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://search.devbox.sh
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://api.github.com

   # off-allowlist — expect a hang/timeout, not a response
   # (api.github.com and search.devbox.sh are ALLOWLISTED devbox/nixpkgs hosts, above;
   #  codeload.github.com — the nixpkgs source tarball — is the off-allowlist example)
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://codeload.github.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://example.com
   ```

   The discriminator: on the **restricted** tier an off-allowlist host **times
   out** (curl exits non-zero on `--max-time`). **Any completed HTTP response —
   a 200, 403, 404, anything** — means you are actually on the docker tier;
   go back to step 0 and re-check the `docker:` flag before drawing any
   conclusion (see the tier-egress-trap warning in `.claude/rules/stack.md`).

2. **Deny-CIDRs hold.** From the same pod, assert these are blocked — they
   render as a leading `action: Drop` belt ahead of every Allow rule, so even a
   poisoned DNS answer for an allowed name resolving into one of these is
   dropped:

   ```sh
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' http://169.254.169.254/    # cloud metadata
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://10.96.0.1/          # kube-apiserver ClusterIP
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://192.0.2.1/        # a node IP in the node CIDR
   ```

   All three should hang/timeout.

3. **CNI realization.** Confirm the ANNP and the default-deny floor are actually
   enforced by Antrea, not merely accepted by the API server:

   ```sh
   # from an antrea-agent pod (kube-system)
   antctl get networkpolicy                 # expect the ANNP "uzi-worker-egress" and its rules
   antctl get networkpolicystats             # expect non-zero traffic/enforcement counters against it
   ```

   Also confirm the ANNP's Allow rules compose with the vanilla default-deny
   `NetworkPolicy` floor (`worker-networkpolicy.yaml`, `networkPolicy.enabled`
   default true) rather than substituting for it — both objects must be present
   and Realized.

This is a procedure for obtaining evidence, not a claim that the evidence
exists — record the actual output against each step rather than assuming it.

## Docker-capable workers: rootless vs non-rootless DinD (PRD #83 / #89)

PRD #83 shipped an opt-in docker-capable hosted-worker tier: a Docker-in-Docker
(DinD) sidecar in its own privileged, fenced `uzi-workers-docker` namespace, so a
worker can run `docker`/`docker compose` for repos whose own tests need it
(including uzi's own e2e). PRD #89 makes the DinD **posture** a per-cluster
toggle, because the safer posture has a node prerequisite not every cluster meets.

`workers.docker.rootless` (chart default `true`) selects it:

| Value | Posture | Daemon runs as | Node needs |
|---|---|---|---|
| `true` (default) | Rootless DinD — PRD #83, unchanged | userns-mapped, unprivileged | `kernel.unprivileged_userns_clone=1` |
| `false` | Non-rootless privileged DinD — PRD #89 | real root, no userns | nothing beyond `--privileged` |

Nothing changes on a cluster that keeps `rootless: true`. `false` is a documented
escape hatch, not a new default.

**Node prerequisite.** Rootless dockerd needs unprivileged user namespaces enabled
on the node kernel (`kernel.unprivileged_userns_clone=1`) — a **node-scoped**
sysctl, not settable per-pod. dev-cluster's nodes ship this **off** as a hardening
default (unprivileged userns disabled at the node kernel), so the rootless sidecar
crash-loops there (`need 'kernel.unprivileged_userns_clone' … set to 1`).
`argo-apps:apps/uzi/values/dev-cluster.yaml` therefore sets `rootless: false`.

**Reduced-capability dead end.** Before accepting non-rootless, a `privileged:
false` variant with a curated capability set (`SYS_ADMIN`, `NET_ADMIN`, `MKNOD`, …)
was tried live on these nodes: dockerd started, but could not create containers —
`mkdir /sys/fs/cgroup/docker: read-only file system`. On cgroup v2, k8s mounts
`/sys/fs/cgroup` read-only for non-privileged containers, and a writable cgroup
needs either `privileged: true` or a host cgroup bind-mount (a node change, out of
scope). So full `--privileged` is required on these nodes — there is no
lesser-privilege middle ground.

**The trade-off, stated plainly.** Non-rootless dockerd is real root with no user
namespace: a hijacked agent that reaches `tcp://127.0.0.1:2375` can `docker run
--privileged` straight to node root, and node root reads every co-scheduled pod's
secrets from the kubelet — including the api pod's `UZI_SECRET_KEY`, the master
key that decrypts every user's forge PAT and Anthropic token. This is an
owner-accepted risk on the mitigation terms below, for dev-cluster specifically —
not a claim that non-rootless is safe by default. See
`prds/done/89-optional-nonrootless-dind.md` (Security framing, Decision Log) for the
full reasoning.

**The mitigations (required together, not a menu):**
- `workers.docker.acknowledgeNonRootlessNodeRoot: true` — the chart **refuses to
  render** `rootless: false` without it, so the node-root acceptance is a loud,
  visible operator choice, never an inherited default.
- Loopback-bind — dockerd listens on `tcp://127.0.0.1:2375` only (the
  `docker:dind` image's automatic `0.0.0.0:2375` listener is suppressed), so the
  unauthenticated root daemon is never reachable off-pod.
- Claim-time repo allowlist — the `docker_repo_allowlist`
  setting (**Admin → Instance → Docker worker repo allowlist**): a docker-capable worker only claims repo-bearing runs whose repo is on it (repo-less judge runs stay claimable, see `api/internal/store/queries/runtime.sql`). This is
  the acceptance's likelihood control and **must be populated before any real
  work runs on a non-rootless cluster**, not an optional follow-up.
- Soft anti-affinity — keeps the api pod (holding `UZI_SECRET_KEY`) and CNPG off
  docker-worker nodes, so wherever node root lands it reads no crown-jewel secret.

**Shared run workdir.** Both postures mount the `runner-shared` subPath of the
`data` PVC (`/data/runner`, the M-workdir Decision-3 amendment as amended by #3153,
see [adr/3153-runner-shared-pvc-subpath.md](../adr/3153-runner-shared-pvc-subpath.md))
into both the worker and the dind sidecar at
the same path, so `docker run -v`/compose bind mounts under the run's checkout
(uzi's own e2e included) resolve in the daemon instead of hitting an empty dir.
Only that subPath is shared with dind, never the whole `data` volume.
The path mirrors `agent/src/git.ts`'s `runnerRoot` — if that root ever moves,
`controller/internal/kube/render_dind.go`'s `dindWorkdirDir` (mounted by `render.go`'s `podTemplate`) must move with it.

**Codex command cache (issue #1598).** A worker pod also carries a worker-only
`emptyDir` (in addition to any the docker lane adds), `codex-cmd-cache`
(`/var/cache/uzi-codex-cmd`), mounted into the `worker` container only: never
into seed-nix, dind-init or dind. It holds one per-run directory
(`GOMODCACHE`/`GOCACHE`/npm cache) for a run's Codex model-authorized
commands, so they reuse a build cache instead of re-downloading every
command; it is rendered for every worker regardless of the uid split, so the
render never forks the pod shape, and it carries no `sizeLimit`: a `sizeLimit`
is itself a kubelet eviction trigger (see [ADR-1598](../adr/1598-codex-command-storage.md)
and `specs/ai.md` §480). It is not a trust boundary: every command root for a
run shares one uid, so treat its contents as untrusted, never reused across
runs. See [adr/1598-codex-command-storage.md](../adr/1598-codex-command-storage.md).

### Pod-epoch shadow reclaim (PRD #3430)

Hosted Docker workers now participate in epoch ownership and can report a
read-only `pod_epoch_reclaim` verdict before run admission. **The shadow pass
does not delete attempt trees, release holds or remove stopped containers.**
Existing startup reapers and DinD maintenance are separate. Compose and
non-Docker lanes are unchanged. See the [worker overview](../docs/worker-docker.md#pod-epoch-shadow-reclaim)
and [design and implementation evidence](../prds/3430-pod-epoch-lock-shadow.md).

**Runtime declaration is an operator attestation, not autodetection.**
`workers.docker.podEpochReclaim.runtime` defaults to `""` (unset). The chart
accepts exactly `containerd-runc` or `crio-crun` when set. The declaration
attests that the nodes eligible for these workers meet the audited stack and
storage prerequisites below; it does not verify versions or storage itself.

| Declaration | Audited stack |
|---|---|
| `containerd-runc` | containerd **2.3.4**, runc **1.5.1**, kubelet **1.37.1** |
| `crio-crun` | CRI-O **1.36.2**, classic conmon with `runtime_type: oci` (unset resolves to `oci`), default crun **1.28 or 1.30.1**, conmon **2.1.10–2.2.1**, kubelet **1.35.3**; no namespace-adjusting NRI plugin |

Leave the value unset for unaudited versions, other runtime handlers or any
`runtimeClassName`. CRI-O conmon-rs, runtimeVM and runc handlers are unsupported.
The [CRI-O source audit](../prds/3430-pod-epoch-crio-audit.md) distinguishes
read evidence from inference and unverified node facts; its function-level
comparisons do not widen the declaration's crun version.

| Prerequisite | Operator responsibility |
|---|---|
| Storage exclusion | Use node-local or attach-enforced data storage so overlapping pods share one kernel's lock view. Shared storage without attach enforcement, including NFS/CephFS, is unsupported. |
| Kubelet/CSI ownership | Verify the actual driver honors matching-root skip semantics. A driver that recursively walks ownership despite a matching root is unsupported; application flock cannot fence kubelet/CSI. |
| Mounted data-root posture | Prepare a trusted root/worker-owned directory with gid **10001**, owner and group permissions covering **0770**, setgid, and no world write. Participating initializers preserve this posture; a root mismatch fails startup rather than being repaired. |
| Node/runtime evidence | Verify installed packages, `/proc` options, NRI configuration and the audit's conmon PID-namespace inference before declaring. Verify the image's util-linux flock as part of the image proof. |

See the [pinned storage audit](../prds/3430-pod-epoch-storage-audit.md) for the
matching-root predicate and CSI delegation boundary. Rendering
`OnRootMismatch` alone does not establish the driver's behavior.

**Default-off still participates.** On ext4/xfs, a Docker worker holds shared
epoch ownership for its process lifetime even with runtime unset, no evidence
projection, an already-consumed pod UUID or a full consumption ledger. A fresh
default-off pod logs `deferred` / `runtime_not_declared` immediately after the
existing startup reapers, without an evidence wait. An absent projection likewise
defers without waiting. Process restart of a consumed pod does not evaluate again.

Protected pre-Node initializers take temporary shared ownership before their
writes; Node acquires epoch ownership before its data-volume writers, startup
reapers and Docker requests. The bounded read-only maintenance `/proc` snapshot
is the explicit pre-lock exception. An ext4/xfs candidate tries exclusive
ownership first and holds it through reaping and shadow evaluation, then settles
its work and reacquires shared ownership before admission.

The filesystem gate permits ext4/xfs. Unsupported filesystems uniformly skip
**epoch-file** flock and defer evaluation; their bounded ledger metadata still
uses its serialization mutex, which supplies no epoch-exclusion authority.
Other participation failures fail closed: missing/unsafe state, flock errors
and uncertain ledger durability refuse startup without admission.

| Bound | Behavior |
|---|---|
| Epoch shared acquisition | **120 s**, including protected initialization; busy tentative EX defers and falls back to bounded SH. |
| Ledger metadata mutex | **5 s**; validation and durable consumption precede evaluation/admission. |
| Controller startup synchronization | **60 s**; incomplete synchronization supplies no positive handoff. |
| Worker evidence wait | **115 s soft / 120 s hard**, including a **5 s** settlement reserve. Negative or invalid evidence defers. |
| Whole shadow pass | **595 s soft / 600 s hard**, including evidence waiting and settlement; cleanup is joined under EX. Uncertain settlement is fatal, retaining EX until process exit. OS filesystem latency is not cancellable. |
| Shared reacquisition after EX | Separate **120 s** wait after cleanup, before admission. |
| Daemon inventory | **10 s** per request; **500 containers**, **500 volumes**, **50 plugins**; inspect concurrency **4**. |
| Input bytes | Finite daemon bodies and finite subprocess output capped at **8 MiB**; cumulative event stream capped at **64 KiB**; projected evidence **8 KiB**. |
| Retained-source discovery | **200 physical tree paths**, **4,096 directory entries**, **1,024 bare repositories**; metadata per bare **8 MiB / 4,096 records**. The operator inspector retains its independent **64-descriptor** default. |
| Consumption history | **4,096 distinct pod UUIDs / 151,560 logical bytes**, on one immutable ledger inode, plus filesystem allocation overhead. No eviction, compaction or automatic reset. |

**Reading verdicts.** `pod_epoch_reclaim` reports `eligible`, `deferred` or
`partial`, a stable `reason`, `examined`, `would_delete`, `kept`,
and `list` (up to **32** entries with sanitized run/path identifiers and sizes).
`list_overflow` counts omitted selected entries. Discovery overflow is exact
only when bounded discovery completed; consult `inventory_overflow_exact`
and `inventory_complete` rather than assuming a complete census.
`bytes_state` is `measured`, `partial` or `unknown`; measured entries do not
make a partial inventory complete. Evidence-wait and pass durations accompany
the verdict. Only a fresh durable consumer under EX evaluates; a new pod at
ledger saturation reports `consumption_full` without evaluation while preserving
SH startup.
A would-delete entry requires terminal ownership, fresh empty holds and an
eligible retained-source disposition; a verdict grants no deletion authority.

**The daemon check is a veto, not an event-subscription proof.** Read-only GET
inventory requires stopped/created containers, no running/restarting/paused
containers, restart policy `no`, inactive Swarm, live-restore off, no enabled
plugins and local volumes with empty options. The unfiltered `since` event
stream aborts on events, malformed data, EOF or transport failure. HTTP headers
or a silent stream prove neither subscription liveness nor writer exclusion.
The exclusion argument rests on the no-client interval, restore/restart-policy
predicate and stopped-state proof described in the PRD.

**Rollout and repair.** Runtime changes use the existing **Recreate** rollout.
Plan for initialization lock waits, evidence latency, the bounded pass and
shared reacquisition before the replacement admits work. A wedged EX holder can
make a successor reach its bounded SH wait and exit nonzero for restart/retry.
Do not unlink or replace `/data/.uzi-epoch.lock` or
`/data/.uzi-epoch-consumed` to fix contention. The ledger records historical
one-shot consumption: do not automatically delete, truncate or reset it,
including when full. Torn/corrupt state, uncertain fsync and unsafe metadata
require operator-owned repair. Positively stop the writers that can reach the
volume before considering repair; stopping them alone does not establish that
resetting historical consumption is safe. This guide provides no automatic
reset or generic safe repair procedure.

**Acceptance remains pending.** The accepted residual is an older pod's writer
surviving its own termination and continuing after a later evidenced epoch;
the immediate predecessor/overlap proof does not exclude every historical
writer. Both base and JVM final-head image proofs remain with the lander on
the storage host: follow the [exact-head image-proof handoff](../prds/3430-pod-epoch-image-proof.md).
Live acceptance remains operator-owned: attest the node/storage prerequisites,
then observe several graceful rolls and record handoff capture, projection
latency, pass times and verdict/list agreement with actual trees and holds, as
specified in the [acceptance boundary](../prds/3430-pod-epoch-lock-shadow.md#delivery-acceptance-and-closure).
The local full-agent gate also remains red, with inconclusive bounded
base/candidate controls; exact-head PR CI is still pending. Before delivery,
a formal owner partial decision must name the actual milestone IDs and the
outstanding M1/M2c image/full-gate criteria and M3 live acceptance. Continuation
guidance does not waive these checks. These pending proofs are not production
verification; deletion remains later work under #3431.

### M4/M5 runbook: known items before go-live

Not blockers to M1–M3 landing, but gate the live rollout. Full detail on 2–4 is
recorded in `specs/ai.md`; this stays a pointer, not a duplicate.

1. **Enable ordering.** Nothing in the chart couples the
   `acknowledgeNonRootlessNodeRoot` ack to the api build actually carrying
   M-allow. Do **not** set `rootless: false` live until the api deploy with the
   M-allow claim gate (repo allowlist) is out — M-allow is the acceptance's
   likelihood control, not an optional follow-up.
2. **M-workdir bind-source scope.** The shared workdir only resolves bind
   sources located *under* the run's checkout; sources outside it don't. Concretely,
   `./e2e/run-e2e.sh` defaults its run dir to `${TMPDIR:-/tmp}/uzi-e2e-$$` —
   outside `/data/runner` — so its own compose binds won't resolve in dind as
   shipped. Fix: point the worker's `TMPDIR` at a subdir under `/data/runner`
   (`run-e2e.sh`'s sanitized-env allowlist already passes `TMPDIR`/
   `E2E_RUN_DIR` through). The concrete M4/M5 gate: e2e green **and** no secret
   transits the now-dind-visible tmp.
3. **Runner-clone durability.** Since #3153 the runner clone lives on the
   `runner-shared` subPath of the persistent `/data` PVC, so on a migrated worker
   a run's working tree survives a pod restart (a data-PVC recycle or delete still
   removes it). A pod still on the old layout (an `emptyDir`, e.g. one held by
   recovery custody until it releases) still loses the working tree on restart.
   Work fetched back to the bare repo at `/data/repos` or checkpointed is safe
   either way (it re-clones on resume); see
   [adr/3153-runner-shared-pvc-subpath.md](../adr/3153-runner-shared-pvc-subpath.md).
4. **Rootless fsGroup workdir read (M4 runtime check).** The workdir is
   `fsGroup: 10001`; whether a future *rootless* cluster's uid-1000 dind sidecar
   can read worker-written bind sources via the fsGroup supplemental group is
   unproven by unit tests. Non-rootless (dev-cluster) is root, unaffected.

## The api TLS listener (`api.tls.*`, PRD #58)

**Off by default, and off is a no-op**: with `api.tls.enabled: false` the chart
renders exactly what it rendered before this existed (no Certificate, no extra
port, no extra env, no mount). Turn it on only together with hosted workers — on
its own it opens a port nothing dials.

**What it is for.** Hosted workers and the worker controller dial the api
**directly**, with no nginx in the path, and a claim response on that hop carries
the run owner's *decrypted* forge PAT and Anthropic token. dev-cluster is a shared
cluster running arbitrary dev pods, so that hop gets its own TLS listener on a
second port (`:8443`). The plain `:8080` port stays exactly as it was — it is what
`web`'s nginx and the kubelet probes use, and `api.networkPolicy` keeps everything
else off it. Users are unaffected: they reach uzi through the ingress, which serves
dev-cluster's `*.example.com` wildcard cert.

**Why the chart ships its own CA instead of using `letsencrypt-prod`.** The name
being certified is `api.<ns>.svc.cluster.local`, a cluster-internal Service name.
dev-cluster's only ClusterIssuer is `letsencrypt-prod` — ACME, DNS-01, scoped to
the `dev.example.com` zone (verified on-cluster 2026-07-17) — and ACME can only
issue for publicly resolvable names, so it **cannot** issue this certificate at
all. The chart therefore renders its own `selfSigned → CA → leaf` chain, scoped to
the release. This is the cluster's existing convention for internal hops:
`ingress-nginx`, `keycloak` and `otel` each run a selfsigned issuer of their own.
The CA's entire trust domain is this one hop; no browser ever sees it.

Point `api.tls.certManager.issuerRef.name` at a real internal CA if one is ever
added, and the chart will issue from it and skip its own chain. Set
`api.tls.certManager.enabled: false` with `api.tls.secretName` to supply a pair
from outside cert-manager.

**Renewal needs no restart.** The api is given the mounted *paths*
(`API_TLS_CERT`/`API_TLS_KEY`) and re-reads them when the files change, so a
cert-manager leaf renewal (90d issue / 30d renew) is picked up live. The CA is
deliberately long-lived: rotating the *root* means re-trusting it in the controller
and every hosted worker pod at once, which is not the routine operation — the leaf
is.

**How clients get the CA.** cert-manager writes `ca.crt` alongside `tls.crt` and
`tls.key` into the leaf Secret, so that Secret is also the CA distribution point.
Any client that mounts it **must project `ca.crt` alone** (`items:`), because
mounting the Secret whole would put the api's **private key** into that client's
filesystem.

> **Status: this is now a fact, not just a requirement** (M6, 2026-07-17). The
> projection exists in `deploy/chart/templates/controller-deployment.yaml` — the
> controller is the first (and today the only) workload that mounts the leaf Secret
> as a *client*, and it projects `ca.crt` alone:
>
> ```yaml
> - name: api-ca
>   secret:
>     secretName: {{ include "uzi.apiTLSSecretName" . }}
>     items:
>       - key: ca.crt          # ONLY this key — never the whole Secret
>         path: ca.crt
> ```
>
> **Hosted workers never mount this Secret at all** (see the cross-namespace note
> below), so the controller is the one place this rule has to hold. Any future client
> owns its own `items:` block.

The controller reads the CA via `UZI_API_CA_FILE` and pins it *exclusively* (the
system roots are not also trusted). There is no skip-verification knob in this path
and there must not be one — an unverified peer is the attack the encryption exists
to stop.

Related trap, worth one line so nobody "simplifies" the projection away: do **not**
mount the `uzi-ca` Secret instead. That one holds the **CA private key**, which is
strictly worse than the leaf's.

> **Cross-namespace note (M3/M6).** Hosted workers run in a **different**
> namespace and so cannot mount this Secret. The CA reaches them via the Secret the
> controller already creates for each worker; the controller mounts the CA itself
> and relays it. This is why no `trust-manager` `Bundle` is involved — there is no
> trust-manager on dev-cluster (verified 2026-07-17).

Values worth knowing:

| Value | Default | Notes |
|---|---|---|
| `api.tls.enabled` | `false` | The whole feature. Off renders nothing. |
| `api.tls.port` | `8443` | The worker/controller port. Also the port the NetworkPolicy admits the worker namespace to (M3). |
| `api.tls.clusterDomain` | `cluster.local` | Used for the FQDN SAN — the name hosted workers actually dial, since the short forms do not resolve cross-namespace. |
| `api.tls.secretName` | `""` → `uzi-api-tls` | Holds `tls.crt` + `tls.key` + `ca.crt`. |
| `api.tls.certManager.enabled` | `true` | `false` = bring your own Secret. |
| `api.tls.certManager.issuerRef.name` | `""` | Empty = the chart's own chain (see above). |

## Verify a live deploy

ArgoCD is **hub-and-spoke**: the `Application` object lives on the **argo-cluster**
cluster (the one running ArgoCD, which reads `argo-apps`) and its
*destination* is dev-cluster. So the workload pods are on dev-cluster but the
Application is NOT — `kubectl -n argocd get application` against dev-cluster fails
with `the server doesn't have a resource type "application"` (no CRD there).

```sh
# workloads: on the destination cluster
kubectl -n uzi get pods --context dev-cluster          # api 1/1, web 2/2, postgres-uzi-cluster-1 1/1

# the Application: on the ArgoCD cluster, NOT the destination
kubectl get application uzi -n argocd --context argo-cluster \
  -o jsonpath='{.status.sync.status}/{.status.health.status}{"\n"}'   # Synced/Healthy
```

Then over HTTPS: the SPA loads at `https://uzi.example.com`, the seeded
admin logs in (Secure cookies hold behind TLS because `FRONTEND_ORIGIN` is
`https://…`), forge connect + issue sync work from the cluster, and a laptop
worker joins via the public URL and completes a run against a test repo. See the
PRD (`../prds/done/52-cicd-argocd-deploy.md`) M6 for the full end-to-end checklist and
its Decision Log for the rationale behind each choice above.
