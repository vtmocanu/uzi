# PRD #103: Dev-loop quality gates — task runner, linters, dead code, formatting, coverage

**GitLab Issue**: [#103](https://gitlab.example.com/vtmocanu/uzi/-/issues/103)
**Status**: Draft (created 2026-07-21)
**Priority**: Medium
**Absorbs**: [#101](https://gitlab.example.com/vtmocanu/uzi/-/issues/101) item 3 (26-file gofmt drift)
**Review**: adversarial review 2026-07-21 (every repo claim re-checked against `main`). It caught a load-bearing factual error — Decision 4 originally specified a "committed baseline" for all four ratcheted tools, and only ESLint has one. Rewritten with per-tool mechanisms, verified against upstream docs. Also corrected: line/size counts, a wrong `-buildvcs=false` citation, M4's calibration symbol (undetectable by the tools M4 adds), Success Criterion 1's scope, and the `stages:` conflict M1 now pre-empts.

**Second review 2026-08-02** (every repo claim re-checked against `e0472a88`; the PRD was last edited 2026-07-21 in `8679e37a` and the tree moved under it within hours). Three blockers, all premise rot rather than design error:

- **Decision 1 argued *for* the silent-green mechanism this repo mandates `-count=1` against.** Its Taskfile justification cited `sources:`/`checksum` fingerprinting as "the real difference" from make. Rewritten, with a hard ban and a measurement (see Decision 1).
- **The "gate is copy-pasted prose" premise was stale.** The `.claude/agent-team.md` line Success Criterion 4 targeted was deleted in `027a4b88`, the commit immediately after this PRD's last edit, and replaced by a deliberate **paste-block** whose whole rationale is duplication. SC3 as written would have deleted that mechanism. Resolved at the user's direction 2026-08-02 (see Decision 9).
- **`-race` was already on `test:api`** (`.gitlab-ci.yml:178`, landed `224b5349` for PRD #108 M4). M6's "riskiest single change" was already done; rescoped to `controller` only.

Plus: every numeric count in the document had drifted and is now struck (Decision 10), `check-docs.mjs` had grown to cover three of the four files Problem #4 claimed were uncovered, Decision 7 repeated a clause `devbox.json` itself corrects as false, and only M4 had a calibration step (now all of M2–M6 do).
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

`gofmt -l ./api` reports a non-empty list on `main`; `./controller` is clean.
**The count is deliberately not recorded here** — see Decision 10. It has read
26, 25, 19 and 16 on four different days, and `.claude/agent-team.md:1088-1094`
forbids recording it in the paste-block for exactly that reason, naming the
error a stale tally already caused. Re-measure at implementation time.

This drift is entirely pre-existing — no recent branch introduced it. PRD #97
recorded it as "a small loose end, worth a one-line fix"; #101 corrected that,
and it moves here because the fix and the gate that prevents recurrence cannot
be separated (Decision 5).

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

Several thousand lines of bash across every tracked `*.sh` (`git ls-files
'*.sh'` for the current set and total — counts not recorded per Decision 10;
`e2e/run-e2e.sh` alone is the largest by an order of magnitude) with no
shellcheck. A `.gitlab-ci.yml` well past a thousand lines with no yamllint.

Markdown link checking is **partial, not absent**, and the previous version of
this paragraph was wrong about it: `web/scripts/check-docs.mjs:161` sets
`extraLinkFiles = ["ARCHITECTURE.md", "README.md", "CLAUDE.md"]` and appends
every `specs/*.md`, validating both relative-link existence and link-*text*-path
correctness. It landed for issue #132 (2026-07-25) after 36 dead PRD paths had
accumulated, 11 in `ARCHITECTURE.md` alone, and it runs in `validate:web` and in
`npm run build`. **The real remaining gap is `prds/` and `adr/` as link
*sources*** — they are valid link *targets* today, but nothing checks the links
they themselves contain. No secret scanner — a gap uzi already
documents to itself in `.claude/agents/auditor.md`: "CI (`.gitlab-ci.yml`)
runs validate/test/build across api/web/agent but has NO secret scanner
(gitleaks/trufflehog)." No `govulncheck`, no `npm audit`.

**5. There is no coverage signal.**

No `-coverprofile`, no `vitest --coverage`, no codecov. `.gitignore:66` carries
a vestigial `coverage.out` entry pointing at tooling that does not exist.

**`-race` is NOT part of this problem for `api`, and the previous version of
this paragraph was wrong that it was.** `.gitlab-ci.yml:178` is
`go test -race -count=1 ./...`, landed 2026-07-25 in `224b5349` for PRD #108 M4,
under a 25-line comment (`:157-178`) stating that dropping either flag silently
undoes a fix, with the cost already measured. `test:api-store-it` has it too
(`:224`). **The only module still without `-race` is `controller`**
(`.gitlab-ci.yml:299`, `go test -count=1 ./...`). M6 is scoped accordingly.

**6. There is no task runner, so the gate recipe is duplicated by hand.**

No Makefile, no Taskfile, no justfile. The same multi-line gate recipe is
written out by hand in at least four places — `CLAUDE.md` §Commands,
`.claude/agent-team.md:1082-1110`, `.claude/agents/coder.md:69-70`,
`.claude/agents/tester.md:158-181` — and drifts independently in each.

**Two corrections to how this problem was originally stated, both material:**

- The quoted consequence — *"Lint command: none dedicated; `npm run build` in
  web/ runs the check-docs + tsc gate"* — **no longer exists anywhere in the
  repo.** It was removed in `027a4b88` (2026-07-21), the commit immediately
  after this PRD's last edit. The current line is
  `lint           none (gap)          # no golangci-lint, no eslint; go vet in CI only`
  (`.claude/agent-team.md:1097`). The underlying complaint still holds; the
  evidence cited for it does not.
- **The duplication is deliberate, and it must survive this PRD.**
  `.claude/agent-team.md:1084-1086` states why: *"Paste this block into every
  tester, reviewer and auditor dispatch — teammates cold-start and never read
  this file, so a slot you do not paste is a slot they cannot run."* So the
  problem is not "the recipe appears more than once". It is that **the
  duplicated copies are hand-maintained and drift silently** — which is exactly
  what happened to the line above. Decision 9 states the resolution.

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
`dot-ai-taskfile` skill), gives `deps:` for free, and handles `dir:` per
component cleanly — which matters when the four gates run in `api/`,
`controller/`, `web/` and `agent/`.

Everything is defined inline. No `includes:`, no remote taskfiles: the gate
must be readable in one file, and uzi's CI has no route to any external task
library anyway.

**🔴 HARD RULE — no `sources:`, `generates:` or `status:` on any gate target.**
Not on `gate`, `gate:*`, `test:*`, `lint:*`, `fmt-check:*`, `typecheck:*` or
`check-docs:*`. If a target must carry `sources:` for some other reason, it also
carries `method: none`.

An earlier version of this decision cited that very feature as the argument
*for* Taskfile — *"Taskfile's `sources:`/`checksum` fingerprinting does cover
phony-style targets, which is the real difference"* — which is the opposite of
what this repo needs. Measured on the installed `task` 3.51.1, 2026-08-02:

```
=== run 1 ===  task: [cached] echo "RAN cached cmds"   RAN cached cmds   rc=0
=== run 2 ===  task: Task "cached" is up to date                          rc=0
=== run 3 ===  (a file NOT in sources: changed)
               task: Task "cached" is up to date                          rc=0
```

Exit 0, nothing executed, output indistinguishable from a passing run. That is
precisely the silent-green failure `CLAUDE.md` mandates `-count=1` against at
both Go gates. Three aggravations specific to this repo:

- **Same cross-module blind spot as Go's test cache.** `checksum` sees only the
  globs listed, and this repo's gates deliberately read files outside the module
  being tested: `fixtures/judge-fidelity/{cases,expected}.json` at the repo root,
  read by `api/internal/workersvc/`; `controller/internal/{protocol,preset}/`
  reading `api/internal/hostedsvc/testdata/`. `.gitlab-ci.yml:145-156` and
  `:277-293` spend 40 lines of comment on exactly this. Run 3 above is that
  shape reproduced in miniature.
- **`.task/` is gitignored** (`.gitignore:42`, already present — nothing to add).
  So CI always runs cold and always executes, while the contributor's `task gate`
  silently skips. That is divergence in the worst direction, and it makes Success
  Criterion 1 false in practice while reporting green.
- **Deps still run when a task is up to date** (verified), so a partial skip is
  harder to spot than a total one.

*Rejected — Makefile*: tab-significant syntax and no native per-target working
directory, so every recipe would carry its own `cd`. (Not because make lacks
change detection — timestamp-based prerequisites are its core feature, and a
gate made of `.PHONY` targets gets nothing from it. Per the rule above, uzi's
gate wants nothing from it either; change detection is not a reason to prefer
either tool here.)

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

**`output: group` at the Taskfile root is required, not cosmetic.** Task runs
`deps:` concurrently and interleaves their stdout line-by-line by default.
Measured on `task` 3.51.1, 2026-08-02, with three deps where one exits 7:

```
default:          A1 C1 B1 A2 B2 C2 A3 B3 C3         <- interleaved
output: group:    C1 C2 C3 | B1 B2 B3 | A1 A2 A3     <- per-task blocks
both:             task: Failed to run task "gate": ... exit status 7   rc=201
```

Two consequences:

- `CLAUDE.md` and `.claude/agent-team.md` both require reading **named failing
  tests**, never a tally. Four suites interleaved makes that impossible, and it
  is a new way to produce the gate-status misreads that file already documents
  four instances of. `output: group` (or `prefixed`) fixes it.
- **`task`'s own exit code is 201, not the underlying 7**, under both output
  modes. Any wrapper reading `$?` numerically must know this. Nothing in the
  plan does today; say so before someone writes one.

**CPU contention is a measured flake source in this repo already.**
`web/vite.config.ts:11-20` raised `testTimeout` to 20000 because "under full-suite
CPU contention, THREE unrelated tests each timed out once across ~20 runs".
Running two Go modules and `node --test` alongside vitest makes that strictly
worse. Nothing in this decision *requires* concurrency: either serialise `gate`'s
component deps (`cmds:` calling each in turn rather than `deps:`), or keep them
concurrent and measure the flake rate before landing. Decide in M1, record which
and why.

**3. Every check added here is enforced in CI, in a new `lint` stage.**

New stage between `validate` and `test`, with `lint:api`, `lint:controller`,
`lint:web`, `lint:agent`, `lint:shell`, `lint:yaml`, `scan:secrets`. Each job
invokes the same `task` target a contributor runs locally.

**The stage is organisational, not ordering.** All eight existing gate jobs set
`needs: []`, so they start immediately regardless of stage. A new `lint` stage
therefore does **not** make lint gate test — everything still runs concurrently
and the pipeline fails if any job fails. Stated because the placement "between
`validate` and `test`" reads as sequencing and is not.

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
gate-first turns CI red on files nobody touched; reformat-first leaves the
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
toolchain when the repo owner has enabled `repo_devbox_opt_in`. Its own header
comment says so. Adding `golangci-lint`, `shellcheck` and `knip` there would
inject them into every opted-in worker run.

**This decision previously added "and each package is on the admin
`tool_allowlist`". That clause is false, and `devbox.json:7-15` carries a dated
CORRECTION block saying so** (2026-07-25, PRD #123: *"That is FALSE and was
load-bearing in the wrong direction"*). Tier-2 is bounded by **shape only** —
`PKG_RE`, a 128-char cap and a 64-package cap in `agent/src/repo-tools.ts`; the
allowlist gates tier-1. The conclusion here is unaffected and in fact
**strengthened**: an unbounded set is a worse place to inject linters, not a
better one.

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

**9. The Taskfile is the single source for gate *recipes*; the agent-team
paste-block stays and names *targets*.**

*Decided by the user, 2026-08-02.* This resolves a direct contradiction between
the original Success Criterion 3 ("the gate command appears in exactly one place
in the repo") and `.claude/agent-team.md:1084-1086`, which mandates pasting the
gate block into every tester/reviewer/auditor dispatch *because* teammates
cold-start and cannot resolve a reference. Both could not hold; SC3 as written
would have deleted the paste mechanism.

The split:

- **`Taskfile.yml` holds every recipe.** No command line is written out
  anywhere else.
- **The paste-block keeps existing and names targets** — `task gate:api`,
  `task lint:web` — never recipes.
- **The block keeps a one-line "why" for each load-bearing flag**, even though
  teammates no longer type them: `-count=1` (both Go modules, cross-module
  fixtures), `-race` (api, PRD #108 M4), `-p 1` (store-it),
  `--test-timeout=30000` (agent). They must still recognise one going missing.
- **Every non-command slot stays verbatim**: the `none (gap)` lines and their
  `noted` markers, the do-not-record-a-gofmt-count warning, the e2e timing
  samples. The Taskfile has no home for any of it.

*Why not keep literal commands in the block (the status-quo-plus option)*: the
decisive argument is that **staleness then fails silently**. A pasted
`go test -count=1 ./...` that has drifted from the real gate still runs, still
prints `ok`, and the teammate reports green. A stale *target* name dies with
`task: Task "gate:api" does not exist` and a nonzero exit. This repo's entire
documented failure history is silent-green over loud-red, and the block has
already rotted once in exactly this way (Problem #6).

*Deferred — generate the block from the Taskfile and assert it matches in CI*:
strictly better than either option, and the right end state. Not scoped here;
worth its own issue once the Taskfile exists.

**10. No count of anything appears in this PRD.**

Not a style preference. `.claude/agent-team.md:1088-1094` already forbids
recording a gofmt count in the paste-block, naming the failure it caused: *"Do
NOT record a count here: it read 26, then 25, and a stale tally invites the
truncated-view error it already caused (a filtered 4-file view reported as the
whole list, 2026-07-25)."* This PRD recorded 26 in five places and, by
2026-08-02, **every numeric claim in it had drifted** — the gofmt list, the CI
file's size, `run-e2e.sh`'s length, the bash total, the jsdom pragma ratio, the
docs and PRD counts, and six line-number citations.

So: state the shape, cite the command that measures it, never the number. Where
a milestone needs a figure it says "re-measure at implementation time" and the
MR description carries the value. Arguments must survive the count changing —
Decision 5's is a good example, since it holds for any non-empty drift list.

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

      Files, **re-derived against `e0472a88` on 2026-08-02 — every citation in
      the previous version of this list was wrong**, and they will drift again,
      so re-derive rather than trust:

      - `Taskfile.yml` (new)
      - `.gitlab-ci.yml`
      - `CLAUDE.md` §Commands — **command lines only**, see the scoping note
        below
      - `.claude/agent-team.md` — the "Quality gates" paste-block at
        `:1082-1110`, per Decision 9. (The `:143` "Lint command: none dedicated"
        line this PRD used to cite **no longer exists**; it went in `027a4b88`.)
        Its closing line already says *"Every gap above is what PRD #103 exists
        to close; re-derive this block when its milestones land"* (`:1131`).
      - `.claude/agents/coder.md:69-70` — the inline slot summary
      - `.claude/agents/tester.md:158-181` — **a second full copy of the slot
        table**, which the previous file map missed entirely
      - `.claude/agents/auditor.md:63` — already reads "PRD #103 M5 adds them"
      - `.claude/agents/reviewer.md:65` — gains the dead-code reference the
        skills-repo deletion lens expects

      **Scoping, because this is where a coder will overreach.** `CLAUDE.md`
      §Commands is ~340 lines across five component sections plus Integration
      tests, and only a small fraction of it is recipes. The rest is measured
      evidence that exists because someone trusted a green that ran nothing.
      **Replace only the command lines; every measurement paragraph stays
      verbatim.**

      **Each Taskfile target carrying a load-bearing flag gets an inline comment
      naming why it is there**, and the flags are not optional:

      | Target | Flag | Why |
      |---|---|---|
      | `test:api` | `-count=1` | cross-module `fixtures/` reads are cache-invisible |
      | `test:api` | `-race` | PRD #108 M4; `.gitlab-ci.yml:157-178` |
      | `test:controller` | `-count=1` | cross-module goldens under `api/internal/hostedsvc/testdata/` |
      | store-it | `-p 1` | package binaries race one shared database |
      | `test:agent` | `--test-timeout=30000` | node's default is *no* timeout; `agent/test/judge-runner.test.ts:167` is written against the cap |

      **`test:api` must carry `-race` AND `-count=1` or M1 silently weakens the
      api gate** while its own text claims it adds no new checks. This is the
      milestone's second real risk after the `task`-install one, and
      `.gitlab-ci.yml:161-163` already names the live threat by name: *"a future
      'simplify the gate' edit"*. Moving these flags into a new file **is** such
      an edit.

      **One self-contained `Taskfile.yml`. No `includes:`, no remote
      taskfiles, no shared task library.** Every target is defined in the
      file itself. A contributor or an agent reading it sees the whole gate
      without resolving anything.

      **Acceptance criterion — prove the gate is live, do not assert it.** No
      `sources:`/`generates:`/`status:` on any gate target (Decision 1), and the
      MR must demonstrate the ban works with the control `CLAUDE.md` already
      mandates for this exact hazard: **delete a case from
      `fixtures/judge-fidelity/cases.json` and confirm `task gate:api` reddens**
      with `fixture broken: cases.json has no case …`, then restore it and
      confirm green. Nothing weaker discriminates a live gate from a cached one
      — a run printing no cache markers, or exiting 0, is satisfied by a gate
      that executed nothing. Set `output: group` at the root and record the
      serialise-vs-concurrent decision (Decision 2).

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
      `test:api-store-it` (`.gitlab-ci.yml:196-262`) wraps `go test` in a
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
      `.claude/agents/coder.md:85` and `.claude/agents/tester.md:184`
      (**not** `CLAUDE.md`, which does not mention it) — it must not be baked
      into the Taskfile's committed targets.

      **Deferrable sub-item**: the `.claude/agents/*` generic-body sync from
      the skills-repo role library (tester static-analysis duties, reviewer
      deletion lens) rides along **only if** that PR has merged by the time
      M1 lands. It is bundled here to avoid editing the same agent files
      twice, but it must not block the critical path: if the PR is still
      open, M1 ships the tail rewrites alone and the body sync becomes a
      follow-up.

**Phase 2 — the four independent tracks (any order, after M1)**

- [ ] **M2 — Formatting: drift cleared and gated, one MR**: commit one is a
      pure `gofmt -w ./api` over the drifted files (**list measured at
      implementation time — no count appears here, per Decision 10**); commit
      two adds `task fmt-check` (`gofmt -l` on both Go modules, non-empty output
      fails) and its CI job. `./controller` is already clean and must stay
      that way. Prettier for `web/`+`agent/` is explicitly **out of scope**
      here — it is a much larger reformat and belongs with M3's ratchet if
      wanted at all.

      **Calibration**: introduce a deliberate misformat in a scratch file, run
      `task fmt-check`, confirm it reddens and names the file; restore and
      confirm green.

      **Do not test the gate by its exit code alone.** `gofmt -l` exits 0
      whether or not it lists anything, so a target written
      `gofmt -l . && echo drift` fires unconditionally. Gate on the *output*
      being empty (`test -z "$(gofmt -l .)"`), which `CLAUDE.md` documents as a
      trap already paid for here.

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

      **Precondition**: resolve Open Question 2 (oxlint vs ESLint react-hooks
      parity) before starting. It is a Decision-8 note today, which is easy to
      start past.

      **Calibration**: add an unchecked error return in a scratch file, confirm
      `errcheck` fires through `task lint:api`; add a violating hook call,
      confirm `react-hooks` fires through `task lint:web`. A first run reporting
      nothing is indistinguishable from a linter that is not wired up.

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
      `"Task"` switch case** (`web/src/components/RunEvent.tsx`, the two `case
      "Task":` arms — still present, re-verified 2026-08-02): it is a dead
      *branch inside a
      live function*, and neither `deadcode` (unreachable functions) nor
      `knip` (unused exports/files/deps) can see it. That is a real coverage
      limit of this milestone and worth stating: dead branches stay the
      reviewer's job, which is why the skills-repo change gives the reviewer
      a deletion lens rather than relying on tooling alone.

      (Cited by content anchor rather than line number on purpose: the arms were
      at `:87` and `:492` when this PRD was written and are at `:97` and `:594`
      today. The claim held throughout; only the citation rotted.)

- [ ] **M5 — The long tail: shell, YAML, secrets, vulns**:

      **`shellcheck` over every tracked shell script — scope it as
      `git ls-files '*.sh'`, not `e2e/*.sh` + `scripts/*.sh`.** Those two globs
      miss a third of the tracked scripts, including
      **`agent/templates/entrypoint.sh`, the worker container entrypoint that
      runs in every hosted worker pod** — the one place in this repo where a
      shell bug reaches production. Also missed:
      `agent/scripts/check-image-content.sh`,
      `agent/devbox-global/assert-toolchain.sh`,
      `web/scripts/gen-doc-placeholders.sh`. Using `git ls-files` also stops the
      scope going stale as scripts are added.

      **First step is a measurement, and the milestone branches on it.**
      Run `shellcheck --severity=error` across that set and report the count
      before committing to an approach. Success Criterion 2 assumes a gating
      job can land on day one; Open Question 4 admits the number is unknown.
      If day-one `error`-severity findings are non-zero, severity staging alone
      cannot deliver a gating job, and the fallback is the same diff-wrapper M4
      writes for `deadcode`. Tighten to `warning` once `error` is clean. Per
      #101, `run-e2e.sh` must be referenced by content anchor, never by line
      number.

      **`yamllint`** over `.gitlab-ci.yml` and `deploy/values/`. **`gitleaks`**
      in CI, which lets `.claude/agents/auditor.md:63` stop documenting its own
      absence. **`govulncheck`** for both Go modules and
      `npm audit --audit-level=high` for both npm packages, initially
      `allow_failure` only until the current finding count is known.

      **Markdown link checking: extend `web/scripts/check-docs.mjs`, do not add
      a second checker.** It already validates relative-link existence and
      link-text-path correctness for `docs/`, `ARCHITECTURE.md`, `README.md`,
      `CLAUDE.md` and `specs/*.md` via the `extraLinkFiles` list at `:161`. The
      gap is `prds/` and `adr/` as link *sources*. Adding them to that list is a
      small change; a parallel checker would have different semantics and would
      have to rediscover why the existing one is gated on `fullCheckout` (the
      web image build context is trimmed to `web/` + `docs/`).

      **Calibration**: add an unquoted `$var` in a scoped script and confirm
      shellcheck fires; add a broken relative link in a `prds/` file and confirm
      the extended check-docs reddens.

**Phase 3 — measurement (after M1; independent of M2–M5)**

- [ ] **M6 — Coverage measured, and `-race` for `controller`**: `-coverprofile`
      for both Go modules and `vitest --coverage` for `web` (which needs
      `@vitest/coverage-v8` added to `web/package.json` — it is not currently
      a dependency), with the totals printed in CI job output and GitLab's
      coverage regex wired up so MRs show the number. **No failing threshold
      in this milestone** (Decision 6).

      **`-race` is scoped to `test:controller` only. `test:api` already has
      it**, and this milestone previously claimed otherwise. `.gitlab-ci.yml:178`
      is `go test -race -count=1 ./...`, landed 2026-07-25 in `224b5349` for
      PRD #108 M4; `test:api-store-it` has it at `:224`. The remaining module is
      `controller` (`.gitlab-ci.yml:299`).

      Consequently this is **no longer "the riskiest single change in the PRD"**
      — that framing was sized for the api suite. It still ships on its own,
      before the coverage work, and the cautions still apply to `controller`:
      `-race` costs runtime (cite `.gitlab-ci.yml:157-178`, which already
      records the api measurement, rather than re-deriving the general figure),
      and it detects latent races, so `main` can go red on code nobody touched.
      There is no ratchet — you cannot gate `-race` on new findings. If the
      first run is red, stop: file the races as their own issue and land `-race`
      behind `allow_failure: true` **with a dated expiry note**, rather than
      leaving a permanent advisory job (Decision 3).

      **Calibration**: confirm the coverage number moves when a test is deleted,
      and that the GitLab regex picks it up on the MR — a coverage job that
      reports nothing looks identical to one reporting a stable number.

      Also fixes the `web/` vitest configuration: `jsdom` is currently opted
      into per-file via `// @vitest-environment jsdom` in a majority but not all
      of the `web/src` test files (re-measure; no count per Decision 10), so a
      missing pragma silently runs a DOM test under node.
      Prefer an explicit per-directory project config over flipping the
      global default — a blanket default flips the remaining non-pragma files
      from node to jsdom, which is the same class of silent-wrong-environment
      bug in the opposite direction. (`environmentMatchGlobs` does the same job
      and works on the pinned `vitest ^2.1.9`, but it is deprecated from
      Vitest 3 in favour of the projects/workspace config, so it buys a
      migration later.) Removes the
      vestigial `coverage.out` line from `.gitignore:66` or makes it real.

## Parallelization

| Phase | Milestones | Depends on | Files touched | Can run in parallel |
|---|---|---|---|---|
| 1 | M1 | — | `Taskfile.yml`, `.gitlab-ci.yml`, `CLAUDE.md`, `.claude/agent-team.md`, `.claude/agents/{coder,tester,reviewer,auditor}.md` | No — blocks all |
| 2 | M2 | M1 | `api/**/*.go` (format only), `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 2 | M3 | M1 | `.golangci.yml`, `web/eslint.config.js`, `agent/eslint.config.js`, **`web/package.json`**, `agent/package.json`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes, except vs M4/M6 |
| 2 | M4 | M1 | `Taskfile.yml`, `knip.json` ×2, **`web/package.json`**, `agent/package.json`, `.gitlab-ci.yml` | Yes, except vs M3/M6 |
| 2 | M5 | M1 | `.shellcheckrc`, `.yamllint`, `.gitleaks.toml`, `web/scripts/check-docs.mjs`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 3 | M6 | M1 | `.gitlab-ci.yml`, `web/vite.config.ts`, **`web/package.json`**, `.gitignore` | Yes, except vs M3/M4 |

**Shared-file warning**: every Phase-2/3 milestone appends to `Taskfile.yml`
and `.gitlab-ci.yml`. Run them as parallel agents only with an explicit
instruction to append at the end of the relevant section and to stop-and-report
rather than resolve a conflict in either file — the same rule
`.claude/agents/coder.md` already applies to `api/go.mod` and
`docker-compose.yml`.

Three exceptions where "append at the end" is not enough:

- **The `stages:` list is a single hot line.** M3 and M5 both need a `lint`
  stage, and both would insert `- lint` at the identical position in
  `.gitlab-ci.yml:44-50`. That is why M1 adds the (empty, inert) stage entry
  up front — appending cannot resolve it.
- **`web/package.json` is a three-way contention** that the previous version of
  this table missed: M3 adds ESLint devDeps, M4 adds knip, M6 adds
  `@vitest/coverage-v8`. `agent/package.json` is a two-way (M3, M4). Same class
  as the `stages:` conflict, and npm's own `devDependencies` ordering makes a
  merge conflict likely rather than possible. Sequence these three, or have each
  agent stop-and-report on that file.
- **M2 must not run in parallel with anything touching `api/**/*.go`**, since
  it rewrites the drifted files wholesale. It is safe against `validate:api`'s
  sqlc-drift gate, but **not for the reason previously given here**: two of the
  drifted files *are* under `api/internal/store/`
  (`pipeline_statuses_integration_test.go`, `skills_integration_test.go`).
  The real reason is that neither is *sqlc-generated* — the gate is
  `git diff --exit-code -- internal/store` after re-running `sqlc generate`,
  and `sqlc` only rewrites its own generated files, so a committed reformat of
  hand-written neighbours cannot trip it. Re-verify at implementation time
  rather than trusting either statement.

  **Latent deadlock worth one sentence in the MR**: `gofmt -l` over the whole
  module and the sqlc-drift gate are in tension if a future `sqlc` version ever
  emits non-gofmt-clean code. Not true today; cheap to note now.

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
   function, or `shellcheck` error fails an MR pipeline — **each demonstrated
   by the milestone's calibration step, not asserted.** The shellcheck half is
   conditional on M5's day-one `--severity=error` measurement: if that count is
   non-zero, this criterion is met via the diff-wrapper, not via severity
   staging. **Unused TS exports
   are the exception and reach this bar last**: knip's staging sets
   `exports: "warn"`, and warn-severity issues do not count toward its error
   total, so a new unused export gates nothing until that type is burned down
   to zero and promoted to `error`. (`--max-issues` is a fixed budget, not a
   ratchet — fix one old finding, add one new, still under budget.) If
   gate-on-new is wanted for exports before the burn-down finishes, knip needs
   the same diff-wrapper M4 writes for `deadcode`.
3. **Gate *recipes* are defined once, in `Taskfile.yml`.** `CLAUDE.md`
   §Commands, the `.claude/agent-team.md` paste-block and the
   `.claude/agents/*` tails name `task` targets and never restate a command
   line — per Decision 9. The paste-block itself stays, keeps its `none (gap)`
   slots and its per-flag "why" lines, and continues to be pasted into every
   dispatch. *(This criterion previously read "the gate command appears in
   exactly one place in the repo", which would have deleted the paste
   mechanism.)*
4. `.claude/agent-team.md:1082-1110` and `.claude/agents/tester.md:158-181` —
   both copies of the slot table — carry a real `lint` command instead of
   `lint none (gap)`, and the `noted` markers on the slots this PRD closes are
   removed rather than left behind. *(This criterion previously targeted the
   string "Lint command: none dedicated", which was deleted in `027a4b88` and
   so was already satisfied while testing nothing.)*
5. `.claude/agents/auditor.md` no longer documents the absence of a secret
   scanner, because one runs.
6. Coverage percentages for `api`, `controller` and `web` are visible on
   every MR.
7. `main` is `gofmt`-clean across both Go modules.
8. **Every check this PRD adds shipped with the mutation that proves it is
   live**, recorded in its MR description: the check reddens on a deliberate
   violation and greens on its removal. Applies to M1 (the
   `fixtures/judge-fidelity/cases.json` control), M2, M3, M5 and M6 — M4 already
   specified one. `CLAUDE.md`'s rule applies verbatim: *"a control that produces
   no output is not a control."* A PRD whose entire subject is quality gates must
   not ship a gate whose liveness is unverified.

## Documentation Corrections Folded In

- ~~`.claude/agent-team.md` "Project signals": "Lint command: none dedicated;
  `npm run build` in web/ runs the check-docs + tsc gate"~~ — **already gone**,
  removed in `027a4b88` (2026-07-21). Nothing to correct. What M1 corrects
  instead is the `lint none (gap)` slot in the paste-block at
  `.claude/agent-team.md:1097` and its duplicate at
  `.claude/agents/tester.md:168`.
- `.claude/agents/auditor.md:63`: "CI (`.gitlab-ci.yml`) runs
  validate/test/build across api/web/agent but has NO secret scanner
  (gitleaks/trufflehog)" — corrected in M5. It already reads "PRD #103 M5 adds
  them", so the edit is to make the present tense true, not to add a pointer.
- `CLAUDE.md` §Commands: the hand-typed recipes are replaced by `task`
  invocations in M1 — **command lines only**. Every measured-evidence paragraph
  around them stays verbatim; see M1's scoping note. The prose about e2e being
  deliberately out of CI stays true and is not touched.
- `.gitignore:66`: the `coverage.out` entry currently refers to tooling that
  does not exist — M6 either makes it real or removes it. (`.task/` is already
  present at `:42`; nothing to add, and adding it is not the work.)
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
   splitting first?** Several thousand lines in one file may produce a baseline
   so large it is meaningless. **M5 now opens with this measurement and branches
   on it** — it is no longer only an open question, because Success Criterion 2
   depends on the answer.
5. **Prettier/`gofumpt`** — deliberately excluded from M2. Worth a follow-up
   or not?
6. **Is `Formula/uzi-cli.rb` in scope for M5?** It has no lint of any kind, and
   `devbox.json` carries ruby as a tier-2 package specifically so a run can
   `ruby -c` it. A `ruby -c` target is nearly free. Include in M5 or declare it
   out of scope — either is fine; leaving it undecided is not.
7. **Should the agent-team paste-block be generated from `Taskfile.yml` and
   asserted in CI?** Decision 9 defers this as the right end state. It removes
   the last hand-maintained copy entirely. Own issue, after M1.

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
