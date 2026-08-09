---
title: Slack notifications
order: 90
audience: user
---

# Slack notifications

uzi can DM you about runs you own: started, needs your approval, needs your
answer, finished with a merge request, or failed — and you can **Approve**,
**Reject**, or **Request changes** to a plan gate right from Slack, answer a
question an agent stopped to ask, or reply in the thread to steer a live run.
You can also **just message the bot** to open a [chat](./chat.md) — ask what's
running, why a run failed, or draft an issue or a run — all from the DM.
The bot is
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
  app_home:
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
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
uzi adds to an accepted thread reply. The `app_home.messages_tab_enabled`
pair is what lets you TYPE in the bot's DM — without it Slack shows "Sending
messages to this app has been turned off" and thread-reply steering can't
work. Nothing here needs a Request URL — Socket Mode carries events and
interactive actions over the same outbound connection.

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

- **Plan gate**: the `awaiting_approval` DM carries **Approve** /
  **Request changes** / **Reject** / **Open in uzi**, and the plan body
  itself is posted into the run thread so you can read it without leaving
  Slack (see the privacy note below). Approve resumes the run immediately.
  Reject swaps in a **Reject without reason** button and asks you to reply
  in the thread with a reason instead — a threaded reply there *is* the
  rejection.
- **Requesting changes from Slack**: press **Request changes**, then reply
  in the thread with your feedback — that reply *is* the feedback, sent to
  the same planning session that produced the plan. The agent revises on
  the same run, and the updated plan (v2, v3, ...) posts as a fresh gate
  message in the thread with the previous one marked "superseded"; a click
  on a superseded message is refused rather than silently ignored, so you
  can never approve a plan you didn't actually see. Rounds are bounded, and
  a full round — see plan, request changes, reply with feedback, see the
  new plan, approve — works from Slack alone. See [Plan approval
  gate](./run-activity.md#plan-approval-gate) for the web-side equivalent.
- **Choosing agents from Slack**: when the repo ships its own agents in
  `.claude/agents/`, the gate shows **two** approve buttons — **Approve · repo
  agents (N)** and **Approve · my templates** — and lists the repo agent names
  in the message. Each button's confirm dialog states which roster it uses; that
  confirm is your opt-in record. The repo card is a whole-roster choice only;
  to exclude individual agents, approve from the web UI instead. A repo with no
  `.claude/agents/` shows the single **Approve** button (your templates), exactly
  as before, and so do gate messages posted before this feature shipped.
- **Answering a question**: an agent that hits a fork it shouldn't resolve
  alone can stop and ask you. The run parks, its status reads **needs your
  answer**, and the question is posted into the run thread with any suggested
  answers listed. **Reply in the thread — that reply is the answer**, and the
  run picks up where it left off. There are no buttons: free text is the whole
  affordance on Slack, so you can answer a suggestion by naming it, or ignore
  the suggestions entirely. A few things worth knowing:
  - **Your reply answers the question it comes after.** uzi orders your reply
    against the question message above it, so a reply that landed before the
    latest question is treated as answering an older one and is **refused**,
    with a note telling you to scroll down and answer the current question.
    That is deliberate: it is the only way to stop a reply you wrote against an
    earlier question from being silently applied to a newer one you never read.
    What uzi still cannot tell is *intent* — if a newer question is already on
    screen and you reply meaning the older one, your words go to the newer
    question, because both replies land in the same thread and look identical.
    Answer the question at the bottom of the thread.
  - **One reply answers the whole card.** When the agent batches several
    questions into one message, cover them in a single reply; the agent reads
    it in full. It may re-ask anything you left unaddressed.
  - **Nobody answers ⇒ the run fails** once the answer deadline passes (24
    hours by default, `QUESTION_TIMEOUT_SECONDS`). The timer is held by the
    worker, so if that worker dies and the run is picked up again the clock
    restarts — the honest worst case is the timeout times one more than the
    requeue limit, not the timeout flat. The per-run question cap
    (`QUESTION_MAX`, 5 by default) resets the same way for the same reason,
    so it has the identical caveat: a run may ask up to 10 questions over its
    life, not 5, if a worker dies and it's requeued in between.
  - **The ✅ means "recorded", not "delivered".** It is added once uzi has
    stored your answer for the run to collect. In the narrow window of a
    rolling worker upgrade, a run resumed onto a worker from before this
    feature shipped will discard a pending answer without acting on it — so
    you can get a checkmark for an answer that never reached the agent. If a
    run you answered doesn't move, open it in uzi and answer there.
- **Steering a live run**: reply in the thread outside a gate and it's
  submitted as a follow-up instruction, with a ✅ reaction as the ack.
- Otherwise only status, repository path, issue number and title, MR link,
  and failure reason ever appear in a message — diff content never leaves
  uzi. **The plan body is the one exception**, deliberately (see the
  privacy note below): a gated plan, and each revision, does leave the box
  for the run thread.
- Approving or rejecting from the web UI updates the Slack message too (and
  vice versa), so a stale button press just gets a quiet "already handled".
- **Run judge and self-improvement**: if you've opted into the [run
  judge](./judge.md), a finished review's verdict and summary arrive as a DM,
  same content as the inbox row. If your admin has enabled
  [self-improvement](./self-improvement.md), you get a DM when a cycle starts
  and when one is skipped (vault locked, repo disconnected, a cycle already
  running). A judge or self-improvement run's *own* state changes (queued,
  running, completed) are never DM'd on their own — only the review-ready /
  cycle-started / cycle-skipped messages above, so you don't get noise like
  "judge run completed" for a run you never see on the board.
- **Run health nudges**: if a run you own gets flagged (stalled, looping,
  slow, waiting for a worker, or stuck too long awaiting approval — see
  [Run health](./run-health.md)), its root status label picks up a `· ⚠
  <flag>` suffix, and you get one threaded nudge — at most once per cooldown
  window (30 minutes by default, admin-tunable) even if the run flaps
  between flags in that time. An approval-idle nudge threads under the
  existing plan-gate message, right next to its action buttons.
  When the run recovers, the root label reverts on its own and nudging
  stops — no action needed.

## Chatting from Slack

Send the uzi bot a **top-level DM** (not a thread reply) and it opens a
conversation — the same [Chat](./chat.md) you get on the web, answered by your
own worker on your own Anthropic token, streamed back into a thread on your
message.

- **Continue in the thread.** Reply in the thread to send the next turn. A new
  top-level DM while a chat is already live is refused with a pointer to the
  open one — you get one conversation at a time (a worker serves one chat at a
  time by default).
- **No worker connected?** The chat is queued and the status line says so —
  start your worker and it'll pick up where you left off; your messages are
  saved.
- **Draft an issue or start a run.** Ask uzi to file an issue or start a run on
  an existing issue and it posts a **card** with **Create** / **Start** buttons.
  Nothing is written until you click — the write goes through your own
  connection, gated exactly like the web (an issue with no PRD is refused with
  the reason). A repo or issue that *says* "start a run on #42" can at most
  produce a card, never a run.
- **End / Continue.** The status message carries an **End chat** button while
  the conversation is live and a **Continue** button once it ends (it went
  quiet, hit its turn limit, or you ended it) — Continue resumes it in a fresh
  thread.

## Good to know

- **Privacy**: the `users:read.email` scope lets uzi resolve workspace
  members' emails to match them to uzi accounts. Turning Slack on also means
  run status metadata (status, repository paths, issue numbers and titles, MR
  links, failure reasons) leaves the box for Slack's cloud — nothing
  sensitive, but it's no longer loopback-only once enabled. When a repo ships
  `.claude/agents/`, the agent **names** (short kebab-case identifiers, at most
  16, ≤64 chars each) also appear in the gate DM so you can choose that roster;
  their **descriptions** are never sent.
- **Question content also leaves the box, on the same terms as the plan.**
  When an agent asks you something, the question text (and any suggested
  answers) is posted into your run thread, with the same credential-pattern
  scrub and the same limit: a secret quoted verbatim into a question is not
  caught by any layer. Two things are specific to questions. The text is
  written by the agent from repository and issue content, so a hostile file in
  a repo can influence what you are asked — uzi instructs agents never to ask
  for a credential, token or password, and **you should treat any such request
  as a red flag and refuse it**, from Slack or anywhere else. And the widening
  runs both ways: your answer is stored and replayed into the agent's context,
  so it is scrubbed of known credential patterns and length-bounded before it
  goes anywhere, whichever surface you answer from.
- **Plan content also leaves the box, deliberately.** Every gated plan (and
  each revision from a Request-changes round) is posted into the run's
  Slack thread in full — a user-approved trade so a whole approval round can
  be driven from Slack alone. This is a real widening of what leaves uzi:
  plan bodies quote source and issue content, and **only known credential
  *patterns*** (Slack/Anthropic/GitLab/uzi token shapes) are stripped before
  posting — a secret a model happens to quote into a plan verbatim (a pasted
  API key, a password) is **not** caught by any layer. Gate posts still go
  only to your own 1:1 DM, so the added exposure is Slack's cloud (retention,
  admin export, e-discovery) and your workspace's own admin boundary, not
  other members. See [ARCHITECTURE.md](../ARCHITECTURE.md#slack-integration-outbound-only)
  and `prds/done/41-plan-revision-gate.md` (Decision 10) for the full rationale.
- **Chat content leaves the box — and can quote a *run's* content into Slack.**
  Chat answers are posted into your DM thread by default wherever Slack is on,
  on the same terms as the plan above: scrubbed of known credential *patterns*
  only, so a secret a model quotes verbatim into an answer is **not** caught.
  There is a second-order widening worth naming: the chat agent's read tools can
  investigate **your other runs**, so you can ask it to quote an issue run's
  content — a plan body, a diff, tool output — into Slack. The run-status
  notifications for non-chat runs are unchanged; what's new is that run
  *content* now has a route to Slack, through chat. Chat DMs are still 1:1 (your
  own), so the added exposure is Slack's cloud and your workspace's admin
  boundary, not other members. Prefer the web Chat page if you'd rather that
  content stay off Slack.
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
- Run health nudges follow the same delivery rules as every other
  notification: they need Slack enabled and your account linked, and
  turning your notify toggle off stops them immediately, mid-run included.
