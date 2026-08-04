# PRD #103 — the last two units: M5's MR-C, and M6

**This file is the SPEC.** Corrections amend this file with a dated `## Amendment N`
entry; messages only name the section that moved. Do not carry a requirement in a
message.

**Sweeping this file needs `git grep`.** `.claude/agent-team-tasks/` is gitignored
(`.gitignore:52`) and this file is force-added, so it is tracked and simultaneously
invisible to `grep -r` / `rg`. `--hidden` is the wrong axis and changes nothing.
See carry-forward item 8.

- **Branch**: `prd-103`, in worktree `/home/user/myorg/workspaces/uzi/prd-103`
- **Branch point**: `0111f01c` — verified `== origin/main` after `git fetch origin`
  (0 ahead, 0 behind, clean tree) at 2026-08-04. Not `git rev-parse main`; the
  remote was fetched first.
- **Prior record**: `prds/103-dev-loop-quality-gates.md` (the PRD),
  `.claude/agent-team-tasks/prd-103-m5.md` (27 amendments; MR-C's design is its
  `# MR-C` section), `.claude/agent-team-tasks/prd-103-carry-forward.md`
  (items 1-22 — **items 5, 6, 9, 12, 16-22 bind this work directly**).

---

## Roster

**units: 2, SEQUENTIAL — Unit A (MR-C) then Unit B (M6).** Not a fan-out, and the
reason is a dependency rather than caution: MR-C ships the vitest 2→4 major, and
M6's jsdom fix has a different design on either side of it (`environmentMatchGlobs`
works on vitest 2 and is deprecated from vitest 3). They also contend on three hot
files — `.gitlab-ci.yml`, `Taskfile.yml`, `web/package.json`. One shared worktree,
lead-enforced writer token, exactly one writer unfrozen at a time.

| role | disposition |
|---|---|
| architect | **dispatched 2026-08-04 at `848cf53d`** — design wave |
| reviewer | **dispatched 2026-08-04 at `848cf53d`** — design wave, then per-unit |
| auditor | **dispatched 2026-08-04 at `848cf53d`** — design wave, then per-unit |
| fact-checker | **REPORTED 2026-08-04 at `848cf53d`** — 1 refuted (R1), 1 ambiguous (A1), 6 notes, 7 targets confirmed, every `measured` figure re-derived independently. Folded in as **Amendment 1**. Evidence: `probes/fc-mrc-m6/INDEX.txt` + 12 raw files. |
| coder | pending — Unit A, then Unit B |
| tester | pending — after each unit's first commit, never at kickoff |
| documenter | pending — CHANGELOG + `docs/dev-conventions.md` owe a line per carry-forward 12 |
| spec-keeper | pending — `specs/` exists |
| web-ux | **closed — no user-facing surface.** Neither unit changes rendered behaviour, so there is nothing to drive in a browser. *(Amendment 1: the original reason said the units "touch CI config, task targets and test configuration only", which understates Unit A — a vitest 2→4 major can require edits to the 118 test files themselves. The closure stands; its stated scope was wrong. The 118/1660 predicted-count control is what covers that risk.)* |
| researcher | **closed — the research is already written.** M5's brief and the carry-forward doc are the corpus; a fresh investigation would re-derive 22 items somebody already measured. |
| release | pending — user-gated, and `/prd-full` stops at MR creation, so this is MR-open only, never merge. |

---

## Day-one state, MEASURED 2026-08-04 on `0111f01c`, in this worktree

Every figure below is mine, taken today. **The M5 record's corresponding numbers are
stale and must not be quoted** — it says so itself: its measurements predate a
134-commit merge, and "whoever asserts the vulnerability state post-merge must
re-run `govulncheck`."

### govulncheck — BOTH MODULES ARE ALREADY CLEAN ON CALLED VULNS

`go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...`, run in each module root:

```
api         0 called   0 in imported packages   2 in required modules   rc=0
controller  0 called   0 in imported packages   1 in required module    rc=0
```

Raw output: `probes/prd-103-mrc-m6-baseline/govuln-{api,controller}.txt`.

**This inverts MR-C's planned shape.** The M5 record's binding ruling 2 —
*"govulncheck's called vulns are BUMPED here, not filed"* — was written against
`api 2 called, controller 3 called`. Those were fixed by the `go.mod` bumps in
`971c5468`, which are **already on `main`**. So there is nothing to bump and the
gate can land at zero on day one. **Re-run it yourself before believing this**; it
is a claim about a remote database and it is a day old the moment it is written.

Note what the counts are NOT: the residual 2 and 1 are vulnerabilities in
*required modules* that no code path reaches. `govulncheck` exits 0 on them.
Gating on those would be a different, stricter check than the PRD specifies.

### npm audit — the full tree, per user ruling 1 (NOT `--omit=dev`)

Built from `--json`, never the text summary — see the trap below.

```
web    total=8   critical=1  high=2  moderate=5  low=0
agent  total=5   critical=0  high=2  moderate=3  low=0
```

high-and-above, with the fix each one wants:

| pkg | sev | package | range | fix | breaking |
|---|---|---|---|---|---|
| web | **critical** | `vitest` | `<=3.2.5` | `vitest@4.1.10` | **yes (semver major)** |
| web | high | `vite` | `<=6.4.2` | `vitest@4.1.10` | **yes (same major)** |
| web | high | `postcss` | `<=8.5.22` | in-range | no |
| agent | high | `fast-uri` | `3.0.0 - 3.1.4` | in-range | no |
| agent | high | `ip-address` | `<=10.3.0` | in-range | no |

Raw: `probes/prd-103-mrc-m6-baseline/audit-{web,agent}.json`.

**`agent` has changed since M5 measured it** — that record says "1 high of 3
(`fast-uri`)"; it is now **2 high of 5**, `ip-address` being new. A second
confirmation that these numbers have a shelf life.

**🔴 THE TEXT OUTPUT HIDES THE ONLY CRITICAL IN THE REPO.** M5's record states this
and it reproduced exactly: `vitest` (critical) and `vite` (high) appear in
`npm audit`'s human-readable output only as unlabelled *"Depends on vulnerable
versions of…"* lines. A fix list built by reading the text output omits them.
Use `--json`.

**The vitest major is therefore not a separate scope item bolted onto a security
MR — it IS the fix for the critical.** That is worth stating because the concern
raised against ruling 3 (a test-runner major inside a tooling MR) reads differently
once the major and the critical are the same object.

### vitest baseline — the predicted-count control for the upgrade

```
vitest/2.1.9 darwin-arm64 node-v26.4.0
Test Files  118 passed (118)
Tests       1660 passed (1660)
Duration    41.84s
```

Raw: `probes/prd-103-mrc-m6-baseline/vitest-baseline.txt`.

**Post-upgrade green must reproduce BOTH numbers.** A green with a lower collected
count is a silently narrowed suite and is invisible to the exit code. Per
carry-forward 21, write the expected pair down before the run and reconcile
afterwards — a predicted count is the one control an output cannot satisfy by
coincidence. (M5's record cites 116/1635 for this same pair; the suite has grown.
**Use 118/1660**, and re-derive it at your own tip rather than quoting either.)

### jsdom pragma coverage — M6's real distribution

```
web/src test files   118
  with `// @vitest-environment jsdom`   76
  without                               42
```

`web/vite.config.ts`'s `test:` block sets `testTimeout: 20000` and **no
`environment`**, so those 42 run under node today.

**This is the measurement that decides M6's approach and it cuts against the easy
fix.** Flipping the global default to `jsdom` moves 42 files from node to jsdom —
the same silent-wrong-environment bug in the opposite direction, which is precisely
what the PRD warns about. That warning stands unchanged.

**🔴 ALL 42 ARE NODE-SIDE. THERE IS NO MISSING-PRAGMA POPULATION.** *(Amendment 1,
2026-08-04. This paragraph originally read "some are genuinely node-side (`lib/`,
contract tests) and some are DOM tests missing their pragma. Those two populations
need separating before any config change is designed … its result may change the
design." The second population does not exist.)*

```
36  web/src/lib      6  web/src/mocks      0 elsewhere
42  .ts              0  .tsx
```

**The decisive instrument was the measurement two sections above it in this very
file.** A DOM test running under node throws `ReferenceError: document is not
defined`; the baseline is **118/118 files, 1660/1660 tests green** with those 42
running under node. A green suite is therefore a proof that no DOM test is among
them. The refutation was already in the brief, one section up, at the moment the
wrong sentence was written.

Three independent probes agree, each shaped to fail differently and each carrying a
positive control over pragma-carrying `.tsx` files: DOM-global references → 1 hit,
the word `localStorage` inside a test *description string*; react / testing-library
imports → 1 hit, a file importing a bare constant with no render; a wider DOM-token
set → 2 hits, the word "screen" in prose comments. Zero real DOM usage.

**So Unit B's classification step is DISCHARGED, not deferred, and the `projects`
split is mechanical**: `src/lib/**` and `src/mocks/**` node, everything else jsdom.
Re-derive the partition at your own tip before relying on it — files get added.

### Two instrument failures I hit taking these numbers, recorded because they are the shape this milestone is about

1. **`npm audit` reported web and agent as byte-identical** (`total=8 crit=1 high=2
   mod=5` for both). Cause: the second invocation had no `cd` — the Bash tool's cwd
   persists, so both runs measured `web`. Caught by the *uniform result across
   every cell is an instrument failure* rule, not by suspecting the data.
2. **`grep -c '^Vulnerability #'` returned 0 for both modules**, which reads as
   "clean" and happens to be the right answer for the wrong reason: govulncheck
   prints that header only when findings exist, so the grep cannot distinguish
   *no findings* from *wrong pattern*. The discriminating read was the prose
   summary, which reports 2 and 1 uncalled — i.e. the file was never empty.

**Failure 1 reproduced independently THREE MORE TIMES within hours**, by the
fact-checker verifying this brief: `git ls-files web`, a `git grep -- web` and a
`git grep -- .gitlab-ci.yml`, each with cwd left inside `web/` by an earlier
command, **each returning a clean empty result**. The Bash tool's cwd persists
between calls and an empty result is indistinguishable from a true negative. Four
instances in one day, by two careful parties, is a property of the tool rather than
of anyone's attention: **pass absolute paths, or `cd` in the same command.**

**And one more instrument note from the same pass, which cost a wrong empty
answer**: `git grep -F -- '--audit-level' <paths>` finds NOTHING, because the
leading `--` is parsed as end-of-options; `git grep -F -e '--audit-level' <paths>`
finds six. **A pattern beginning with `-` needs `-e`.** It was caught only because
a positive control was run on the same form.

---

## Unit A — MR-C

**Deliverable**: `govulncheck` and `npm audit` gating in CI, the npm high-and-above
findings at zero, vitest on 4.x.

### Binding user rulings, carried verbatim from `.claude/agent-team-tasks/prd-103-m5.md`

1. **`npm audit` gates the FULL tree, fixed first, at zero.** Not `--omit=dev`.

   **🔴 THE GATING FLAG IS `--audit-level=high`, AND OMITTING IT MAKES THE GATE RED
   ON DAY ONE AFTER EVERY PLANNED FIX LANDS.** *(Amendment 1, 2026-08-04: the flag
   was in the PRD (`:1877`) and in M5's record (six sites) and **nowhere in this
   brief**, which said only "at zero". That is under-specified in the direction that
   costs a red pipeline.)* Measured: **bare `npm audit` exits 1 on both packages
   today**, and it still will afterwards. `vitest@4.1.10` clears web's
   `@vitest/mocker`, `esbuild` and `vite-node` moderates as a side effect, but
   **five moderates survive every planned fix** — web's `react-router` and
   `react-router-dom`, agent's `@hono/node-server`, `@modelcontextprotocol/sdk` and
   `hono`. All five report `fixAvailable: true` (in-range, no major), so clearing
   them is *possible*; it is not what the PRD specifies, and widening scope to do it
   is a decision to surface, not to take. **"At zero" means high-and-above at zero.**
2. **`govulncheck`'s called vulns are BUMPED here, not filed.** *(Vacuously satisfied
   today — there are none. Do not read that as licence to skip the gate.)*
3. **The vitest 2.x → 4.x major is IN.** Raised as a concern and reaffirmed.
4. **No `allow_failure` anywhere.** The pipeline has none today and M5 does not
   introduce the first.

### Placement, already designed — do not re-derive

- **`govulncheck` folds into `lint:api` / `lint:controller`**, which already carry
  the `lint-` cache prefix for the GOCACHE reason it needs.
- **`npm audit` folds into `validate:web` / `validate:agent`**, which already run
  `npm ci`.
- **Zero `.gate_needs` / `.publish_needs` edits** — and *verify that by parsing,
  not by grepping* (carry-forward 2 and 7: this repo lost a chart object for days
  to a grep that found `kind:` in a merged document).
- **Neither may enter `task gate` or any `gate:*`.** They make network calls and a
  contributor's gate stays offline — the same reason carry-forward 6 bans a bare
  `npx`.

### Three findings that change the implementation, measured during M5 and not to be re-derived

- **`go run` destroys govulncheck's exit-code discrimination, and the split is
  FOUR-way, not three.** *(Amendment 1, 2026-08-04: this said "exits 3 for findings
  and 1 for its own errors". There is a third nonzero code and a wrapper built on a
  two-way split misreads it.)* Read out of `x/vuln@v1.1.4`: **0** clean, **3**
  findings (`internal/scan/text.go:84`), **2** usage error (`errUsage`,
  `internal/scan/errors.go:26`, e.g. a typo'd flag), **1** everything else via
  `main.go`'s `default:`. Under `go run` every nonzero flattens to **1** — this
  repo's "there are findings" code. Use `go install …@vX` into a temp GOBIN: keeps
  the pin *and* the discrimination. *(My baseline above used `go run` deliberately —
  I wanted counts, and rc=0 is unambiguous either way. The gate needs the codes.)*
- **`govulncheck -format json` always exits 0**, even with called vulns present, and
  the mechanism is stronger than "observed": `errVulnerabilitiesFound` has **exactly
  one return site**, in the text formatter, so JSON *cannot* produce it. The source
  comment says so outright: *"This returns exit status 3 when running without the
  -json flag."* A wrapper reading a JSON run's status is permanently green — the
  fail-open shape, and the natural thing to write if you reach for JSON to count.
- **A version BUMP of a devDependency fails SILENTLY, unlike a new one.** Carry-forward
  12 is about a *new* dep: `npm run` puts `node_modules/.bin` on PATH and fails
  closed with `command not found`. A bump cannot fire that — the binary is already
  there, so a stale checkout runs **vitest 2 while the lockfile says 4**, green,
  with nothing naming the mismatch. The remedy is **not**
  `--ignore-scripts --save-dev --save-exact`; that guards the other hazard. It wants
  a lockfile-versus-`node_modules` staleness check, or a `vitest --version`
  assertion in the gate. **Design this deliberately; it is the one genuinely open
  design question in Unit A.**

### `node_modules` in THIS worktree — the standing hazard does NOT apply, and check before you trust that

Carry-forward 19 says a PRD worktree's `node_modules` are symlinks into `main`, so
an install writes *through* them into the shared reference tree. **That is not the
state here.** Both were ABSENT; the lead installed real local trees on 2026-08-04
with `npm ci --ignore-scripts` in each, and verified
`/opt/homebrew/bin/agent-browser` still points into
`/opt/homebrew/Cellar/agent-browser/0.31.1` on both sides of the `agent/` install.

**Re-check with `ls -ld web/node_modules agent/node_modules` before any npm
operation** rather than trusting this paragraph — it is exactly the kind of claim
that is true when written and false when read, and `git status` cannot show you
(they are gitignored, so no diff can reveal them).

Any further install in `agent/` still needs `--ignore-scripts`: npm 11.17 prints
`npm warn allow-scripts … not yet covered by allowScripts:` naming
`agent-browser`, **which reads as "these were skipped" and is advisory — the
postinstall runs anyway.**

---

## Unit B — M6

**Deliverable**: coverage visible on every MR for `api`, `controller` and `web`;
`-race` on `test:controller`; the jsdom environment fix; `.gitignore`'s vestigial
`coverage.out` line resolved.

- **`-race` is scoped to `test:controller` ONLY.** `test:api` already has it
  (`go test -race -count=1 ./...`), and `test:api-store-it` has it in its
  `-run 'LiveDB$'` sweep. The PRD previously claimed otherwise and corrects itself.
- **No failing threshold** (Decision 6). Totals printed in CI job output, GitLab's
  coverage regex wired so MRs show the number.
- **`web` needs `@vitest/coverage-v8`** added — it is not currently a dependency.
  Per carry-forward 12 that owes a CHANGELOG line and a `docs/dev-conventions.md`
  line, and per carry-forward 6 the Task target **delegates to a `package.json`
  script** rather than reimplementing the command.
- **jsdom: `environmentMatchGlobs` is REMOVED in vitest 4, not merely deprecated.**
  Settled against three package tarballs rather than doc prose: 2.1.9 has it
  implemented with no `@deprecated` tag; 3.0.0 has it with a `@deprecated` JSDoc
  *and* a runtime `logger.warn`; **4.1.10 has zero occurrences of it in the entire
  package.** After Unit A the tree is on 4.x, so it is gone, not noisy.
- **The route is `test.projects`, and `test.workspace` is NOT an alternative.**
  *(Amendment 1, 2026-08-04. This line read "the projects / workspace config is the
  only route", inherited verbatim from `prds/103-dev-loop-quality-gates.md:1947-1949`
  where it was written against vitest ≤3.)* `test.workspace` was **removed in
  Vitest 4** and throws: *"The `test.workspace` option was removed in Vitest 4.
  Please, migrate to `test.projects` instead."* It fails loud rather than silent, so
  this is a spec precision fix and not a live hazard — but the brief is the spec.
  M5's recommendation, adopted: **re-scope M6 onto `test.projects`; do not pull the
  jsdom migration into MR-C.** Start from the discharged 76/42 split above.
- **If the first `-race` run on `controller` is RED, STOP.** File the races as their
  own issue and land `-race` behind `allow_failure: true` **with a dated expiry
  note** rather than leaving a permanent advisory job (Decision 3). Note this is the
  one sanctioned exception to Unit A's ruling 4, and it is Decision 3's, not a
  licence to reach for `allow_failure` elsewhere.
- **Calibration**: confirm the coverage number MOVES when a test is deleted, and
  that the GitLab regex picks it up on the MR. A coverage job reporting nothing
  looks identical to one reporting a stable number.

---

## Hard constraints — both units

These are not style preferences; each one is a measured failure somewhere in
`CLAUDE.md` or the carry-forward doc.

1. **`Taskfile.yml`**: no `sources:`/`generates:`/`status:`, no `dotenv:`, no
   `includes:`, no dynamic root `vars:`, **no unquoted variable spliced into a
   `cmds:` line** (branch names legally contain `;`, `$` and backticks, and the MR
   author picks the target branch). npm targets **delegate** to a `package.json`
   script — a target that reimplements the command drops that script's flags
   silently.
2. **ANY new CI job must be added to BOTH `.gate_needs` and `.publish_needs`**, and
   gets **its own `cache:` prefix** if it builds something the jobs sharing that key
   do not. Verify list membership by parsing, never by grepping.

   *(Amendment 1, 2026-08-04: this read "a new **lint-stage** CI job", which
   NARROWED both its sources — carry-forward 2 says "any new CI job",
   `.gitlab-ci.yml:292` says "a new gate job". That is carry-forward 16(d)'s
   widening trap running in the other direction, and it bites **Unit B
   specifically**: M6 is the milestone most likely to want a coverage job, which is
   not lint-stage. The precise version survives at both original sites; the narrow
   one was at the top of this brief, where a reader hits it first.)*
3. **Prefer a pinned `go run pkg@version` / `go install pkg@version` inside the
   already-pinned `golang:1.26`** over a new image family. Do not add dev tooling
   to root `devbox.json` — that file is tier-2 *worker* config.
4. **Never a bare `npx <tool>`** — it fetches from the network when the dep is
   missing, which a gate may not do.
5. **The CI-skip marker in a commit message skips the pipeline**, quoted or not,
   including in a merge tip. Check with
   `git log -1 --format=%B | grep -c -F '[skip ci]'` before pushing a tip you need
   a pipeline for. A `skipped` pipeline is not `failed` — the MR still reads
   mergeable, because `allow_merge_on_skipped_pipeline: true` is set here.
6. **Commit with a pathspec** — `git commit -- <paths>`. Staging by path is
   necessary and not sufficient: `git commit` takes the whole index, and in a
   shared worktree another agent's staged work is in it (carry-forward 20, where
   exactly this swept a third agent's source file into a docs commit).
7. **Restore a mutation with a `cp`-based backup, never `git checkout --`** — it
   reverts to HEAD and silently wipes uncommitted work. A broken restore reddens
   loudly and reads as proof.
8. **`grep` here is ugrep**: use `-F` for literals, `git grep` when the question is
   about tracked content. `$?` after a pipe reads the last command — redirect to a
   file, read `$?` on the next line, then grep the file. zsh is
   `${pipestatus[1]}`, one-indexed, **not** bash's `${PIPESTATUS[0]}`, which
   expands to nothing here.

## Calibration — Success Criterion 8, which is the PRD's whole point

**Every check either unit adds ships with the mutation that proves it is live**,
recorded in the MR description: the check reddens on a deliberate violation and
greens on its removal. A PRD whose subject is quality gates must not ship a gate
whose liveness is unverified.

The bar is **four properties, not one** (carry-forward 5):

1. **Non-zero exit.** `task` returns **201** on any failure, never the underlying
   code. Test for non-zero, never a number. *(And 109 means the Taskfile did not
   parse and nothing ran.)*
2. **The rule or tool NAME in the output.** rc≠0 alone is satisfiable by an
   unrelated finding.
3. **A sane, repo-root-relative path.** A `../` in a finding path is an invalid run,
   not a finding.
4. **Green on restore, verified with `git status`** — not a grep count. A count
   only means something if you already know how many occurrences ought to exist;
   the VCS does not require you to know that.

**A control that produces no output is not a control**, and the frequency is the
point: carry-forward 17 records **four** non-controls in a single round, by a
careful agent, in a milestone about gate liveness — all four failing toward
"looks fine". Assert that the control **observed something specific**.

## Reporting discipline (carry-forward 9)

Three milestones' worth of validator reports have been long, correct, and expensive.
For these two units:

- **Lead with your tip SHA.** Verify THAT SHA, never `HEAD` — `HEAD` moves under you.
- Findings as a **short ranked list**.
- **Every measurement written to a file inside the worktree**, with the report
  naming the path. The evidence must exist; it must not all arrive in a message.
- **No inline transcripts** the lead can read for itself.
- **Label every claim `measured` or `suspected`.** M5's handover did this and it is
  the format worth copying — half of it was suspected, and shipping those as facts
  is the failure this PRD is about.

---

## Amendment 1 — 2026-08-04, from the fact-checker's design-wave pass

**Landed while the architect, reviewer and auditor were still reading `848cf53d`.**
Their reports are pinned to that SHA and are **EARLY, not stale**: correct at the
state they read, and correct forever at that state. Reconcile them against this
amendment; do not re-dispatch them for it. *(That distinction is the skill's own and
the two have different fixes — stale means re-pin what you verify, early means
re-read, not re-run.)*

Every claim I labelled `measured` was **re-derived independently** and every one
reproduced: govulncheck 0/0/2 and 0/0/1; web 8/1/2/5/0 and agent 5/0/2/3/0 (with a
`cmp` distinctness control aimed squarely at my instrument-failure #1); all five
high-and-above rows exact; 118 files / 1660 tests; 76/42 from two separate
enumerations. The text-output-hides-the-critical claim reproduced exactly.

What moved, in order of what a wrong answer would have cost:

| | section | change |
|---|---|---|
| **R1** | jsdom coverage | **REFUTED.** All 42 pragma-less files are node-side; the missing-pragma population does not exist. Unit B's classification step is discharged, not deferred. |
| **N1** | Unit A ruling 1 | `--audit-level=high` was in the PRD and M5 and **absent here**. Without it the gate is red on day one after every planned fix, because five moderates survive. |
| **N2** | Hard constraint 2 | Widened back to **any** new CI job. The narrow "lint-stage" form would have missed M6's coverage job. |
| **A1** | Unit B jsdom | `test.workspace` is **removed** in vitest 4 and throws; the route is `test.projects` alone. |
| **N3** | Unit A findings | govulncheck's exit split is four-way (0/1/2/3), not three; and the `-format json` fail-open has a source-level proof rather than an observation. |
| **N5** | Roster, web-ux | Closure reason understated Unit A's blast radius. Closure itself unchanged. |
| **N6** | instrument failures | Failure 1 reproduced three more times, by a second party, within hours. Plus the `git grep -e` leading-dash trap. |

**One transit change reviewed and KEPT**: Hard constraint 3 extended carry-forward 6's
`go run pkg@version` to `go run … / go install …`. That is a deliberate widening,
justified by N3 — `go run` cannot preserve the exit codes the gate needs — and the
source's rationale (pin the version, leave `go.mod` untouched, stay inside the
already-pinned image) carries over to `go install` unchanged.

**Nothing in the brief was found unverifiable.**
