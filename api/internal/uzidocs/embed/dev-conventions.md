---
title: Developer conventions
audience: contributor
---

# Developer conventions

Conventions for people scripting or testing uzi itself, rather than setting up
their own bot through the UI. The `glab` and E2E-bot sections below were split
out of the user-facing [GitLab bot setup](./gitlab-bot-setup.md) page.

## The quality gate

**`Taskfile.yml` at the repo root is the single source of truth for every gate
recipe.** No command line for a check is written anywhere else: `CLAUDE.md`, the
agent-team docs and the CI jobs all name targets from it. `task --list`
enumerates them.

Install the runner first:

```sh
go install github.com/go-task/task/v3/cmd/task@v3.51.1
```

Pinned to the same version CI installs, verified through Go's checksum database,
and it needs only the Go toolchain this repo already requires. `brew install
go-task` also works but is unpinned and will drift from CI. Deliberately **not**
in `devbox.json`: that file is tier-2 worker configuration whose `packages` array
gets provisioned into opted-in agent runs, not a contributor environment.

`go install` builds from source, so your binary is **not** byte-identical to the
release tarball CI fetches and sha256-verifies. Matching `task --version` is the
equivalence check, not a matching hash. This is the same trust model the repo
already uses for `sqlc@v1.31.1`.

```sh
task gate              # everything, serially: gate:repo first, then all four components
task gate:repo         # repo-wide checks with no component: shell, YAML, the brew formula
task gate:api          # one component: fmt-check + vet + build + lint + deadcode + test
task gate:controller   # same shape
task gate:web          # deps-check + lint + deadcode + check-docs + typecheck + test
task gate:agent        # deps-check + lint + deadcode + typecheck + test
task fmt-check         # the format slot alone, both Go modules
task lint              # the lint slot alone, all seven: four components plus shell/YAML/formula
task deadcode          # the dead-code slot alone, all four components
```

*(The three `gate:*` comments above were written before PRD #103 M3 and M4 and
listed neither `lint` nor `deadcode`. Corrected 2026-08-02 with M4; the slots
themselves are described further down this page. A further correction,
2026-08-03 with M5: `task gate` gained a fifth step, `gate:repo`, run first —
three repo-wide checks that belong to no component, described below.)*

Individual slots exist too (`task test:api`, `task typecheck:web`,
`task check-docs:web`, …), and `.github/workflows/ci.yml` calls those fine-grained targets
rather than the component wrappers, because `validate:*` and `test:*` are separate
jobs and a wrapper would run the tests twice.

Four things about it that are decisions rather than accidents:

- **The load-bearing flags live in the targets, with their reason beside them.**
  `-race` and `-count=1` on `test:api` and (since PRD #103 M6) on `test:controller`,
  `--test-timeout=120000` inside `agent/package.json`'s `test` script. None is
  optional and each one's absence is invisible in a passing run, which is why
  the Taskfile is not `silent:` — Task echoes every command, so you can read the
  flags in the output instead of trusting a file.
- **No `sources:`, `generates:` or `status:` on any target.** A fingerprinted
  target prints `Task "x" is up to date`, exits 0 and runs nothing, which is
  indistinguishable from a pass. uzi's gates deliberately read fixtures from
  outside the module under test, so a checksum over one module cannot see them
  change — the same blind spot `-count=1` exists to close.
- **Components run serially.** Concurrency is a measured flake source here
  (`web/vite.config.ts` raised its `testTimeout` for exactly that), and
  interleaved output makes the named failing test unreadable.
- **`task` exits 201 on any failure**, not the underlying command's code. Test
  for non-zero; never for a specific number.

Not everything is a target. A single-test run, `sqlc generate`, the compose
stack, `./e2e/run-e2e.sh` and `./scripts/smoke.sh` are not gate recipes and stay
written out as commands in `CLAUDE.md` and the `.claude/rules/` files it indexes.

The Taskfile installs no *project* dependencies — no `npm ci`, no
`go mod download`. Your `node_modules` and module cache are expected to exist; CI
does that in its `before_script`.

**One exception since PRD #103 M3, and it is why that sentence is now qualified:**
`task lint:api` and `task lint:controller` acquire golangci-lint through
`scripts/golangci-lint.sh v2.12.2` (PRD #230 M5) — a curl + sha256-verified
release binary, cached under `$HOME/.cache`. It replaced a
`go run …golangci-lint@v2.12.2` that **compiled** the tool from source on a cold
cache (**51.6s**, the pipeline's cold long pole); the release binary is a ~1-2s
download instead. It is version-pinned and writes nothing to either `go.mod`, so
it changes no dependency of yours — the first run fetches once, which is expected,
not a hang.

**🔴 BEFORE YOUR FIRST `task gate:web` OR `task gate:agent` AFTER PULLING M3 OR
M4, YOU MUST INSTALL IN BOTH npm PACKAGES.** M3 added oxlint and M4 added knip,
both as devDependencies in both packages, and the lint and dead-code steps fail
closed with `oxlint: command not found` / `knip: command not found` until they are
present — in a package you may not have touched. Run it in **both** `web/` and
`agent/`:

```sh
npm install --ignore-scripts
```

**`--ignore-scripts` is not optional in `agent/`.** That package depends on
`agent-browser`, whose `postinstall` rewrites `/opt/homebrew/bin/agent-browser` to
point inside whatever `node_modules` just installed it, breaking the CLI host-wide
for every other session and every other worktree — `.claude/rules/agent.md` documents the
breakage and the `brew unlink` / `brew link --overwrite` repair. The flag is
already settled for this repo with its own measurements (`agent/src/js-deps.ts`,
PRD #121), including that **a repo `.npmrc` setting `ignore-scripts=false` does not
override the CLI flag**. The cost, stated honestly: it also skips *other* packages'
legitimate build steps, so if something later fails for want of a native binary,
reinstall that package normally or symlink `node_modules` from a sibling worktree.

**The format check is `task fmt-check`** (`gofmt -l` over both Go modules, added
by PRD #103 M2). It is a composite, and it is the per-module `fmt-check:api` and
`fmt-check:controller` that run first inside `task gate:api` and
`task gate:controller`, and first in CI's `validate:api` and `validate:controller`
— they go first because they cost fractions of a second and a misformat should
surface before the `-race` compile. The composite is fail-fast like every other
composed target here: with drift in both modules it stops at the api half rather
than reporting both.

It fails on any drift and prints the offending files, module-relative
(`internal/…`, not `api/internal/…`, because the targets carry `dir:`). Three
things about the recipe are deliberate and easy to undo by accident. It assigns
`gofmt -l`'s output to a variable rather than testing it inline, because the
inline form both swallows the filenames and goes **green** on a Go file that does
not parse. It carries an explicit `|| exit 2` on that assignment, so the
fail-closed behaviour lives in the line rather than in Task's errexit shell —
`2` because it reproduces gofmt's own status, which keeps a parse failure
(`exit status 2`) distinguishable from a misformat (`exit status 1`) where
`task`'s own exit code is 201 for both.
And it is named `fmt-check` rather than `fmt` because nothing in the gate may be
a fixing variant. All three reasons are written beside the recipe.

There **is** a linter, as of PRD #103 M3: `task lint` runs all four components,
and each `task gate:<component>` runs its own. Go (`api`, `controller`) is
golangci-lint with `errcheck`, `staticcheck`, `ineffassign`, `unused`,
`unparam` and `nolintlint`; `web` and `agent` are oxlint, whose configuration
promotes `react-hooks/rules-of-hooks` explicitly because it is a `pedantic` rule
that the `correctness` tier cannot reach.

`nolintlint` lints the **suppressions** rather than the code. Without it a bare
`//nolint` silences every other linter on that line with no warning and exit 0,
so write `//nolint:errcheck // <why>`: specific, and with a reason. It is the Go
counterpart of the npm half's
`--report-unused-disable-directives-severity=error`, which has been there since
M3 — the two lint halves now guard their own escape hatches symmetrically.

The Go half is **ratcheted** and the npm half is not, which is the one thing to
know before your first red. `.golangci.yml` carries
`issues: {new-from-merge-base: origin/main, whole-files: true}`, so only findings
your branch introduces block — and `whole-files` means a pre-existing finding in a
file you merely touched blocks too. That is deliberate, and it is the cost of
adopting a linter on a codebase with a backlog. `task lint:api:all` and
`task lint:controller:all` print that backlog unfiltered; they are reported, never
gating, and are not part of `task gate`. The npm half needed no ratchet because
its debt was 16 findings, all fixed in the same milestone.

In worker runner clones the clone setup now advances `origin/main` to the real
default-branch head before any gate runs (issue #262), so `new-from-merge-base`
gates only branch-introduced findings rather than a stale backlog — without it the
clone's `origin/main` inherited the bare's frozen mirror and the ratchet
false-red the whole pre-existing backlog. On a resume leg, though, the value
#262 advances to can itself be that frozen mirror — a stale commit strictly
behind the branch's real base — so issue #313 clamps it further: whenever the
resolved default-branch commit is a strict ancestor of the branch base
`baseSha`, the runner clone's `origin/main` is set to `baseSha` instead, so
the ratchet base can never regress below the branch's true fork point; this
only removes false positives, and `.golangci.yml` is unchanged. If a Go lint
target exits with
`origin/main is unresolvable`, run `git fetch origin main`; that guard is
unchanged and still fires when the ref genuinely does not resolve. The guard
exists because without the ref golangci-lint does not skip the ratchet — it
reports the entire backlog behind a single warning line, which reads as a large
new regression.

There **is** a dead-code check, as of PRD #103 M4: `task deadcode` runs all four
components, and each `task gate:<component>` runs its own. Go (`api`,
`controller`) is `golang.org/x/tools/cmd/deadcode -test ./...`, invoked through
`scripts/deadcode-gate.sh`; `web` and `agent` are knip. *(This paragraph read
"There is still no dead-code check and no coverage signal" until M4 landed. The
coverage half is still true and is M6's.)*

**The two halves gate differently, and that is the thing to know before you read
a green.** The Go half holds both modules at **zero**: the baselines
(`api/.deadcode-baseline`, `controller/.deadcode-baseline`) are committed and
**empty**, so any unreachable function reddens the gate. The routine fix is to
delete the function — M4 itself deleted the one finding that existed
(`HookManager.SettingsPath`) rather than baselining it. Adding a line to a
baseline is a deliberate suppression and owes a reason in a comment beside it;
the script treats an entry that is no longer reported as a failure, so a
suppression cannot outlive the finding it covered.

The npm half is **staged by severity** instead. Unused files, dependencies,
unlisted imports, binaries, unresolved imports and duplicate exports are `error`
and gate at zero. The **unused-export family is `warn`: printed in full on every
run and setting no exit code** — 22 findings on `web` and 53 on `agent` as of
2026-08-02. So a green `task deadcode:web` means no *gating* tier fired, not
"no unused exports". Burning that tier down and promoting it to `error` is
tracked as issue #206; `--max-issues` is not a stopgap for it, because it counts
error-severity issues only.

**Neither tool sees a dead *branch*.** `deadcode` finds unreachable functions and
knip finds unused exports, files and dependencies; a `case` arm that nothing
reaches inside a live function is invisible to both. The known instance is the
legacy `"Task"` switch case in `web/src/components/RunEvent.tsx`. Dead branches
stay a review question, which is why the reviewer role keeps a deletion lens
rather than deferring to the slot.

Two companion targets, `task deadcode:api:all` and `task deadcode:controller:all`,
drop `-test` and print what the gating invocation cannot see: a function whose
only remaining caller is a test (43 and 4 respectively, re-derived 2026-08-03 at
`1076b133`). Unlike
`task lint:api:all`, **they always exit 0** — deadcode has no failure status of
its own, so read their output rather than their exit code.

There is still no coverage signal. That is PRD #103's M6, and a target for it
arrives with the check itself rather than as an empty stub.

There are also three repo-wide checks with no component, as of PRD #103 M5:
`task lint:shell` (shellcheck), `task lint:yaml` (yamllint `--strict`) and
`task lint:formula` (`ruby -c` on `Formula/uzi-cli.rb`, which release CI copies
verbatim into the shared Homebrew tap on every tag). `task gate:repo` composes
the three and runs **first** inside `task gate`, cheap-first, ahead of any Go
module's `-race` compile or npm step — none of the three needs a build or a
warm toolchain. Like the other repo-wide slots, their scope comes from the git
index rather than a hand-maintained list: shellcheck's is `git ls-files
'*.sh'` **unioned with a shebang scan**, because the extension alone missed
`agent/bin/agent-browser` — a `#!/bin/sh` shim with no `.sh` suffix, COPYd into
every worker image — and yamllint's is every tracked `*.yml`/`*.yaml` except
`deploy/chart/templates/` (Helm templates are Go templates, not YAML).

**Tool absent is a loud skip at exit 0, locally; tool present at the wrong
version is a hard `exit 2`, unconditionally.** `gate:repo` runs first inside
`task gate`, so a hard failure on a tool you have not installed would block
every other component gate from running at all — the same argument that
already governs `lint:formula`'s ruby fallback above. So a missing shellcheck,
yamllint or ruby prints a banner naming what to install and exits 0. CI must
not be able to take that skip: it sets `UZI_LINT_SHELL_REQUIRED`,
`UZI_LINT_YAML_REQUIRED` and `UZI_LINT_FORMULA_REQUIRED` on the `task
gate:repo` script line itself (never in a job `variables:` block — GitLab
ranks pipeline- and project-level variables above a job's own, so a
same-named manual-pipeline variable would silently displace it), plus a `CI`
fallback GitLab sets on its own, so the skip is unreachable there.

**The version gradient this produces is perverse-looking and is the ruled
behaviour anyway: a contributor who `brew upgrade`s shellcheck is worse off
than one who never installed it.** `lint:shell` pins the version **exactly**
(0.11.0 today) and exits 2 on any other one — present, older, or newer — with
no fallback. That is deliberate: an *older* shellcheck is **blind** (0.10.0
does not emit SC3067 at all, so this repo's three per-instance disables in
`agent/templates/entrypoint.sh` would be suppressions for a diagnostic that
never fires, and the gate would go green by the tool's blindness rather than
by the code being right), where a *newer* one is merely **loud**. When brew
moves past the pin, the remedy is one line: bump the version literal in both
`Taskfile.yml`'s `lint:shell` target and `.github/workflows/ci.yml`'s `lint-repo` job —
the duplication is self-checking, so bumping only one makes the other's assert
exit 2 naming both numbers — then re-derive the finding count under the new
version and say so in the commit. yamllint and ruby carry no such pin in the
wrapper scripts (their two candidate versions were measured to agree on this
tree), though `lint:repo`'s CI job does assert its own apt-installed versions
of both, symmetrically with shellcheck's, so a Debian point release that moves
either is something the pipeline states rather than something a reader has to
infer.

**`--severity=warning` cannot see an unquoted expansion.** SC2086 is `info`
severity, so the shipped threshold structurally excludes that whole class —
including in `agent/templates/entrypoint.sh`, the worker container entrypoint
that runs in every hosted worker pod. "shellcheck now gates the worker
entrypoint" does not mean injection is covered. The tree has zero SC2086
findings today; tightening the threshold is a deliberate follow-up rather than
a flag flip, because the rest of that tier is 11 pre-existing findings
(6× `SC2016`, 2× `SC2001`, 1× `SC2329`) that are benign or intentional, and
tightening would surface all of them at once.

There is also a secret scanner, as of PRD #103 M5 MR-B: `task scan:secrets`
(gitleaks), inside `gate:repo` alongside the shell/YAML/formula checks above.
Unlike those three it has **no skip branch** — gitleaks arrives through the
same mandatory Go toolchain `gate:api` already `go run`s two pinned modules
through, so anyone who cannot obtain it cannot run `gate:api` either.

**It is wrapped in a canary, and the canary is the control.** `.gitleaks.toml`
is auto-discovered from the scan root, so it is an ordinary tracked file a
contributor could silently widen in the same commit that adds a secret — a
scanner you can switch off in the commit that needs it scanned is worse than
no scanner, because it reports green. `scripts/scan-secrets.sh` plants two
known tokens (`scripts/gitleaks-canary.txt` and
`api/internal/config/gitleaks_canary_test.go`, in two different regions on
purpose — a narrowing rule scoped to one does not touch the other) and exits 2
unless **both** are reported. A clean run that does not print "canaries
DETECTED" is not a clean run; it is unproven.

**Gating scope is the git index, not the walk.** gitleaks itself is handed the
whole tree — it silently widens to `.` the moment it is given more than one
target, so a file list cannot be passed to it directly — and the wrapper
filters the *report* against `git ls-files` afterward. Findings in **tracked**
files gate (exit 1). Findings in **untracked** or gitignored files (gitleaks
does not honour `.gitignore`) print under a `NOTE — N finding(s) … NOT
GATING` banner, capped at 10, and exit 0: your verdict must not differ from
CI's, and CI never sees an untracked file. Two consequences worth knowing
before you rely on either: a secret that is `git add`-ed but never committed
**gates before the commit exists** (staged is enough — this is where a local
scanner earns its keep), and a secret inside a **git submodule** is classified
untracked — only the gitlink is indexed — and does not gate (none exist in
this repo today; `inspiration/` was unvendored 2026-08-03).

**🔴 `task gate` in an agent worktree will print an untracked NOTE naming
`.entire/…/full.jsonl` — that is the harness's own session transcript, not a
finding.** It is gitignored (so `git status` shows nothing) and gitleaks scans
it anyway, because it does not honour `.gitignore`. Every agent session that
runs `task gate` here will hit this; a reviewer reading a teammate's gate log
must not read that line as a finding, and it is not something to fix — gating
on it is exactly what the untracked-does-not-gate rule above forbids.

Existing secrets in test fixtures are suppressed per-instance with
`//gitleaks:allow` and a written reason (13 today, all load-bearing) — never a
`.gitleaks.toml [allowlist] paths` regex, which would silently exempt every
file the pattern matches rather than the one line that needs it.

### Dependency vulnerabilities: `task vulncheck`, and why it is NOT in `task gate`

Added by PRD #103 M5 MR-C. `govulncheck` for the two Go modules, `npm audit` for
the two npm packages:

```sh
task vulncheck             # all four
task vulncheck:api         # govulncheck, api module
task vulncheck:controller  # govulncheck, controller module
task vulncheck:web         # npm audit --audit-level=high, FULL tree
task vulncheck:agent       # npm audit --audit-level=high, FULL tree
```

**This is the one target family `task gate` deliberately does not reach**, and it
must stay out of `gate` *and* out of every `gate:<component>`, including any sixth
component a future milestone adds.

**The reason is the VERDICT, not the network, and the network version of the
argument is measurably false.** `task gate` is not offline: `lint:api`,
`deadcode:api` and `scan:secrets` are all `go run pkg@version` and all three need
the network on a cold module cache — control: `task scan:secrets` under
`GOPROXY=off` returns 201 with *"module lookup disabled by GOPROXY=off"*. What
separates those from these two is that a pinned `go run` fetches a
checksum-verified artifact **once** and then answers **from the tree**, whereas
these query a **remote mutable database on every run**. A contributor's gate must
be deterministic against the tree, and these two can answer differently on two runs
of one commit with nobody's diff in between. Writing the offline version of this
reason down would invite someone to "fix" gitleaks out of `gate:repo` on the same
logic.

They do run on every MR, as extra script lines of the existing per-toolchain jobs
(`lint:api`, `lint:controller`, `validate:web`, `validate:agent`) rather than as new
jobs. All four of those jobs are in `*publish_needs`, so **the release-blocking
property is inherited**: a CVE published on a Tuesday can redden a `v*` tag publish
with nobody's diff in it. That is accepted rather than overlooked.

**npm audit gates at `--audit-level=high`, not at zero**, because two moderate
`react-router` advisories survive every available option — no patched 6.x exists,
`--force` will not touch them, `overrides` has nothing to point at — so clearing
them is a React Router 6 → 7 major through shipped SPA routing code. Filed as its
own issue rather than left as an unexplained threshold.

### 🔴 The rule this milestone earned: a gate script names the environment variables that can shrink its view, and refuses them

Every gate in this repo shares one assumption that nobody had examined: **that the
gate's environment is trusted.** Three tools turned out to have a variable that
narrows what the tool looks at while leaving the output shaped exactly like a clean
run. They were found separately, by three different probes, at three different
times, by two different people — which is the argument for writing the rule down
rather than the three guards:

| tool | variable | what it does | where it is refused |
|---|---|---|---|
| gitleaks | `GITLEAKS_CONFIG`, `GITLEAKS_CONFIG_TOML` | substitutes a config that can allowlist anything | `scripts/scan-secrets.sh:153` |
| npm audit | `NPM_CONFIG_OMIT=dev` | drops the dev tree — `rc=0`, *"found 0 vulnerabilities"* | `scripts/npm-audit-gate.sh`, via an asserted `--include=dev` |
| govulncheck | `GOPACKAGESDRIVER` | a 3-line stub answers for the whole package graph | `scripts/govulncheck-gate.sh` (the `GOPACKAGESDRIVER` refusal) |
| govulncheck | `GOFLAGS=-tags=…` | build-tags the vulnerable call out of the build | `scripts/govulncheck-gate.sh` (the `GOFLAGS`/`-tags` refusal, via `go env GOFLAGS`) |

Two details in that table are the interesting part.

**`GOFLAGS` gets the NARROW treatment — `-tags` only, never the variable
wholesale** — because `GOFLAGS=-buildvcs=false` is a documented workflow here (a
linked worktree's `.git` pointer file trips Go's VCS stamping). A blanket refusal
would break a real workflow to close a narrow hole.

**`--include=dev` is ASSERTED, not merely passed**, and that resolves a genuine
conflict between two of this repo's own rules. The Taskfile header requires npm
targets to *delegate* to a `package.json` script, because a target that reimplements
the command drops that script's flags silently — but this particular flag must not
be droppable. So the flag lives in `package.json` and
`scripts/npm-audit-gate.sh:122` exits 2 if it is not there. Delegation plus an
assertion is strictly stronger than putting the flag on the Taskfile line, where
nothing would guard it.

**npm audit has no in-band canary**, unlike gitleaks: a disarmed run and a clean run
are indistinguishable from their own output. That is why the guard is a refusal
rather than a detection.

One path neither refusal closes, recorded because it is a known limit rather than an
oversight: `go install` inside the wrapper inherits `GOPROXY`/`GOSUMDB`, so a hostile
proxy could serve a trojaned `govulncheck` that always exits 0. That is the identical
trust model to every `go run pkg@version` already in this repo, so it is not a new
weakness — but it is not closed either.

**The next gate this repo adds will have a fourth variable nobody has met yet.** The
rule, not the table, is the durable artifact.

### `task deps-check:web` / `deps-check:agent`: is `node_modules` stale?

Runs **first** in `gate:web` and `gate:agent`, ahead of the cheaper lint step, and
that ordering is deliberate. A stale `node_modules` does not make the later slots
*wrong*, it makes them **answers about a different tree**: lint, knip, `tsc` and
vitest would all run, all pass, and all report on the versions that happen to be on
disk rather than the ones the branch declares.

**It is two checks, because the first covers one of six dependency changes.**
`npm ls --depth=0` compares against **declared ranges**, so a *transitive* bump —
which has no declared range at all — is invisible to it. Measured on the stale state
a real `git pull` produces: `gate:agent` was green on a tree holding every one of the
five vulnerable versions MR-C bumps, because all five are transitive and
`agent/package.json` never changed. The second line is a lockfile join, which catches
that. So `npm ls` catches a stale **direct** dependency and is inert for a
lockfile-only transitive bump — do not describe it as a lockfile-versus-`node_modules`
check.

It is offline by construction and never a bare `npx`, which would fetch from the
network when the dep is missing.

### `web` is pinned to vitest `4.1.10`, exactly

MR-C took `web` across the vitest 2 → 4 major. The pin is **exact, not a range**, and
so is `@vitest/coverage-v8` — it is an exact-version optional peer of vitest, so the
two must move together or the install breaks.

The control for the upgrade was a **predicted count written down before the run**:
118 test files / 1660 tests, reproduced after. A green with a lower collected count is
a silently narrowed suite and is invisible to the exit code. Re-derive that pair at
your own tip before quoting it; the suite grows.

### Coverage: measured, printed, and deliberately not a gate

`task test:api` and `task test:controller` write `coverage.out` in their module and
print the statement total; `task test:web` runs vitest with the v8 provider and prints
its summary block. All three are gitignored artifacts — nothing is committed.

**No threshold, on purpose.** Nothing in these targets can fail because of the number.
A threshold picked before the current value was known is either below it (vacuous) or
above it (blocks every MR on unrelated work); the follow-up chooses one, starting with
the security-critical packages rather than a global floor.

Four things to know before you quote a figure:

- **The Go totals exclude packages with no test files of their own.** Without
  `-coverpkg`, each package is measured only by its own tests and an untested package
  is absent from the profile rather than counted as zero, so the total is an
  over-estimate of module-wide coverage.
- **The single percentage GitLab shows on an MR is an unweighted mean across the three
  jobs**, not a repo-wide number — `coverage_array.sum / coverage_array.size` in
  GitLab's own pipeline model. It weights a 437-block module the same as an
  11920-block one. Read the per-job numbers.
- **`scripts/coverage-total.sh` refuses to summarise a profile that measured nothing.**
  A run in which no test executed leaves a one-line profile, and `go tool cover -func`
  reports `total: (statements) 0.0%` for it with rc=0 — a number that reads like
  terrible coverage and is an absence of data. That guard is an instrument check, not a
  threshold: it never compares the percentage to anything.
- **The retired GitLab pipeline's `coverage:` regexes could not be anchored with `^`** (GitHub Actions has no `coverage:` equivalent). Task runs
  with `output: prefixed`, so the totals reach the log as `[test:api] total: …`. They
  also cannot contain a capturing group — GitLab requires all groups to be
  non-capturing and extracts the number from the matched text itself. Both constraints
  are counter-intuitive, both fail silently, and both are recorded with their
  measurements in that file's `.coverage_notes`.

### `web` test environments: `test.projects`, not a docblock per file

DOM tests in `web/` opted into jsdom one file at a time with
`// @vitest-environment jsdom`. A file that forgot it ran under node — usually a loud
`ReferenceError`, but not reliably: `src/lib/prefs.ts` guards every access with
`typeof window === "undefined"`, so a pragma-less test of it passes while touching no
DOM at all.

`web/vite.config.ts` now splits the suite into two projects: **`src/lib` and
`src/mocks` run under node**, and **everything else under `src` runs under jsdom**,
including any directory added later. Two consequences:

- a new test under `src/components` or `src/pages` gets jsdom with no docblock;
- a DOM test under `src/lib` or `src/mocks` still needs its docblock, because those
  two directories hold every node-side test in the suite.

**The docblock still outranks the project's `environment`** — measured on vitest 4.1.10
with both controls, not carried over from the vitest 2 result it happens to agree with.
That is what makes the split safe despite both node directories containing
pragma-carrying files: 14 of them run under jsdom today and still do. The check that
proves it is a per-file census of what all 118 files actually run under, taken before
and after and byte-identical, rather than a count or a classification.

## Contextual doc links: the DocLink registry

`web/src/lib/doclinks.ts` is the single source of truth for every SPA link into
the in-app docs (`/docs/:slug`). It exports bare-slug `DOC_*` constants (e.g.
`DOC_ADMIN_SETTINGS = "admin-settings"`) plus `ALL_DOC_SLUGS`, the list of all
of them.

UI surfaces link out via `<DocLink slug={DOC_...}>guide title</DocLink>`
(`web/src/components/DocLink.tsx`), which renders a react-router `Link` to
`/docs/${slug}`. Never hand-write a `/docs/…` route string and never link out
to a GitHub-blob URL for a `docs/*.md` file — both bypass the registry and
the in-app renderer. The link text is the guide's title, never "click here";
navigation stays same-tab (no `target="_blank"`).

Every registry slug must name an `audience: user` doc. `web/src/lib/doclinks.test.ts`
enforces this by cross-checking each slug against `listUserDocs()`, with a
non-empty-corpus guard so the check can't pass vacuously — renaming or
de-`user`-ing a linked doc reddens `npm test` before it can ship a soft-404
(PRD #57).

When you add a new user-facing setup/management/admin surface, add its
guide's slug to the registry (and to `ALL_DOC_SLUGS`) and drop a `DocLink`
into the card's always-visible intro, not only into an error state.

## Scripting the bot setup with `glab`

The UI steps in [GitLab bot setup](./gitlab-bot-setup.md) have `glab`
equivalents, useful for automation:

```sh
# gitlab.example.com quirk: an exported GITLAB_TOKEN takes precedence over
# glab's own stored credentials and 401s against this host — always run glab
# with it unset for this instance.
env -u GITLAB_TOKEN glab api --hostname gitlab.example.com user
```

Adding the bot as a Developer member of a project:

```sh
env -u GITLAB_TOKEN glab api --hostname gitlab.example.com \
  "projects/group%2Fsubgroup%2Fproject/members" -X POST \
  --raw-field "user_id=<bot-user-id>" \
  --field "access_level=30"
```

(`access_level=30` is GitLab's numeric code for Developer; the project path
must be URL-encoded, `/` → `%2F`, when used as the `:id`.) `scripts/create-gitlab-bot.sh`
already wraps both calls for the common case — reach for these directly only
when scripting something the helper script doesn't cover.

## E2E test bot

Some of uzi's forge tests exercise a real GitLab instance rather than a mock
(the `httptest`-based unit tests in `api/internal/forge` and the
`fakeForge`-mock-based ones in `api/internal/forgesvc` don't need this, only
a live, end-to-end run does).
The convention for supplying that bot's credentials is three variables in
your gitignored `.env`, **never read by the application itself** (grep
`api/internal/config/config.go` — they are not among `Config`'s fields):

```sh
# E2E-only: a real GitLab bot PAT for tests that hit gitlab.example.com for
# real. Never read by the api binary — test-harness use only.
UZI_E2E_BOT_PAT=
UZI_E2E_BOT_USERNAME=
UZI_E2E_PROJECT=
```

- `UZI_E2E_BOT_PAT` — an `api`-scoped PAT for a bot set up exactly as in
  [GitLab bot setup](./gitlab-bot-setup.md), dedicated to testing (don't
  reuse your personal connection's bot).
- `UZI_E2E_BOT_USERNAME` — that bot's username, for assertions that check
  the identity `VerifyToken` returns.
- `UZI_E2E_PROJECT` — a scratch project (path or numeric id) the bot is a
  Developer on, safe for tests to create/label/move issues in.

As of this milestone no test in the repo reads these yet; this section
exists to fix the convention ahead of that work so a future E2E suite
doesn't invent a second naming scheme. When it lands, it should skip (not
fail) when these are unset, the same way `scripts/smoke.sh` requires an
already-running stack rather than assuming one.

## The mock/demo build

`web/` can build itself entirely against in-browser fake data
(`src/mocks/`) instead of a real API — no backend, no compose stack. This
is what `web/Dockerfile.mock` produces: `docker build -f web/Dockerfile.mock
-t uzi-ux-mock .` (context is the repo root) gives a static, backend-free
image, with `web/nginx.mock.conf` 404ing any stray `/api/` call as a
tripwire rather than silently proxying it anywhere.

**`npm run dev` alone does NOT reach mock mode.** The switch is
`VITE_UZI_MOCK=1`, read once at build time (`web/src/lib/api.ts`) to swap in
`src/mocks/mockApi.ts` and `MockRunSocket` for the real `api`/socket — so a
mock bundle contains no code path to a live backend at all, not just a
disabled one. There's no separate demo `npm` script for this; run
`VITE_UZI_MOCK=1 npm run dev` directly (Vite reads the var the same way at
dev-server start), or use the Dockerfile above for a full static build.

**Demo scenarios**, once in mock mode: `?mock=<name>` on the URL, or the
`uzi_mock_scenario` `localStorage` key for a sticky choice across reloads
(`src/mocks/mockApi.ts`'s `mockScenario()`). It's a single string, so
scenarios are mutually exclusive by construction. Known values:

- `oidc`, `oidc-degraded`, `sso-only` — the PRD #45 OIDC UX, otherwise
  unreachable in the demo (OIDC off, password on, by default).
- `truncated-backlog` — `/judge?mock=truncated-backlog` puts the
  [Judge menu](./judge-menu.md) over its row cap (`MOCK_BACKLOG_MAX_ROWS`,
  the demo's small stand-in for the server's real `JudgeBacklogMaxRows`).
  This doesn't just make the truncation banner reachable, it **reproduces
  the under-count the banner warns about**: the same recurring
  recommendation reads "seen in 3 runs" without the toggle and "seen in 1
  run" with it, while the tab counts (which are exempt from the cap) stay
  truthful throughout — so the screen's own inconsistency between a group's
  count and the tallies above it is visible in one view, not just asserted
  in a warning banner. (PRD #98.)

### What keeps the mock build current

Nothing forces a new feature into the demo. What exists is a small stack of
mechanical checks, each narrower than it sounds — worded here exactly so
"enforced" isn't read as "the demo is good":

- **Endpoint parity — type system.** `web/src/lib/api.ts` exports
  `api: typeof realApi`, so `mockApi` is type-checked against the real
  client: a new or changed *endpoint* fails `tsc` (in `task gate:web`)
  until it is mocked. This forces a mock *implementation* to exist, not a
  realistic *scenario* — it says nothing about whether the mocked data
  looks like production. Oldest and strongest layer.
- **The image builds — CI (PRD #311 M1).** The `build:web-mock` job runs
  `VITE_UZI_MOCK=1 npm run build` against `web/Dockerfile.mock` on
  `web/**`/`docs/**` merge requests and on protected refs, `--no-push`
  with no artifact. So a broken fixture or a mock-only import error can
  no longer silently stop the demo image from building — it only proves
  the build succeeds, not that the result is a good demo.
- **Every route mounts without throwing — a test (PRD #311 M2).**
  `web/src/App.routes.test.tsx` forces mock mode and mounts every route
  in the `App.tsx` `APP_ROUTES` table. The router and the test consume
  the SAME table, so a route added *to the table* is smoke-tested
  automatically. (The honest limit: a bare `<Route>` written directly in
  `App.tsx` outside the table — as the `*` catch-all is — would bypass it,
  so keeping routes in the table is the convention that makes the guard
  hold.) It asserts only that each page renders without throwing on the
  mock fixtures — not that the page is correct or complete.
- **The issue #195 divergence invariant — a test on named fixtures
  (PRD #311 M3).** `web/src/mocks/data.realism.test.ts` pins that the
  result frames the cost/usage surfaces actually read
  (`mockDoneMessages`, `mockFailedMessages` in `src/mocks/data.ts`, and
  `PLAN_RESULT_FRAME` / `RUN_RESULT_FRAME` in `src/mocks/engine.ts`)
  carry more than one model key and a `modelUsage` that diverges from
  the frame's top-level `usage`, in the real under-reading direction.
  It guards only those named fixtures, and only that one invariant —
  it is not a general realism check.

What none of the above gates: whether a new user-facing feature shows up
in the demo at all. There is no mechanical signal for "this is a new
feature that needs a scenario," so the standing convention is: **a new
user-facing feature must add or extend a mock scenario so its state is
reachable in the demo, not merely covered by a unit test** —
`truncated-backlog` above is the worked example. Likewise "the mock data
is realistic" in general is still a convention to uphold by hand, not a
gate to rely on: the PRD #311 M3 guard covers one specific invariant on
five named fixtures, nothing broader.
