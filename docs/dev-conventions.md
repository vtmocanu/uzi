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
