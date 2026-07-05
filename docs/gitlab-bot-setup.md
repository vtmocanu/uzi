---
title: GitLab bot setup
order: 20
audience: user
---

# GitLab bot setup

uzi acts on the forge as **your own bot account**, never your personal identity: a revocable, individually-scoped identity instead of one shared credential. See [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration) for why.

## 1. Create the bot account

If self-registration is open on your GitLab instance, register a second account for the bot yourself (e.g. `uzi-bot-<yourname>`). If it's closed, ask an instance admin to create it for you, either by hand or by running `scripts/create-gitlab-bot.sh <bot-username> <group/project>`, which creates the bot, mints its PAT, and adds it as Developer in one shot.

## 2. Create a personal access token

If you provisioned the bot yourself, create its token from the bot account's **Settings → Access Tokens**:

- **Scope: `api`**, not `read_api` (moving a card in uzi writes labels, and `read_api` is read-only).
- **Expiry**: as short as your workflow tolerates. uzi has no expiry-warning UI, so put a reminder on your calendar to rotate before it lapses.

## 3. Add the bot to your project

Project → **Manage → Members → Invite members**, add the bot with role **Developer** (not Reporter, not Maintainer). Developer is a hard requirement: uzi's project discovery only sees projects where the bot has at least Developer access.

![Adding the bot account as a Developer member of a GitLab project](img/gitlab-bot-setup-enable.png)

## 4. Connect the bot in uzi

1. Log in and open **Settings → Forge**.
2. Pick a base URL: only the operator-configured allowlist is offered (the SSRF guard; see [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration)).
3. Paste the bot's PAT and submit. uzi verifies it immediately and shows the bot's username; the token itself is never shown again.

![The Forge page, pasting and verifying a bot PAT](img/gitlab-bot-setup-connect.png)

## 5. Enable the repo

Open **Boards**, pick the connection, and enable each project you added the bot to. This makes its board appear in the sidebar and starts syncing its `PRD`-labeled issues (see [Board](./board.md)).

If verification fails, check: the PAT's scope is `api`, it hasn't expired, and the bot is at least Developer on the target project.

## Protect your main branch

If you'll run agents against a project, protect its default branch (**Settings → Repository → Protected branches**): allow only Maintainer+ to merge or push. Because the bot is Developer-only, it can open a merge request but can never merge or push there itself: the platform-enforced half of uzi's ["an agent can only ever open an MR" guarantee](../ARCHITECTURE.md#guardrail-layers-the-primary-directive).

For scripting these steps with `glab` and the E2E test bot convention, see [Developer conventions](./dev-conventions.md).
