---
title: Claude rate limits
order: 42
audience: user
---

# Claude rate limits

Anthropic caps every account against two rolling windows — **5-hour** and
**7-day** — and once either is exhausted your runs queue until it resets.
uzi reads both windows for you, server-side, using your own
[Anthropic tokens](./anthropic-token.md), and shows them as live meters.

**A meter is per token, not per account.** Each credential you store is
polled and metered on its own, because that is the unit Anthropic actually
caps — two tokens pointing at two different accounts have two independent
budgets, and a single merged bar would describe neither. If you hold one
token, you see one meter and nothing has changed.

## Where to look

| Surface | What you see |
|---|---|
| **Settings → Claude limits** | A card under your tokens, with one block per stored token — its name, a **default** badge, both windows as bars, current percentage, and a reset countdown. |
| **Sidebar** | Two thin bars per readable token under your signed-in name — a glance without leaving the page you're on. |
| **Admin → Rate limits** | Every user's meters on one page, one row per token, sorted so whoever is closest to a limit shows first. |

Your token names appear next to the bars only when you hold more than one —
with a single credential the surfaces look exactly as they did before.

Both surfaces are hidden entirely until you've saved a token. While uzi is
waiting on its first reading, the sidebar stays hidden (no empty bars to
puzzle over), but the Settings card shows a "No reading yet" placeholder
with two greyed bars — a reading appears within a few minutes of saving.
A token added later shows that placeholder until its first poll, while your
other meters keep reading normally.

## Reading a meter

A bar fills as you approach the limit and shifts color early — the same
green/amber/red language used elsewhere in uzi for "resource nearly
exhausted", but tuned to give you a heads-up well before things get tight.
Amber is your cue to pace yourself or switch to another token; red means
a window is genuinely close to its cap. The **reset countdown** ("resets
in 1h 23m") counts down to when that window clears, independent of the
other one.

## How fresh is this?

uzi polls Anthropic in the background, by default every 5 minutes, once per
stored token. A reading can be a few minutes old — fine for windows measured
in hours and days.

Saving a token also nudges the poller so your **default** meter appears
within seconds rather than at the next tick. A newly added *non-default*
token gets no such nudge: its first reading arrives on the next scheduled
poll, so expect up to one interval of "no reading yet" after adding one.

If your vault is locked, uzi can't open your tokens to poll them: your
last known reading stays on screen but greys out and is marked **stale**
(or explicitly **vault locked** on the admin page) until you unlock again.
Nothing is lost, and nothing renders as a false zero.

## The probe, and turning it off

Anthropic's free usage endpoint doesn't work for every credential type (it
currently refuses `claude setup-token` credentials). When it refuses, uzi
falls back to a minimal Claude request that costs about **1 token** of your
own quota to read the same numbers from the response headers. Worst case,
at the default 5-minute poll interval, that's roughly 300 tokens a day per
affected credential (the probe is per stored token, like the poll it backs
up) — negligible next to normal usage, but an instance
operator who'd rather not spend tokens on polling at all can turn the probe
off (`UZI_USAGE_PROBE=false` — see [Configuration](./configuration.md));
affected accounts then show **no reading yet** instead.

## Admins see everyone

An admin's **Rate limits** page lists every user, including anyone who
hasn't saved a token yet (shown as **no token**) — a capacity view for
planning factory work, not just a personal gauge. A user holding several
credentials gets one row per token, named and default-badged, grouped under
their identity; the sort still keys on whoever is nearest a wall.
