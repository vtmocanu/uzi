---
title: Slack notifications
order: 90
audience: user
---

# Slack notifications

uzi can DM you about runs you own: started, needs your approval, finished with
a merge request, or failed — and you can **Approve** or **Reject** a plan
gate right from Slack, or reply in the thread to steer a live run. The bot is
**outbound-only** (Socket Mode): it opens a connection out to Slack, so there's
no public URL or inbound port to expose. See
[ARCHITECTURE.md](../ARCHITECTURE.md#slack-integration-outbound-only) for the
trust model.

## 1. Create the Slack app

Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New App** →
**From a manifest**, pick your workspace, and paste this:

```yaml
display_information:
  name: uzi
  description: Run notifications and plan-approval buttons from uzi
features:
  bot_user:
    display_name: uzi
    always_online: true
oauth_config:
  scopes:
    bot:
      - chat:write
      - im:write
      - im:history
      - users:read
      - users:read.email
      - reactions:write
settings:
  event_subscriptions:
    bot_events:
      - message.im
  interactivity:
    is_enabled: true
  socket_mode_enabled: true
  org_deploy_enabled: false
  token_rotation_enabled: false
```

`users:read.email` is what lets uzi match a workspace member to a uzi account
by email (see the privacy note below); `reactions:write` is only for the ✅
uzi adds to an accepted thread reply. Nothing here needs a Request URL —
Socket Mode carries events and interactive actions over the same outbound
connection.

## 2. Mint the two tokens

- **App-level token**: app **Settings → Basic Information → App-Level
  Tokens** → **Generate Token and Scopes**, add scope `connections:write`,
  generate. This is the `xapp-…` token.
- **Bot token**: **OAuth & Permissions** → **Install to Workspace**, approve,
  then copy the **Bot User OAuth Token** (`xoxb-…`) from the same page.

## 3. Configure uzi

Three ways, pick one:

- **Env overlay**: set `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, and
  `UZI_PUBLIC_BASE_URL` (the base URL Slack message links point at) on the
  `api` service. These win over anything saved in the UI and grey out the
  corresponding fields.
- **Startup seed**: set `UZI_SEED_SLACK_BOT_TOKEN`, `UZI_SEED_SLACK_APP_TOKEN`
  (and optionally `UZI_SEED_PUBLIC_BASE_URL`) instead. On first boot they are
  stored encrypted and Slack is enabled automatically; afterwards the admin UI
  stays fully editable and later `.env` edits are ignored — see
  [configuration.md](./configuration.md). Don't combine a seed var with its
  overlay counterpart (the API refuses to boot).
- **Admin UI**: as an admin, open **Admin → Instance settings → Slack**:

1. Paste the bot token and app-level token; uzi validates each against Slack
   (`auth.test` for the bot token, a Socket Mode handshake for the app token)
   before storing it, and never shows either again.
2. Set **Public base URL** if the default `http://127.0.0.1:8080` won't
   resolve for your teammates (a Tailscale/LAN URL works).
3. Check **Enable Slack notifications** and save. The status chip moves
   `disabled → connecting → connected` within a few seconds.

**A field set from the environment renders disabled** ("set from
environment") and a write to it is rejected server-side — the greying is
enforced policy, not just a hint.

## 4. Link your own account

Open **Settings → Notifications**:

- uzi first tries to match you by your account email against the workspace.
  On a match, you get a one-time **Confirm / Not me** DM — notifications
  start only after you press **Confirm**.
- If the email match misses (or you'd rather not rely on it), paste your
  **Slack member ID** (Slack profile → **⋯** → **Copy member ID**) into the
  override field and save; this also (re-)sends the Confirm DM to that
  target.
- **Send test DM** checks the link end to end without waiting for a real run.
- The notify toggle is a per-user kill switch, on by default; turn it off to
  stop DMs without clearing the link.

## Using it

- **Plan gate**: the `awaiting_approval` DM carries **Approve** / **Reject** /
  **Open in uzi**. Approve resumes the run immediately. Reject swaps in a
  **Reject without reason** button and asks you to reply in the thread with a
  reason instead — a threaded reply there *is* the rejection.
- **Steering a live run**: reply in the thread outside a gate and it's
  submitted as a follow-up instruction, with a ✅ reaction as the ack.
- Only status, repository path, issue number and title, MR link, and failure
  reason ever appear in a message — never the plan body or a diff; the plan
  is one click away behind the deep link.
- Approving or rejecting from the web UI updates the Slack message too (and
  vice versa), so a stale button press just gets a quiet "already handled".

## Good to know

- **Privacy**: the `users:read.email` scope lets uzi resolve workspace
  members' emails to match them to uzi accounts. Turning Slack on also means
  run status metadata (status, repository paths, issue numbers and titles, MR
  links, failure reasons) leaves the box for Slack's cloud — nothing
  sensitive, but it's no longer loopback-only once enabled.
- **Rotating a token from Settings**: uzi hot-reloads a changed token within
  one settings poll (about 5 seconds) and tears down the old socket — there's
  a brief window where the previous connection can still be live.
- **DM rate limits**: the account-link override and test-DM actions are
  throttled per user (`SLACK_DM_RATE_LIMIT_MAX`/`_WINDOW`,
  [configuration.md](./configuration.md)) plus a 30-second cooldown per Slack
  target, so repeatedly hammering either can't be used to spam an arbitrary
  workspace member.
- Slack outages or a dropped socket never affect a run — notifications are
  best-effort and the web UI stays the source of truth throughout.
