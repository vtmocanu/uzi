# PRD #103: Dev-loop quality gates — task runner, linters, dead code, formatting, coverage

**GitLab Issue**: [#103](https://gitlab.example.com/vtmocanu/uzi/-/issues/103)
**Status**: In progress (created 2026-07-21) — **M1 merged 2026-08-02** (MR !154), **M2 merged 2026-08-02** (MR !155), **M3 and M4 implemented 2026-08-02 on `feature/prd-103-m3-m6` but NOT MERGED and NOT YET PIPELINE-TESTED** (see their status blocks); M5-M6 open. M2 closed Success Criterion 7 and took the exclusive lock on `api/**/*.go`, so M3-M6 are now freely parallel (modulo the `web/package.json` three-way in Parallelization).
**Priority**: Medium
**Absorbs**: [#101](https://gitlab.example.com/vtmocanu/uzi/-/issues/101) item 3 (26-file gofmt drift)
**Review**: adversarial review 2026-07-21 (every repo claim re-checked against `main`). It caught a load-bearing factual error — Decision 4 originally specified a "committed baseline" for all four ratcheted tools, and only ESLint has one. Rewritten with per-tool mechanisms, verified against upstream docs. Also corrected: line/size counts, a wrong `-buildvcs=false` citation, M4's calibration symbol (undetectable by the tools M4 adds), Success Criterion 1's scope, and the `stages:` conflict M1 now pre-empts.

**Second review 2026-08-02** (every repo claim re-checked against `e0472a88`; the PRD was last edited 2026-07-21 in `8679e37a` and the tree moved under it within hours). Three blockers, all premise rot rather than design error:

- **Decision 1 argued *for* the silent-green mechanism this repo mandates `-count=1` against.** Its Taskfile justification cited `sources:`/`checksum` fingerprinting as "the real difference" from make. Rewritten, with a hard ban and a measurement (see Decision 1).
- **The "gate is copy-pasted prose" premise was stale.** The `.claude/agent-team.md` line Success Criterion 4 targeted was deleted in `027a4b88`, the commit immediately after this PRD's last edit, and replaced by a deliberate **paste-block** whose whole rationale is duplication. SC3 as written would have deleted that mechanism. Resolved at the user's direction 2026-08-02 (see Decision 9).
- **`-race` was already on `test:api`** (the `test:api` job, `go test -race -count=1 ./...`). M6's "riskiest single change" was already done; rescoped to `controller` only. *(Provenance corrected 2026-08-02 during M1 — see Decision 11. This bullet read "`.gitlab-ci.yml:178`, landed `224b5349` for PRD #108 M4", and all three parts were wrong or imprecise: the line number has since moved twice, `224b5349` is a branch merge rather than the introduction, and the combined line is two PRDs' work, not one.)*

Plus: every numeric count in the document *had* drifted and *was* struck (Decision 10 — narrowed rather than deleted, since the count ban is on an undated tally of a moving population, and the counts since re-added each carry a date, a tool version and an invocation), `check-docs.mjs` had grown to cover three of the four files Problem #4 claimed were uncovered, Decision 7 repeated a clause `devbox.json` itself corrects as false, and only M4 had a calibration step (now all of M2–M6 do).
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
26, 25, 19 and 16 on four different days, and the `format` slot's own comment in
`.claude/agent-team.md`'s Quality gates paste-block forbids recording it there for exactly that reason, naming the
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
this paragraph was wrong about it: `web/scripts/check-docs.mjs` sets
`extraLinkFiles = ["ARCHITECTURE.md", "README.md", "CLAUDE.md"]` and appends
every `specs/*.md`, validating both relative-link existence and link-*text*-path
correctness. It landed for issue #132 (2026-07-25) after 36 dead PRD paths had
accumulated, 11 in `ARCHITECTURE.md` alone, and it runs in `validate:web` and in
`npm run build`. **The real remaining gap is `prds/` and `adr/` as link
*sources*** — they are valid link *targets* today, but nothing checks the links
they themselves contain. No secret scanner — a gap uzi already
documents to itself in `.claude/agents/auditor.md`: "CI (`.gitlab-ci.yml`)
runs validate/test/build across api/controller/web/agent but has NO secret
scanner (gitleaks/trufflehog)." No `govulncheck`, no `npm audit`.

**5. There is no coverage signal.**

No `-coverprofile`, no `vitest --coverage`, no codecov. `.gitignore` carries
a vestigial `coverage.out` entry pointing at tooling that does not exist.

**`-race` is NOT part of this problem for `api`, and the previous version of
this paragraph was wrong that it was.** The `test:api` job runs
`go test -race -count=1 ./...` (since M1, via `task test:api`), under a long
comment stating that dropping either flag silently undoes a fix, with the cost
already measured. `test:api-store-it` has it too, in its `-run 'LiveDB$'` sweep.
**The only module still without `-race` is `controller`**
(the `test:controller` job, `go test -count=1 ./...`). M6 is scoped accordingly.

*(Corrected 2026-08-02 during M1, twice over. The line numbers are gone because
M1 edits this file and every anchor in it moves; cite by job name — see
Decision 11. And the provenance previously given here, "landed 2026-07-25 in
`224b5349` for PRD #108 M4", was wrong in all three parts: `224b5349` is a merge
on the PRD #98 wave-3 branch, `main` got the line on **2026-07-26** via
`77cb96e4`, and the combined command is two PRDs' work — `-count=1` is PRD #98's,
`-race` is PRD #108 M4's, first introduced alone as `go test -race ./...` in
`8f1b0c9b`.)*

**6. There is no task runner, so the gate recipe is duplicated by hand.**

No Makefile, no Taskfile, no justfile. The same multi-line gate recipe is
written out by hand in at least four places — `CLAUDE.md` §Commands,
the `.claude/agent-team.md` Quality gates paste-block, the
`.claude/agents/coder.md` and `.claude/agents/tester.md` `## For this repo`
tails (the tester's being a second full copy of the slot table) — and drifts
independently in each.

**Two corrections to how this problem was originally stated, both material:**

- The quoted consequence — *"Lint command: none dedicated; `npm run build` in
  web/ runs the check-docs + tsc gate"* — **no longer exists anywhere in the
  repo.** It was removed in `027a4b88` (2026-07-21), the commit immediately
  after this PRD's last edit. Its replacement, `# no golangci-lint, no eslint;
  go vet in CI only`, rotted the same way in turn: M1 itself rewrote that
  comment while landing (the criterion governing this slot, SC3 — a target
  name replaced a command line, per Decision 9). Cite the slot, not its
  wording, for exactly the reason this bullet exists — the paste-block's
  `lint` slot in `.claude/agent-team.md`. The underlying complaint still
  holds; the evidence cited for it does not, twice over now.
- **The duplication is deliberate, and it must survive this PRD.**
  the paste-block's own intro states why: *"Paste this block into every
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

**M1 added three more bans to the same list, and they are security-shaped rather
than correctness-shaped** (they live in `Taskfile.yml`'s header, in one list, so a
later milestone sees all four at once):

- **No `dotenv:`.** Task loads the named env files into *every* target's
  environment, so a `dotenv: ['.env']` at the root silently injects a developer's
  real secrets into every gate target. `.env` is gitignored and `CLAUDE.md` spends
  a section on compose ranking shell environment above `--env-file`.
- **No `includes:` at all, and the remote form is the sharp edge.** Task 3.x can
  include a taskfile over https: unpinned remote code executing inside the gate.
  `.gitignore` already carries a "Task remote-include cache" comment, so the
  mechanism is one line from being invited back. The self-contained rule above is
  the style reason; this is the security one.
- **No dynamic root `vars: {X: {sh: ...}}`.** Those execute on every invocation,
  including unrelated targets and a bare `task --list`.

**And never splice a variable unquoted into a `cmds:` line.** Task templates are Go
`text/template` and the result reaches `sh -c` unquoted, so `{{.CI_COMMIT_BRANCH}}`
or `{{.CLI_ARGS}}` inside a command is injectable by anyone who can push a branch.
M1 needs neither; **M6's coverage measurement is where the temptation appears.**

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
  reading `api/internal/hostedsvc/testdata/`. The `test:api` and
  `test:controller` jobs each carry a long comment block on exactly this. Run 3 above is that
  shape reproduced in miniature.
- **`.task/` is gitignored** (its own `.gitignore` entry, already present —
  nothing to add).
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

**RESOLVED IN M1: `prefixed`, not `group` — and the heading above is now wrong
about which is "required".** Once M1 made `gate` serial (see the CPU-contention
paragraph below), the interleaving `group` exists to fix cannot occur, so `group`
bought nothing and cost streaming. Measured on `task` 3.51.1, 2026-08-02, log read
at t=1.5s into a target that prints `EARLY`, sleeps 3s, prints `LATE`:

```
group        task: [a] sh -c '...'                <- EARLY ABSENT, buffered
prefixed     task: [a] sh -c '...'   [a] EARLY    <- streams
interleaved  task: [a] sh -c '...'   EARLY        <- streams
```

Two costs, and the second is the serious one: locally `task gate:api` would print
nothing for the whole 41s `go test -race` run and then dump; and in CI every gate
job sets `interruptible: true`, so a cancelled or timed-out job under `group`
loses **all** buffered output. A gate that discards its evidence on timeout is the
failure class this PRD exists to remove. The parenthetical "(or `prefixed`)" above
was always the escape hatch; M1 took it.

**CPU contention is a measured flake source in this repo already.**
`web/vite.config.ts` raised `testTimeout` to 20000 because "under full-suite
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

**The stage is organisational, not ordering.** All nine existing toolchain gate
jobs — `validate:api`, `test:api`, `test:api-store-it`, `validate:controller`,
`test:controller`, `validate:web`, `test:web`, `validate:agent`, `test:agent` —
set `needs: []`, so they start immediately regardless of stage. **Eight of the
nine are the set M1 rewires into `task` targets; `test:api-store-it` is the
exception** — it inherits `.task_setup` but invokes no `task` target, since its
pipefail + `grep -c '^--- PASS'` assertion is CI-specific and stays inline (see
the two-jobs-keep-their-current-shape note in M1's Files section). A new `lint`
stage
therefore does **not** make lint gate test — everything still runs concurrently
and the pipeline fails if any job fails. Stated because the placement "between
`validate` and `test`" reads as sequencing and is not.

**PARTIAL DEVIATION, RECORDED DURING M3 (2026-08-02): the `lint` stage is used for
the Go half only. The npm half folds into the existing `validate:web` /
`validate:agent` jobs.** This decision prescribes seven new lint-stage jobs; M3
shipped two (`lint:api`, `lint:controller`) and put `task lint:web` /
`task lint:agent` inside jobs that already exist. The split is not a compromise
between two readings — each half has its own reason:

- **The Go jobs need `GIT_DEPTH: "0"`** for the merge-base ratchet. Putting that on
  the shared `.go_job` template would give **five** jobs a full-history clone for a
  check only two need; folding lint into `validate:api` instead would hand the full
  clone to the one job whose `sqlc generate` + `git diff --exit-code` already makes
  its git state load-bearing. A dedicated job isolates the clone-strategy change to
  the job that needs it.
- **The npm checks cost ~0.06s and ~0.05s** against a ~30s `npm ci` in each job, so
  a separate job would pay a 500× setup overhead for the check. It also matches M1's
  own Files section verbatim — *"M3 adds `task lint:web` to the first"*.

**🔴 AND THE ANCHOR LISTS ARE A SECOND SINGLE-HOT-LINE CONTENTION THAT THE
PARALLELIZATION SECTION DOES NOT LIST.** `.gate_needs` and `.publish_needs` in
`.gitlab-ci.yml` enumerate gate jobs **by name**; `*gate_needs` is consumed by the
kaniko build template and `e2e:kind-smoke`, `*publish_needs` by the publish-image
template and `publish:chart`. **A new gate job absent from both means a `v*` tag
pushes every image and the OCI chart to Harbor while that job is failing** — the
pipeline reddens afterwards and the artifacts are already out. M3 took the two
lists from 9 to 11 and from 11 to 13 entries. The Parallelization section names only
`stages:` and the two `package.json` files; these two lists belong beside them, and
they are worse than `stages:` in one respect: **appending resolves the merge but not
the omission**, so a milestone that appends its job and forgets the lists produces a
green MR pipeline and a silent tag-time hole.

**M5 inherits this obligation in full, and it is part of its definition of done.**
Its checks (`shellcheck`, `yamllint`, `gitleaks`, `govulncheck`) are repo-wide, so
unlike the npm half they genuinely cannot fold into a per-toolchain `validate:*`
job — M5 **will** open lint-stage jobs, and every one of them goes into **both**
lists in the commit that creates it.

**AND M5 NOW INHERITS A COMPLETENESS PROPERTY, NOT MERELY A MEMBERSHIP RULE.** M3
shipped the "both lists, always" rule while an existing job was violating it:
`test:api-store-it` (`stage: test`, no `rules:` of its own, so it runs on tag
pipelines) had been absent from **both** lists since `add0390d` introduced it, with
no recorded reason anywhere. A `v*` tag therefore pushed every image, the OCI chart
and the brew formula while the **sole CI coverage of the LiveDB suite** was failing
— the one job `CLAUDE.md` identifies as the only thing that catches a
`+goose`-poisoned migration, whose blast radius is every later migration staying
unapplied at API boot. M3 added it to both, taking them to 12 and 14 entries, and
corrected `.gate_needs`'s own header, whose *"The full validate+test+helm gate set"*
admits exactly one reading and was false while that job was missing.

**The check that finds this is stage-based, and the obvious one does not.** M3's
first pass verified completeness with a filter keyed on job-name shape
(`validate:*` / `test:*` / `lint:*` per component), which `test:api-store-it`
satisfies in name and slips through in practice — it was reported as "no gate-shaped
job is missing" while this hole was open. **Enumerate every job whose resolved
`stage` is `validate`, `lint` or `test` and assert it appears in both lists**,
resolving `stage` through `extends`. On the post-M3 tree that is 12 jobs, none
missing from either, with the two lists differing only by the two `publish:assert-*`
entries.

**The `.gate_needs` half of that fix carries an unmeasured latency cost, taken
deliberately** — its consumers are `.kaniko_build` and `e2e:kind-smoke`, so a
`postgres:17` service container now sits on every MR's validation-build path for no
correctness gain there. It was added anyway because the cost is unmeasured, and
granting an exception on an unmeasured cost is the inversion this PRD exists to
correct. **The criterion for revisiting needs no baseline**: `.kaniko_build` starts
when the slowest of the twelve finishes, so compare `test:api-store-it`'s duration
against the max of the other eleven — if it is not the max it added exactly zero, and
if it is, the cost is store-it minus the runner-up. Read it on a warm pipeline, since
pipeline 1's cold `lint` cache (~52s of golangci-lint building from source) would
otherwise confound it.

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

> **🔴 AMENDED 2026-08-02 (M4). THE PREMISE IN THE SENTENCE ABOVE IS
> EMPIRICALLY FALSE FOR `deadcode` HERE, AND THE `--max-issues` MECHANISM IN
> THE TABLE BELOW DOES NOT EXIST AS DESCRIBED.** Both were measured
> independently by two agents during M4's design wave, at two tool versions,
> agreeing (`probes/prd-103-m4-architect.txt`, `probes/prd-103-m4-reviewer.txt`).
>
> **(a) `deadcode` produces ONE finding, not a large list.** `deadcode -test
> ./...` reports **1** on `api` and **0** on `controller` — the api finding
> being `HookManager.SettingsPath`, a one-line exported wrapper with zero
> callers. So the ratchet this decision budgets for was guarding a population
> of one. M4 **deleted** the function and shipped **empty** baselines gating at
> zero, which is strictly stronger than gate-on-new and disposes of the
> baseline-rot and position-anchor problems (Decision 11) rather than managing
> them. *The wrapper still ships*, for a reason this decision does not name:
> `deadcode` exits 0 whether it finds 0, 1 or 44 dead functions, and rc=1 only
> on a load error where stdout is **0 bytes** — so a naive additions-only diff
> reads a module that does not compile as clean and passes green.
>
> **(b) `--max-issues` IS INERT FOR A WARN TIER, so knip's row below is wrong
> where it says "plus `--max-issues` as an issue budget".** It counts
> **error-severity issues only**. Measured: every rule at `warn` with
> `--max-issues 0` exits **0** with 24 findings printed. The two mechanisms do
> not compose — severity staging and the budget apply to disjoint populations,
> and a warn-tier issue type therefore has no budget and no gate at all. The
> same claim appears in the M4 milestone bullet and in Success Criterion 2;
> both are corrected in place. The real options for gating unused exports
> before the burn-down finishes are a diff-wrapper, promoting `exports` to
> `error` with an `ignore` list, or accepting a reported-only tier. **M4 shipped
> the third**, and filed the burn-down as **issue #206** with its counts
> (web 22, agent 53, 2026-08-02) so that "a warning nobody must act on" does not
> become the end state by default.
>
> Nothing here changes the decision's shape: the mechanism is still different
> for every tool, and that is still the point.

An earlier draft of this decision said each tool lands with "a committed
baseline capturing today's findings". **That was wrong for four of the five
tools** (verified against current upstream docs, 2026-07-21), and the
correction matters enough to record, because the wrong version is the
intuitive one and it will be re-proposed otherwise:

| Tool | Baseline file? | Actual ratchet mechanism |
|---|---|---|
| `golangci-lint` | **No** | Diff-based only: `--new-from-merge-base=main` (upstream's own large-project advice), plus `--new-from-rev` / `--new-from-patch`. No file records existing findings. |
| `knip` | **No** | Severity staging: `rules: { exports: "warn", files: "error" }` per issue type, promoted to `error` as each reaches zero. ~~Plus `--max-issues` as an issue budget~~ — **struck 2026-08-02, see the amendment above: `--max-issues` counts error-severity issues only and is inert for a warn tier** — and workspace scoping. |
| `deadcode` | **No** | Nothing whatsoever. Plain report output. Gating on new findings requires a wrapper script: run the tool, diff against a committed findings file, fail on additions. **And the wrapper's real load-bearing job is the exit code, not the diff: `deadcode` exits 0 on any number of findings and 1 only on a load error, where stdout is empty** (amendment above). |
| `oxlint` | **No** | `--max-warnings <n>` as a count budget (verified on a **37-warning corpus** — not the correctness-tier debt below; see the TypeScript paragraph below for what that corpus was and was not. **Errors fail regardless of the budget**). No baseline file exists, and none is needed — the measured debt is small enough to fix outright. |
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
  budget, not a flag. *(M4 shipped it as `scripts/deadcode-gate.sh`, and a
  script rather than an inline Taskfile recipe because M5 adds shellcheck over
  `git ls-files '*.sh'`: a script gets linted, an inline `cmds:` string gets
  nothing. Its baseline key is **position-free** — deadcode's `-f` template,
  keyed on (import path, function name) — because the default output is
  `path:line:col:` and one inserted comment line above a baselined symbol turns
  it into a spurious addition **and** removal, which Decision 11 bans. Both
  sides are sorted under `LC_ALL=C`: the tool's output is stable across runs but
  ordered by (file, **line number**) within a package, so `sort` is not
  idempotent on it.)*

**The TypeScript half is REWRITTEN, because the mechanism it named does not exist
in the tool this PRD now uses.** *(Amended 2026-08-02 with Decision 8.)* ESLint's
native bulk suppressions (`--suppress-all` writing a committed
`eslint-suppressions.json`) have no oxlint equivalent, so a decision that shipped
unamended would have sent M3 looking for a flag that is not there.

**No baseline is needed, because the debt is not baseline-sized.** Measured
2026-08-02 at oxlint 1.76.0, **with `--react-plugin`** (`oxlint --react-plugin src
test` in `agent/`, `oxlint --react-plugin src` in `web/` — web's tests live under
`src/`, hence the shorter invocation): **10 findings on `agent/`, 6 on `web/`, 16
combined.** `--react-plugin` is the configuration M3 actually ships (Decision 8's
whole point is the hooks rules it gates), and it costs `agent/` nothing — `agent/`
has no React, so its count is 10 either way. At the shipped **default** tier
(no `--react-plugin`), `web/` reports 4 rather than 6 (14 combined); the gap is
the plugin flag, not version drift — web's default-tier `src` count held at 4
across oxlint 1.70.0, 1.73.0, 1.75.0 and 1.76.0. That is
an afternoon, not a burn-down, so **M3 fixes them** rather than recording them.

**oxlint does have a ratchet, and its limit is worth stating before someone leans on
it.** `--max-warnings <n>` is a count budget: 37 and 40 exiting 0, 36 and 0 exiting
1, with errors failing regardless of the budget. Three things about that corpus,
in increasing order of certainty lost:

1. **Certain.** The flag's own boundary semantics (exits 0 iff warnings ≤ n) fixes
   the corpus this ran against at exactly **37 warnings** — the 36→37 crossing from
   fail to pass is what a 37-warning corpus and no other size produces. This is
   not the 10/6/16 correctness-tier debt measured above.
2. **Certain, measured 2026-08-02 at oxlint 1.76.0.** No plain severity tier on
   `agent/ src test` produces 37: `correctness` 10, `suspicious` 136, `pedantic`
   1292, `style` 10435, `restriction` 2825. So the corpus is not any shipped tier
   either.
3. **Unconfirmed hypothesis, labelled as such.** 37 matches the `agent/` component
   of the type-aware split below ("37 + 178 = 215"), which would mean this ratchet
   demo ran against the tier M3's own SCOPE RULING excludes. Type-aware needs
   `oxlint-tsgolint`, not installed at either measurement above, so this is not
   settled — re-run with it to settle it.

None of the three affects the mechanism this decision is actually about: **a count cannot
distinguish a fixed finding from a new one**, so it permits churn under the cap —
fix one old finding, add one new, still green. It is a regression brake, not a
ratchet, and it is the honest reason the fix-them-now route is preferred here.

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

**5. The gofmt reformat and the gofmt gate land in the same MR.**

*(Heading corrected 2026-08-02 during M2: it read "The 26-file gofmt reformat".
That is an undated count of a population that moves with every commit — the
thing Decision 10 bans, naming this exact metric as its example. It measured 16
at `755861e8`. No number is needed: the argument below holds for any non-empty
drift list, as the decision itself says. Line 6's "26-file gofmt drift" is
deliberately left alone — it quotes issue #101's own wording and correcting it
would falsify the citation.)*

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

**8. oxlint for `web/` and `agent/`.** *(Amended 2026-08-02. This decision
previously read "ESLint … pending one verification" and deferred to oxlint only
"if parity holds". Open Question 2 measured it; parity holds; amended rather than
worked around, exactly as the previous text instructed.)*

The hooks rules are still the highest-value TypeScript lint available here, and
`tsc` cannot express them. What changed is which tool provides them. Measured by
the researcher, with the hooks-parity half re-verified independently by the lead.
*(No first-party version record survives for this specific run; consistent with
oxlint 1.76.0, the only version this PRD names anywhere else, and with two
leftover scratchpad installs from what looks like the same research pass — but
that is inference about someone else's run, not a measurement, and is recorded
as such.)*

- **Identical findings to ESLint on `web/src`** — 2 each, same files, same missing
  dependency.
- **36 of 37 identical rule sets** on a purpose-built fixture corpus, and **no file
  flagged by one linter and not the other.**
- **oxlint honours `// eslint-disable-next-line react-hooks/exhaustive-deps`
  verbatim**, so this repo's two existing suppressions migrate with **zero source
  edits**. Re-run as a matrix: the control fires 1, the suppressed fixtures 0.
- **~20-50x faster**, and `typescript-plugin` covers `recommended` 20/20 and
  `recommended-type-checked` 43/43 by name, with type-aware support available via
  `oxlint-tsgolint`.

**Two configuration traps, either of which produces a SILENT NO-OP.** Both must be
in the M3 config, and neither is discoverable from a clean run:

- **The `react` plugin is OFF BY DEFAULT.** Not enabling it means the hooks rules
  this decision exists for never load.
- **`rules-of-hooks` is `pedantic`, not `correctness`.** oxlint's default
  `correctness` category therefore does **not** enable it. A config that turns the
  plugin on and stops there still does not run the rule.

**Three limits, recorded rather than glossed, because each is a claim this decision
does NOT make:**

- **typescript-eslint BEHAVIOUR parity is unmeasured.** What was measured is *name*
  parity across the rule sets, which is a different and weaker claim.
- **Every non-hooks rule was compared oxlint-against-oxlint across tiers**, never
  against an ESLint equivalent. The cross-linter comparison covers the hooks family
  and the `web/src` finding set, nothing wider.
- **The React Compiler family is one experimental nursery rule in oxlint against 15
  individually-tunable rules in ESLint.** This is **deliberately out of M3**: the
  repo is on React 18.3.1, so the compiler is not in use. Weighed and excluded, not
  missed. Revisit if the repo moves to React 19 and adopts the compiler.

*Rejected — ESLint*: slower, and its one remaining advantage over oxlint for this
repo (the granular React Compiler rules) applies to a compiler this repo does not
run.

**9. The Taskfile is the single source for gate *recipes*; the agent-team
paste-block stays and names *targets*.**

*Decided by the user, 2026-08-02.* This resolves a direct contradiction between
the original Success Criterion 3 ("the gate command appears in exactly one place
in the repo") and the `.claude/agent-team.md` paste-block's intro, which mandates pasting the
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

**10. No count of a population that moves with every commit appears in this
PRD, undated.**

*(Heading narrowed 2026-08-02. The original absolute form — "no count of
anything" — is false in this document: Decision 1's `task`-cache transcript
and Decision 2's output-mode comparison are both counts, both legitimate, and
both were added after this decision landed. The distinction below is what the
body already argued; the heading now says it too.)*

Not a style preference. The `format` slot in `.claude/agent-team.md`'s paste-block already forbids
recording a gofmt count in the paste-block, naming the failure it caused: *"Do
NOT record a count here: it read 26, then 25, and a stale tally invites the
truncated-view error it already caused (a filtered 4-file view reported as the
whole list, 2026-07-25)."* This PRD recorded 26 in five places and, by
2026-08-02, **every numeric claim in it had drifted** — the gofmt list, the CI
file's size, `run-e2e.sh`'s length, the bash total, the jsdom pragma ratio, the
docs and PRD counts, and six line-number citations.

**The ban is on an undated tally of a population that moves with every
commit — a gofmt list, a bash-line total, a docs count.** Nothing dates it,
nothing names the tool that produced it, and the population it counts changes
under the next unrelated commit, so the number is stale before anyone reads
it twice. **A dated, fixed-experiment measurement that names its tool version
is not what this bans**, because it is not describing a moving population —
it is the recorded result of one specific run that stays true regardless of
what the codebase does afterward. Decision 1's `task` 3.51.1 cache transcript
and Decision 2's `output: group` vs `prefixed` timing comparison are the
worked examples: both are dated, both name the tool version, both report the
exit codes of a fixed experiment, and both stay correct forever because
nobody can retroactively change what that run printed.

So: state the shape, cite the command that measures it, never an undated
number. Where a milestone needs a figure it says "re-measure at
implementation time" and the MR description carries the value. Arguments must
survive the count changing — Decision 5's is a good example, since it holds
for any non-empty drift list. Where a decision needs a fixed-experiment
number instead, date it and name the tool version, the way Decisions 1, 2 and
4 do.

**11. No line anchor into any file this PRD's milestones edit appears in this
document, and a commit SHA is not a provenance citation on its own.**

*Added 2026-08-02 during M1; widened the same day after review.* Decision 10
banned counts for a reason that turns out to apply verbatim to line anchors here:
**M1 edits `.gitlab-ci.yml`, `Taskfile.yml`, `CLAUDE.md`, `.claude/agent-team.md`
and the role files, so every line number this PRD cited into them moved** — and
M2 through M6 each edit them again. The set is therefore `.gitlab-ci.yml`,
`Taskfile.yml`, `.claude/agent-team.md`, `.claude/agents/*.md`, `CLAUDE.md`,
`.gitignore`, `web/vite.config.ts`, `web/scripts/check-docs.mjs`, and both
`package.json` files. Cite the **job name** (`test:api`, `validate:web`), the
**section or slot name**, an **entry** (`coverage.out`, `.task/`), or an
**exact quoted string**.

*`.gitignore` was added to this set on the review pass that followed: M6 edits
it, and this document cited `.gitignore:66` three times and `:42` twice — M6's
own sentence citing `:66` in the same breath as the edit that invalidates it,
which is the identical shape the paragraph below describes. Its entries are
unique literals, so an entry cite greps where the anchor rots.*

**The first version of this decision was FALSE IN THE DOCUMENT THAT STATED IT.**
It said "no `.gitlab-ci.yml` line number appears in this PRD" while six remained,
one of them **M1's own load-bearing-flag table** citing a range M1 had just moved.
That is worse than ordinary staleness: those anchors were **accurate at
`a87fd521` and this series invalidated them** — right before, wrong after, in the
commit adding the rule against them.

**Deleted rather than re-derived, deliberately.** Eighteen freshly-correct numbers
would rot at the next milestone, and **a correct-looking anchor is the one nobody
re-checks**. Two constraints on the replacements, because a bad fix relocates the
problem instead of removing it: a **paraphrased** cite-by-string fails silently
where a quoted one greps, so quote exactly or name something that exists; and a
criterion must stay **falsifiable** — Success Criterion 4 names "the
`## Quality gates` paste-block and its duplicate slot table in `tester.md`", which
is checkable, where anything softer would satisfy the ban by becoming untestable.

**Two carve-outs, and they are the same carve-out twice: a line number inside a
correction block, quoting retired text, is a past-tense claim about a past state.**
Rewriting it destroys the correction. That covers the `.gitlab-ci.yml:178` in the
`-race` bullet at the top of this document and the `224b5349`-era anchors in the
provenance notes below. **Two anchors are also deliberately left standing because
they fall outside the rule** rather than inside a carve-out: `devbox.json` (which
Decision 7 forbids this PRD from touching) and `agent/test/judge-runner.test.ts`
(which no milestone edits). If a later milestone starts editing either, they come
in scope.

The `-race` citation is the worked example, and it was wrong three times in three
different ways before anyone ran the query. This PRD said, at three sites,
*"landed 2026-07-25 in `224b5349` for PRD #108 M4"*:

- **`224b5349` is a merge on the PRD #98 wave-3 branch**, not the introduction.
- **The date is wrong for `main`**, which got the line on **2026-07-26** via
  `77cb96e4`, a commit this PRD never named. "Landed" reads as "landed on main".
- **The command is two PRDs' work.** `-count=1` is PRD #98's; `-race` is
  PRD #108 M4's, introduced alone as `go test -race ./...` in `8f1b0c9b`.
  `.gitlab-ci.yml`'s own comment had this right all along.

**The reproducing command, not the identifier, is the citation:**

```
git log --oneline -s --diff-merges=first-parent -S 'race -count=1 ./...' -- .gitlab-ci.yml
```

Three teeth, each of which cost someone a wrong answer: **plain `git log -S`
returns NOTHING here**, because the line was produced by a conflict resolution
inside a merge and git omits merge diffs by default — a fail-open instrument whose
silence reads as refutation; **`--oneline` and `-s` are both required**, since
`--diff-merges` turns patch output on and, without `--oneline`, every hit still
carries a full commit header — the shape is that `--oneline -s` gives one line per
commit while dropping either flag costs an order of magnitude, and no number is
recorded here because it grows with history (Decision 10); and
**keep the path filter**, since unfiltered the query also matches PRD documents
that merely quote the string. Measured at `1778f359`, the three forms return **0,
2 and 1** hits for the same string — and that disagreement *is* the finding.

One more, measured rather than predicted: **`-S` counts occurrences, so M1's move
of the command out of `.gitlab-ci.yml` is not a hit at all.** The explanatory
comment left behind still quotes the string, so the count went 1 → 1. The chain
head remains `77cb96e4`. Predicting "the newest hit will be the move" is the
natural inference and it is wrong; read the chain, not the head. The corrected
three-form control is now recorded in `.claude/agent-team.md`'s citation section,
which previously prescribed only the fail-open form.

**Phase 1 — enabling work (blocks everything else)**

- [x] **M1 — `Taskfile.yml` is the single source of truth for the gate**:

      > **STATUS 2026-08-02: MERGED.** MR
      > [!154](https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/154),
      > merge commit `d4c6ac8a` on `main`. The box is now ticked for the reason it
      > was previously left unticked: this landed.
      >
      > **The one risk nothing local could test is resolved.** M1's stated main
      > hazard was that "a `task`-install or PATH failure reds CI having changed no
      > check at all" — the checks are unchanged, but every job's invocation is not.
      > Pipeline #20241 at `d95bc9b8`: **15 of 15 jobs succeeded, none skipped**,
      > including all nine `.task_setup` inheritors across four toolchain images.
      > And the runner did not merely install — `test:api`'s job log carries
      > `task: [test:api] go test -race -count=1 ./...`, so the sha256-pinned fetch
      > produced a working `task` that echoed the command with both load-bearing
      > flags intact. Read the echo, not the exit code: that line is the whole
      > milestone's thesis observed in the one place local runs cannot reach.
      >
      > **No blocking findings on the code, at any point, across two review waves.**
      > The Taskfile, the CI rewire and the supply chain all passed. Every blocking
      > finding either wave produced was documentation, most of them in this file.
      > *(The earlier wave's three are recorded in `7a716900`; "four validators"
      > appeared here as an uncheckable head-count and is replaced by that commit,
      > which is checkable.)* The second wave added: Decision 3's corrected count
      > carrying the wrong population's gloss, Decision 10 citing as its exemplar
      > the one decision that failed its rule, and a `--test-timeout` justification
      > that turned out to be refuted in **seven** files.
      >
      > **The gate's liveness is proven, not asserted.** The tester's four-arm
      > control came back conclusive, and **arm B is the one that matters: it
      > reproduced the silent green on a broken fixture** (`ok … (cached)`,
      > EXIT=0, zero FAILs), which is what makes arm C evidence about `-count=1`
      > rather than about a cold cache. Equivalence verified 1:1 across all eight
      > jobs, old recipes against new targets. The fingerprint ban was verified by
      > parsing and behaviourally.
      >
      > **The gate found a real defect on day one**, which is this milestone's best
      > evidence that it works: a pre-existing 1-in-6 race between two agent test
      > files sharing a hard-coded `/tmp` worktree path, invisible until something
      > ran a mandated whole-repo gate. Fixed here and controlled at 24 runs, zero
      > failures. A second, timing-dependent flake
      > (`agent/test/batcher-poison.test.ts`) is filed separately and remains open
      > as issue #198 (and #162, an older issue naming the same file).
      >
      > **Acceptance controls: all four component gates ship with the mutation
      > that proves them live**, which is more than M1's own criterion asked for
      > (it specified the `gate:api` fixture control alone). `gate:api` twice, by
      > two agents at two SHAs, deliberately using different fixture cases so the
      > second run cross-checked the first rather than repeating it — byte-identical
      > failure messages. `gate:controller` on **both** cross-module read paths,
      > since `internal/protocol` and `internal/preset` read different goldens from
      > different packages and reddening one says nothing about the other.
      > `gate:web` and `gate:agent` by a broken assertion each, which proves the
      > target reaches the real suite and nothing about those suites' own quality.
      >
      > **Not proven, recorded so green controls do not imply coverage they lack:**
      > `check-docs:web` and the two `typecheck:*` targets ran green in every arm
      > but were never mutated, so only `test:*` has a control; and the `gate:web` /
      > `gate:agent` arms broke *test* assertions, which says nothing about whether
      > those suites are bound to shipped code.
      >
      > **What remains for this PRD: M2 through M6.** They were always separate
      > MRs and are unaffected by this one beyond now having a Taskfile to hang
      > targets off.

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
      - `.claude/agent-team.md` — the `## Quality gates` paste-block, per
        Decision 9. (The `:143` "Lint command: none dedicated"
        line this PRD used to cite **no longer exists**; it went in `027a4b88`.)
        Its closing line already says *"Every gap above is what PRD #103 exists
        to close; re-derive this block when its milestones land"*.
      - `.claude/agents/coder.md` — the inline slot summary in its
        `## For this repo` tail, and the recipe block below it (two distinct
        regions, edited for different reasons)
      - `.claude/agents/tester.md` — **a second full copy of the slot
        table**, which the previous file map missed entirely
      - `.claude/agents/auditor.md` and `.claude/agents/reviewer.md` — **listed
        here as already satisfied, not as pending edits.** Both already carry
        what this milestone would otherwise add — the auditor's secret-scanner
        note already reads "PRD #103 M5 adds them", and the reviewer already has
        the dead-code reference the skills-repo deletion lens expects — because
        that skills-repo sync (`d69fe53e`) merged into `main` before this branch
        existed (it is an ancestor of `a87fd521`, M1's base). The "Deferrable
        sub-item" below was therefore moot rather than deferred: there was
        nothing left to ride along. Left in this list so a reader does not go
        looking for a missing edit in either file.

      **Scoping, because this is where a coder will overreach.** `CLAUDE.md`
      §Commands is mostly not recipes: the bulk of it is measured evidence that
      exists because someone trusted a green that ran nothing.
      **Replace only the command lines; every measurement paragraph stays
      verbatim.**

      **THE DISCRIMINATOR IS RECIPE-VERSUS-MEASUREMENT, AND IT APPLIES TO EVERY
      FILE IN THIS REPO, NOT JUST `CLAUDE.md`.** A command written as an
      *instruction* ("run this before reporting done") is a recipe and becomes a
      target. A command written as an *observation* ("`go test ./...` printed
      `ok (cached)` on a gutted fixture") is evidence, and rewriting it makes the
      paragraph describe a run nobody performed. The same string appears in both
      roles, so the test is the sentence around it, never the command itself.

      Sites where this was live during M1, all of them measurements that stay
      byte-identical: the "all green" list in `CLAUDE.md`'s goose parse-failure
      paragraph and the `npm run typecheck | tail -3` example in its gate-status
      paragraph; two rows in `fixtures/judge-fidelity/README.md`; the
      `ok (cached)` reproduction in `api/internal/workersvc/`'s fidelity test; and
      **the `ok (cached)` measurement inside `.claude/agent-team.md`'s own
      `-count=1` paragraph** — which a blanket "replace every `cd <dir> &&`" would
      have destroyed. That last one is the sharp case, because it sits three lines
      below a slot table that genuinely did need rewriting, and because it is the
      evidence M1's own acceptance control re-runs.

      **Sweep the whole file rather than the line someone names.** After the
      rewrite, `cd api &&` / `cd web &&` / `cd agent &&` / `cd controller &&`
      appeared exactly **once** in `.claude/agent-team.md`, and that once was the
      measurement. A per-file sweep is what turns "the line I was warned about is
      safe" into "the rule held everywhere it applies".

      **Each Taskfile target carrying a load-bearing flag gets an inline comment
      naming why it is there**, and the flags are not optional:

      | Target | Flag | Why |
      |---|---|---|
      | `test:api` | `-count=1` | cross-module `fixtures/` reads are cache-invisible |
      | `test:api` | `-race` | PRD #108 M4; see the comment block above the `test:api` job |
      | `test:controller` | `-count=1` | cross-module goldens under `api/internal/hostedsvc/testdata/` |
      | store-it | `-p 1` | package binaries race one shared database |
      | `test:agent` | `--test-timeout=30000` | node's default is *no* timeout; `agent/test/judge-runner.test.ts:167` is written against the cap |

      **`test:api` must carry `-race` AND `-count=1` or M1 silently weakens the
      api gate** while its own text claims it adds no new checks. This is the
      milestone's second real risk after the `task`-install one, and
      the `test:api` comment block already names the live threat by name: *"a future
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
      that executed nothing. Set an output mode at the root (**M1 chose
      `prefixed`, not `group` — see Decision 2**) and record the
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
      devDependency by the existing `npm ci` (knip, oxlint), or a
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
      the `test:api-store-it` job wraps `go test` in a
      pipefail + `grep -c '^--- PASS'` / `'^--- SKIP'` assertion that exists
      to catch the suite silently skipping against a missing Postgres; that
      logic is CI-specific and stays in `.gitlab-ci.yml`. Local and CI
      therefore do not fully converge, and Success Criterion 1 is scoped
      accordingly.

      M1 also adds the empty `- lint` entry to `stages:`
      (the `stages:` list) even though it adds no lint job. That single
      line is genuinely inert, and it is the one edit M3 and M5 would
      otherwise both make at the identical position (see Parallelization).

      Note `-buildvcs=false` is a local-only flag per
      the `.claude/agents/coder.md` and `.claude/agents/tester.md` tails
      (**not** `CLAUDE.md`, which does not mention it) — it must not be baked
      into the Taskfile's committed targets.

      **Deferrable sub-item — turned out moot, not deferred.** The
      `.claude/agents/*` generic-body sync from the skills-repo role library
      (tester static-analysis duties, reviewer deletion lens) was written here
      to ride along **only if** that PR had merged by the time M1 landed. It
      had: `d69fe53e` is an ancestor of `a87fd521`, the commit M1 branched
      from, so both files already carried the synced content before this
      branch's first commit. There was nothing left for M1 to bundle — see the
      Files list above.

**Phase 2 — the four independent tracks (any order, after M1)**

- [x] **M2 — Formatting: drift cleared and gated, one MR**:

      > **STATUS 2026-08-02: MERGED.** MR
      > [!155](https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/155),
      > merge commit `3824e89d`. **`gofmt -l ./api ./controller` on `main` is now
      > empty — Success Criterion 7 is met**, and it is the first criterion in this
      > PRD to close.
      >
      > **The gate is proven live in CI, not only locally.** Pipeline #20248 at
      > `d60ceaac`: **15 of 15 jobs succeeded, none skipped**. Read the echo rather
      > than the exit code, per M1's own lesson — `validate:api` and
      > `validate:controller` both logged
      > `task: [fmt-check:api] out="$(gofmt -l .)" || exit 2; …` as their FIRST
      > script line, so the shipped recipe with its guard intact ran on the runner
      > under go1.26.4, which is the one place local runs cannot reach.
      >
      > **This PRD prescribed a recipe that fails OPEN, and M2 corrects it in
      > place.** `test -z "$(gofmt -l .)"` gates on the output correctly and exits
      > **0** on a Go file that does not parse: `gofmt` exits 2 and writes to
      > stderr, so the substitution captures nothing and `test -z` is trivially
      > true. Reproduced independently four times. The shipped form carries
      > `|| exit 2` so the property is intrinsic to the recipe rather than
      > inherited from Task's errexit, and `2` rather than `1` so gofmt's
      > parse-failure status stays distinguishable from drift — `task`'s own code
      > is 201 for both. **The fail-open window is exactly a tree with no other
      > drift**, because gofmt still lists the other misformatted files while
      > erroring on the unparseable one: clearing the drift is what arms the hole,
      > so this milestone's own success is the precondition for that failure mode.
      >
      > **M1 had left a live tag-time failure and this MR defuses it.** `d4c6ac8a`
      > touches `docs/dev-conventions.md`, which `is_shipping()` matches, cites
      > `103`, and never touched `CHANGELOG.md` — while `[Unreleased]` carried no
      > `#103`. The next `v*` tag would have failed `publish:assert-changelog`,
      > which sits in `*publish_needs` and therefore blocks every image and chart
      > push. Verified fixed by running the script over the merged range.
      >
      > **Known limitation, and it is this PRD's problem now**: that script is
      > satisfied when **any one** number a merge cites appears in the release
      > section. `#103` is now present, and M3-M6 all cite 103 — so each can merge
      > with no CHANGELOG entry and the gate stays green. No per-milestone token
      > exists, so it cannot be fixed with a better predicate. **Each of M3-M6 owns
      > its own `[Unreleased]` line as part of its definition of done.**
      >
      > **Acceptance controls (Success Criterion 8): the gate reddens and names the
      > file**, proven by the tester in its own worktree with its own paths rather
      > than by the implementer — rc=201 printing `internal/tsprobe/drift_probe.go`
      > (module-relative, as `dir:` implies), green on restore verified with
      > `git status`. Also proven fail-closed on a non-parsing file (rc=201,
      > `exit status 2`), free of fingerprinting with the detector calibrated first
      > against a `sources:`-carrying target that does skip, and module-scoped
      > against a constructed tree where a root-scoped `gofmt -l .` really does
      > list `.go/pkg/mod/…` and `.gocache/…`.
      >
      > **Commit 1 is semantically inert, measured three ways by three agents**:
      > `gofmt -w` re-applied to the parent reproduces it exactly; a `go/scanner`
      > pass over all 510 `.go` files under `api/` reports 0 differ; and the api and
      > controller gates produce identical named per-test result sets at both SHAs
      > (2925 + 175 lines, plus 257 from the live-DB sweep to reach the one
      > reformatted file the ordinary gate skips). **It is not whitespace-only** —
      > gofmt inserts a bare `//` into `ci_fix.go`'s `snapshotSecretPatterns` doc
      > comment, the PAT scrubber's rationale block. No pattern literal changed.
      >
      > **R6 confirmed by execution**: `sqlc generate` after commit 1 is a no-op,
      > and verified as an *executed* one — 29 of 29 `*.sql.go` newer than a marker
      > afterwards, since a run that never executed produces the same empty diff.
      > `gofmt -l internal/store` after regeneration is also empty, so sqlc
      > v1.30.0's output is itself gofmt-clean and the latent deadlock noted under
      > Parallelization is confirmed not-true-today.
      >
      > **What this milestone actually cost, recorded because it is the useful
      > part**: 19 commits, of which **two do the work**. The `fmt-check` recipe has
      > been byte-identical since the third, `.gitlab-ci.yml` untouched since the
      > second. Four validators, seven review rounds, **zero behavioural defects** —
      > every finding was a false or unsupported *claim* about code that was already
      > correct. The dominant failure shape had a name by the end: a sound
      > conclusion accumulating supporting evidence that nobody checks, precisely
      > because the conclusion is agreed. It is recorded in
      > `.claude/agent-team.md`.

      commit one is a
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
      being empty, which `CLAUDE.md` documents as a trap already paid for here
      — and gate on it with an **assignment carrying an explicit guard**, not
      with `test -z "$(...)"`. The shipped form is `Taskfile.yml`'s
      `fmt-check:api` target; read the recipe there rather than restating one
      here (Decision 9, Success Criterion 3).

      *(Corrected 2026-08-02 during M2. This paragraph prescribed
      `test -z "$(gofmt -l .)"`, which **fails OPEN on a Go file that does not
      parse** — gofmt exits 2 to stderr, the substitution captures nothing,
      `test -z` is trivially true, and the gate goes GREEN while printing the
      parse error. Four independent reproductions on 2026-08-02 —
      `CLAUDE.md`'s R10 paragraph says *three* and does not conflict: it
      records the first three (reviewer, lead, tester) and predates the
      fourth. It also
      swallows the filenames, which Success Criterion 8's calibration needs.
      **This mattered beyond M2**: M3 and M5 add gates of exactly this shape
      (`golangci-lint`, `oxlint`, `shellcheck`, `yamllint`), and a coder
      following the old sentence writes the same hole again. Two further
      details are recorded beside the target: the fail-open window is exactly
      a tree with **no other drift**, so clearing the drift is what arms it;
      and the guard is `|| exit 2` rather than `|| exit 1`, to reproduce
      gofmt's own status and keep "does not parse" distinguishable from
      "misformatted" when `task`'s own rc is 201 for both.)*

- [ ] **M3 — Linting: golangci-lint + oxlint, each ratcheted its own way**:

      > **STATUS 2026-08-02: IMPLEMENTED, NOT MERGED. The box stays unticked
      > deliberately** — M1's own status block defines what ticking it means
      > (*"the box is now ticked for the reason it was previously left unticked:
      > this landed"*), and this has not landed. It sits on
      > `feature/prd-103-m3-m6`, and **no pipeline has run against it.**
      >
      > **Four things are settled only by CI and are listed here rather than
      > buried in an MR description**, because a reader who finds this merged
      > should be able to check that they were watched: whether `origin/main`
      > resolves **and has a merge-base** after `GIT_DEPTH: "0"` plus the
      > explicit fetch; the absence of `Can't process results by diff processor`
      > in `lint:api`'s log (grep the message prefix, **never a status code** —
      > the two failure arms emit `exit status 1` and `exit status 128` behind an
      > identical outcome); `test:api-store-it`'s duration against the **max of
      > the other eleven** on a warm pipeline; and `@oxlint/binding-linux-x64-musl`
      > resolving on alpine.
      >
      > **The acceptance bar (Success Criteria 2 and 8) is met four times over,
      > and it is stricter than the criterion asked for.** Four calibration arms —
      > `lint:api`, `lint:controller`, `lint:web`, `lint:agent` — each asserting
      > **all four** properties rather than a red: non-zero exit, the **rule name**
      > in the output (`(errcheck)`, `react-hooks(rules-of-hooks)`,
      > `eslint(no-dupe-keys)`), a repo-root-relative path, and green on restore
      > verified with `git status`. Reproduced independently by the tester in its
      > own tree with its own paths. Plus three controls nobody specified: the
      > `:all` companions' **positive pair** (0 ratcheted against 107 and 5
      > unfiltered), the pre-flight in **both** directions, and a strip-and-restore
      > over all 13 `eslint-disable` comments proving each suppression is
      > load-bearing rather than decorative.
      >
      > **Zero behavioural defects were found in the shipped code, across three
      > review rounds and five validators. Every blocking finding was a false or
      > unsupported CLAIM about code that already worked** — the same result M2
      > recorded. The difference is where one of them lived: `.gitlab-ci.yml`'s
      > `.gate_needs` header asserted it was *"The full validate+test+helm gate
      > set"* while `test:api-store-it` was absent from both anchor lists and runs
      > on tag pipelines. **A wrong claim about what gates a release IS a release
      > hole**, and a `v*` tag could have published every image and the OCI chart
      > over a red LiveDB suite — the only CI coverage of `store.Migrate`. Fixed
      > here (lists at 12 and 14, verified complete **and exact** by parse), and
      > the completeness check is now by **resolved stage through `extends`**,
      > never by job-name shape: a name-based filter is exactly what produced the
      > false claim.
      >
      > **Success Criterion 4 is met**: both copies of the slot table carry real
      > `task lint:*` targets instead of `lint none (gap)`. The `dead code` slot
      > deliberately stays `none (gap, noted 2026-07-26)` with a clause naming
      > `unused`'s **partial** coverage — it finds unused unexported symbols within
      > a package and does not do `deadcode`'s cross-package reachability, nor
      > anything about unused TS exports. M4 still owns that slot. *(M4 has since
      > closed it, on this same branch, and found that **five** tracked files
      > assert that slot rather than the two this paragraph counts — see M4's
      > status block.)*
      >
      > **Deviations from this document, all recorded in place rather than worked
      > around**: `goconst` is OFF (Decision 4's enable list amended — measured at
      > 1211 of 1344 combined findings, and under `--whole-files` its blast radius
      > is 87 non-test files in `api`); `govet` is OFF because `vet:*` already runs
      > it **unratcheted**, so folding it into a ratcheted run is a net weakening;
      > and Decision 3 is **partially** deviated from — Go opens `lint`-stage jobs
      > while the npm half folds into `validate:web`/`validate:agent`, because
      > `GIT_DEPTH: "0"` belongs on the two jobs that need it rather than on a
      > template five jobs share.
      >
      > **What it cost, recorded because it is the useful part.** 20 commits, of
      > which 4 do the work. Three review rounds. **Five numbers were wrong at some
      > point and three of them were wrong in the lead's own dispatches** — a
      > relayed goconst split that would have corrupted a correct figure, a
      > recapped list item that never existed, and a cap-flag attribution written
      > into the very document warning against carried-through numbers. The
      > dominant shape, named by the reviewer, is worth more than the tally:
      > **an inference written in the register of a measurement.** Four instances
      > in one day's prose; the one that was caught was a *narrative*, and the ones
      > that shipped were *numbers already carrying a date and a tool version* —
      > which is the thing nobody re-runs.

      `.golangci.yml`
      modelled on git-manager's (v2 schema, `staticcheck` `errcheck`
      `ineffassign` `unused` `unparam` on; each *disabled* linter
      carries a one-line justification in the file, per that repo's
      convention) applied to both Go modules with per-linter test-file
      exclusions.

      *(**`goconst` was struck from that list during M3 implementation**, 2026-08-02,
      and the disabled-linter convention above is where its justification now lives.
      Amended rather than worked around, per the rule this document applies to
      itself. Three reasons, measured at golangci-lint 2.12.2 / go1.26.5
      darwin/arm64 **with `--max-issues-per-linter=0 --max-same-issues=0`**: it is
      **1178 findings in `api`** uncapped (931 of them in `_test.go`, 247 not), which
      is **91.5%**
      of the combined backlog (goconst 1178 + controller's 33 = 1211, against a
      non-goconst backlog of api 107 + controller 5 = 112; re-derived on the
      **shipped** enable set — the "90%" this previously read was computed on the
      design-wave set, whose non-goconst backlog was 134, and it was the one figure
      in the block nobody re-took when the enable set changed);
      **the 21 non-test findings visible in the capped run
      *across both modules* were read by hand, plus a 1-in-20 re-sample across all 86
      non-test files in `api`, and
      none is a defect** — CLI subcommand names, JSON field names, and the
      `queued → claimed → running` run-state vocabulary this PRD's own Architecture
      section documents, which is a goconst finding at every switch arm; and
      **`--whole-files` makes its blast radius 86 non-test files in `api` alone**, so
      touching `internal/handler/auth.go` for any reason would demand extracting
      `"status"` into a constant. That is the ratchet's sharpest edge pointed at the
      linter with the least signal. The argument for it here was git-manager's "136
      goconst warnings accumulated unnoticed" — but this document also quotes
      git-manager's CLAUDE.md saying that repo has **no CI gate at all**, so the
      lesson there is "no gate ⇒ accumulation", not "goconst has signal".*

      *Every earlier goconst figure in the M3 design record was a **cap reading**:
      the default `--max-issues-per-linter` is 50 and goconst reported exactly 50,
      which is the tell nobody looks at because 50 is a plausible number. The two cap
      flags are not redundant — on the shipped config `api` reads **56 capped against
      107 uncapped**, while `controller` reads **5 either way**. The smaller
      module agreeing exactly is the camouflage.*

      *🔴 **THE TWO FLAGS COMPOSE IN ORDER; NEITHER "OWNS" A LINTER**, and the
      difference decides whether one of them looks droppable. Isolation matrix on the
      shipped config, `cache clean` before each cell:*

      ```
      --new-from-merge-base=                            56   errcheck 36  staticcheck 13
      + --max-issues-per-linter=0                       56   errcheck 36  staticcheck 13
      + --max-same-issues=0                             78   errcheck 50  staticcheck 21
      both                                             107   errcheck 79  staticcheck 21
      ```

      *`--max-same-issues` folds duplicate **messages** first; `--max-issues-per-linter`
      then truncates the survivors at 50. So with `goconst` off nothing exceeds 50 until
      the fold is lifted, which makes **`--max-issues-per-linter=0` alone a complete
      no-op** — and the per-linter flag's real effect is **errcheck 50→79**, visible only
      after the other flag has fired. **That is why checking it the obvious way is a
      trap**: run cell two, see 56→56, conclude the flag does nothing, delete it, and
      errcheck reads **50** forever — which is exactly the plausible-looking number this
      section exists to warn about.*

      *(This PRD and `Taskfile.yml` both previously read "`--max-same-issues` takes
      errcheck 36→79, `--max-issues-per-linter` takes staticcheck 13→21". **Both halves
      were false** and both came from a 2-cell measurement that cannot separate the
      flags. Refuted and re-derived independently four times — reviewer, lead,
      fact-checker, and the tester, which retracted its own earlier endorsement after
      running the 2×2 itself. Recorded at length because the wrong version propagated
      out of a Taskfile comment into a PRD carry-forward before anyone re-measured: a
      comment is a citation source, so an unsupported figure in one does not stay
      there.)*

      *🔴 **AND `issues.uniq-by-line` DEFAULTS TO `true`, DEDUPPING ACROSS LINTERS**, so
      any single linter's count depends on which others are enabled: the 107 above
      becomes 108 with `--uniq-by-line=false` (staticcheck 21→22). One finding today,
      recorded for **M4 and M5**, which add linters — this is how a staticcheck finding
      disappears from an "unfiltered" total without anyone touching staticcheck.*

      *🔴 **NAME THE POPULATION OR THE goconst FIGURES WILL NOT RECONCILE.** Two records
      quote a string literal containing a newline, so each wraps across two output lines.
      `1178` is the tool's own tally, `1176` is the well-formed single-line records (the
      denominator behind Amendment 3's "395 distinct strings over 1176 sites"), and a
      `(goconst)$`-anchored grep also returns 1178 **by coincidence** — counting the two
      pathless continuations while missing the two headers that lost their suffix. Split
      the wrong one and the non-test bucket reads 249 across 87 files. Both wrapped
      records are in `_test.go`, so **247 across 86** is correct either way. This cost
      three people a wrong figure.)*

      **`govet` is also deliberately NOT in the golangci-lint set**, and it owes its
      justification for the opposite reason: `task vet:api` / `vet:controller`
      already run `go vet ./...` inside `gate:*` and in CI, **unratcheted**. Folding
      it in would be a net weakening, since today every vet finding blocks and
      ratcheted only new ones would. It is a golangci-lint default, so "the tool
      enables it by default" is exactly the premise that will motivate re-adding it. **oxlint** for `web/` and `agent/` per Decision 8 — including
      its two silent-no-op traps: **the `react` plugin is off by default, and
      `rules-of-hooks` is `pedantic` rather than `correctness`**, so a config
      that enables the plugin and stops there still never runs the rule this
      milestone exists for.

      **🔴 THE GATE TRAP, AND IT IS A HARD REQUIREMENT: `oxlint src test` EXITS 0
      WHILE REPORTING FINDINGS.** `correctness` defaults to `warning` severity and
      warnings do not set the exit code. Reproduced: the bare invocation exits 0
      with 10 findings, while `-D correctness` exits 1 on the same 10. **A target
      without `-D correctness` (or `--deny-warnings`) is a report that always
      passes** — a lint job that can never fail, in the milestone that adds
      linting. Same family as the `gofmt -l` trap `CLAUDE.md` already carries,
      arriving through a default *severity* instead of through a flag.

      Ratchet mechanisms differ per Decision 4 and must be stated in the MR:
      Go uses `--new-from-merge-base=main --whole-files`, which requires
      raising GitLab's shallow `GIT_DEPTH` for the lint job or merge-base
      resolution fails. **TypeScript has no baseline and needs none: measured with
      `--react-plugin` (the configuration M3 ships), the debt is 10 findings on
      `agent/` and 6 on `web/`, 16 combined, and M3 fixes them.** `--max-warnings`
      exists as a regression brake, with the limit
      Decision 4 records. Also record the **unfiltered** Go finding count in the
      MR description, since `--new-from-merge-base` reports nothing on `main`
      pipelines and that debt is otherwise uncountable. Go burn-down gets its own
      issue.

      **SCOPE RULING — correctness-only.** *(Figures below are the fixed-experiment
      kind Decision 10 exempts from the count ban, but the date and tool version
      they were measured under were never recorded at the time — flagged rather
      than backfilled, since backfilling a date onto a measurement nobody dated
      would manufacture a precision that is not there. Re-measure at M3
      implementation time and stamp it then.)* Type-aware linting becomes **its own
      issue**: 37 + 178 = 215 findings with a real burn-down, and **119 of web's
      are one shape** (`onClick={asyncHandler}`), which is a behavioural refactor
      rather than a lint adoption and must not ride in on a tooling MR. **The
      mechanism, so nobody reads 119 as mechanical:** wrapping a promise-returning
      handler to satisfy the rule changes what happens to a REJECTION — the
      rejection stops being the returned promise's and becomes unhandled, or gets
      swallowed by whatever the wrapper does with it. That is 119 opportunities to
      change error behaviour under cover of a lint fix. Pedantic
      is dead on arrival at **3,015 combined**, with `no-inline-comments` alone at
      **474** in a repo whose inline comments are mostly load-bearing recorded
      evidence. **A later fact-check independently measured the pedantic combined
      total at 2,976** (web `src` 1684 + agent `src test` 1292), against this
      section's 3,015 — and nothing recorded here lets a reader decide which is
      right. That is the finding, not the gap itself: `no-inline-comments`'s 474
      reproduces exactly as 246 + 228, which is the tell that whatever moved is
      scope or oxlint-version drift between the two runs, not an arithmetic slip
      in either one. Do not silently adopt either number; re-measure at M3 and
      record the version and invocation that produced whichever total lands.

      **Precondition**: ~~resolve Open Question 2~~ — **RESOLVED 2026-08-02**, see
      Decision 8. Nothing blocks M3 now.

      **Calibration**: add an unchecked error return in a scratch file, confirm
      `errcheck` fires through `task lint:api`; add a violating hook call,
      confirm `rules-of-hooks` fires through `task lint:web`. **The calibration
      must assert a NONZERO EXIT, not merely that output appeared** — per the gate
      trap above, output and exit status are independent here, and a first run
      reporting nothing is indistinguishable from a linter that is not wired up.

- [x] **M4 — Dead code detection**: `golang.org/x/tools/cmd/deadcode -test
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
      zero, ~~with `--max-issues` as a regression budget in the meantime~~.

      > **🔴 AMENDED 2026-08-02, IMPLEMENTED. See the amendment on Decision 4
      > for the measurements.** Two claims in the paragraph above are false and
      > one is a mis-sized budget:
      >
      > - **`--max-issues` is inert for a warn tier** (it counts error-severity
      >   issues only), so it is struck rather than merely qualified. What
      >   shipped: `exports`/`types`/`nsExports`/`nsTypes`/`enumMembers`/
      >   `namespaceMembers` at `warn` — **reported on every run, gating
      >   nothing** — with `files`/`dependencies`/`devDependencies`/`unlisted`/
      >   `binaries`/`unresolved`/`duplicates` and the catalog pair at `error`,
      >   gating at zero. The export burn-down (web 22, agent 53) is
      >   **issue #206**, not this milestone.
      > - **The wrapper was not the milestone.** `deadcode -test ./...` finds 1
      >   in `api` and 0 in `controller`. The function was deleted, both
      >   baselines ship **empty**, and both Go modules gate at **zero**. The
      >   wrapper (`scripts/deadcode-gate.sh`) survives for the **exit code**:
      >   deadcode exits 0 on any number of findings and 1 only on a load error
      >   with an empty stdout, so an additions-only diff passes green on a
      >   module that does not compile.
      >
      > Shipped: five targets under a `deadcode:` prefix (`deadcode:api`,
      > `:controller`, `:web`, `:agent`, plus the `deadcode` aggregator), each
      > wired into its `gate:<component>`; two reported-never-gating companions
      > (`deadcode:api:all`, `deadcode:controller:all`) running **without**
      > `-test`, because `-test` makes a production-dead function invisible the
      > moment a test calls it and `unused` misses it too — 44 and 4 findings of
      > that class today. **Zero new CI jobs**: the targets hang off `lint:api`,
      > `lint:controller`, `validate:web` and `validate:agent`, so `.gate_needs`
      > and `.publish_needs` stay at 12 and 14 with their two-element delta
      > intact (verified by parsing, not grepping).
      >
      > `deadcode` is pinned at **v0.48.0**; v0.38.0 was measured independently
      > on the same tree and gave identical counts. `knip` is **6.31.0**.
      > The package pattern must stay the **module root** — only `main` packages
      > are call-graph roots, so `./internal/...` inflates `api` from 1 to 86
      > findings at rc=0, silently.

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

      > **STATUS — M4 IMPLEMENTED 2026-08-02 on `feature/prd-103-m3-m6`. NOT
      > MERGED, NOT YET PIPELINE-TESTED.** Same standing as M3's block above:
      > every figure below is from a local run, and no MR pipeline has executed
      > any of it. Calibration transcript:
      > `probes/prd-103-m4-calibration.txt`; design-wave evidence:
      > `probes/prd-103-m4-{architect,reviewer}.txt`.
      >
      > **Success Criterion 8 is met, six arms**, each with the four-property bar
      > (non-zero exit, the finding's **identity** in the output, a
      > repo-relative path, green on restore verified with `git status`):
      > an exported dead Go function (`deadcode:api` exit 1 naming it, while
      > `lint:api` says "0 issues." on the same tree); an unused TS **file**
      > (`deadcode:web` exit 1 naming it); the mistyped-rule-name control, three
      > cells one character apart; a module that does not compile (**exit 2**,
      > the instrument-broken status, closing the fail-open hole); a stale
      > baseline entry (exit 1); and the position-free key surviving a line
      > shift **with a control showing the default positional output moved
      > 452 → 453 across the same two trees**. Restores were `cp`-based, never
      > `git checkout --`.
      >
      > **Success Criterion 4 is met for this slot in FIVE files, not the three
      > M4's brief listed.** The two it missed are the two already missed once:
      > `.claude/agents/reviewer.md` (which M3 had to correct on this exact
      > point) and `.claude/agents/coder.md` (which phrases the claim differently
      > from every other copy — the shape that defeats a literal grep).
      > `api/internal/agenttmpl/builtins/reviewer.md` was deliberately not
      > touched.
      >
      > **Deviations from this document, recorded rather than worked around**:
      > the wrapper is not the milestone and both baselines ship empty (see the
      > amendment above); `--max-issues` is struck; `knip.jsonc` rather than
      > `knip.json`, so every suppression carries its reason inline; one extra
      > target pair (`deadcode:*:all`) that this bullet does not ask for,
      > because `-test` hides the write-it/test-it/never-wire-it class from the
      > gating invocation entirely.
      >
      > **What is deliberately NOT met**: gate-on-new for unused TS exports.
      > That tier is `warn` and gates nothing; the burn-down is a follow-up issue
      > carrying its counts (web 22, agent 53, 2026-08-02) so Decision 3's "a
      > warning nobody must act on" does not become the end state by default.

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
      in CI, which lets `.claude/agents/auditor.md`'s security-scan slot stop documenting its own
      absence. **`govulncheck`** for both Go modules and
      `npm audit --audit-level=high` for both npm packages, initially
      `allow_failure` only until the current finding count is known.

      **Markdown link checking: extend `web/scripts/check-docs.mjs`, do not add
      a second checker.** It already validates relative-link existence and
      link-text-path correctness for `docs/`, `ARCHITECTURE.md`, `README.md`,
      `CLAUDE.md` and `specs/*.md` via the `extraLinkFiles` list. The
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
      it**, and this milestone previously claimed otherwise. `task test:api` is
      `go test -race -count=1 ./...`; `test:api-store-it` has it in its
      `-run 'LiveDB$'` sweep. The remaining module is `controller`
      (`task test:controller`, `go test -count=1 ./...`).

      *(Provenance corrected 2026-08-02 during M1: this said "landed 2026-07-25
      in `224b5349` for PRD #108 M4". `224b5349` is a merge on the PRD #98
      wave-3 branch; `main` got the line on 2026-07-26 via `77cb96e4`; and the
      two flags come from two PRDs. Reproduce with
      `git log --oneline -s --diff-merges=first-parent -S 'race -count=1 ./...' -- .gitlab-ci.yml`
      — the plain `-S` form returns nothing, because the line was produced inside
      a merge. See Decision 11.)*

      Consequently this is **no longer "the riskiest single change in the PRD"**
      — that framing was sized for the api suite. It still ships on its own,
      before the coverage work, and the cautions still apply to `controller`:
      `-race` costs runtime (cite the comment block above `test:api`, which
      already records the api measurement, rather than re-deriving the general
      figure),
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
      vestigial `coverage.out` line from `.gitignore` or makes it real.

## Parallelization

| Phase | Milestones | Depends on | Files touched | Can run in parallel |
|---|---|---|---|---|
| 1 | M1 | — | `Taskfile.yml`, `.gitlab-ci.yml`, `CLAUDE.md`, `.claude/agent-team.md`, `.claude/agents/{coder,tester,reviewer,auditor}.md` | No — blocks all |
| 2 | M2 | M1 | `api/**/*.go` (format only), `Taskfile.yml`, `.gitlab-ci.yml` | Yes |
| 2 | M3 | M1 | `.golangci.yml`, `web/.oxlintrc.json`, `agent/.oxlintrc.json`, **`web/package.json`**, `agent/package.json`, `Taskfile.yml`, `.gitlab-ci.yml` | Yes, except vs M4/M6 |
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
  the `stages:` list. That is why M1 adds the (empty, inert) stage entry
  up front — appending cannot resolve it.
- **`web/package.json` is a three-way contention** that the previous version of
  this table missed: M3 adds the oxlint devDep, M4 adds knip, M6 adds
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
   to zero and promoted to `error`. ~~(`--max-issues` is a fixed budget, not a
   ratchet — fix one old finding, add one new, still under budget.)~~ If
   gate-on-new is wanted for exports before the burn-down finishes, knip needs
   the same diff-wrapper M4 writes for `deadcode`.

   > **Amended 2026-08-02 (M4).** The struck parenthetical **understated the
   > problem**: `--max-issues` is not a weak ratchet for the warn tier, it is
   > **inert** for it — measured, it counts error-severity issues only and exits
   > 0 with every warn finding printed. So the sentence before it is right for a
   > stronger reason than it gave. **The criterion's dead-Go-function half is
   > now MET and demonstrated** (`probes/prd-103-m4-calibration.txt`): an
   > exported dead function gives `task lint:api` "0 issues." and
   > `task deadcode:api` a nonzero exit naming the symbol. The **unused-TS-export
   > half is deliberately NOT met** and is tracked as **issue #206** with its
   > counts, rather than closed by a mechanism that does not work.
   >
   > One thing the criterion does not say and should: **the Go arm must use an
   > EXPORTED symbol.** golangci-lint `unused` catches an unexported one and
   > runs earlier in `gate:api`, so the gate fail-fasts at lint and `deadcode`
   > never executes — a calibration built that way demonstrates M3's check while
   > claiming to demonstrate M4's. Measured both ways in-repo.
3. **Gate *recipes* are defined once, in `Taskfile.yml`.** `CLAUDE.md`
   §Commands, the `.claude/agent-team.md` paste-block and the
   `.claude/agents/*` tails name `task` targets and never restate a command
   line — per Decision 9. The paste-block itself stays, keeps its `none (gap)`
   slots and its per-flag "why" lines, and continues to be pasted into every
   dispatch. *(This criterion previously read "the gate command appears in
   exactly one place in the repo", which would have deleted the paste
   mechanism.)*
4. The `## Quality gates` paste-block in `.claude/agent-team.md` and its
   duplicate slot table in `.claude/agents/tester.md`'s `## For this repo` tail —
   both copies — carry a real `lint` command instead of
   `lint none (gap)`, and the `noted` markers on the slots this PRD closes are
   removed rather than left behind. *(This criterion previously targeted the
   string "Lint command: none dedicated", which was deleted in `027a4b88` and
   so was already satisfied while testing nothing.)* **The two copies have
   already drifted on this exact point, so "remove the `noted` markers" is not
   a symmetric instruction: `.claude/agent-team.md`'s paste-block carries four
   (`dead code`, `coverage`, `security scan`, `pre-commit`), and
   `.claude/agents/tester.md`'s slot table carries none and never has.** A
   milestone told to remove the markers "in both" will find only one file has
   any, and should not read that as having edited the wrong file.
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
  instead is the `lint           none (gap)` slot line in the paste-block and its
  duplicate in `.claude/agents/tester.md`'s slot table.
- `.claude/agents/auditor.md`'s security-scan slot: "CI (`.gitlab-ci.yml`) runs
  validate/test/build across api/controller/web/agent but has NO secret scanner
  (gitleaks/trufflehog)" — corrected in M5. It already reads "PRD #103 M5 adds
  them", so the edit is to make the present tense true, not to add a pointer.
- `CLAUDE.md` §Commands: the hand-typed recipes are replaced by `task`
  invocations in M1 — **command lines only**. Every measured-evidence paragraph
  around them stays verbatim; see M1's scoping note. The prose about e2e being
  deliberately out of CI stays true and is not touched.
- `.gitignore`: the `coverage.out` entry currently refers to tooling that
  does not exist — M6 either makes it real or removes it. (`.task/` is already
  present; nothing to add, and adding it is not the work.)
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
2. ~~**oxlint vs ESLint react-hooks parity** (Decision 8)~~ — **RESOLVED
   2026-08-02: parity holds, and oxlint is adopted.** Identical findings on
   `web/src` (2 each, same files, same missing dep), 36 of 37 identical rule sets
   on a fixture corpus with no file flagged by one and not the other, and oxlint
   honours `// eslint-disable-next-line react-hooks/exhaustive-deps` verbatim, so
   the repo's two existing suppressions migrate with zero source edits. Decision 8
   is amended accordingly and records the two config traps and the three limits of
   the measurement.
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
6. ~~**Is `Formula/uzi-cli.rb` in scope for M5?**~~ — **RESOLVED 2026-08-02: IN
   scope, via a `ruby -c` target.** It has no check of any kind today, `devbox.json`
   already carries ruby as a tier-2 package so a run can `ruby -c` it, and the
   target is nearly free. M5 adds it alongside the shell and YAML checks.
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
