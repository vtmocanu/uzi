---
title: Admin settings
order: 70
audience: user
---

# Admin settings

uzi keeps a small set of instance-wide settings in the database, editable by
an admin from **Admin → Instance settings**. Today: two forge labels, a
default theme, and the run judge.

## The two labels

| Setting | Default | Controls |
|---|---|---|
| `uzi` label | `uzi` | The single run-eligibility gate: which GitLab label marks an issue as uzi's own work and makes it runnable. It's the label uzi *writes* to mark an issue as its own work (Promote, a judge-filed issue, board issue creation); every board fetches its full history (any state), alongside every other open issue. See [Run eligibility](#run-eligibility). |
| Autopilot label | `autopilot` | Which GitLab label, added alongside the `uzi` label, triggers an unattended run for an opted-in user. See [Autopilot](./autopilot.md). |

## Run eligibility

Run-eligibility is one label, one gate: an issue carrying the configured
`uzi` label is runnable, full stop. There's no PRD link, no escape-hatch
label, and no admin waiver to reason about — a prior model tangled
those together, and it's gone. `Planned` and `bug` are unaffected **sweep
selectors** (see [Scheduling](./scheduling.md)): they decide which open
issues a sweep even considers, but a picked candidate only actually fires
once it also carries `uzi`.

A linked `prds/*.md` file is now **optional**. uzi still detects it
automatically — no label or setting controls that — and the agent still
implements or updates it exactly as before when a run has one. The board
card and the runs view show a neutral "PRD" badge whenever an issue links
one, whether or not that run needed it to start. See
[Board](./board.md#which-issues-show-up).

### Validation

- The `uzi` and autopilot labels must be distinct — an equal pair would
  autopilot every runnable issue, conflating "uzi's to run" with "skip the
  plan gate".
- A label value may not be empty, longer than 64 characters, or contain a
  comma (GitLab's own label-list separator).

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

- A label value (`uzi` or autopilot) may not be empty, longer than 64
  characters, or contain a comma (GitLab's own label-list separator).
- The `uzi` and autopilot labels must be distinct. An equal pair would
  autopilot every runnable issue, conflating "uzi's to run" with "skip the
  plan gate".
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
GitLab's.

## Resync after a change

Saving a changed `uzi` or autopilot label triggers a full resync of every
enabled repo, not just the next incremental poll, so the effect isn't
instant: boards drop issues that only carried the old label and pick up the
new set once that repo's resync completes. See "Freshness contract" in
[Configuration](./configuration.md) for how sync cadence otherwise works.
This resync fires only on those two — since only they change which issues a
board's any-state fetch keys on — not a default-theme-only save, which is
presentation-only and never affects what a board shows.

## No secrets here

Instance settings are plain values, readable by any admin — never put a
token, password, or PAT in a settings field. Secrets (Anthropic tokens,
forge PATs) have their own encrypted-at-rest storage; see
[ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest).
