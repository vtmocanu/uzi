---
title: Admin settings
order: 70
audience: user
---

# Admin settings

uzi keeps a small set of instance-wide settings in the database, editable by
an admin from **Admin → Instance settings**. Today: three forge labels, the
run-eligibility and board-membership label lists, a default theme, and the
run judge.

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
on (**Judge model**, `opus` by default — the judge's recommendations feed
self-improvement, so the strongest model is the default; pin a cheaper alias
like `haiku` or `sonnet` to spend less). This switch
only arms the feature instance-wide; each user still opts in under their own
Settings, and the judge always spends that user's own Anthropic token, never
the admin's.

**Upgrade note:** an instance that already had the judge enabled, with no
`judge_model` pinned, starts spending `opus` the moment it upgrades to a
version carrying this default — there's no migration that preserves the old
(cheaper) value. Pin **Judge model** to `haiku` or `sonnet` before upgrading
if you want to keep spending where it was.

### Judge mode: off, optional, enforced

A second checkbox, **Enforce the judge on every run (no per-user opt-in)**
(`judge_enforce_all`, off by default — greyed out while the kill-switch
above is off), combines with the kill-switch into three effective modes:

| Setting combination | Mode | Who gets judged |
|---|---|---|
| `judge_enabled=false` | **Off** | Nobody — the kill-switch always wins, even with `judge_enforce_all=true`. |
| `judge_enabled=true`, `judge_enforce_all=false` | **Optional** | Only users who've opted in themselves. |
| `judge_enabled=true`, `judge_enforce_all=true` | **Enforced** | Every user who holds an Anthropic token, whether or not they've opted in; a token-less user is still skipped, since there's nothing to spend. |

Enforcing the judge never redirects *who pays*: spend always stays on each
run owner's own Anthropic token — an admin can force that judging happens,
never send the bill to a different account. It also overrides a per-user
force-disable set on **Admin → Users**: the per-user toggle there is shown
greyed and labelled "Inert: enforced mode judges every run regardless of
this flag" while enforcement is on, since one boolean can't tell "an admin
disabled you" from "you opted out." The only opt-out left to an enforced
user is deleting their own Anthropic token, which also stops their own
runs — see [Judge mode](./judge.md#judge-mode-off-optional-enforced) in the
judge doc.

If your users hold Anthropic **subscription** plans rather than metered
console keys, remember that an enforced `opus` judge spends their
plan/rate-limit **quota**, not dollars — a busy judge can eat into the quota
their real runs need. Pin **Judge model** to `haiku`/`sonnet` before
enforcing, or leave enforcement off and let users opt in with their own
per-user model choice (**Settings → Run judge → Judge model**).

### Spend guards

Two more per-user, count-based, best-effort fields bound how often the judge
fires for any one user, in every mode (not just enforced) — a runaway
failure loop is a footgun even for an opted-in user:

| Setting | Default | Controls |
|---|---|---|
| Per-user cooldown (seconds) (`judge_cooldown_seconds`) | `60` | Skip enqueuing a judge for a user who already had one enqueued within the last N seconds. `0` disables the cooldown; otherwise `60`–`86400`. |
| Per-user daily budget (runs) (`judge_daily_budget`) | `0` (unlimited) | Skip enqueuing a judge for a user who already had `N` or more judge runs in the rolling last 24 hours. `0` disables the cap; otherwise a positive count. |

Both are blunt on purpose — a legitimate high-throughput burst can also lose
a few retrospectives — and both fail **open**: a settings-read hiccup lets
the judge through rather than silently going quiet. On trip, the judge is
skipped the same way an ineligible run is: silently, with no notification
and no queued run.

## Run summaries

The model [run summaries](./run-summaries.md) generate on, instance-wide
(**Summary model**, `haiku` by default — summaries are cheap and run once or
twice per run, so the default favors speed and cost over depth). Each user
can override it for their own runs from **Settings → Run defaults → Run
summaries → Summary model**; the admin default only applies while they
haven't. Summary generation always spends the run owner's own Anthropic
token, never the admin's, same as the judge above — there's no kill-switch
here, since a failed or slow summary is skipped silently and never blocks a
run.

## Self-improvement

Self-improvement is no longer an admin instance setting. It ships as the
`self-improve` default job on the [Schedules](./scheduling.md#default-jobs)
page: any user can enable it on a repo they own, no admin gate, and it
spends that user's own Anthropic token on a standing basis. See
[Scheduling](./scheduling.md#default-jobs).

## Agent source

Whether uzi syncs your [agent templates](./agent-templates.md) from a git
repository at runtime: the source URL + ref, the source folder (the
repo-relative subfolder role files are read from, default `.claude/agents`),
an enable toggle, a sync interval, and (for a private repo) a sealed
credential. Off by default with no URL pre-filled — a fresh install stays
offline until an admin opts in. A **Use uzi skills preset** button fills
the URL, folder, and latest-tag ref for uzi's own shared roster in one
click, but still leaves you to review and Save. A **Check for updates**
button reports whether the source has published something newer than
what's configured, and a **Bump pin** button moves the pinned ref to it
(sync and approval are still required to apply it).
See [Agent source](./agent-source.md) for the full config walkthrough, the
preset's preconditions, the stage-then-approve flow, the update badge, and
the trust model.

## GitHub Projects v2 sync

Whether uzi keeps a GitHub repo's board columns synced with a linked GitHub
Projects v2 board's Status field: the `github_project_sync_enabled` instance
kill switch, off by default. See
[GitHub Projects v2 sync](./github-project-sync.md) for what it does, the
required PAT scope, and how a repo gets linked.

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
| Slow after | 2700s (45m) | Wall-clock seconds since start before a running run is flagged slow. For a milestone-scaled run (frozen `budget_wall_seconds`, PRD #122) this threshold scales up with the run's budget, so a long-budget run isn't flagged at the flat default while it is still working. |
| Stuck queued after | 600s (10m) | Seconds a run may sit queued before it's flagged waiting for worker. |
| Awaiting approval after | 3600s (1h) | Seconds a run may sit awaiting approval before it's flagged; skipped for autopilot runs. |
| Slack nudge cooldown | 1800s (30m) | Minimum time between Slack DMs about the same run's flag — see [Slack notifications](./slack.md). |

**A repo-bearing run stuck past "Stuck queued after" can also be waiting on
the Docker worker repo allowlist.** If every online worker is Docker-capable
and the run's repo isn't on that allowlist, the owner's reason names it
directly, distinct from "no worker online" or "all workers busy" — see
[Run health](./run-health.md#what-the-flags-mean). That allowlist is keyed
by **repo id**, not path, so a repo re-added to uzi (say, after moving it to
a new forge) gets a new id and silently drops off — nothing re-adds it for
you. The Repos page's **Setup** chip surfaces each repo's optional
capabilities (repo skills, repo instructions, tool profile, Docker workers)
as on/off with where to set them, staying neutral while a repo sits on its
safe defaults and escalating to an info tone only once a queued run is
actually blocked this way.

## Guardrail override (per repo)

uzi refuses to enable a repo, or to start or claim a run against it, if its bot
could push or merge to the default branch — see "Least privilege: what uzi
verifies" in the [GitLab](./gitlab-bot-setup.md#least-privilege-what-uzi-verifies),
[Forgejo](./forgejo-bot-setup.md#least-privilege-what-uzi-verifies), and
[GitHub](./github-bot-setup.md#least-privilege-what-uzi-verifies) setup docs
for what triggers the block on each forge.

This is not one of the instance-wide settings above: it is a **per-repo, per-decision**
exception, not a knob. An **instance admin only** can grant it — a member cannot
self-allow, not even for a repo they own; they see the block on their own Repos
page with a pointer to ask an admin. Allowing a repo requires a written reason,
and the write is recorded with the admin's identity and a timestamp — there's no
anonymous or unattributed override.

Admins act on it in two places: **inline on the Repos page**, for any repo they
can already see, with "Allow anyway" (blocked) or "Revoke" (already allowed); and
from a cross-user **Admin → Blocked repos** page, which lists every user's
blocked or overridden repos so an admin doesn't have to hunt through each user's
own Repos page to find one. **Revoke** re-arms the block immediately. An
override never auto-expires — silently re-blocking a repo with nobody present to
fix it would be worse than the problem the guardrail exists to prevent — but the
Blocked repos page flags an override as stale once it's roughly 30 days old, so
an old accept-risk decision doesn't quietly outlive its reason.

**The override can never waive the case where uzi couldn't read the repo's
protection at all.** A forge read error, timeout, or an unverifiable answer (see
the GitHub classic-branch-protection case in the setup doc above) still refuses
the run even on a repo an admin has allowed — an admin can accept a risk uzi
told them about, never one uzi couldn't see.

The `uzi admin guardrail-impact` and `uzi admin blocked-repos` CLI commands give
the same picture from a terminal instead of the web UI — see
[docs/cli.md](./cli.md#commands) for both.

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
