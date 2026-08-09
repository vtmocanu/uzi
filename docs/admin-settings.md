---
title: Admin settings
order: 70
audience: user
---

# Admin settings

uzi keeps a small set of instance-wide settings in the database, editable by
an admin from **Admin → Instance settings**. Today: three forge labels, the
run-eligibility and board-membership label lists, a default theme, the run
judge, and the self-improvement job.

## The three labels

| Setting | Default | Controls |
|---|---|---|
| PRD label | `PRD` | The **primary** label: which GitLab label marks an issue as uzi's own work. It's the label uzi *writes* to mark an issue as its own work (Promote, a judge-filed issue, board issue creation), the label boards fetch with, and the only label autopilot ever matches. Every board shows it, always. Which labels a *human* may additionally start a run on is the run-eligible list below; which extra labels a board shows by default is the board-extras list below. |
| Autopilot label | `autopilot` | Which GitLab label, added alongside the PRD label, triggers an unattended run for an opted-in user. See [Autopilot](./autopilot.md). |
| PRDLESS label | `PRDLESS` | Which GitLab label lets an issue start a run with no `prds/*.md` link, when the toggle below is on. See [PRDLESS label](./prdless.md). |

## The PRDLESS toggle

Unlike the other two, the PRDLESS label also has its own instance-wide on/off
switch, separate from its name — **on** by default. Turning it off requires
every run on this instance to have a real PRD link again; it doesn't touch a
run already in flight. The name field is editable only while the switch is on.

## Run eligibility and board membership

Three more keys, added alongside the PRD label, generalise "which issues can
a human run" and "which issues does a board show" from one label to a
configurable set. All three are **admin-only instance policy** — a user's
own board preference (see [Board](./board.md#the-issues-popover)) can never
widen what's runnable, only what's visible:

| Setting | Default | Controls |
|---|---|---|
| Run-eligible labels | `PRD,bug` | The set of labels a **human** may click Start run on. The primary is always included and can't be removed. Autopilot is unaffected — it still matches only the primary label, never this set (see [Autopilot](./autopilot.md)). |
| Also show on boards (`board_extra_labels`) | `bug` | The **default** set of labels, beyond the primary, that a board shows a user who hasn't customised their own view. Each user can override this for themselves, per repo, from the board's [Issues popover](./board.md#the-issues-popover); the admin default only applies while they haven't. |
| A non-primary eligible label waives the PRD-link requirement (`eligible_label_waives_prd_link`) | on | When on, an issue eligible via a label *other than* the primary (e.g. `bug`) can start a run with no `prds/*.md` link — the same judgement PRDLESS expresses, applied to a whole label instead of one issue. Off: such an issue needs a link or PRDLESS, same as a `PRD` issue. Scoped to interactive, human-initiated Start clicks only — see the next paragraph. |

**Eligibility is not the same as visibility.** A label can be run-eligible
without being a board default (runnable the moment its card is shown some
other way), and a label can be a board default without being run-eligible
(shown, but offers Promote instead of Start run). Membership is always
`primary ∪ extras`, never `eligible ∪ extras` — an eligible-by-default label
like `bug` stays something a user can untick off their own board.

**The link waiver never reaches an unattended run.** It applies only to a
human clicking Start (the board, issue view, or `uzi run start`). Autopilot
never gets it — it still requires a real PRD link or PRDLESS, exactly as
before this setting existed — and neither does a scheduled (timer or sweep)
run: a schedule with auto-approve off still needs a link or PRDLESS, because
nobody is present at fire time to have earned the human-click waiver. See
[Autopilot](./autopilot.md) and [Scheduling](./scheduling.md).

### Validation, on top of the rules above

- Both label lists are comma-separated (not JSON), following the existing
  hosted-worker Docker-repo allowlist. A label may not contain a comma, so
  the separator can never collide with a real label name.
- Neither list may contain a duplicate entry, or the autopilot label, or the
  PRDLESS label — those are workflow markers, never membership or
  eligibility content.
- Each list is capped at 32 entries, a generous bound meant only to catch a
  runaway paste.
- The primary is always folded into the effective run-eligible set even if
  you save it without one — a hard rejection would wedge an unrelated save
  (say, the default theme) on any instance that had renamed its PRD label
  before this setting existed. The admin settings form pins the primary in
  the run-eligible field so an ordinary save always carries it explicitly.
- The waiver toggle stores a strict `true` or `false` — nothing else is
  accepted.

## Default theme

Which theme a user with no personal choice sees — new users, and anyone who
hasn't picked one under Settings → Appearance. A user's own pick, once made,
always wins over this setting. Saving restyles the admin's own session live;
every other un-overridden user picks up the change on their next `me`
refresh (in practice, their next login or reload — there's no push). See
[Theming](./theming.md) for how themes work and how to add one.

## Run judge

A global kill-switch for the [run judge](./judge.md), plus the model it runs
on (**Judge model**, a cheap alias like `haiku` by default — a retrospective
is a single trace round-trip, so a cheap model is usually right). This switch
only arms the feature instance-wide; each user still opts in under their own
Settings, and the judge always spends that user's own Anthropic token, never
the admin's.

## Self-improvement

The [self-improvement job](./self-improvement.md)'s settings: enable/disable,
the connected repo it targets, and its interval (default `48h`). Unlike the
run judge, enabling this spends **the enabling admin's own token** on a
standing basis — see the linked doc for what that means before turning it on.

## Hosted worker quota

On a k8s deployment with [hosted workers](./hosted-workers.md) turned on, a
single setting bounds self-service provisioning:

| Setting | Default | Controls |
|---|---|---|
| Hosted worker quota | 2 | Max hosted workers any one user may hold at once. `0` disables self-service entirely — the provision card disappears for everyone, but nobody's existing hosted workers are touched or hidden. |

Every size counts the same 1 against this quota; see
[Hosted workers](./hosted-workers.md#type-and-size). This setting exists (and
is editable) on every instance, including compose ones — it's simply inert
there, since hosting itself is off unless an admin turns it on for the
deployment (see [Configuration](./configuration.md#hosted-k8s-workers-prd-58)).

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

Changing the run-eligible or board-extras lists doesn't trigger it either,
and deliberately so. Every open issue is already synced into the cache
regardless of label — only the PRD label's sync fetch narrows what's kept —
so these two lists only change how the *already-cached* set is filtered and
rendered, on the next ordinary poll. A resync here would do nothing useful.

## No secrets here

Instance settings are plain values, readable by any admin — never put a
token, password, or PAT in a settings field. Secrets (Anthropic tokens,
forge PATs) have their own encrypted-at-rest storage; see
[ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest).
