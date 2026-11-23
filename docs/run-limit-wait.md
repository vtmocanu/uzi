---
title: Paused on a usage limit
order: 38
audience: user
---

# Paused on a usage limit

When a Claude run hits your Anthropic usage cap, or a Codex subscription run hits a recognized window usage limit, uzi can park it instead of failing it and resume it automatically after the reset.

## On by default

Waiting is on by default for recognized usage windows, with two controls:

- **Settings → Usage limits** — *"Pause my new runs on a usage limit instead of failing them"* sets the default inherited by new runs, including autopilot, CI-fix and self-improvement runs.
- **The run view** — *"Wait out future usage limits on this run"* overrides the default for one run, live, for as long as it's still going.

Starting a run by hand stays one click and inherits the Settings default; there's no checkbox at start. The API accepts the flag per run for scripted callers.

**The run-view toggle does not stop a paused run** — it only decides what happens the *next* time this run hits a limit, and leaves an already-paused run exactly as paused. To actually stop one, **cancel** it, the same way as any other run: that works right away, without waiting for the window to reset.

## What you'll see

- **Board card**: a **limit wait** badge.
- **Run view**: a warn-toned strip naming **Codex** or **Anthropic** from the run’s harness, with a countdown to its scheduled resume, which window it hit (5-hour, 7-day, …), and — from the second pause on — *"attempt N"*.
- **Activity feed**: the usage-limit event names the provider when the full run is available, with the window and reset time alongside it. Board latest-run summaries have no harness and use neutral usage-limit wording.
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

A successful park capture preserves committed progress and uncommitted edits through a throwaway WIP marker; recovery restores the marker as uncommitted work. Durability depends on capture and publish succeeding: a failed sink does not claim the checkpoint holds the latest work, and failed capture retains the source clone for recovery. It cannot promise recovery after that retained tree is lost. An already-approved plan is reused when work is recovered; total loss of both committed and uncommitted progress keeps the existing re-approval policy.

A parked run keeps its issue’s active-run lock and worker disk. For a run-bound hosted worker, it can also hold the worker and PVC for up to `RUN_LIMIT_MAX_PARK` per park (8 days by default).

For **Claude**, the pause itself also updates that credential's rate-limit meter right away, so it reads as exhausted for anyone else's claim too — see [Claude rate limits](rate-limits.md) for what that looks like. And the resuming claim goes further: it skips the credential that just paused it even when the meter alone wouldn't have ruled it out, which is what keeps an `auto` worker from immediately picking the very token that just refused it.

A run can pause and resume more than once if the limit keeps recurring, backing off between attempts, up to a cap; a second cap bounds how far out any single pause may reach. Both are operator-configured — see [Configuration](configuration.md) to change them.

## Codex: window evidence and same-account resume

A final `usageLimitExceeded` failure parks only for subscription authentication with accepted structured `account/rateLimits/updated` evidence from that turn. The latest bucket snapshot replaces earlier evidence; uzi does not infer resets from provider prose, an account-read request, or a poller. Exhausted windows (at least 100% used) select the latest reset. With `rate_limit_reached` but no window at 100%, the highest-used windows are selected, including ties. If any selected window lacks a usable reset, or the reset is already past, the bounded fallback applies: 15 minutes doubling per park, capped at 4 hours. The default five-park budget can therefore exhaust before a weekly window reopens.

Limits classified as non-window fail with typed `rate_limited` origin and no reset promise: missing or unaccepted snapshots, API-key or undefined authentication, spend-control flags, or credit, quota, plan and unknown rejection evidence. The last snapshot cannot prove that the terminal error carried it: an earlier exhausted sampling snapshot followed by a quota or plan failure without contrary evidence can still park, bounded by the wait and park budgets. `rateLimitExceeded` remains outside this window-park behavior: it uses bounded provider recovery and the [provider outage owner hold](run-recovery-wait.md#provider-outage-streaks) instead (#1733/#2361). `sessionBudgetExceeded` is unchanged.

Codex resumes on its committed account binding: no Anthropic token switching, pool lowering, or Anthropic gauge updates. The explicit [live owner switch](codex-credentials.md#switching-a-live-run) refuses this park, including when a worker still holds it. The updated API revokes the parked flight’s Codex capability; a resumed claim gets fresh authority. If that account becomes unavailable or quarantined, promotion does not bypass the claim check: Claim refuses it, then Sweep parks it at `recovery_wait` / `codex_account_unavailable`. On a non-Docker worker with a resolvable thread, resume continues that thread. Docker attempt paths retain the existing fresh-thread lineage break, with work restored from the branch, tracking ref, or checkpoint when available.

## Switching to a different token while parked

A Claude park driven by one token can also end on a **different** one, instead of
waiting out the reset — two ways:

- **Automatically, for an auto-select worker.** A parked run is re-checked on
  every sweep, not only at the moment it parked: the instant another one of
  your pooled tokens gains enough headroom, the run promotes itself back to
  `queued` — at most once per account per sweep tick — well before
  `retry_not_before` arrives. This only happens for a worker set to
  [auto-select from the pool](anthropic-token.md#letting-uzi-pick-the-token-auto-selection),
  or a run carrying a per-run `auto`
  [override](anthropic-token.md#choosing-the-token-for-one-run). A **pinned**
  or **default**-bound run is never promoted early this way, at park time or
  later — switching accounts on its behalf isn't something either mode
  decides for you, and doing it anyway would only park it again.
- **On demand, with `uzi run set-token`.** Pick a token yourself and the run
  resumes right away, whatever its bind mode:
  `uzi run set-token <run-id> <label>` (or `--auto` / `--default`) promotes
  it straight to `queued`, and the next claim spends the token you named — no
  waiting for the window, and nothing in flight to lose (a parked run has no
  live claim). The dead token stays excluded from that next claim, so
  pointing the run back at the very one that just hit its limit can park it
  again — uzi warns about that up front rather than refusing, see
  [Claude rate limits](rate-limits.md#choosing-a-token-near-its-limit). The
  run page's token picker does the same switch.

## Why the run did not move to another token

A run parked on a **7-day** window (`limit_wait` with window `seven_day`), or paused by the [weekly usage reserve](run-pause.md#paused-by-the-weekly-usage-reserve), carries a weekly-limit block that says why it did not move to another token, what you can do about it, and what will happen if you do nothing. It is owner-only and computed by the server, so every surface shows the same answer. 5-hour and per-model parks are unchanged and carry no block, as do runs on a [custom model provider](providers.md) and Codex runs authenticated with an OpenAI API key (they have no subscription window).

**Reason.** One of these, shown as a plain sentence:

| Reason | Meaning |
|---|---|
| `pinned` | The run is pinned to this token, so it never switches on its own. |
| `not_auto` | The run's worker (or the run) is not in auto-select mode, so it never switches on its own. |
| `can_switch` | The run can move to another token on its next claim. |
| `selection_unchanged` | Auto-select still prefers this token even though another pooled one is usable. |
| `headroom_outside_pool` | Another token has weekly headroom but is not in the auto-select pool. |
| `pool_exhausted` | Every other pooled token is at its weekly limit or held by the reserve. |
| `no_alternative` | There is no other usable token to switch to, including when this is your only token. |
| `switch_pending` | A credential switch is already in progress for this run. |
| `account_bound` | A Codex run, which stays on the account it was created with (see [Codex credentials](codex-credentials.md#when-a-codex-run-hits-its-weekly-limit)). |
| `unknown` | The server could not tell whether the run can switch. |

**Wait.** What happens if you do nothing, one of:

- `waits`: the run resumes on its own after the reset.
- `fails`: the run will fail on this limit rather than wait (waiting is off for the run, the wait count is spent, or the park would exceed the maximum park, see [Configuration](configuration.md)).
- `reserve_pause`: a weekly-reserve pause. It releases itself once a fresh reading is below the reserve (or the reserve or "Also pause running runs" is cleared), or when you resume it; see [Paused by the weekly usage reserve](./run-pause.md#paused-by-the-weekly-usage-reserve).
- `unknown`: no usable reset time is known.

`fails` and `unknown` never promise a resume.

**Before a park.** The block also appears on a running or queued run whose credential is near or at its weekly limit, before anything parks; the reason and fix rows read the same, and the wait statement is a preview of what would happen at the limit. It is controlled by your warning margin and omitted for stale readings, API keys and custom providers; see [Early warning before the weekly limit](rate-limits.md#early-warning-before-the-weekly-limit). If you have Slack linked, one best-effort DM per weekly period tells you too; the run page banner is the guaranteed surface ([Slack warning](rate-limits.md#slack-warning)).

**Where it shows.** The run page, `uzi run get` (the `WEEKLY_LIMIT` and `FIX` rows, see [CLI](cli.md)), the TUI run detail, and the Slack park message (`⏸️ Paused · usage limit` or the weekly-reserve pause line), which appends the reason and, when there is one, a copyable fix command.

**Fix commands.** Each fix appears only when it would apply to this run. Commands that name a token put every flag before a `--` and the quoted label after it, so a label such as `--auto` is read as a label, not a flag:

```sh
uzi run set-token <run-id> -- 'prod token'
uzi token pool --on --kind anthropic -- 'spare token'
uzi run resume <run-id>
```

Other fixes are `uzi run set-token <run-id> --auto`, `uzi worker set-token <worker-id> --auto` and `uzi settings codex-mode auto`; a weekly-reserve pause also points at the reserve setting in Settings. Advice that only helps runs created later is listed separately as future runs.

A token label that cannot be copied safely (for example a legacy label that holds a tab, a newline, a control or format character, or a backtick) gets no command. The fix still names the token, with a hint to rename it in Settings to get a copyable command, because sanitizing the label would change which token the command names.

## If a run isn't waiting out limits

A run that opts out fails on a recognized usage limit with a server-composed reason naming **Codex** or **Anthropic** and the window, plus the reset when known: *"Codex usage limit (seven_day) reached; resets at 2026-10-13T02:00:00Z"*. Without a usable reset, the reason makes no reset promise. Waiting uses the existing `wait_on_limit` default and per-run override; there is no new setting or migration.

## Alert when the 7-day window resets early

Anthropic's 5-hour window resets like clockwork, but the 7-day (weekly) window
sometimes reopens *earlier* than the reset time it originally advertised —
whether or not your runs were ever blocked or parked on that window. uzi's
usage poller already tracks that expected reset time per token, so it notices
when this happens: either the advertised reset time moves forward, or your
weekly usage drops back to (near) zero ahead of schedule. If a token's weekly
window comes back more than 8 hours before its previously-recorded reset, uzi
can tell you right away instead of you finding out only when the nominal time
finally arrives. One case it can't tell apart from ordinary noise, and so stays
quiet on: a window that was barely used before it cleared (weekly usage already
near zero), or one whose reset time the poller can't read from Anthropic at that
moment.

- **On by default** — the same **Settings → Usage limits** card as
  the pause toggle above has its own checkbox, *"Alert me when my 7-day limit
  resets early"*. This existing setting covers Anthropic tokens and Codex
  subscription accounts, independently of whether waiting on limits is turned on.
- **A Slack DM, loud on purpose, and Slack-only** — a `🚨 7-DAY RATE LIMIT
  RESET EARLY` message naming the expected reset time, when it was actually
  observed, and how many hours you got back. Codex uses `🚨 CODEX WEEKLY LIMIT
  RESET EARLY` with the same timing facts. It needs a linked, confirmed Slack
  account with notifications enabled to reach you (see [Slack notifications](slack.md));
  with no Slack linked, you don't get this alert at all — the reset still shows up on the
  rate-limit meters, just without the push.
- **The 8-hour threshold is fixed**, not a setting you can tune.
- **It only tells you** — it does not resume a paused run early or otherwise
  act on your behalf. A run parked with [wait on limit](#on-by-default) still
  resumes on the schedule described above; this alert just lets you know
  sooner that the account itself has room again.

### Codex subscription accounts

Codex early-reset alerts compare successful account usage polls, even with no
active or previously blocked run. Saved aliases of the same subscription account
share one comparison basis and alert; they do not each generate a separate alert.
OpenAI API keys are outside this behavior.

Detection uses the main `codex` bucket (with no display name), with exactly one
weekly window whose reported duration is **604800 seconds**. It ignores extra
buckets, non-weekly windows and ambiguous main or weekly windows. The previous
successful reading must have had a reset still pending when that reading was
recorded, and the new observation must be **strictly more than 8 hours** before
that expected reset. Then either of these signals qualifies:

- The advertised reset advances by **strictly more than 1 hour**.
- The reset stays identical and usage falls from **at least 5%** to **at most
  1%**, with both percentages present, finite and within 0–100%.

A missing or invalid comparison basis stays silent, including missing or invalid
reset times. The percentage signal also stays silent when either percentage is
absent or invalid; the advancing-reset signal does not require percentages.

Delivery is **persist-first and best effort**: uzi accepts the new meter reading
before creating the notification row, and persists that row before queuing the
Slack DM. Account authority and enabled aliases are checked again before the
post. Failed notification creation or delivery is not retried, and this alert has
no durable redelivery. A crash or dropped DM can therefore lose an alert; API
replicas observing the same change can send duplicates. This is notification
only: it does not promote a parked run, switch accounts or change a resume time.

## A custom provider's gateway rate limit

A run that uses a [custom model provider](providers.md) has no subscription window to wait on. If its gateway keeps answering 429 or overloaded, the run parks as `limit_wait` with the reason "provider gateway limit" and retries on a fixed schedule instead of a reset time:

| Gateway parks since you last resumed the run | The run retries |
|---|---|
| first | 1 minute later |
| second | 5 minutes later |
| third | 15 minutes later |
| fourth and later | never on its own; it waits for you |

uzi shows the retry time, or "waiting for you to resume" once automatic retries stop. It never shows a subscription reset time, and it does not read or change your Claude token pool. **Resume** (web, `uzi run resume`) re-queues the run from any of these parks and starts the schedule over; **Cancel** ends it. The wait for you has no time limit, and the retry budget and maximum park (`RUN_LIMIT_MAX_WAITS`, `RUN_LIMIT_MAX_PARK`) do not apply to it. A run set not to wait out limits fails instead of parking. Provider chats never park: the gateway error is shown in the chat.

Resume applies only to this kind of park. A subscription `limit_wait` still resumes on its own.

## Not the same as waiting for a pooled token or an empty model turn

A **`limit_wait`** pause (this page), a **`pool_wait`** hold and a
**`recovery_wait`** park are all non-terminal waits, but for different
reasons with different resolutions. `limit_wait` means the run hit a recognized
provider usage window, and it is scheduled to resume after the reset or bounded
fallback. `pool_wait` means an `auto`-lane worker's token pool was
genuinely empty — there was nothing to spend at all — and it clears when you
opt a token into the pool, or on demand with `uzi run resume-now`. See
[Letting uzi pick the token (auto-selection)](anthropic-token.md#letting-uzi-pick-the-token-auto-selection)
for the pooled-token wait. `recovery_wait` covers transient interruptions and
other recovery causes; `worker_requeue_exhausted` requires explicit owner
Resume or Cancel and has no automatic retry, as does an integrity refusal with
cause `retained_recovery_adoption_failed` (Resume is refused with 409 when its
`recovery_wait_detail` is `secret_remediation_active`; Cancel remains) and an unwitnessed retained
continuation with cause `retained_continuation_unwitnessed` (not an integrity
refusal: the continuation's departure was never witnessed).
`retained_recovery_exhausted`
instead has bounded timed retries followed by an owner hold with a NULL retry
and no countdown — see
[Recovering from a transient interruption](run-recovery-wait.md).

## Not the same as pausing

A `limit_wait` park (this page) happens *to* the run — its provider usage window was exhausted — and it is scheduled to resume on its own, with a fresh clock, after the reset or bounded fallback. An [ordinary pause](run-pause.md) can be requested by the owner or the run's lead; it waits for owner resume, handing back exactly the budget that was left rather than a fresh clock. The two can overlap: a pause requested while a run is still `running` survives a `limit_wait` park that overtakes it, and takes effect at the first boundary once the run is working again — you don't have to ask twice.

## Not the same as the weekly usage reserve

A `limit_wait` park reacts to a provider's *actual* limit: the window was exhausted and the provider refused. The [weekly usage reserve](rate-limits.md#weekly-usage-reserve) is your own earlier stop: an opt-in threshold on the 7-day window at which uzi holds new work and, optionally, pauses running runs before the provider refuses. A held run stays `queued` and a reserve pause is a [`weekly_reserve` pause](run-pause.md#paused-by-the-weekly-usage-reserve); neither uses the wait count, the fresh clock a `limit_wait` resume grants, or the `limit_wait` status.

Related: [Claude rate limits](rate-limits.md) · [Anthropic tokens](anthropic-token.md) · [Run health](run-health.md) · [Pausing and resuming a run](run-pause.md) · [Configuration](configuration.md) · [Slack notifications](slack.md) · [Recovering from a transient interruption](run-recovery-wait.md)
