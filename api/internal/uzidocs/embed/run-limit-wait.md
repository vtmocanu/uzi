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

Limits classified as non-window fail with typed `rate_limited` origin and no reset promise: missing or unaccepted snapshots, API-key or undefined authentication, spend-control flags, or credit, quota, plan and unknown rejection evidence. The last snapshot cannot prove that the terminal error carried it: an earlier exhausted sampling snapshot followed by a quota or plan failure without contrary evidence can still park, bounded by the wait and park budgets. `rateLimitExceeded` remains outside this window-park behavior ([follow-up #2361](https://github.com/vtmocanu/uzi/issues/2361)); `sessionBudgetExceeded` is unchanged.

Codex resumes on the same frozen account: no Anthropic token switching, pool lowering, or Anthropic gauge updates. The updated API revokes the parked flight’s Codex capability; a resumed claim gets fresh authority. If that account becomes unavailable or quarantined, promotion does not bypass the claim check: Claim refuses it, then Sweep parks it at `recovery_wait` / `codex_account_unavailable`. On a non-Docker worker with a resolvable thread, resume continues that thread. Docker attempt paths retain the existing fresh-thread lineage break, with work restored from the branch, tracking ref, or checkpoint when available.

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
  resets early"*. It's independent of whether waiting on limits is turned on.
- **A Slack DM, loud on purpose, and Slack-only** — a `🚨 7-DAY RATE LIMIT
  RESET EARLY` message naming the expected reset time, when it was actually
  observed, and how many hours you got back. It needs a linked Slack account
  to reach you (see [Slack notifications](slack.md)); with no Slack linked,
  you don't get this alert at all — the reset still shows up on the
  rate-limit meters, just without the push.
- **The 8-hour threshold is fixed**, not a setting you can tune.
- **It only tells you** — it does not resume a paused run early or otherwise
  act on your behalf. A run parked with [wait on limit](#on-by-default) still
  resumes on the schedule described above; this alert just lets you know
  sooner that the account itself has room again.

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
Resume or Cancel and has no automatic retry — see
[Recovering from a transient interruption](run-recovery-wait.md).

## Not the same as pausing

A `limit_wait` park (this page) happens *to* the run — its provider usage window was exhausted — and it is scheduled to resume on its own, with a fresh clock, after the reset or bounded fallback. [Pausing](run-pause.md) is something the run's owner asks for, on demand, and it only ever resumes when the owner says so, handing back exactly the budget that was left rather than a fresh clock. The two can overlap: a pause requested while a run is still `running` survives a `limit_wait` park that overtakes it, and takes effect at the first boundary once the run is working again — you don't have to ask twice.

Related: [Claude rate limits](rate-limits.md) · [Anthropic tokens](anthropic-token.md) · [Run health](run-health.md) · [Pausing and resuming a run](run-pause.md) · [Configuration](configuration.md) · [Slack notifications](slack.md) · [Recovering from a transient interruption](run-recovery-wait.md)
