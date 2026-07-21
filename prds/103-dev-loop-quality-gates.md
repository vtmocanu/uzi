# PRD #103: Dev-loop quality gates — task runner, linters, dead code, formatting, coverage

**GitLab Issue**: [#103](https://gitlab.example.com/vtmocanu/uzi/-/issues/103)
**Status**: Draft (created 2026-07-21)
**Priority**: Medium
**Absorbs**: [#101](https://gitlab.example.com/vtmocanu/uzi/-/issues/101) item 3 (26-file gofmt drift)
**Review**: adversarial review 2026-07-21 (every repo claim re-checked against `main`). It caught a load-bearing factual error — Decision 4 originally specified a "committed baseline" for all four ratcheted tools, and only ESLint has one. Rewritten with per-tool mechanisms, verified against upstream docs. Also corrected: line/size counts, a wrong `-buildvcs=false` citation, M4's calibration symbol (undetectable by the tools M4 adds), Success Criterion 1's scope, and the `stages:` conflict M1 now pre-empts.
**Related**: [#85](https://gitlab.example.com/vtmocanu/uzi/-/issues/85) (agenttmpl builtins drifted from the versioned role library)

Six milestones, phased so the enabling work (M1) unblocks five independent
tracks. M2 must land as one MR (see Decision 5); M2–M6 can ship in any order
once M1 exists.

## Problem

uzi's CI is not a rubber stamp: `.gitlab-ci.yml` runs nine real gate jobs
across `api`, `controller`, `web` and `agent`, plus `helm lint`/`template`
and four kaniko validation builds, and they run on every MR. The problem is
not that nothing is enforced. It is that **everything enforced is a compiler
or a test, and nothing else exists at all.**

**1. There are no linters.**

Go is checked by `go vet ./...` and `go build ./...` (`validate:api`,
`validate:controller`). TypeScript is checked by `tsc --noEmit`. That is the
complete list. There is no `.golangci.yml`, no `eslint.config.*`, no
`biome.json`, no `.prettierrc`, no `.editorconfig`. `go vet` catches a
deliberately narrow class of bugs; it is not a substitute for `staticcheck`,
`errcheck`, `ineffassign`, or `unparam`.

**2. Formatting has already drifted, measurably.**

`gofmt -l ./api` reports **26 files** on `main` (re-measured 2026-07-21;
`./controller` is clean). This is entirely pre-existing — no recent branch
introduced it. PRD #97 recorded it as "a small loose end, worth a one-line
fix"; #101 corrected that, and it moves here because the fix and the gate
that prevents recurrence cannot be separated (Decision 5).

**3. Nothing detects dead code.**

Not for Go (`unused`, `deadcode`), not for TypeScript (`knip`, unused
exports). The only pressure that exists is file-local: Go's compiler rejects
unused imports and locals, and both tsconfigs set `noUnusedLocals` /
`noUnusedParameters`. Neither says anything about a **cross-package** dead
function or an **unused export**. This is the failure mode of every large
migration in this repo, and we have already paid for it: PRD #99 found
`RunEvent.tsx` still switching on the legacy `"Task"` tool name, "a dead path
for live runs", found by reading rather than by tooling.

**4. The long tail is entirely unchecked.**

~4.2k lines of bash — `e2e/run-e2e.sh` alone is 3646 lines — with no
shellcheck. An 809-line, 40 KB `.gitlab-ci.yml` with no yamllint. ~35 docs
and ~70 PRDs with no markdown link checking (`web/scripts/check-docs.mjs`
covers `docs/` frontmatter and links, but nothing covers `prds/`, `adr/`,
`CLAUDE.md`, or `ARCHITECTURE.md`). No secret scanner — a gap uzi already
documents to itself in `.claude/agents/auditor.md`: "CI (`.gitlab-ci.yml`)
runs validate/test/build across api/web/agent but has NO secret scanner
(gitleaks/trufflehog)." No `govulncheck`, no `npm audit`.

**5. There is no coverage signal, and `-race` runs almost nowhere.**

No `-coverprofile`, no `vitest --coverage`, no codecov. `.gitignore` carries
a vestigial `coverage.out` entry pointing at tooling that does not exist.
`-race` appears exactly once, in `test:api-store-it` (`.gitlab-ci.yml:187`);
the main `go test ./...` in `test:api` runs without it.

**6. There is no task runner, so the gate is copy-pasted prose.**

No Makefile, no Taskfile, no justfile. The same multi-line gate recipe is
written out by hand in at least four places — `CLAUDE.md` §Commands,
`.claude/agent-team.md`, `.claude/agents/coder.md`, `.claude/agents/tester.md`
— and drifts independently in each. `.claude/agent-team.md` states the
consequence outright: **"Lint command: none dedicated; `npm run build` in
web/ runs the check-docs + tsc gate."** Every agent spawned into this repo
inherits that, and no role is told to run a linter because there is none to
name.

## Solution Overview

Add a `Taskfile.yml` as the single source of truth for the gate, then hang
each missing check off it and wire the same task into `.gitlab-ci.yml`, so
local and CI enforcement cannot diverge.

The model is `wxs/git-manager`, whose `task build` is
`deps: [fmt, lint, deadcode, test-coverage-check]` before it will compile.
Two things are adapted rather than copied:

- git-manager is a **single Go module**; uzi is four toolchains (two Go
  modules, two npm packages) plus helm, bash and docs. The gate is therefore
  composed per component, not flat (Decision 2).
- git-manager's gate is **local-only** — it has no CI job that runs `task
  build`, and its own `CLAUDE.md` records the cost: "There is no CI gate
  running `task build` on PRs in this repo today, so the local gate is the
  only gate… Skipping it is how 136 `goconst` warnings accumulated unnoticed
  in late 2025." uzi already has CI that gates MRs, so every check added here
  is enforced there too (Decision 3).

## Design Decisions

**1. Taskfile, not Makefile or root npm scripts.**

A single self-contained `Taskfile.yml` at the repo root, with per-component
namespaces. Matches git-manager and the wider org convention (there is a
`dot-ai-taskfile` skill), gives `deps:`/`sources:` for free, and handles
`dir:` per component cleanly — which matters when the four gates run in
`api/`, `controller/`, `web/` and `agent/`.

Everything is defined inline. No `includes:`, no remote taskfiles: the gate
must be readable in one file, and uzi's CI has no route to any external task
library anyway.

*Rejected — Makefile*: tab-significant syntax and no native per-target working
directory, so every recipe would carry its own `cd`. (Not because make lacks
change detection — timestamp-based prerequisites are its core feature. That
applies to file targets; a gate made of `.PHONY` targets gets nothing from it,
which is the shape here. Taskfile's `sources:`/`checksum` fingerprinting does
cover phony-style targets, which is the real difference.)

*Rejected — scripts in a root `package.json`*: would make the two Go modules
subordinate to an npm package that exists only to hold scripts, and there is
no root `package.json` today.

**2. `task gate` composes per-component gates; it is not one flat recipe.**

`task gate` depends on `gate:api`, `gate:controller`, `gate:web`,
`gate:agent`, and each of those composes **the applicable subset** of
`fmt-check`, `lint`, `typecheck`, `test` for that component — not a uniform
four. The Go modules have no typecheck step (the compiler is it), and `web`
and `agent` have no `fmt-check` unless Prettier is adopted, which M2 excludes
and Open Question 5 leaves undecided. A contributor touching only `web/` runs
`task gate:web` and gets a fast, complete answer.

*Rejected — a single flat `task gate`*: forces a full four-toolchain run for
a one-line web change, which is how a gate stops being run.

**3. Every check added here is enforced in CI, in a new `lint` stage.**

New stage between `validate` and `test`, with `lint:api`, `lint:controller`,
`lint:web`, `lint:agent`, `lint:shell`, `lint:yaml`, `scan:secrets`. Each job
invokes the same `task` target a contributor runs locally.

*Rejected — local-only enforcement (git-manager's model)*: explicitly
rejected on git-manager's own evidence, quoted above. uzi has working CI;
declining to use it would be a deliberate downgrade.

*Rejected — warn-only jobs (`allow_failure: true`) permanently*: a warning
nobody must act on is a warning nobody reads. `allow_failure` is used only as
a transitional state inside a single milestone, never as an end state.

**4. Ratchet: gate on new findings first, burn down the rest after — but the
mechanism is different for every tool, and only one of them has a baseline
file.**

The first run of each tool on a codebase this size will produce a large
finding list, and blocking the gate until that list is zero is not an option.
An earlier draft of this decision said each tool lands with "a committed
baseline capturing today's findings". **That was wrong for four of the five
tools** (verified against current upstream docs, 2026-07-21), and the
correction matters enough to record, because the wrong version is the
intuitive one and it will be re-proposed otherwise:

| Tool | Baseline file? | Actual ratchet mechanism |
|---|---|---|
| `golangci-lint` | **No** | Diff-based only: `--new-from-merge-base=main` (upstream's own large-project advice), plus `--new-from-rev` / `--new-from-patch`. No file records existing findings. |
| `knip` | **No** | Severity staging: `rules: { exports: "warn", files: "error" }` per issue type, promoted to `error` as each reaches zero. Plus `--max-issues` as an issue budget, and workspace scoping. |
| `deadcode` | **No** | Nothing whatsoever. Plain report output. Gating on new findings requires a wrapper script: run the tool, diff against a committed findings file, fail on additions. |
| ESLint | **Yes** | Native bulk suppressions (`--suppress-all` writing a committed `eslint-suppressions.json`, with `--suppressions-location` and `--prune-suppressions`), ESLint ≥ 9.24. The only true baseline file in the set. **Caveat: it only suppresses rules configured as `error`** — `warn`-level rules are neither suppressed nor gated, so the M3 config must enable rules as `error`, not `warn`. |
| `shellcheck` | **No** | Severity staging (`--severity=error`, tightened to `warning` later), `.shellcheckrc` rule-level disables (blanket, not per-instance), per-line `# shellcheck disable=` comments, or the same diff-wrapper as `deadcode`. |

So each milestone states its own mechanism rather than inheriting a shared
one. Two consequences worth pricing in now:

- **`--new-from-merge-base` reports nothing on `main` pipelines** (the
  merge-base of `main` with itself is `main`), so the debt does not stay
  "visible" the way a baseline file would keep it. It also needs real git
  history: set `GIT_DEPTH: "0"` on the lint job **and** make sure `origin/main`
  is actually fetched — MR pipelines do not fetch it by default, and
  merge-base needs the ref, not merely the depth. `--whole-files` is needed
  too, or findings that are not on a changed line are skipped.
- **`deadcode` needs a small committed wrapper** (~20 lines: run, sort, diff
  against `.deadcode-baseline`, fail on additions). That is real work M4 must
  budget, not a flag.

*Rejected — fix every finding before enabling*: produces one unreviewable
mega-diff and blocks the gate for weeks, during which new findings
accumulate.

*Rejected — `warn` severity as an end state*: Decision 3 already rejects
warnings nobody must act on. knip's `warn` is a transitional state per issue
type, promoted to `error` when that type hits zero — not a permanent setting.

*Rejected — a hand-rolled baseline for golangci-lint too*: possible, but it
would duplicate a mechanism upstream has deliberately not built, and
`--new-from-merge-base` is what upstream's FAQ recommends for exactly this
situation. Accept the main-branch blind spot instead, and measure the total
finding count separately (a plain unfiltered run, reported not enforced) so
the debt is still countable.

**5. The 26-file gofmt reformat and the gofmt gate land in the same MR.**

This is the reason item 3 moved out of #101. The two orderings both fail:
gate-first turns CI red on 26 files nobody touched; reformat-first leaves the
tree clean with nothing preventing re-drift, and it re-drifts. #101's own
text anticipated this — "it wants its own commit, ideally with a CI check
afterwards" — but "afterwards" cannot span two issues.

The MR is two commits: one pure `gofmt -w ./api` (mechanically verifiable, no
review needed beyond confirming it is inert), one adding the gate. #101 item
4's AST comment-inertness checker, if it lands first, is exactly the tool that
proves commit one is inert — but this MR does not depend on it (`gofmt -d`
against the parent tree is sufficient for a pure-format commit).

**6. Coverage is measured before any threshold is set.**

M6 adds `-coverprofile` and `vitest --coverage`, reports the numbers in CI job
output, and sets **no** failing threshold. A threshold is chosen in a
follow-up, once the real number is known, starting with the security-critical
packages (`internal/store`, secretbox, the PAT redactor) rather than a global
floor.

*Rejected — pick a global threshold now*: any number chosen before measuring
is either below the current value (vacuous, and it ratchets nothing) or above
it (blocks every MR on unrelated work). git-manager's per-package floors are
the right end state, but it reached them across three PRDs of measurement,
not in one guess.

**7. Do NOT put dev tooling in the existing `devbox.json`.**

`devbox.json` at the repo root is **not** a contributor environment file. It
is tier-2 *worker* configuration: `agent/src/repo-tools.ts`
`extractRepoDevboxPackages()` reads its `packages` array into a run's
toolchain when the repo owner has enabled `repo_devbox_opt_in` and each
package is on the admin `tool_allowlist`. Its own header comment says so.
Adding `golangci-lint`, `shellcheck` and `knip` there would inject them into
every opted-in worker run.

A contributor toolchain, if one is added, goes in a separate file
(`devbox.dev.json` or `.devbox-dev/`), and the existing `devbox.json` gains a
one-line comment pointing at it so the next reader does not repeat the
mistake. Treated as optional and deferred (Open Question 3).

**Whatever is decided there, it is a local convenience only.** CI does not run
`devbox run`; CI images carry the tools directly. The Taskfile targets invoke
`golangci-lint`/`shellcheck`/`knip` by name and are indifferent to what put
them on `PATH`, so a contributor with a devbox shell and a CI job with a
prepared image run the identical target.

**8. ESLint for `web/` and `agent/`, pending one verification.**

Default to ESLint flat config with `typescript-eslint`, plus
`eslint-plugin-react-hooks` for `web/` — the hooks rules are the
highest-value TypeScript lint available for this repo, and `tsc` cannot
express them.

*Rejected for now — oxlint*: dramatically faster and near-zero config, and
worth revisiting if ESLint wall-time becomes a problem. **Not chosen because
its `react-hooks` rule parity has not been verified by anyone here** —
implementer must check this before the M3 MR rather than trusting either
option. If parity holds, oxlint is the better pick and this decision should
be amended, not worked around.

## Milestones

**Phase 1 — enabling work (blocks everything else)**

- [ ] **M1 — `Taskfile.yml` is the single source of truth for the gate**:
      root `Taskfile.yml` with `gate`, `gate:api`, `gate:controller`,
      `gate:web`, `gate:agent`, plus only the individual targets that have
      something to run today — `typecheck` and `test` per component, plus
      `web`'s `check-docs`. **`fmt-check` and `lint` are NOT created here**:
      no format or lint check exists yet, and M2 and M3 add those targets
      alongside the checks themselves. `task gate` reproduces exactly today's
      recipes and nothing more: **this milestone adds no new checks.** It does change how every CI job invokes them, which is not
      the same as changing nothing — see the CI caveats below.

      Files: `Taskfile.yml` (new); `.gitlab-ci.yml`; `CLAUDE.md` §Commands;
      `.claude/agent-team.md` (including its `:143` "Lint command: none
      dedicated" line); `.claude/agents/coder.md` (`:47-50`),
      `.claude/agents/tester.md` (`:56`), and `.claude/agents/reviewer.md`
      (which gains the dead-code reference the skills-repo deletion lens
      expects). Line numbers re-derived at implementation time, not trusted
      from here.

      **One self-contained `Taskfile.yml`. No `includes:`, no remote
      taskfiles, no shared task library.** Every target is defined in the
      file itself. A contributor or an agent reading it sees the whole gate
      without resolving anything.

      **`task` is not in the CI images.** `golang:1.26`, `node:22-alpine`
      and `alpine/helm` are digest-pinned and ship no task runner, so every
      job needs an install step (or a shared `.task_setup` `before_script`
      fragment, which is the cheaper option and keeps the pin in one place).
      Same rule for every tool a later milestone adds: **the CI job must
      provide it — never devbox.** "Provide" means whichever of these fits:
      baked into a digest-pinned image (the lint jobs can use the official
      `golangci/golangci-lint`, `koalaman/shellcheck-alpine` and `gitleaks`
      images rather than installing into `golang:1.26`), fetched at job time
      via a pinned `go run …@vX.Y.Z` (the `sqlc@v1.30.0` precedent in
      `validate:api`, and how M4 will invoke `deadcode`), installed as an npm
      devDependency by the existing `npm ci` (knip, ESLint), or a
      `before_script` install.

      **Anything fetched in a `before_script` must be version- and
      sha256-pinned**, matching what `e2e:kind-smoke` already does for
      kind/kubectl/helm. Every image in this file is digest-pinned; an
      unverified `curl | sh` for `task` would be the first unpinned fetch in
      the pipeline, and it would arrive in the MR that claims to add no new
      checks.

      This is the milestone's main risk: the checks are unchanged, but a
      `task`-install or PATH failure reds CI having changed no check at all.
      Land it as its own MR and watch the first pipeline.

      **The Taskfile does not know about devbox.** Targets invoke tools
      directly (`golangci-lint run`, `shellcheck …`), and whatever put them
      on `PATH` is not its concern — a devbox shell locally, the image in
      CI. CI jobs call `task <target>`, never `devbox run -- task <target>`.

      **Two jobs do not map cleanly onto a component gate and must keep
      their current shape:** `validate:web` (check-docs + tsc) and
      `test:web` (vitest) are separate jobs in separate stages, so a job
      calling `task gate:web` would run vitest twice — CI calls the
      fine-grained targets instead (at M1 that is `task check-docs:web` +
      `task typecheck:web` for `validate:web` and `task test:web` for
      `test:web`; M3 adds `task lint:web` to the first), and `task gate:web`
      stays the local convenience wrapper. And
      `test:api-store-it` (`.gitlab-ci.yml:185-197`) wraps `go test` in a
      pipefail + `grep -c '^--- PASS'` / `'^--- SKIP'` assertion that exists
      to catch the suite silently skipping against a missing Postgres; that
      logic is CI-specific and stays in `.gitlab-ci.yml`. Local and CI
      therefore do not fully converge, and Success Criterion 1 is scoped
      accordingly.

      M1 also adds the empty `- lint` entry to `stages:`
      (`.gitlab-ci.yml:44-50`) even though it adds no lint job. That single
      line is genuinely inert, and it is the one edit M3 and M5 would
      otherwise both make at the identical position (see Parallelization).

      Note `-buildvcs=false` is a local-only flag per
      `.claude/agents/coder.md:58` (**not** `CLAUDE.md`, which does not
      mention it) — it must not be baked into the Taskfile's committed
      targets.

      **Deferrable sub-item**: the `.claude/agents/*` generic-body sync from
      the skills-repo role library (tester static-analysis duties, reviewer
      deletion lens) rides along **only if** that PR has merged by the time
      M1 lands. It is bundled here to avoid editing the same agent files
      twice, but it must not block the critical path: if the PR is still
      open, M1 ships the tail rewrites alone and the body sync becomes a
      follow-up.

**Phase 2 — the four independent tracks (any order, after M1)**

- [ ] **M2 — Formatting: drift cleared and gated, one MR**: commit one is a
      pure `gofmt -w ./api` over the 26 drifted files (list re-measured at
      implementation time, not copied from this PRD); commit two adds
      `task fmt-check` (`gofmt -l` on both Go modules, non-empty output
      fails) and its CI job. `./controller` is already clean and must stay
      that way. Prettier for `web/`+`agent/` is explicitly **out of scope**
      here — it is a much larger reformat and belongs with M3's ratchet if
      wanted at all.

- [ ] **M3 — Linting: golangci-lint + ESLint, each ratcheted its own way**: `.golangci.yml`
      modelled on git-manager's (v2 schema, `staticcheck` `errcheck`
      `ineffassign` `unused` `unparam` `goconst` on; each *disabled* linter
      carries a one-line justification in the file, per that repo's
      convention) applied to both Go modules with per-linter test-file
      exclusions. ESLint flat config for `web/` and `agent/` per Decision 8,
      after verifying the oxlint question.

      Ratchet mechanisms differ per Decision 4 and must be stated in the MR:
      Go uses `--new-from-merge-base=main --whole-files`, which requires
      raising GitLab's shallow `GIT_DEPTH` for the lint job or merge-base
      resolution fails; TypeScript uses ESLint's native bulk suppressions
      (`--suppress-all` → a committed `eslint-suppressions.json`, ESLint
      ≥ 9.24). Also record the **unfiltered** finding count for each in the
      MR description, since `--new-from-merge-base` reports nothing on `main`
      pipelines and the debt is otherwise uncountable. Burn-down gets its own
      issue.

- [ ] **M4 — Dead code detection**: `golang.org/x/tools/cmd/deadcode -test
      ./...` per Go module (invoked via `go run` with a pinned version, the
      way `sqlc` already is at `v1.30.0` — git-manager's dependence on a
      preinstalled global binary is a trap worth not copying), plus `knip`
      for `web/` and `agent/` covering unused exports, files and
      dependencies.

      Neither tool has a baseline file (Decision 4). `deadcode` needs a small
      committed wrapper — run, sort, diff against `.deadcode-baseline`, fail
      on additions — and writing it is part of this milestone, not a flag to
      pass. `knip` uses severity staging (`rules: { exports: "warn", files:
      "error", … }`), promoting each issue type to `error` as it reaches
      zero, with `--max-issues` as a regression budget in the meantime.

      **Calibration**: the first run must be checked against a symbol known
      to be dead, so that "no findings" is distinguishable from "tool not
      wired up". Choose an actually-detectable one: an unexported Go function
      with no callers, or an unused TS export. **Do not use PRD #99's legacy
      `"Task"` switch case** (`web/src/components/RunEvent.tsx:87` and `:492`
      — still present, re-verified 2026-07-21): it is a dead *branch inside a
      live function*, and neither `deadcode` (unreachable functions) nor
      `knip` (unused exports/files/deps) can see it. That is a real coverage
      limit of this milestone and worth stating: dead branches stay the
      reviewer's job, which is why the skills-repo change gives the reviewer
      a deletion lens rather than relying on tooling alone.

- [ ] **M5 — The long tail: shell, YAML, secrets, vulns**: `shellcheck` over
      `e2e/*.sh` and `scripts/*.sh` (shellcheck has no baseline file either —
      start at `--severity=error` and tighten to `warning` once clean; 3646 lines of
      `run-e2e.sh` will not be clean on day one, and per #101 that file must
      be referenced by content anchor, never by line number); `yamllint` over
      `.gitlab-ci.yml` and `deploy/values/`; `gitleaks` in CI, which lets
      `.claude/agents/auditor.md` stop documenting its own absence;
      `govulncheck` for both Go modules and `npm audit --audit-level=high`
      for both npm packages, initially `allow_failure` only until the
      current finding count is known. A markdown link checker over `prds/`,
      `adr/` and the root docs (git-manager's network-free
      link-plus-anchor test is the reference implementation) is included if
      it fits; otherwise it splits out.

**Phase 3 — measurement (after M1; independent of M2–M5)**

- [ ] **M6 — Coverage measured and `-race` everywhere**: `-coverprofile` for
      both Go modules and `vitest --coverage` for `web` (which needs
      `@vitest/coverage-v8` added to `web/package.json` — it is not currently
      a dependency), with the totals printed in CI job output and GitLab's
      coverage regex wired up so MRs show the number. **No failing threshold
      in this milestone** (Decision 6).

      `-race` added to `go test ./...` in `test:api` and `test:controller`
      (currently only `test:api-store-it` has it). **This is the riskiest
      single change in the PRD and ships first, on its own, before the
      coverage work.** `-race` typically costs 2–10× runtime, so measure the
      before/after on both modules and check it against the job timeout; and
      it detects real races that have been latent, so `main` can go red on
      code nobody touched. Unlike M2 and M3 there is no ratchet available —
      you cannot gate `-race` on new findings. If the first run is red,
      stop: file the races as their own issue and land `-race` behind
      `allow_failure: true` **with a dated expiry note**, rather than leaving
      a permanent advisory job (Decision 3).

      Also fixes the `web/` vitest configuration: `jsdom` is currently opted
      into per-file via `// @vitest-environment jsdom` in 54 of 79 test
      files, so a missing pragma silently runs a DOM test under node.
      Prefer an explicit per-directory project config over flipping the
      global default — a blanket default flips the remaining ~25 files from
      node to jsdom, which is the same class of silent-wrong-environment bug
      in the opposite direction. (`environmentMatchGlobs` does the same job
      and works on the pinned `vitest ^2.1.9`, but it is deprecated from
      Vitest 3 in favour of the projects/workspace config, so it buys a
      migration later.) Removes the
      vestigial `coverage.out` line from `.gitignore:49` or makes it real.

## Parallelization

| Phase | Milestones | Depends on | Files touched | Can run in parallel |
|---|---|---|---|---|
| 1 | M1 | — | `Taskfile.yml`, `.gitlab-ci.yml`, `CLAUDE.md`, `.claude/agent-team.md`, `.claude/agents/{coder,tester,reviewer}.md` | No — blocks all |
| 2 | M2 | M1 | `api/**/*.go` (format only), `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 2 | M3 | M1 | `.golangci.yml`, `web/eslint.config.js`, `agent/eslint.config.js`, `*/package.json`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 2 | M4 | M1 | `Taskfile.yml`, `knip.json` ×2, `.gitlab-ci.yml` | Yes |
| 2 | M5 | M1 | `.shellcheckrc`, `.yamllint`, `.gitleaks.toml`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 3 | M6 | M1 | `.gitlab-ci.yml`, `web/vite.config.ts`, `web/package.json`, `.gitignore` | Yes |

**Shared-file warning**: every Phase-2/3 milestone appends to `Taskfile.yml`
and `.gitlab-ci.yml`. Run them as parallel agents only with an explicit
instruction to append at the end of the relevant section and to stop-and-report
rather than resolve a conflict in either file — the same rule
`.claude/agents/coder.md` already applies to `api/go.mod` and
`docker-compose.yml`.

Two exceptions where "append at the end" is not enough:

- **The `stages:` list is a single hot line.** M3 and M5 both need a `lint`
  stage, and both would insert `- lint` at the identical position in
  `.gitlab-ci.yml:44-50`. That is why M1 adds the (empty, inert) stage entry
  up front — appending cannot resolve it.
- **M2 must not run in parallel with anything touching `api/**/*.go`**, since
  it rewrites 26 files wholesale. (It is safe against `validate:api`'s
  sqlc-drift gate: none of the 26 drifted files are sqlc-generated — all are
  hand-written, mostly tests — so `gofmt -w` cannot trip the
  `git diff --exit-code -- internal/store` check.)

## Success Criteria

1. `task gate` from a clean checkout reproduces every **toolchain** check CI
   runs (format, lint, typecheck, test across `api`, `controller`, `web`,
   `agent`), and CI runs no toolchain check that `task gate` does not.
   Deliberately excluded from this criterion, because they cannot run
   meaningfully from a plain local checkout: the four kaniko image builds,
   `helm_chart`, the sqlc-drift `git diff --exit-code` in `validate:api`,
   `test:api-store-it`'s Postgres service and its ran/skipped assertion, and
   `e2e:kind-smoke`.
2. A newly introduced `gofmt` violation, `staticcheck` finding, dead Go
   function, or `shellcheck` error fails an MR pipeline. **Unused TS exports
   are the exception and reach this bar last**: knip's staging sets
   `exports: "warn"`, and warn-severity issues do not count toward its error
   total, so a new unused export gates nothing until that type is burned down
   to zero and promoted to `error`. (`--max-issues` is a fixed budget, not a
   ratchet — fix one old finding, add one new, still under budget.) If
   gate-on-new is wanted for exports before the burn-down finishes, knip needs
   the same diff-wrapper M4 writes for `deadcode`.
3. The gate command appears in exactly one place in the repo; `CLAUDE.md` and
   the `.claude/agents/*` tails reference it rather than restating it.
4. `.claude/agent-team.md` no longer says "Lint command: none dedicated".
5. `.claude/agents/auditor.md` no longer documents the absence of a secret
   scanner, because one runs.
6. Coverage percentages for `api`, `controller` and `web` are visible on
   every MR.
7. `main` is `gofmt`-clean across both Go modules.

## Documentation Corrections Folded In

- `.claude/agent-team.md` "Project signals": **"Lint command: none
  dedicated; `npm run build` in web/ runs the check-docs + tsc gate"** —
  false after M3; corrected in M1 to name `task lint`.
- `.claude/agents/auditor.md`: "CI (`.gitlab-ci.yml`) runs validate/test/build
  across api/web/agent but has NO secret scanner (gitleaks/trufflehog)" —
  corrected in M5.
- `CLAUDE.md` §Commands: the four hand-typed recipes are replaced by `task`
  invocations in M1. The surrounding prose about e2e being deliberately out
  of CI stays true and is not touched.
- `.gitignore`: the `coverage.out` entry currently refers to tooling that
  does not exist — M6 either makes it real or removes it.
- `docs/dev-conventions.md` (`audience: contributor`) currently covers only
  the `glab` bot scripting and the `UZI_E2E_BOT_*` vars, and says nothing
  about lint/format/test policy. M1 adds the gate; each later milestone adds
  its check.

## Open Questions

1. ~~**Are the CI gate jobs actually *required* to merge?**~~ **RESOLVED
   2026-07-21.** They were not — `only_allow_merge_if_pipeline_succeeds` was
   `false`, so a maintainer could merge a red MR and every gate here would
   have been advisory. Now set to `true` (with `allow_merge_on_skipped_pipeline:
   true`, so the repo's `[skip ci]` doc commits stay mergeable). CI is a real
   gate, and the PRD's premise holds.
2. **oxlint vs ESLint react-hooks parity** (Decision 8) — must be verified
   before M3, not assumed in either direction.
3. **Is a contributor `devbox` environment wanted at all?** The toolchain
   (go 1.26.4, node 22, helm, sqlc, glab, jq, openssl, docker, and now
   golangci-lint, shellcheck, gitleaks) is currently unpinned and documented
   only in prose. Decision 7 says *where* it must not go; whether to add one
   is undecided. If yes, it is its own milestone, and it changes nothing
   about the Taskfile or CI — it is a local way to get the tools on `PATH`,
   nothing more.
4. **Does `e2e/run-e2e.sh` survive shellcheck severity staging, or does it want
   splitting first?** 3646 lines in one file may produce a baseline so large
   it is meaningless. M5 should measure before committing to the approach.
5. **Prettier/`gofumpt`** — deliberately excluded from M2. Worth a follow-up
   or not?

## Out of Scope

- **Bringing e2e into CI.** `e2e/run-e2e.sh` is deliberately out of CI (it
  needs docker compose on the runner) and `e2e:kind-smoke` is
  protected-refs-only for stated privilege reasons. Both stay as they are.
  Making the ~30-minute local e2e gate verifiable is a real problem and a
  different one.
- **Renovate / Dependabot.** Dependency *update* automation is separable from
  dependency *auditing*; `govulncheck` and `npm audit` are in M5, automated
  bumps are not.
- **`api/internal/agenttmpl/builtins/*.md`** — the product role library
  shipped to users' runs. It drifted from the versioned library and that is
  tracked in #85. M1 touches `.claude/agents/` (this repo's own dev team)
  only; the builtins reach parity via #85's sync, not by hand here.
- **#101 items 1, 2 and 4** — the viewer-identity regression guards, the
  `GetSkill` properties, and the AST comment-inertness checker stay in #101.
- **New tests.** This PRD adds the machinery that measures and enforces; it
  does not write test cases. M6 deliberately sets no coverage threshold for
  that reason.
