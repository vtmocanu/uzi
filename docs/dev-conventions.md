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
task gate:api          # one component: vet + build + test
task gate:controller
task gate:web          # check-docs + typecheck + test
task gate:agent
```

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

The Taskfile installs nothing — no `npm ci`, no `go mod download`. Your
`node_modules` and module cache are expected to exist; CI does that in its
`before_script`.

There is no linter, no format check, no dead-code check and no coverage signal
yet. That is PRD #103's remaining milestones, and a target for each arrives with
the check itself rather than as an empty stub.

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
