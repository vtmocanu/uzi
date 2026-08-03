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
already uses for `sqlc@v1.30.0`.

```sh
task gate              # everything, serially
task gate:api          # one component: fmt-check + vet + build + lint + deadcode + test
task gate:controller   # same shape
task gate:web          # lint + deadcode + check-docs + typecheck + test
task gate:agent        # lint + deadcode + typecheck + test
task fmt-check         # the format slot alone, both Go modules
task lint              # the lint slot alone, all four components
task deadcode          # the dead-code slot alone, all four components
```

*(The three `gate:*` comments above were written before PRD #103 M3 and M4 and
listed neither `lint` nor `deadcode`. Corrected 2026-08-02 with M4; the slots
themselves are described further down this page.)*

Individual slots exist too (`task test:api`, `task typecheck:web`,
`task check-docs:web`, …), and `.gitlab-ci.yml` calls those fine-grained targets
rather than the component wrappers, because `validate:*` and `test:*` are separate
jobs and a wrapper would run the tests twice.

Four things about it that are decisions rather than accidents:

- **The load-bearing flags live in the targets, with their reason beside them.**
  `-race` and `-count=1` on `test:api`, `-count=1` on `test:controller`,
  `--test-timeout=30000` inside `agent/package.json`'s `test` script. None is
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
written out as commands in `CLAUDE.md`.

The Taskfile installs no *project* dependencies — no `npm ci`, no
`go mod download`. Your `node_modules` and module cache are expected to exist; CI
does that in its `before_script`.

**One exception since PRD #103 M3, and it is why that sentence is now qualified:**
`task lint:api` and `task lint:controller` invoke
`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`, which on a
cold cache fetches and builds that tool's dependency tree. Measured at **51.6s
cold, then under 2s warm**. It is pinned and sumdb-verified and writes nothing to
either `go.mod`, so it changes no dependency of yours — but the first run is slow
and that is expected, not a hang.

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
for every other session and every other worktree — `CLAUDE.md` documents the
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
golangci-lint with `errcheck`, `staticcheck`, `ineffassign`, `unused` and
`unparam`; `web` and `agent` are oxlint, whose configuration promotes
`react-hooks/rules-of-hooks` explicitly because it is a `pedantic` rule that the
`correctness` tier cannot reach.

The Go half is **ratcheted** and the npm half is not, which is the one thing to
know before your first red. `.golangci.yml` carries
`issues: {new-from-merge-base: origin/main, whole-files: true}`, so only findings
your branch introduces block — and `whole-files` means a pre-existing finding in a
file you merely touched blocks too. That is deliberate, and it is the cost of
adopting a linter on a codebase with a backlog. `task lint:api:all` and
`task lint:controller:all` print that backlog unfiltered; they are reported, never
gating, and are not part of `task gate`. The npm half needed no ratchet because
its debt was 16 findings, all fixed in the same milestone.

If a Go lint target exits with `origin/main is unresolvable`, run
`git fetch origin main`. The guard exists because without the ref golangci-lint
does not skip the ratchet — it reports the entire backlog behind a single warning
line, which reads as a large new regression.

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
-t uzi-ux-multica .` (context is the repo root) gives a static, backend-free
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
