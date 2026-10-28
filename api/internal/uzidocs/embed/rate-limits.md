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

That covers a run that hasn't started yet. A run already **in progress**
when it hits one of these windows is a different page —
[Paused on a usage limit](run-limit-wait.md).

**A meter is per token, not per account.** Each credential you store is
polled and metered on its own, because that is the unit Anthropic actually
caps — two tokens pointing at two different accounts have two independent
budgets, and a single merged bar would describe neither. If you hold one
token, you see one meter and nothing has changed.

## Where to look

| Surface | What you see |
|---|---|
| **Settings → Claude limits** | A card under your tokens, with one block per stored token — its name, a **default** badge, both windows as bars, current percentage, and a reset countdown. |
| **Sidebar** | One usage list under your signed-in name: each shown token under a Claude logo and its name, with two thin bars — a glance without leaving the page you're on. Codex accounts follow in the same list (see [Codex account limits](#codex-account-limits)). |
| **Admin → Rate limits** | Every user's meters on one page, one row per token, sorted so whoever is closest to a limit shows first. |

In the sidebar and on the Settings card every token is named by its own label,
even when you hold only one, so adding a second token never renames the first.

A [disabled](./anthropic-token.md#disabling-a-token) Anthropic token has no
meter anywhere: uzi stops checking its usage, and it drops out of the
sidebar, **Admin → Rate limits** and `uzi rate-limits` (which says "no
enabled Anthropic token" when that leaves nothing to show). A Codex meter
belongs to the account, not to one login, so disabling one login of an
account leaves the meter in place while another login of the same account is
still enabled; the meter drops out once every login of that account is
disabled. When you enable a token or login again its meter reads "checking
usage…" until a fresh reading arrives.

The Settings card stays hidden until you've saved a token. While uzi is
waiting on a token's first reading, that token stays out of the sidebar (no
empty bars to puzzle over), but the Settings card shows a "No reading yet"
placeholder with two greyed bars — a reading appears within a few minutes of
saving.
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

That countdown is Anthropic's advertised reset time, and the 7-day window
occasionally clears earlier than it advertised — see [the early-reset Slack
alert](run-limit-wait.md#alert-when-the-7-day-window-resets-early) for how
uzi tells you when that happens.

## Reading the forecast

The bar tells you how much of a window is gone; a lighter **ghost** extending
past the fill tells you where it's *heading*. uzi takes the window's current
reading — how much is used and how long the window has been open — and
projects that pace forward to when the window resets:

- **Nothing** — the common, calm case. At the current rate the window finishes
  with room to spare, so there's nothing to watch. Silence means safe.
- **A gold ghost** — on pace to land right around the cap by the time it resets.
- **A coral ghost** — burning fast enough to overshoot the cap well before the
  window resets. This is the one worth acting on: pace down or switch to another
  token.

A small **`»`** rides the end of the ghost whenever the projection crosses the
cap — always on a coral ghost, and on a gold one when it's heading just past
100%. Read it as a "past the limit" flag.

Hover a forecasting bar to read the projected figure ("projected 106% by
reset"). It's a straight-line estimate from the window's current reading, not
a promise — usage comes in bursts, so treat it as "if this pace holds", not a
countdown.

Because it only needs that one reading, the forecast is there from the
moment a window shows up — including a window that's just sitting idle and
the slow 7-day window, the two cases a trend-based forecast would otherwise
miss. Very early in a fresh 5-hour window (the first fifteen minutes or so)
too little of it has passed to project reliably, so the bar shows the plain
fill until then. The forecast only ever informs you; it never changes which
token a run picks or when.

## How fresh is this?

uzi polls Anthropic in the background, by default every 5 minutes, once per
stored token. A reading can be a few minutes old — fine for windows measured
in hours and days.

Saving a token also nudges the poller so your **default** meter appears
within seconds rather than at the next tick. A newly added *non-default*
token gets no such nudge: its first reading arrives on the next scheduled
poll, so expect up to one interval of "no reading yet" after adding one.

Hitting a limit updates the meter right away too, ahead of the next poll.
When a run pauses on a usage limit (see
[Paused on a usage limit](run-limit-wait.md)), uzi immediately records that
window as 100% consumed for the credential that hit it, so the meter — and
every other claim reading it — reflects reality without waiting out the
interval. That reading carries a **Recorded at usage limit** badge and says
so next to the timestamp, because the timestamp itself still shows the last
poll, not this park.

If your vault is locked, uzi can't open your tokens to poll them: your
last known reading stays on screen but greys out and is marked **stale**
(or explicitly **vault locked** on the admin page) until you unlock again.
Nothing is lost, and nothing renders as a false zero.

## Choosing a token near its limit

Pointing a run at a token yourself — the start-run picker, the plan gate,
`uzi run set-token`/`run create --token`/`run approve --token`, or a
schedule's own picker — warns instead of refusing when the choice looks
likely to run into trouble:

- **Pinning the token that just exhausted this very run** (the one that
  parked it in [`limit_wait`](run-limit-wait.md)) warns that it may re-park
  until that window reopens.
- **Pinning any token whose meter reads exhausted or stale** warns the same
  way — the pin still goes through, but the gauge says the account may have
  no room to spend right now.
- **Choosing auto-select when nothing in the pool is eligible** (once the
  just-exhausted token is excluded) warns that the run will hold in
  [`pool_wait`](anthropic-token.md#waiting-for-a-token) until something
  pools.

None of these three refuses the choice — they tell you what to expect, and
the switch still applies. The CLI prints the warning to stderr; the web shows
it next to the picker. See [choosing the token for one
run](anthropic-token.md#choosing-the-token-for-one-run) for every place you
can make this choice.

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

## Codex account limits

A [Codex subscription login](codex-credentials.md) — not an OpenAI API key —
gets an account-scoped meter alongside your Claude token meters. The shape is
different: Codex reports usage in **named buckets**, each with up to two
windows of whatever duration the provider actually reports, not Claude's
fixed 5-hour/7-day pair. A window's label reads "5-hour", "7-day", or
whatever Codex says it is — uzi never assumes.

**Collection needs no active worker or run.** A dedicated poller in the API
process reads each linked Codex subscription account's usage on its own
schedule (`UZI_CODEX_USAGE_POLL_INTERVAL`, default 5 minutes — see
[Configuration](./configuration.md)), so a meter fills in even while nothing
of yours is running, and it picks up usage the provider reports you incurred
outside uzi entirely.

**Duplicate logins of one account show one budget.** If you've imported the
same Codex account under two saved aliases, they share a single meter —
the quota is a property of the account, not of how many times you saved its
login.

The existing default-on **Settings → Usage limits** option, *"Alert me when my
7-day limit resets early"*, also covers Codex subscription accounts, independently
of waiting on limits. It compares the main bucket's weekly window (604800
seconds), once per account across saved aliases; extra buckets and other window
lengths do not trigger it. A linked, confirmed Slack account with notifications
enabled is required for the DM. See [Codex early-reset alerts](run-limit-wait.md#codex-subscription-accounts)
for the strict timing and percentage thresholds, silent missing-evidence cases,
and best-effort delivery limits. The alert only notifies; it does not resume runs
or switch accounts.

**An OpenAI API key has no subscription windows and is not metered.** Only a
linked Codex subscription login produces a meter; an API-key-only default
shows nothing here, and Settings says so next to the credential itself.

### Temporary account admission waits

Linked Codex subscription accounts normally have unlimited account admission.
A model-reported rate limit temporarily delays new claims on that account.
Overload needs reports from **two distinct runs within five minutes**; three
retries in one run are insufficient. Idle time, silence, generic transport
errors or 5xx responses, usage-quota exhaustion, authentication failures and
a usage-meter GET returning 429 do not trigger this wait.

The wait is shared by your saved aliases of the same verified account,
including after refresh or re-import. Another authorized account remains
independent; uzi does not rotate accounts automatically.

| Evidence | Initial wait | Exponential cap before jitter | Quiet reset |
|---|---|---|---|
| Model rate limit | 30 seconds | 15 minutes | 30 minutes |
| Repeated overload | 15 seconds | 5 minutes | 15 minutes |

Each opening adds persisted positive jitter of 0–20%. The classes keep
independent schedules; the later expiry controls, with rate limit winning a
tie. Qualifying evidence during a class's active wait does not extend its
expiry or advance its step. Quiet-reset clocks follow qualifying unique
observations; a repeated overload vote does not extend its five-minute life.
Recovery adds a stable spread per run, up to the opening wait's duration or
60 seconds, whichever is smaller. A new episode cannot bring eligibility
forward; after the finite recovery window, account admission is unlimited again.

New runs stay **queued** while held. Already claimed subscription runs check
the same canonical-account policy before each supported **new model dispatch**
and wait in place, keeping their flight, session, positive claim generation,
custody, gate presentation and pending feedback. Their lifecycle stays
`claimed`, `running` or `awaiting_approval`. A turn already dispatched continues.
There is no standing account seat limit, active counter, mid-turn pause or
force-park. Claim/provisioning statement snapshots and operation reads can race
concurrent cooldown evidence or credential/source updates; neither is atomic
revocation or an account seat lock.

For queued waits, your run detail, list, board and CLI show the current class
and eligibility time, including recovery spread:

- `waiting: provider reported a rate limit; account admission eligible after <time>`
- `waiting: provider reported overload; account admission eligible after <time>`

Eligibility is **not a promised start time**. The copy clears on a read after
expiry; maintenance and account-unavailability reasons take precedence, with
admission details available independently. Shared views and admin summaries
for another owner omit these fields. There is no per-run Slack burst,
account settings panel or Resume now override for this wait.

For required blocked work in a claimed run, owner surfaces instead say
`waiting for account cooldown`, with `account_cooldown_class` (`rate_limit`
or `overload`) and `account_cooldown_eligible_at`. The absolute earliest time
includes finite per-run spread and is not a promised dispatch time. This is
`waiting_account_cooldown` health, not a lifecycle state or `waiting_worker`.
Optional background advice cannot label an otherwise working root blocked.
The display requires current ownership, positive generation, credential binding
and revision source, plus an accepted active snapshot with a 45-second lease.
Omission, lease/eligibility expiry or a source/status change clears it on an
accepted fresh read. Foreign shared/admin summaries omit account class and time.
Missing, disabled or unsupported optional snapshots (including an old API) mean
specialized visibility is unknown; operation enforcement is independent.
Independent faults, freshness warnings and near-spent wall budgets retain
precedence. See [Run health](run-health.md#account-cooldown-waits).

Each supported new dispatch needs fresh authorization: root planning and initial
implementation, human and automatic revisions, prose/clarification continuations,
classified-provider retries and fresh-session fallbacks; worker-created children;
production advice for intent, plan, Now, delivery/PR description, judge and diff
review; and both plan/code checker stages. An earlier admission is not reused.
Native HTTP retries, tool continuations and possible compaction/internal requests
inside a dispatched turn have no demonstrated worker seam. This does not gate
every upstream request; native `multi_agent` and `multi_agent_v2` stay disabled.
Caller-key evaluators and offline replay have no worker subscription authority.

Waiting charges elapsed wall time or the existing whole-call deadline once,
with the model idle watchdog disarmed and no fabricated progress, processing
or productivity. It neither consumes nor creates an owner recovery grant.
Cancel, reject, replacement revision, pause, wall deadlines and vault/ownership
controls remain effective; Resume cannot bypass admission. Capture/checkpoint,
refresh needed for preservation, tool settlement, persistence and teardown are
not gated. Children wait independently within deadlines that include existing
setup time, without an exemption, extension or parent-held permit; notification
reading and settlement continue without a demultiplexer deadlock. Optional
summaries may time out without dispatch.

**An admission-service outage blocks the next dispatch of every negotiated
subscription flight, including accounts with no known cooldown.** Already
running turns and preservation continue under existing controls, budgets and
deadlines. Never-negotiated flights retain legacy behavior; once negotiated,
feature loss, clear/re-registration, transport errors, 5xx, malformed replies
and an untyped route 404 cannot downgrade a flight to allow or fabricate a
provider failure. See [upgrade and rollback limits](installation.md#codex-account-admission-upgrades)
and the [durable operation boundary](../adr/1106-codex-harness.md#durable-model-operation-boundary-3531-2026-10-28).

Queued maintenance and weekly reserve precedence, credential selection and
worker slots remain independent; waiting can occupy a slot. Usage-window and
account-unavailability parking, OpenAI API keys, Claude, chat, versioned jobs
and isolated-lane eligibility keep their existing policy. There is no new
provider streak, backoff policy, automatic account rotation or Retry-After use.

### Where to look

| Surface | What you see |
|---|---|
| **Settings → Codex limits** | A card next to your Codex credentials, one block per linked subscription account: its aliases, a **default** badge, a **Show in sidebar and TUI** checkmark, every reported bucket with its own windows, percentages and reset countdown. |
| **Sidebar** | Your default subscription account, plus any additional account you've checked, in the same usage list as your Claude tokens (after them), each under an OpenAI logo and its account name. The account's main limit is unlabeled; any additional bucket is captioned with its name. One "+N more accounts in Settings" link counts the hidden Claude tokens and Codex accounts together. |
| **Admin → Rate limits** | A separate Codex section, grouped by user then by account, sorted by utilization. |
| **CLI / TUI** | `uzi rate-limits --provider codex` and `uzi admin rate-limits --provider codex` (see [uzi CLI](./cli.md)); the TUI board strip and detail rail carry the Codex meters too, each account named, behind a single `codex` tag, with window lengths (`5h`, `7d`; `?` when Codex reports no length). |

**Choosing which accounts appear in the sidebar and TUI** works exactly like
the Claude side: your subscription's **default** Codex account always shows,
and the Settings checkmark on each additional linked account adds or removes
it from both the sidebar and the TUI. Nothing here changes polling, which
covers every linked account regardless of what's checked; it only changes
what you see. The same anchored forecast ghost from [Reading the
forecast](#reading-the-forecast) applies per bucket, using that bucket's own
reported window length rather than a hardcoded 5h/7d.

### Statuses

Each linked account carries one of these, derived server-side — never
re-derived by the client:

| Status | Meaning |
|---|---|
| `pending` | Linked, but no poll has completed yet. |
| `no_reading` | A poll was attempted and failed, with nothing successful yet. |
| `fresh` | A successful reading no older than 3× the poll interval. |
| `stale` | A reading exists but has aged past that window — shown greyed rather than dropped. |
| `vault_locked` | Your vault is locked, so uzi can't open the login to poll it; the last known reading stays visible, marked stale. |
| `credential_action_required` | The login needs re-authentication (re-import it) before polling can resume. |
| `polling_disabled` | The operator has turned the Codex poller off entirely (`UZI_CODEX_USAGE_POLL_INTERVAL=0`); any retained reading shows as stale, since nothing will ever refresh it again. |

**Staleness follows the same 3× rule as Claude's meters, with one addition**:
a disabled poller marks every retained reading stale unconditionally, since
there is no next poll to make it fresh again.

A user with **no linked Codex subscription account** — including one whose
only saved Codex credential is an API key — sees no accounts at all here: no
row, no placeholder meter, on any surface. That's the empty-set shape, not a
status of its own.

## Weekly usage reserve

If you share one subscription between uzi and your own work, you can keep part
of the weekly allowance untouched. The **weekly usage reserve** is a per-user
threshold on the **7-day** window: while the credential a run would spend reads
at or above it, uzi starts no new work on that credential and, optionally,
pauses running runs at their next milestone. It is **off by default**; with no
threshold set, nothing in this section applies to you.

### Setting it

| Surface | How |
|---|---|
| **Settings → Run defaults → Usage limits** | The **Keep a weekly usage reserve** switch, a threshold (a whole number from 1 to 100) and the pause-running toggle. |
| **CLI** | `uzi settings reserve --threshold <1..100\|off> [--pause-running=true\|false]`. Omitting `--pause-running` keeps the stored value. `uzi settings get` ends with a `Weekly usage reserve:` line. |
| **API** | `PUT /api/me/usage-reserve` with `{"threshold_pct": <1..100 or null>, "pause_running": <bool>}`. Both fields are required; `null` turns the reserve off. A `uzc_` Bearer token works. |

**Also pause running runs** is on by default once a threshold is set. A change
takes effect on the next claim and the next sweeper pass; it is not
instantaneous. See [CLI](cli.md#weekly-usage-reserve) for the command.

### What is measured

- **Only the 7-day window counts.** The 5-hour window is ignored by the
  reserve; a 5-hour exhaustion is still handled by
  [`limit_wait`](run-limit-wait.md).
- **A credential is held when its 7-day percent is at or above the
  threshold** (`pct >= threshold`, compared on the unrounded reading), so a
  70% threshold holds at exactly 70%.
- **Claude:** the `seven_day` reading of the token the run would spend.
- **Codex:** the account window whose reported duration is exactly 7 days,
  whether it is the primary or the secondary window. Codex buckets carry no
  model, so when several buckets report a weekly window the **highest** one
  decides.

### When the reading is unknown, new work holds

With a threshold set, a reading that cannot be trusted reads as **unknown**
and **holds** new work rather than silently dropping the protection. Unknown
means:

- no reading at all (not polled yet);
- a **stale** reading, older than 3× the poll interval (the same rule as
  [How fresh is this?](#how-fresh-is-this));
- a reading synced **before** a 7-day reset that has since passed, because it
  describes the old week: release waits for the next poll after the reset;
- a **partial** reading, such as a successful poll with no weekly utilization
  (Claude), or a weekly window whose percent is missing or invalid (Codex).

Two consequences to plan for:

- **Console API keys hold.** Every Anthropic credential is treated as metered,
  because OAuth tokens and console API keys cannot be told apart reliably. A
  console API key has no subscription usage to read, so with a threshold set
  it reads unknown and holds new work. If a console key is the only Anthropic
  credential you use, leave the reserve off.
- **A disabled poller holds.** With the usage poller turned off
  (`UZI_USAGE_POLL_INTERVAL=0` for Claude, `UZI_CODEX_USAGE_POLL_INTERVAL=0`
  for Codex), no reading is ever fresh, so every subscription credential reads
  unknown while a threshold is set. This is an operator setting; see
  [Configuration](./configuration.md).

An unknown reading holds **new work only**. It never pauses a run that is
already running, so a poller outage cannot stop every active run.

### What it holds

A held run stays `queued`; it is not a new status and it does not touch the
usage-limit counters or wall budget. A run is held when it would spend a
credential that reads held or unknown:

- **Every worker run kind except chat**: issue, task, prompt, self-improve,
  CI-fix, MR-rework, job, judge and cross-check, whether started by hand, by a
  schedule or as a resume.
- **Chat is never gated.** It is the interactive use the reserve protects.
- **Judge and self-improve** are gated on your judge-lane credential, and a
  **cross-check** on its own credential, which is on the opposite harness from
  the run it checks. A held checker keeps its deadline; if it expires, it
  settles as an expired check does today.
- **Not gated, by structure:** a Codex API-key alias and an OpenAI API key have
  no subscription meter, so they are never held. A missing reading is never
  used to decide this.
- **The auto pool skips held tokens.** A run on [auto
  credential selection](anthropic-token.md) can start on another pooled token
  that reads below the threshold; it never leaves the pool or its pinning
  rules to do so. It holds only when every pooled token is held or unknown.

A previously started run's resume claim is gated only while **Also pause
running runs** is on. With it off, started runs continue above the threshold
and only never-started work stays held.

### What you see

A held queued run shows the reason `weekly usage reserve: 72% >= 70%`, or
`weekly usage reserve: weekly usage unknown` when the reading is unknown. The
run DTO carries it as `reserve_hold_reason`, with `queue_reason` set to
`weekly_reserve` (`maintenance` wins when both apply). `uzi run get` prints a
`RESERVE` row and a `QUEUE_REASON` row. The reason is written by the periodic
sweeper, so it can appear a short while after the run is queued. It is dropped
on the next pass once you clear the threshold, whatever the freshness of any
reading. Turning the toggle off drops it only for a run that has already
started; never-started work stays held. On `/api/v1/jobs` the job carries the
reason as `reserve_hold_reason`, the fixed text `Waiting for the owner's weekly
usage reserve.` without your usage figures, and leaves `queue_reason` to
maintenance.

Running runs pause after their current milestone and resume on a fresh reading
below the threshold; see [Pausing and resuming a run](run-pause.md#paused-by-the-weekly-usage-reserve).

### It is best effort

The reserve is not a strict cap, and usage can exceed the threshold:

- the poll lags (default 5 minutes), so a reading can trail real usage;
- a pausing run first finishes its current milestone;
- kinds that cannot pause mid-run (CI-fix, MR-rework, job, judge, cross-check,
  interactive tasks) are held at admission only, and one already running
  finishes;
- chat is exempt;
- a pause whose checkpoint fails lets the run continue and is re-requested at
  the next boundary.

Overshoot is not bounded.
