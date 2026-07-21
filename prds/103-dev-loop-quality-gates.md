# PRD #103: Dev-loop quality gates — task runner, linters, dead code, formatting, coverage

**GitLab Issue**: [#103](https://gitlab.example.com/vtmocanu/uzi/-/issues/103)
**Status**: Draft (created 2026-07-21)
**Priority**: Medium
**Absorbs**: [#101](https://gitlab.example.com/vtmocanu/uzi/-/issues/101) item 3 (26-file gofmt drift)
**Related**: [#85](https://gitlab.example.com/vtmocanu/uzi/-/issues/85) (agenttmpl builtins drifted from the versioned role library)

Five milestones, phased so the enabling work (M1) unblocks four independent
tracks. M2 must land as one MR (see Decision 5); M3–M5 can ship in any order
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

~3.9k lines of bash — `e2e/run-e2e.sh` alone is 3646 lines — with no
shellcheck. An 858-line, 40 KB `.gitlab-ci.yml` with no yamllint. ~35 docs
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

`Taskfile.yml` at the repo root, with per-component namespaces. Matches
git-manager and the wider org convention (there is a `dot-ai-taskfile` skill
and a shared `wxs/task` library), gives `deps:`/`sources:` for free, and
handles `dir:` per component cleanly — which matters when the four gates run
in `api/`, `controller/`, `web/` and `agent/`.

*Rejected — Makefile*: tab-significant syntax, no native per-target working
directory, and no fingerprinting; every target would re-run every time.

*Rejected — scripts in a root `package.json`*: would make the two Go modules
subordinate to an npm package that exists only to hold scripts, and there is
no root `package.json` today.

**2. `task gate` composes per-component gates; it is not one flat recipe.**

`task gate` depends on `gate:api`, `gate:controller`, `gate:web`,
`gate:agent`, and each of those is itself `fmt-check` + `lint` + `typecheck`
+ `test` for that component. A contributor touching only `web/` runs
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

**4. Ratchet: baseline first, then hard-gate — never "fix everything, then
enable".**

For `golangci-lint`, `knip` and `deadcode`, the first run on a codebase this
size will produce a large finding list. Each of M2/M3 lands in two commits:
(a) enable the tool with a committed baseline/allowlist capturing today's
findings, gate ON for anything **new**; (b) burn the baseline down in
follow-up MRs, tracked as separate issues if large.

*Rejected — fix every finding before enabling*: produces one unreviewable
mega-diff and blocks the gate for weeks, during which new findings
accumulate.

*Rejected — enable with everything disabled and turn rules on one at a time*:
same end state, many more MRs, and the baseline file makes the remaining debt
visible in a way a disabled-rule list does not.

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

M5 adds `-coverprofile` and `vitest --coverage`, reports the numbers in CI job
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
      `gate:web`, `gate:agent`, plus the individual `fmt-check`, `lint`,
      `typecheck`, `test` targets each composes. `task gate` reproduces
      exactly today's four hand-typed recipes and nothing more — this
      milestone adds no new checks, so a green `main` stays green.
      The duplicated recipes are then deleted and replaced with `task`
      invocations in `CLAUDE.md` §Commands, `.claude/agent-team.md`
      (including its "Lint command: none dedicated" line),
      `.claude/agents/coder.md` and `.claude/agents/tester.md`. Those four
      agent files also take the generic-body sync from the skills-repo role
      library (new tester static-analysis duties, new reviewer deletion
      lens) so the roster and the Taskfile land together rather than
      rewriting the same tails twice. `.gitlab-ci.yml` gate jobs call
      `task gate:<component>`. Note `-buildvcs=false` stays a local-only
      flag per `CLAUDE.md` — it must not be baked into the Taskfile's
      committed targets.

**Phase 2 — the four independent tracks (any order, after M1)**

- [ ] **M2 — Formatting: drift cleared and gated, one MR**: commit one is a
      pure `gofmt -w ./api` over the 26 drifted files (list re-measured at
      implementation time, not copied from this PRD); commit two adds
      `task fmt-check` (`gofmt -l` on both Go modules, non-empty output
      fails) and its CI job. `./controller` is already clean and must stay
      that way. Prettier for `web/`+`agent/` is explicitly **out of scope**
      here — it is a much larger reformat and belongs with M3's ratchet if
      wanted at all.

- [ ] **M3 — Linting: golangci-lint + ESLint, with baselines**: `.golangci.yml`
      modelled on git-manager's (v2 schema, `staticcheck` `errcheck`
      `ineffassign` `unused` `unparam` `goconst` on; each *disabled* linter
      carries a one-line justification in the file, per that repo's
      convention) applied to both Go modules with per-linter test-file
      exclusions. ESLint flat config for `web/` and `agent/` per Decision 8,
      after verifying the oxlint question. Both land with a committed
      baseline (Decision 4) and a `lint:*` CI job that fails on new findings
      only. Baseline burn-down gets its own issue.

- [ ] **M4 — Dead code detection**: `golang.org/x/tools/cmd/deadcode -test
      ./...` per Go module (invoked via `go run` with a pinned version, the
      way `sqlc` already is at `v1.30.0` — git-manager's dependence on a
      preinstalled global binary is a trap worth not copying), plus `knip`
      for `web/` and `agent/` covering unused exports, files and
      dependencies. Baselined then gated. Success is measured, not assumed:
      the first run must be checked against at least one known-dead symbol
      (PRD #99's legacy `"Task"` path in `RunEvent.tsx`, if still present)
      to confirm the tool actually sees this repo's dead code.

- [ ] **M5 — The long tail: shell, YAML, secrets, vulns**: `shellcheck` over
      `e2e/*.sh` and `scripts/*.sh` (baselined — 3646 lines of
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

- [ ] **M6 — Coverage measured and `-race` everywhere**: `-race` added to
      `go test ./...` in `test:api` and `test:controller` (currently only
      `test:api-store-it` has it); `-coverprofile` for both Go modules and
      `vitest --coverage` for `web`, with the totals printed in CI job output
      and GitLab's coverage regex wired up so MRs show the number. **No
      failing threshold in this milestone** (Decision 6). Also fixes the
      `web/` vitest configuration so `jsdom` is the default environment
      rather than a per-file `// @vitest-environment` pragma present in 54 of
      79 test files — a missing pragma today silently runs a DOM test under
      node. Removes the vestigial `coverage.out` line from `.gitignore` or
      makes it real.

## Parallelization

| Phase | Milestones | Depends on | Files touched | Can run in parallel |
|---|---|---|---|---|
| 1 | M1 | — | `Taskfile.yml`, `.gitlab-ci.yml`, `CLAUDE.md`, `.claude/agent-team.md`, `.claude/agents/{coder,tester,reviewer}.md` | No — blocks all |
| 2 | M2 | M1 | `api/**/*.go` (format only), `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 2 | M3 | M1 | `.golangci.yml`, `web/eslint.config.js`, `agent/eslint.config.js`, `*/package.json`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 2 | M4 | M1 | `Taskfile.yml`, `knip.json` ×2, `.gitlab-ci.yml` | Yes |
| 2 | M5 | M1 | `.shellcheckrc`, `.yamllint`, `.gitleaks.toml`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 3 | M6 | M1 | `.gitlab-ci.yml`, `web/vite.config.ts`, `.gitignore` | Yes |

**Shared-file warning**: every Phase-2/3 milestone appends to `Taskfile.yml`
and `.gitlab-ci.yml`. Run them as parallel agents only with an explicit
instruction to append at the end of the relevant section and to stop-and-report
rather than resolve a conflict in either file — the same rule
`.claude/agents/coder.md` already applies to `api/go.mod` and
`docker-compose.yml`. M2 is the exception that must not be parallelised with
anything touching `api/**/*.go`, since it rewrites 26 files wholesale.

## Success Criteria

1. `task gate` from a clean checkout reproduces every check CI runs, and CI
   runs no check that `task gate` does not.
2. A newly introduced `gofmt` violation, `staticcheck` finding, dead Go
   function, unused TS export, or `shellcheck` error fails an MR pipeline.
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

1. **Are the CI gate jobs actually *required* to merge?** GitLab's
   "pipelines must succeed" setting is a project setting, not visible in-repo
   (no `.gitlab/` dir, no CODEOWNERS). If it is off, every job added here is
   advisory and the PRD's premise is weaker than stated. Check before M1.
2. **oxlint vs ESLint react-hooks parity** (Decision 8) — must be verified
   before M3, not assumed in either direction.
3. **Is a contributor `devbox` environment wanted at all?** The toolchain
   (go 1.26.4, node 22, helm, sqlc, glab, jq, openssl, docker, and now
   golangci-lint, shellcheck, gitleaks) is currently unpinned and documented
   only in prose. Decision 7 says *where* it must not go; whether to add one
   is undecided. If yes, it is its own milestone.
4. **Does `e2e/run-e2e.sh` survive shellcheck baselining, or does it want
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
