# Renovate and other dependency PRs

Moved from the `uzi-release` skill (2026-09-23); this is the canonical home. The review
lane for Renovate-class work is SKILL.md step 2; this file is what to read before and
around it. `S` is `.agents/skills/uzi-lander/scripts/`.

## Before touching one

- **Diff its target against `origin/main`** (`git show origin/main:<file>`): another merge
  may already satisfy it, a redundant PR Renovate will auto-close.
- **Renovate authors as the repo owner**, so the ruleset's non-author approval can never
  be met: merge with `--admin` directly (references/merge-mechanics.md).
- **Never push to a `renovate/*` branch.** Renovate rebases and force-pushes it, clobbering
  your commit; `S/land-prep.sh` refuses one. A needed fix goes on your own branch (below).

## A major and its minor sibling: prefer the major when safe

`renovate.json` extends `config:recommended`, so `separateMajorMinor` is on: a package at
`1.2` with `1.3` and `2.0` available gets two PRs. Landing the major supersedes the minor
(Renovate auto-closes it once the major merges, or close it yourself). A major is breaking
by definition, so vet it first: read the breaking-changes entries in the PR body's Release
Notes (`gh pr view <n> --json body -q .body`) or the upstream CHANGELOG between the two
versions, for anything touching how uzi uses the package.

- Nothing breaking affects uzi and checks are green: take the major, drop the minor.
- Adopting it needs real code changes: merge the safe minor/patch now, file the major as
  its own `dependencies`/`enhancement` issue (`gh issue create`), report it at batch end.
- An `@anthropic-ai/claude-agent-sdk` major also gets the adoption review below.

## A minor/patch bump is not automatically a drop-in

CI is the arbiter, and two classes redden a green-looking bump:

- **Tool/analyzer** (knip, oxlint, a linter): can be stricter and surface a pre-existing
  backlog the zero-tolerance gates (`deadcode:web`, `lint:*`) reject. It needs a
  triage/ignore pass, not a merge; land the safe siblings and defer it.
- **k8s node image** (`kindest/node`): can outrun the SHA-pinned `helm/kind-action` that
  ships `kind`. A newer node (observed at v1.37.0) rejects the obsolete
  `kubeadm.k8s.io/v1beta3` config the pinned `kind` emits (`Create KinD cluster` fails with
  `uses an old API spec`), so bump the action in tandem.

## `@anthropic-ai/claude-agent-sdk`: read for adoption, not just the lockfile

It is the agent-runtime core (`agent/` builds on it, incl. the `PreToolUse` guardrail hooks
and `settingSources`), so a new version can ship capabilities uzi should adopt.

- Read **every intermediate version's notes**, not only the target's: `0.3.235 → 0.3.238`
  spans `.236` and `.237`. Renovate embeds one section per version in the PR body; if thin,
  read `anthropics/claude-agent-sdk-typescript` `CHANGELOG.md` across the whole span.
- Scan for new hooks, tool-runner/permission APIs, session/streaming features and options;
  cross-check against the four guardrail layers and the run lifecycle in `ARCHITECTURE.md`.
- Merging the bump implements none of it. Propose anything useful to the user and offer an
  `enhancement` issue, separate from the bump; never implement it in the bump PR. Report
  what you found, or "nothing actionable", at batch end.

## Worker-toolchain bump PRs (devbox, not Renovate)

`agent/devbox-global` (go, gcc, python3, chromium, semgrep, helm, …) is not Renovate-covered.
The weekly `.github/workflows/devbox-update.yml` re-resolves it against nixpkgs and opens
`chore(deps): refresh devbox worker toolchain` (branch `chore/devbox-toolchain-update`).
Treat it as Renovate-class, plus:

- It tracks `nixpkgs-unstable`, so it can carry MAJOR tool bumps: read the lock delta. This
  is intended (workers should get current, patched tools).
- The check that validates it is **`build-agent` (base + jvm)**, which builds the worker
  image and runs the toolchain guard; the other `ci.yml` jobs are unaffected by the lock.
- The workflow opens it with `GITHUB_TOKEN`, so the PR may not trigger its own CI
  (GitHub's recursion guard). Push an empty commit with your own token, or rely on the
  scheduled run's base-image build plus post-merge `main` CI.

## A Renovate PR red because the new version needs a code change

Do NOT fix it on the Renovate branch. Make your own branch that bumps the dep and carries the
fix, and let Renovate auto-close its PR.

1. **Diagnose against both versions first.** Reproduce in a throwaway worktree (off the
   Renovate branch, or off `main` with the version installed) and run the same thing on the
   current and target versions. A new failure may be a stricter check, an equivalent
   behaviour change, or a tool now reporting something always true (go-chi/chi v5.3.2, PR
   #1148). Establish which, so the fix matches the change and you can state the production
   impact (often "none").
2. **One branch off `main`** (`fix/<dep>-<version>` or `chore/<dep>-<version>`): bump the
   dep yourself (`go get <mod>@<v> && go mod tidy`, or `npm install <pkg>@<v>`), apply only
   the fix the bump forces, run `task gate:<component>`, push, open a PR. Title it honestly
   (`fix(deps): …` / `chore(deps): …`); never contort the title to dodge the bot skip.
3. **Land it with the loop.** It is Renovate-class for SKILL.md step 2. CodeRabbit
   auto-skips a `*(deps)` title (`.coderabbit.yaml` `ignore_title_keywords`, beside
   `ignore_usernames: renovate[bot]`); if you judge a bot review warranted, get the user's
   approval, then `gh pr comment <n> --body "@coderabbitai review"` overrides the skip
   regardless of title. Otherwise one local reviewer, or CI alone with the decision stated.
   Admin-merge when green.
4. **Renovate auto-closes its PR** on its next run once `main` is at the target version.
   Do not close it by hand; to not wait, fire a run (the `/renovate` skill has how).
