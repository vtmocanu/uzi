# ADR-1265: RC-first release train (prerelease channel, lockstep promote)

**Status**: Accepted (PRD #1265 M1–M6 merged; M7 first-live-cycle is the maintainer's, out of band)
**Date**: 2026-09-13
**Deciders**: Vlad Mocanu + agent team (PRD #1265 Decision Log)
**PRD**: [prds/1265-rc-release-train.md](../prds/1265-rc-release-train.md) — the PRD carries the full Decision Log, milestone tests, and the measured semver table; this ADR carries the durable design shape and the seams a future change would break silently.

## Decision (summary)

Releases are cut as **release candidates by default**. `release-cut X.Y.Z` cuts `vX.Y.Z-rc.1`; the tag publishes the five images and the chart at `X.Y.Z-rc.1` exactly as a stable tag does, creates a GitHub Release flagged **pre-release** (never latest), and does **not** publish the stable Homebrew formula (since PRD #1378 an RC publishes only the separate, opt-in `uzi-cli-rc` formula). A stable `vX.Y.Z` is a **promotion** of the in-flight candidate, built from the candidate's own commit on a throwaway release branch, cut in **lockstep** with the next candidate by a single `release-cut Y.Z.W --promote` (or alone, by `release-cut X.Y.Z --promote-only`, when the next candidate is not wanted yet). Stable-only surfaces — the Homebrew formula, the GitHub Release marked latest, and the in-app update check — never surface a candidate. A deployment opts into running candidates by tracking the Helm `targetRevision` range `0.*-0` instead of `0.*`.

## Context

A `vX.Y.Z` tag was one irreversible step: it published every artifact, marked the Release latest, published the Homebrew formula, and (because the GitOps-tracked ArgoCD Application tracks the chart with a semver range) rolled the running instance within minutes. The first real exercise of a build was also its publication to every stable-only surface. There was no channel between "green on main" and "stable", and no way to promote *what was actually run* rather than whatever `main` had moved on to.

The publish pipeline was already largely prerelease-aware: `release.yml`'s `assert-version` accepts a SemVer prerelease tag, `publish-release` marks any `-`-carrying tag pre-release (never latest), and the in-app update check reads GitHub's `releases/latest` (which excludes prereleases server-side) and compares with `golang.org/x/mod/semver` (which orders `X.Y.Z-rc.N < X.Y.Z`). What was missing was the *cutting* side: the changelog gates, the tag-selection logic, and the release scripts all assumed a single stable shape.

## The decisions

### D1 — RC by default; stable is a promotion, or an explicit escape

`release-cut <X.Y.Z>` names the version a candidate is cut **for**; the tag shape is derived. `release-cut.sh` (`.agents/skills/uzi-release/scripts/`) is a tag-derived state machine. From the tags it derives the highest stable `S` and the **in-flight RC** — the highest-versioned `vX.Y.Z-rc.N` whose base has no stable tag and sits above `S` (a base at or below `S` with no stable of its own was abandoned by `--skip-promote`; lockstep always left a newer candidate to mask it, a promotion that cuts none does not, so discovery excludes it). Verbs:

- default, no RC in flight → `vX.Y.Z-rc.1`;
- default, RC in flight for **this** base (`release-cut B` while `vB-rc.N` exists) → the next candidate `vB-rc.(N+1)` (folds `[Unreleased]` into the open section per subsection, guarded, D3);
- default, RC in flight for a **lower** base → refuse (exit 3) and print the facts, so the lead picks a verb;
- `--promote` → promote the in-flight RC to stable (D5), then cut `vX.Y.Z-rc.1` on main;
- `--promote-only` (given the in-flight base itself) → promote the in-flight RC to stable (D5) and stop: no next candidate, `main` untouched (amendment, below);
- `--skip-promote` → abandon the RC, rename its open section to `X.Y.Z` and fold `[Unreleased]` in, cut `vX.Y.Z-rc.1`;
- `--stable` → a plain `vX.Y.Z` from main's tip (the old one-step model), refused while an RC is in flight.

**Promotion without a next candidate (amended 2026-09-19).** Lockstep assumes the next candidate is wanted the moment a stable is. It is not when `main` has merged work the owner wants held back from the candidate channel: the dogfooding deployment tracks `0.*-0` (D8), so a candidate deploys the moment it publishes. `--promote-only` decouples the halves. It runs D5's promote alone, keyed by the in-flight base so the argument names the stable being created; a next-version argument is refused, since it signals the lead expected a candidate. It keeps the published-RC refusal (D11) and refuses every main-half option (`--changelog-file`, `--no-commit`, `--prev-tag`). Afterwards no RC is in flight, so the next plain `release-cut <next>` is an ordinary `rc.1` whose coverage window starts at the new stable (D4), and the work held back ships there. `--promote` already took this branch implicitly when nothing shipping had landed since the RC and `[Unreleased]` was empty; the verb makes it available by intent instead of by the state of `main`. Cost: `main` is untouched, so until the next cut its `Chart.yaml` stays at the RC version and its `## [B]` heading keeps the RC-cut date while the stable tag's copy carries the promotion date. The next cut of a new base reconciles that heading from the `vB` tag's copy (D3: promotion dates the section), so the drift never outlives one cycle; the same applies to the implicit promote-only branch.

**Tag discovery never sorts a mixed stable/prerelease list.** Git's `version:refname` orders `-rc.N` against its base by string length; the scripts filter stable tags first, and find the in-flight RC by grouping `v*-rc.*` per base and comparing `N` numerically. The script never prompts (D7): refusals exit 3 with the facts and the `uzi-release` skill owns the conversation.

### D3 — One changelog section per stable version, opened at the first RC

The first RC of `X.Y.Z` folds `[Unreleased]` into `## [X.Y.Z] - <date>`; later RCs of the same base append to that open section (`rc.(N+1)` moves each `[Unreleased]` `###` bucket to the end of the same subsection, or opens it; the fold refuses, leaving the CHANGELOG untouched, on text outside a subsection, a name outside Keep a Changelog's six, a duplicate subsection in the result, or any non-heading line lost or invented. Amended 2026-09-25: the earlier empty-`[Unreleased]` rule fired on every re-spin, since merged PRs land their entries there, and forced a hand fold); promotion refreshes the date. Every gate finds the section by stripping the `-rc.N` suffix (`${VERSION%%-*}`), so the coverage oracle and the section title read the same `[X.Y.Z]` section for a candidate and its stable alike. The published Release *body*, however, is not always the whole section: accumulation is stable-only (below).

**Accumulation is stable-only in the Release body.** The CHANGELOG *file* keeps the single accumulating `## [X.Y.Z]` section described above, but `scripts/changelog-section.sh body` emits, for a candidate tag `vX.Y.Z-rc.N` (N >= 2), only the DELTA since the previous RC of that base (`vX.Y.Z-rc.(N-1)`): the bullets added or amended in the section since then, under the subsection headers they fall in. A stable tag, and `rc.1` (which has no prior RC), still emit the whole section, so the combined notes appear once, on the stable Release, while each candidate advertises just its own additions; the stable-publish prune (D9-adjacent, #1340) then removes the superseded RC Release entries. The delta is diffed against the previous RC tag's own copy of the section and falls back to the full section when that tag cannot be resolved (offline, absent, not a git tree), so a mis-resolve can never fail a publish (worst case is the old over-inclusive body). This changes only the published Release body; the CHANGELOG file, the coverage oracle, `check-changelog.mjs`, and the web changelog drawer all still read the one accumulated section. `scripts/release-scripts-test.sh` drives both directions (delta excludes the unchanged bullet, stable accumulates, a re-spin with no new bullets says so).

The `chore(release):` commit is now exempt from the coverage oracle by its conventional message. Today's single-step cut always folds CHANGELOG, so the release commit was exempt by the touched-CHANGELOG rule; a next-candidate cut (`rc.N+1`) bumps only `Chart.yaml` and does not touch CHANGELOG, so that rule no longer covers it. Exempting `chore(release):` is principled — a release commit is a mechanical version bump, never a feature merge — and only widens exemptions.

### D4 — The coverage window is "since the highest lower stable tag by version", never `git describe`

`assert-changelog-covers-release.sh` derives PREV as the highest **stable** tag (`vX.Y.Z`, no prerelease) strictly below the base version, filtering stable tags first and comparing numerically. `git describe` ancestry is wrong under the train for two reasons: a promoted stable tag lives on a throwaway release branch and is **not** an ancestor of main, and a prerelease tag between releases would silently narrow an ancestry-nearest window. A hotfix tag (`vX.Y.Z` off a promoted `vX.Y.W`) does not change the next RC's window, because it shares main's merge-base (the RC commit) with the promoted stable.

### D5 — Promotion builds stable from the RC commit on a throwaway release branch

`--promote` creates a `release/X.Y.Z` worktree at the in-flight RC commit, makes metadata-only edits (`Chart.yaml` version/appVersion to the stable, the `## [X.Y.Z]` section date to today, the worker autobump — which leaves the pin at the RC tag when the runtime surface is unchanged, D11), commits `chore(release): vX.Y.Z (promotes vX.Y.Z-rc.N)`, and asserts the commit's diff is a subset of `{CHANGELOG.md, deploy/chart/Chart.yaml, deploy/chart/values.yaml, scripts/assert-worker-tag-decoupled.sh}` **before any tag exists**; anything else aborts and removes the worktree and branch. The stable tag is created locally; the branch is never pushed and is removed (the tag holds the commit). Stable tags are therefore not ancestors of main — the Kubernetes release-branch shape, not git-flow's merge-back (D4 makes ancestry irrelevant). **Push order is stable tag, then main, then the RC tag**, because the RC's `assert-changelog` computes PREV from the tags it can see and needs the new stable present; the wrong order fails loudly in CI, not silently.

### D6 — Promotion rebuilds; artifact retagging is deferred

`release.yml` rebuilds the stable images from the promote commit rather than retagging the RC's digests. With digest-pinned base images and a metadata-only promote commit, the only intended difference is the ldflags version stamps (the binaries must report `X.Y.Z`, not `-rc.N`, in `/api/version`, the TUI and the footer). Retagging would still need the chart re-packaged and a commit under Model B, and would not avoid the deployment re-roll. Revisit only for digest-level provenance.

### D11 — The worker pin is never rewritten on a stable cut

The hosted-worker pin (`workers.image.tag`) is a concrete image tag that moves only when the agent runtime surface changes (ADR-422), RC or stable alike. `release.yml` builds a promote tag's `agent-*:X.Y.Z` images by re-tagging the RC's digest when that surface is unchanged, so those images still report the baked `X.Y.Z-rc.N` version. The api classifies worker upgrade health by semver ordering, and `X.Y.Z-rc.N < X.Y.Z`, so **normalizing the pin to the stable on promotion would leave the whole fleet `outdated` forever** — the cry-wolf failure ADR-422's roll-health code exists to avoid. Forcing a rebuild instead would roll the fleet on every promotion, reintroducing the churn ADR-422 removed. So `0.83.0-rc.2` inside the `0.83.0` chart is correct and names the exact worker image that was tested — **as long as that RC was PUBLISHED**. The pin may read an RC inside a stable chart, but only a published one: the re-tag-the-RC's-digest mechanism above assumes `release.yml` actually ran for the RC. An RC cut at a commit whose tag is kept local-only never runs `release.yml`, so its `agent-*` image is never built, yet `worker-tag-autobump` has already pinned it — shipping the stable chart then ImagePullBackOffs every new hosted worker. That is the 0.83.0-rc.7 failure (2026-09-19): the supported route for "stable = current `main` while a stale RC is in flight" is to cut the RC at the tip, push and publish it, then `--promote`, never a local-only tag. Four guards keep the invariant "the pin names a published image": `release-cut.sh --promote` refuses an in-flight RC absent from origin; `release.yml` gates `publish-chart` on every worker image resolving on GHCR at the pinned tag; `release-verify.sh` re-checks the shipped pin post-publish; and `worker-tag-autobump.sh` repins a dead pin to the version being cut rather than failing open.

### D8 — The deployment opts in with `0.*-0`, out of tree

A deployment that should run candidates tracks the Helm `targetRevision` range `0.*-0`; one that should only ever see stable keeps `0.*`. Measured against Masterminds/semver v3 (the engine ArgoCD uses), `0.*-0` admits `0.x` and `0.x-rc.N` and excludes `1.0.0` and `1.0.0-rc.1`; the explicit-range spelling `>= 0.0.0-0, < 1.0.0` leaks `1.0.0-rc.1` and was rejected. The flip is the maintainer's, in the out-of-tree GitOps repo; only `deploy/README.md` describes it generically. It is a no-op until the first RC chart is published (with only stable `0.x` charts present, `0.*-0` resolves to the same version as `0.*`).

## Stable-only surfaces (D9), and how the train protects them

- **Homebrew** — the stable `uzi-cli` formula is published by stable tags only. Originally `brew.yml`'s tag trigger was `["v*", "!v*-*"]`, so an RC tag created no run at all; since PRD #1378 it triggers on every `v*` tag and its Detect-channel step routes an RC to the separate, opt-in `uzi-cli-rc` formula, so stable users still never see a candidate and `release-watch.sh` waits on `brew.yml` for both channels. The strict Validate regex remains the second line of defense for `workflow_dispatch`.
- **The GitHub Release "Latest" badge** — `publish-release` marks any `-`-carrying tag pre-release.
- **The in-app update check** — `releasecheck` reads `releases/latest` (server-side non-prerelease) and compares with `x/mod/semver`. No test pinned a prerelease running version; M5 added them (no behaviour change).
- **`release-verify.sh`** is channel-aware: stable asserts the Release is latest; RC asserts it is flagged pre-release and `releases/latest` is unchanged. **`release-watch.sh`** watches `release.yml` and `brew.yml` for every tag (since PRD #1378 an RC publishes the opt-in `uzi-cli-rc` formula; originally it watched `release.yml` only for an RC). Both read the channel from the shared `scripts/lib/release-mode.sh`, so they never disagree about whether a tag is an RC.
- **The web changelog drawer** — `web/src/lib/semver.ts` parses an `X.Y.Z-rc.N` running version to its stable base, so on the RC-dogfooding instance the `[X.Y.Z]` section still marks "You're running this" and no false "available" banner fires (precise `rc < stable` ordering would wrongly flag the in-progress section as an update).

## Scope beyond the PRD's stated list

The M1 tree sweep found two sites the PRD's "Needs changing" list omitted, both fixed in the train:

- `web/scripts/check-changelog.mjs` compared the newest CHANGELOG heading to `Chart.yaml`'s `version` exactly; at `rc.1` (`version: 0.83.0-rc.1`, section `[0.83.0]`) that mismatch would redden `gate:web` / `validate-web` on the first RC commit. It now strips the prerelease suffix before comparing, and still catches genuine drift.
- `web/src/lib/semver.ts` (above, D9).

The PRD's M1 text also referred to a `release-verify.sh` `img_has_tag` previous-tag sort that does not exist; the real channel-dependent site is check 4 (`releases/latest`). The PRD was corrected.

## Consequences

- Cutting is one command per cycle (the Rust beta/stable shape); the in-flight state is derived from tags at every run, with no state file.
- Lockstep means one bug can block both the promotion and the next RC at release time; the offline fixture (`scripts/release-scripts-test.sh`, `task test:release-scripts`) drives every verb before the first live cycle, and the promote half aborts before any tag exists.
- The single dogfooding instance runs candidates only; rollback is `rc.(N+1)` or a temporary exact pin.
- A stable chart legitimately pins a PUBLISHED RC worker image (D11); `uzi worker list` shows it as `up_to_date`. A pin at an unpublished RC is the 0.83.0-rc.7 failure, now blocked by four release guards (D11).
