---
title: Developer conventions
audience: contributor
---

# Developer conventions

Two conventions split out of the user-facing [GitLab bot setup](./gitlab-bot-setup.md)
page: they're for people scripting or testing uzi itself, not for someone
setting up their own bot through the UI.

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
