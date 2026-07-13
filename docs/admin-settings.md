---
title: Admin settings
order: 70
audience: user
---

# Admin settings

uzi keeps a small set of instance-wide settings in the database, editable by
an admin from **Admin → Instance settings**. Today: three forge labels and a
default theme.

## The three labels

| Setting | Default | Controls |
|---|---|---|
| PRD label | `PRD` | Which GitLab label marks an issue as factory work; only issues carrying it appear on any board. |
| Autopilot label | `autopilot` | Which GitLab label, added alongside the PRD label, triggers an unattended run for an opted-in user. See [Autopilot](./autopilot.md). |
| PRDLESS label | `PRDLESS` | Which GitLab label lets an issue start a run with no `prds/*.md` link, when the toggle below is on. See [PRDLESS label](./prdless.md). |

## The PRDLESS toggle

Unlike the other two, the PRDLESS label also has its own instance-wide on/off
switch, separate from its name — **on** by default. Turning it off requires
every run on this instance to have a real PRD link again; it doesn't touch a
run already in flight. The name field is editable only while the switch is on.

## Default theme

Which theme a user with no personal choice sees — new users, and anyone who
hasn't picked one under Settings → Appearance. A user's own pick, once made,
always wins over this setting. Saving restyles the admin's own session live;
every other un-overridden user picks up the change on their next `me`
refresh (in practice, their next login or reload — there's no push). See
[Theming](./theming.md) for how themes work and how to add one.

## Run health

uzi can flag a run that looks slow, stuck, or looping — see
[Run health](./run-health.md) for what each flag means. Tune it, or turn a
signal off, from **Admin → Instance settings → Run health**:

| Setting | Default | Controls |
|---|---|---|
| Enable run-health detection | on | Turns the whole detector on or off. |
| Stalled after | 300s (5m) | Seconds of silence, with no tool call in flight, before a running run is flagged stalled. |
| Slow after | 2700s (45m) | Wall-clock seconds since start before a running run is flagged slow. |
| Stuck queued after | 600s (10m) | Seconds a run may sit queued before it's flagged waiting for worker. |
| Awaiting approval after | 3600s (1h) | Seconds a run may sit awaiting approval before it's flagged; skipped for autopilot runs. |
| Slack nudge cooldown | 1800s (30m) | Minimum time between Slack DMs about the same run's flag — see [Slack notifications](./slack.md). |

## Validation

- A label value (PRD, autopilot, or PRDLESS) may not be empty, longer than 64
  characters, or contain a comma (GitLab's own label-list separator).
- The three labels must be pairwise-distinct. Equal PRD and autopilot would
  autopilot every PRD issue; a PRDLESS label equal to the PRD label would
  exempt every issue from the gate, equal to the autopilot label would
  conflate "hands-off" with "spec-less". The PRDLESS label stays distinct
  even while its toggle is off, so re-enabling it later is always safe.
- The PRDLESS on/off switch stores a strict `true` or `false` — nothing else
  is accepted.
- Each field accepts `0` or a whole number of seconds from 60 to 86400 (one
  day); anything else — negative, non-integer, or 1–59 — is rejected, so a
  fat-fingered value can't silently misconfigure a signal for a day or more.
  For the four detection thresholds (stalled, slow, stuck queued, awaiting
  approval), `0` **disables that signal**. For the Slack nudge cooldown, `0`
  means something different: no rate limit, so a nudge fires on every
  ok→flagged transition instead of at most once per window. The slow
  threshold is further clamped, at read time, to stay below `RUN_TIMEOUT`
  ([Configuration](./configuration.md)) — a value at or past the timeout
  would never fire, since the run fails first.
- An invalid save is rejected before anything is written. The same rules run
  client-side first for immediate feedback, but the server is the source of
  truth.

## Changing a label never touches GitLab

Renaming a label here doesn't create or rename anything on the forge —
create the label in GitLab yourself (or it simply never matches anything).
uzi only reads label names; the label objects themselves stay entirely
GitLab's. The one exception is applying the PRDLESS label from uzi's own UI,
which does create it on first use — see [PRDLESS label](./prdless.md).

## Resync after a change

Saving a changed PRD or autopilot label triggers a full resync of every
enabled repo, not just the next incremental poll, so the effect isn't
instant: boards drop issues that only carried the old label and pick up the
new set once that repo's resync completes. See "Freshness contract" in
[Configuration](./configuration.md) for how sync cadence otherwise works.
This resync fires only on a changed board-filtering label — the PRD label or
the autopilot label — since only those change which issues a board shows.
A PRDLESS change (its name or its on/off switch) and a default-theme-only save
do **not** trigger it: the PRDLESS keys change only whether a run can start
without a PRD link, and theming is presentation-only, so neither affects what
a board shows.

## No secrets here

Instance settings are plain values, readable by any admin — never put a
token, password, or PAT in a settings field. Secrets (Anthropic tokens,
forge PATs) have their own encrypted-at-rest storage; see
[ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest).
