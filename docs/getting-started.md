---
title: Getting started
order: 10
audience: user
---

# Getting started

The golden path from a fresh account to a running board, in five steps.

## 1. Register

Open the app and register. The first account created becomes admin
automatically; every account after that is a regular user.

![Registration form, the first step to a working uzi account](img/getting-started-register.png)

## 2. Create a bot and connect a forge

uzi acts on your behalf through a bot account, never your own identity.
Create one and connect its token: see [GitLab bot setup](./gitlab-bot-setup.md) or
[Forgejo bot setup](./forgejo-bot-setup.md), whichever forge you use.

## 3. Open a board

Once a repo is enabled, its board appears in the sidebar, showing the
repo's open issues; the `uzi`-labeled ones are the runnable ones. Dragging
a card between columns relabels the issue on the forge. See [Board](./board.md).

## 4. Add your Anthropic token

Agents run with your own Anthropic credential. Paste it once in Settings:
see [Anthropic token](./anthropic-token.md).

## 5. Run an agent

Register a worker and start a run from a board card: see
[Worker setup](./worker-setup.md).

## Along the way

- [Agent templates](./agent-templates.md): the roles (`coder`, `reviewer`,
  ...) your agents play, and how an admin edits them.
- [Admin settings](./admin-settings.md): the two forge labels an admin can
  reconfigure instance-wide.
- [Autopilot](./autopilot.md): skip the plan-approval step and let a labeled
  issue run end to end unattended, on your own opt-in.
- [uzi CLI](./cli.md): drive uzi from the terminal or from an agent/CI job
  instead of the browser.
