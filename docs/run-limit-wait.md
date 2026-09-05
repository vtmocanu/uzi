---
title: Paused on a usage limit
order: 38
audience: user
---

# Paused on a usage limit

When a run hits your Anthropic 5-hour or 7-day usage cap partway through, uzi can pause it instead of failing it outright, and pick it back up on its own once the window resets — no re-running by hand.

## On by default

Every new run waits out a usage limit unless you turn it off, in two places:

- **Settings → Anthropic usage limits** — *"Pause my new runs on a usage limit instead of failing them"* sets the default for every new run, and is the **only** way to change it for an autopilot, CI-fix, or self-improvement run: none of those has a start button of its own.
- **The run view** — *"Wait out future Anthropic usage limits on this run"* overrides the default for one run, live, for as long as it's still going.

Starting a run by hand stays one click and inherits the Settings default; there's no checkbox at start. The API accepts the flag per run for scripted callers.

**The run-view toggle does not stop a paused run** — it only decides what happens the *next* time this run hits a limit, and leaves an already-paused run exactly as paused. To actually stop one, **cancel** it, the same way as any other run: that works right away, without waiting for the window to reset.

## What you'll see

- **Board card**: a **limit wait** badge.
- **Run view**: a warn-toned strip reading *"Paused on an Anthropic usage limit"*, with a countdown to when it resumes, which window it hit (5-hour, 7-day, …), and — from the second pause on — *"attempt N"*.
- **Activity feed**: *"Anthropic usage limit reached — paused until it resets"*, with the window and reset time alongside it.
- **Slack** (if you've linked a Slack account): the run's DM thread gets a
  `⏸️ Paused · usage limit` reply when it parks, and a `▶️ Resumed · usage
  limit cleared` reply the first time it's working again — the resume
  carrying how long it waited and, from the second pause on, the pause
  count. If it comes back straight into the plan gate, a question, or a
  finished state, that reply carries the news and no separate resume reply
  is posted. (A Slack *edit* raises no notification, which is why the resume
  is a fresh reply, not the root line quietly flipping back to running.)

The run's status stays visibly "waiting" the whole time — never "stalled" — and it is never killed by its own run timeout while paused.

## What happens automatically

Once the window resets, the run usually resumes on the same worker, in the same session, keeping its branch and its history — including any edits it hadn't committed yet when it paused, which come back exactly as they were and never show up as their own commit in the eventual merge request. If you had already approved its plan before the pause, it goes straight back to work instead of asking you to approve the plan again. In the uncommon case it has to come back on a different worker, it recovers your work whenever it cleanly can and resumes just the same. An already-approved run asks you to approve a fresh plan again only on a total loss — when it comes back and can recover neither its committed progress nor those uncommitted edits; if it kept its committed progress but not the last uncommitted edits, it resumes without re-approving.

That's the trade: a parked run keeps holding its issue and its worker's disk for as long as it waits, which is the price of never losing work to a mid-run limit.

The pause itself also updates that credential's rate-limit meter right away, so it reads as exhausted for anyone else's claim too — see [Claude rate limits](rate-limits.md) for what that looks like. And the resuming claim goes further: it skips the credential that just paused it even when the meter alone wouldn't have ruled it out, which is what keeps an `auto` worker from immediately picking the very token that just refused it.

A run can pause and resume more than once if the limit keeps recurring, backing off between attempts, up to a cap; a second cap bounds how far out any single pause may reach. Both are operator-configured — see [Configuration](configuration.md) to change them.

## If a run isn't waiting out limits

A run that isn't set up to wait still fails the moment it hits a limit, the same as before — but the failure now says why, instead of a bare error: *"Anthropic usage limit (5-hour) reached; resets at 2026-07-28T02:00:00Z"*. Re-run it once that time passes, or turn on waiting so next time it doesn't have to.

## Alert when the 7-day window resets early

Anthropic's 5-hour window resets like clockwork, but the 7-day (weekly) window
sometimes reopens *earlier* than the reset time it originally advertised. uzi's
usage poller already tracks that expected reset time per token, so it notices
when this happens: if a token's weekly window comes back more than 8 hours
before its previously-recorded reset, uzi can tell you right away instead of
you finding out only when the nominal time finally arrives.

- **On by default** — the same **Settings → Anthropic usage limits** card as
  the pause toggle above has its own checkbox, *"Alert me when my 7-day limit
  resets early"*. It's independent of whether waiting on limits is turned on.
- **A Slack DM, loud on purpose** — a `🚨 7-DAY RATE LIMIT RESET EARLY` message
  naming the expected reset time, when it was actually observed, and how many
  hours you got back. It needs a linked Slack account to reach you (see
  [Slack notifications](slack.md)); with no Slack linked, the alert is still
  recorded as a notification in your inbox, just not DMed.
- **The 8-hour threshold is fixed**, not a setting you can tune.
- **It only tells you** — it does not resume a paused run early or otherwise
  act on your behalf. A run parked with [wait on limit](#on-by-default) still
  resumes on the schedule described above; this alert just lets you know
  sooner that the account itself has room again.

## Not the same as waiting for a pooled token

A **`limit_wait`** pause (this page) and a **`pool_wait`** hold are both non-terminal waits, but for different reasons with different resolutions. `limit_wait` means a token you were actually spending hit its Anthropic rate limit, and it clears when that window resets. `pool_wait` means an `auto`-lane worker's token pool was genuinely empty — there was nothing to spend at all — and it clears when you opt a token into the pool, or on demand with `uzi run resume-now`. See [Letting uzi pick the token (auto-selection)](anthropic-token.md#letting-uzi-pick-the-token-auto-selection) for the pooled-token wait.

Related: [Claude rate limits](rate-limits.md) · [Anthropic tokens](anthropic-token.md) · [Run health](run-health.md) · [Configuration](configuration.md) · [Slack notifications](slack.md)
