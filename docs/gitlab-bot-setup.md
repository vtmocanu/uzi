# GitLab bot setup

uzi never uses your own GitLab identity to talk to the forge. Each uzi user connects their **own bot account and personal access token (PAT)** in Settings → Forge; uzi stores the PAT encrypted at rest (`UZI_SECRET_KEY`, see [configuration.md](configuration.md)) and acts as that bot for every repo you enable. This gives every user a revocable, individually-scoped identity instead of one shared credential — see [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration) for why.

There are two ways to provision the bot account: the **non-admin path** (register it yourself, like any other GitLab user) and the **admin path** (an instance admin runs `scripts/create-gitlab-bot.sh` for you). Both end at the same place: a bot user with a Developer membership on your project(s) and an `api`-scoped PAT pasted into uzi.

## 1. Create the bot account

### Non-admin path

If self-registration is open on your GitLab instance, register a second account for the bot yourself (e.g. `uzi-bot-<yourname>`, a `+bot`-style email alias if your mail provider supports it). If self-registration is closed (the common case on an internal instance), ask an instance admin to create it for you — either by hand in the GitLab admin UI, or by running `scripts/create-gitlab-bot.sh` (below) on your behalf.

### Admin path: `scripts/create-gitlab-bot.sh`

An instance admin with `glab` configured against the target host can provision everything in one shot:

```sh
./scripts/create-gitlab-bot.sh uzi-bot-vmocanu group/subgroup/project
```

This creates the bot user (skipping if it already exists), mints a fresh `api`-scoped PAT via the [admin PAT API](https://docs.gitlab.com/api/user_tokens/#create-a-personal-access-token-for-a-user), and adds the bot as Developer on the given project. It prints the PAT once to stdout — copy it immediately, GitLab never shows it again. See the script's own header comment for the full usage and requirements (glab authenticated as an instance admin against the target host).

## 2. Create a personal access token

If you provisioned the bot yourself (non-admin path), create its PAT from the bot account's own **Settings → Access Tokens** page (or `glab` — see below):

- **Scope: `api`** — not `read_api`. Moving a card in uzi calls GitLab's issue-update endpoint (`add_labels`/`remove_labels`), which is a write; `read_api` is read-only and every label move would fail with a permission error. `api` is the minimum scope that covers both the read side (listing projects/issues/labels) and the write side (label moves).
- **Expiry**: pick the shortest expiry your workflow tolerates — GitLab instances commonly enforce an admin-configured maximum PAT lifetime (Admin Area → Settings → General → Account limits), so a very long expiry may get silently clamped. Put a reminder on your calendar to rotate before it lapses: uzi has no expiry-warning UI in this MVP, and an expired token just starts failing verification silently until you reconnect.
- Rotating the PAT (letting it expire, or revoking it) means reconnecting in uzi's Settings → Forge — there's no automatic re-verify-and-refresh loop, just a `VerifyToken` call on demand ("Verify" button) or on next connect.

Via `glab` instead of the UI (run as the bot, or as the admin creating it on the bot's behalf — see the script for the admin-API equivalent):

```sh
# gitlab.example.com quirk: an exported GITLAB_TOKEN takes precedence over
# glab's own stored credentials and 401s against this host — always run glab
# with it unset for this instance.
env -u GITLAB_TOKEN glab api --hostname gitlab.example.com user
```

## 3. Add the bot to each project

The bot needs **Developer** access on every project uzi should see — not Reporter, not Maintainer. This is a hard requirement, not just a suggestion: uzi's project-discovery call (`ListProjects` in `api/internal/forge/gitlab.go`) queries GitLab with `min_access_level=Developer`, so a bot with only Reporter access on a project simply won't see it in uzi's Repos page at all, regardless of whether label writes would otherwise work at a lower role.

Via the GitLab UI: project → **Manage → Members → Invite members**, search the bot's username, role **Developer**.

Via `glab` (remember the `GITLAB_TOKEN` quirk above):

```sh
env -u GITLAB_TOKEN glab api --hostname gitlab.example.com \
  "projects/group%2Fsubgroup%2Fproject/members" -X POST \
  --raw-field "user_id=<bot-user-id>" \
  --field "access_level=30"
```

(`access_level=30` is GitLab's numeric code for Developer; the project path must be URL-encoded, `/` → `%2F`, when used as the `:id`.)

## 4. Connect the bot in uzi

1. Log in to uzi and open **Settings → Forge**.
2. Pick a base URL from the dropdown — uzi only offers URLs in the operator-configured allowlist (`FORGE_ALLOWED_BASE_URLS`; defaults to `https://gitlab.example.com`), so free-text forge URLs aren't possible (this is the SSRF guard, see [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration)).
3. Paste the bot's PAT and submit. uzi calls `VerifyToken` against the forge immediately; on success it shows the bot's username and never displays the token again (re-paste to rotate).
4. Open **Repos**, pick the connection, and enable the project(s) you added the bot to. Enabling a repo makes its board (kanban view of `PRD`-labeled issues) appear in the sidebar and starts the background poller for it.

If verification fails, double-check: the PAT scope is `api` (not `read_api`), the token hasn't expired, and the bot is at least Developer on the target project.

## Protected main branch

If you plan to run agents against a project (PRD #4's worker), protect its default branch. This is the **documented half** of uzi's layered primary directive, "an agent can only ever open an MR, never write to `main`" (the other half is enforced in the worker/agent code itself: see [ARCHITECTURE.md](../ARCHITECTURE.md#guardrail-layers-the-primary-directive)); this GitLab-side setting is the outermost, platform-enforced backstop, independent of anything uzi's code does or fails to do.

Via the GitLab UI: project → **Settings → Repository → Protected branches**, protect `main` (or whatever the default branch is) with:

- **Allowed to merge**: Maintainer (or higher), never Developer.
- **Allowed to push and merge**: No one, or Maintainer, never Developer.

Because the bot is only **Developer** (step 3, above), it is structurally unable to push to or merge into a branch protected this way; it can only open a merge request, which a human with Maintainer access reviews and merges. This holds regardless of what the bot's PAT is used for, so it is a real backstop, not just a convention.

**Not yet covered:** verifying the bot PAT's *own* scope is no broader than it needs (today it's `api`, the minimum that covers both reads and label writes) beyond what step 2 already documents; a dedicated least-privilege audit of the PAT is a separate, tracked backlog item (see `plan.md`), not something this setup guide currently checks for you.

## E2E test bot (developers only)

Some of uzi's forge tests exercise a real GitLab instance rather than a mock (the `httptest`-based unit tests in `api/internal/forge` and `api/internal/forgesvc` don't need this — only a live, end-to-end run does). The convention for supplying that bot's credentials is three variables in your gitignored `.env`, **never read by the application itself** (grep `api/internal/config/config.go` — they are not among `Config`'s fields):

```sh
# E2E-only: a real GitLab bot PAT for tests that hit gitlab.example.com for
# real. Never read by the api binary — test-harness use only.
UZI_E2E_BOT_PAT=
UZI_E2E_BOT_USERNAME=
UZI_E2E_PROJECT=
```

- `UZI_E2E_BOT_PAT` — an `api`-scoped PAT for a bot set up exactly as above, dedicated to testing (don't reuse your personal connection's bot).
- `UZI_E2E_BOT_USERNAME` — that bot's username, for assertions that check the identity `VerifyToken` returns.
- `UZI_E2E_PROJECT` — a scratch project (path or numeric id) the bot is a Developer on, safe for tests to create/label/move issues in.

As of this milestone no test in the repo reads these yet; this section exists to fix the convention ahead of that work so a future E2E suite doesn't invent a second naming scheme. When it lands, it should skip (not fail) when these are unset, the same way `scripts/smoke.sh` requires an already-running stack rather than assuming one.
