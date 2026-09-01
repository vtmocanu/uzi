---
title: Scheduling
order: 82
audience: user
---

# Scheduling

A **schedule** starts run(s) at a future time, unattended — a third way to
start work alongside the manual **Start run** button and
[autopilot](./autopilot.md). Where autopilot reacts to a label appearing on
an issue, a schedule reacts to the clock: **one-time** (fires once at a
timestamp, then goes terminal) or **recurring** (a cron cadence, interpreted
in a named IANA timezone so DST doesn't drift it).

## Targets

Every schedule fires against exactly one target:

- **Pinned issue** — one repo + issue, the same shape as clicking Start run
  on that issue yourself.
- **Label sweep** — at fire time, every *open* issue on the repo matching a
  label selector; an empty selector defaults to the `uzi` label, i.e. every
  runnable issue. A sweep also caps how many issues one fire
  starts, oldest issue first — see [Sweep cap](#sweep-cap) below. A sweep's
  selector doesn't have to be a label: the built-in `assigned-sweep` default
  (see [Default jobs](#default-jobs)) matches on a different, non-label
  selector kind instead — issues **assigned to the uzi-bot account** — and
  carries no labels at all; this kind isn't offered on a custom sweep today.
- **Ad-hoc prompt** — no issue at all. A stored prompt runs against the repo
  and opens a merge request directly, for standing "hunt for X and open an
  MR" work with no throwaway tracking issue.

A pinned issue or label sweep can also carry optional **guidance** — see
[Guidance](#guidance) below. A user-authored prompt target carries its own
prompt text instead, so guidance isn't offered there; the exception is a
prompt-target **default** (a catalog job you enable), whose baked prompt is
catalog-owned, so owner guidance *is* offered to steer it.

## The same gates a manual start has, plus auto-approve

Pinned-issue, label-sweep, and assigned-sweep fires go through the **exact
run-creation path** autopilot uses, so the eligibility gate — an issue
carrying the `uzi` label **or** assigned to the uzi-bot account, see [Run
eligibility](./admin-settings.md#run-eligibility) — a fresh forge fetch of
the issue's labels and assignees, active-run dedup, and the usage-limit park
all behave
exactly as for a manual start — a schedule can't do anything a manual start
couldn't. A label selector only picks *candidates*; the gate still decides
what fires. `Planned` and `bug` are pure selectors, not eligibility signals in
their own right, so a sweep over one of them (e.g. everything tagged `bug`)
fires only on the candidates that are also eligible — pair a raw-bug-report
sweep with the `uzi` label (or bot assignment) if you want it to run
anything. A bare selector issue that's neither `uzi`-labelled nor
bot-assigned is a benign skip (see [Fire outcomes](#fire-outcomes)) that
advances the schedule like any other. The `assigned-sweep` default sidesteps
this pairing by construction: its selector *is* the eligibility signal, so
every candidate it matches is already eligible. The **ad-hoc prompt** target
is the deliberate exception: with no issue to gate on, it bypasses the
eligibility requirement by design.

Each schedule also has its own **auto-approve** toggle, **on by default** —
the point of a 02:00 fire is that it actually proceeds instead of sitting at
the plan-approval gate waiting for someone awake. Turn it off for a given
schedule to make its runs stop and wait for a human, same as a manual start.
Either way, the plan is still recorded on the run to read afterwards, and a
human still merges the resulting MR: `main` is never written under any
target, timing, or approval setting.

A schedule also has its own **wait-on-limit** toggle, **on by default** for a
new schedule: a fired run parks until the Anthropic usage window reopens
instead of failing — the right behavior for something unattended, typically
firing off-hours. This takes effect for auto-approve schedules too (the
default case), not just the ones that stop at the plan gate. Turn it off for
a given schedule to have a fired run fail outright on a usage limit instead
of waiting. An existing schedule keeps whatever it already had — this is a
create-time default, not a retroactive change.

## Sweep cap

A label sweep also has a **max issues per fire**, applied oldest issue first
(lowest issue number). The cap counts runs **started**, not candidates
matched: when the oldest candidate can't start — missing the `uzi` label,
already mid-run from a previous fire, or a transient fetch error — the fire flags it
(see [Fire outcomes](#fire-outcomes)) and walks on to the next eligible
issue, so a stale issue at the head of the backlog no longer wastes a slot or
blocks newer work. That walk is bounded by a **scan window** — the cap plus a
fixed headroom — so one fire's cost stays predictable even when the head is a
wall of ineligible issues; if every issue in that window is ineligible the
fire under-fills, and each skipped issue stays flagged for you to fix. A new
sweep defaults to **10**, so one fire can't fan out across an entire label's
backlog at once; raise it, or in the web modal blank the field for unlimited
(today's original behavior — the CLI always sends a cap, defaulting to 10, so
an unlimited sweep is web-only). An existing sweep created before this cap
existed stays unbounded until you set one.

## Guidance

A pinned-issue or label-sweep schedule can carry optional **guidance**: free
text from the owner, injected into the run instruction as a section clearly
separate from the issue body, to steer *how* a run approaches its task
("always add a failing test first", "keep the diff small, no new deps")
without editing every issue. It doesn't change *what* the task is, and it
has no effect on which issues are eligible to fire. Guidance is capped at
8 KiB; on the rare issue whose body is already near the run's size limit,
the guidance is truncated rather than the issue being skipped, so the run
still happens. A user-authored prompt schedule has no guidance field — its
prompt text already is the instruction. The exception is a prompt-target
**default**: its prompt is catalog-owned (and auto-tracks catalog updates), so
owner guidance *is* offered and is overlaid onto the catalog prompt at fire
time, the same "generic base + owner overlay" split the issue/sweep targets use.
A **sweep default** likewise offers owner guidance as an **overlay**: its baked
guidance stays catalog-owned (shown read-only and auto-tracking catalog updates)
and your overlay is composed onto it at fire time, under a single guidance header.

## Managing schedules

- **Web**: the **Schedules** page lists your schedules, and a "Schedule…"
  entry point on an issue opens the create modal pre-pinned to it. The modal
  offers cadence presets (weekdays, every day, every N hours) plus an
  advanced raw-cron field, and a live "next fires" preview. Pause, resume,
  and run-now are per-schedule row actions on the list.
- **CLI**: `uzi schedule create | list | get | pause | resume | run-now |
  delete` — see [the CLI reference](./cli.md#commands) for the full flag list.

### Running a schedule on several repos

Creating a custom schedule can target more than one repo at once: the "New
schedule" modal's repo picker is a multi-select, and picking N repos creates
**N independent schedules** — "N repos → N schedules" — one per repo, each
with its own cadence, pause/resume, and run history. Editing an existing
schedule still targets exactly one repo, unchanged; the multi-select is a
create-time affordance only.

Schedules created together this way are shown grouped. On the **My
schedules** list, a custom schedule that exists on two or more repos
collapses into one expandable summary row — the schedule's name plus how
many repos it's on — over a per-repo sub-row each. It's the same
grouped/expandable look the [Default jobs](#default-jobs) tab already uses
for a default enabled on several repos. A schedule that only exists on one
repo still shows as a plain, standalone row.

**The grouping is a display convenience, not a linked job.** Each sibling is
its own independent schedule row: editing, pausing, or removing one never
touches the others, and an edit on one never propagates to the rest of the
group.

**Add another repo**, on a row or a group's summary, extends that same job
onto one more repo you own as a new sibling, without reopening the create
modal and re-entering everything. If the schedule already has a sibling on
that repo, this is a clean no-op rather than an error. A multi-repo create
also runs the [sweep-label guardrail](#sweep-label-guardrail) once per
selected repo, same as any other sweep schedule.

**Issue-target schedules cannot span repos.** An issue number is
repo-relative (issue #7 on one repo is a different issue on another), so a
sweep or prompt schedule can group across repos but an `issue`-target one
cannot. Add-repo on an issue schedule is refused (HTTP 422), and the
control is disabled on those rows; creating one across multiple repos at
once (`--repo A --repo B ... --issue N`) is likewise refused. To run the
same kind of work on another repo, create a fresh schedule against that
repo's own issue.

**CLI**: `uzi schedule create --repo A --repo B ...` creates the same
grouped siblings as the web multi-select; `uzi schedule add-repo <id> --repo
<id>` is the CLI twin of "Add another repo" — see [the CLI
reference](./cli.md#commands) for the full flag list.

## Fire outcomes

A schedule can fire right on time and still start **zero** runs — every
candidate can be benign-skipped by the same gate a manual start goes
through. The motivating case: a `bug` label sweep whose oldest candidates
all lack the `uzi` label — [backfill](#sweep-cap)
walks past them to start any runnable issue in reach, but when the whole
scan window is `uzi`-less the fire runs every night, `Last run` keeps
advancing, and nothing ever starts. Without a fire outcome, that looks
identical to a healthy schedule.

Each fire records how many candidates it **examined** (attempted — this can
exceed `max_issues` once backfill walks past a skip), which ones
**started** (paired with the run they produced), and which were
**skipped**, each with a typed reason — never free text:

- `not_eligible` — the candidate carries neither the `uzi` label nor a
  bot assignment, so the eligibility gate refuses it. A bare selector-only
  candidate (say, `bug` with no `uzi` and not assigned to the uzi-bot)
  skips here; it's benign, and the schedule advances normally.
- `already_running` — an active run already exists for that issue (or,
  for the schedule itself, a dedup at fire time).
- `description_too_large` — the composed run instruction (issue body
  plus any [guidance](#guidance)) exceeds the size limit.
- `fetch_failed` — a transient forge or database error while checking
  a sweep candidate. The same underlying error is handled differently
  by target: on a **pinned issue**, it's transient and the fire retries
  next tick with nothing recorded; on a **sweep**, one bad candidate
  can't stall the rest of the fan-out, so that candidate is bucketed
  `fetch_failed` and the sweep continues.

`examined == started + skipped` always holds — every candidate the fire
reaches lands in exactly one bucket, so the tally never silently drops one.

The outcome surfaces everywhere a schedule's status does: the
Schedules page's `Last run` cell (an outcome badge — started work vs.
started nothing) with an expandable **Last fire** panel giving the
per-issue breakdown; `uzi schedule get`'s **Last fire** block (and its
`--json` `.last_fire`); and `uzi schedule run-now`'s per-candidate
summary. In the web **Last fire** panel, each row's `#<iid>` links to
the issue on the forge for the issues the fire actually fetched
(started rows, and a sweep's post-fetch skip); a candidate skipped
before it was fetched (e.g. `already_running`) shows a plain number
instead — the CLI's `Last fire` block always prints a plain number. A
sweep fire with more `uzi`-labeled issues than its [scan
window](#sweep-cap) reached that still started nothing also carries the
actionable hint — raise `max_issues` or add `uzi` to the
issues behind it.

Only the **last** fire is kept, and only the last *scheduled* one:
`last_fire` is written on the same path that advances the schedule, so
a **parked** schedule (bad repo or config) or a fire that hit a
transient error (retried next tick, see `fetch_failed` above) leaves
`last_fire` untouched — it shows whatever fired before, or nothing. A
`run-now` fire reports its own outcome in the response without
touching `last_fire` at all, since a manual fire must not disturb the
cadence. A schedule that has never fired reads `last_fire: null`.

## Default jobs

uzi ships a small **catalog of built-in default jobs** — eight generic,
repo-agnostic schedules covering the standing automations most projects want
from day one: a weekly test-improvement pass, a weekly docs-hygiene sweep, a
deep bug-hunt audit, a feature-brainstorm prompt, daily sweeps over the
`bug` and `Planned` labels and over issues **assigned to the uzi-bot
account** (`assigned-sweep`), and self-improvement — an autonomous audit of
the enabled repo's own codebase that picks one top improvement. Each has a
baked cron cadence and, for the four prompt
jobs, a baked prompt; two of the three sweeps instead carry a baked label
selector, and the third (`assigned-sweep`) carries the non-label "assigned"
selector kind instead (see [Targets](#targets) above); self-improvement
carries neither, since its directive is baked into the worker rather than
the catalog — its entry is cadence and model only (see
[Self-improvement](#self-improvement) below). You don't write these — you
**enable** them.

- **Enable per repo.** Enabling a default on a repo creates a real schedule —
  the same kind `schedule create` makes — stamped as a default rather than a
  custom one. Its prompt (or sweep selector) is **read-only**: it's resolved
  from the shipped catalog every time the job fires, not copied onto your
  schedule, so a prompt improvement uzi ships later reaches every repo that
  already enabled the job, automatically, with nothing to re-enable. Cadence,
  model, and the run options (auto-approve, wait-on-limit, max issues, and
  whether the model is also applied to agents — the "apply model also to
  agents" toggle, `override_subagent_model`) are yours to edit like any
  schedule — as is owner **guidance** on a prompt-target or sweep-target
  default (on a sweep default it is an overlay composed onto the read-only
  baked catalog guidance); a **Reset to default** action puts an edited
  default back to the catalog's cadence/model/options in one step — including
  clearing `override_subagent_model` back to its catalog baseline of `false`
  — and also clears any owner guidance you added to a prompt or sweep
  default.
- **Enable on several repos at once.** Enabling a default (or creating a
  custom schedule) against multiple repos creates one independent schedule
  per repo — each with its own cadence, its own pause/resume, its own run
  history — rather than one schedule shared across repos.
- **Clone to make it your own.** Cloning any job — default or custom — makes
  a fully editable copy. Cloning a default **unlocks the prompt**: the baked
  text is copied onto the new schedule as ordinary, editable content, and it
  stops tracking the catalog (a later catalog update no longer reaches it).
  Cloning a **sweep** default instead copies the read-only baked catalog
  guidance into the new row's editable guidance, without carrying over any
  owner guidance overlay you had added. Cloning into a different repo than
  the source is how you replicate a schedule across repos.
- **Auto-approve, on by default.** Like any new schedule, a default is
  created with auto-approve and wait-on-limit both on — the point of a
  default is that it runs unattended off-hours. Every default job only opens
  merge requests; nothing it does ever merges on its own, so an unattended
  run is safe to leave running. Each prompt job opens a merge request when it
  produces a change: docs hygiene lands its mechanical fixes (broken links,
  stale refs, frontmatter only — never prose rewrites, `CLAUDE.md`, or
  anything under `.claude/`), weekly test improvement lands new tests (test
  files only, no production code), bug hunt lands one focused fix for its
  single highest-confidence bug, and feature bingo lands an idea file (an
  MR titled `bingo: <feature>`). A run that commits nothing opens no branch or
  empty MR either (issue #341), so a quiet week produces no off-hours MR noise,
  and every job falls back to a plain report when it has nothing worth landing.
- **Sweep-label guardrail.** Enabling one of the two label-selector sweep
  defaults (or creating or editing a label-selector sweep schedule) checks
  whether its selector label actually exists on the target repo, and offers
  to create it if not — see [the sweep-label guardrail
  below](#sweep-label-guardrail). It's advisory: it warns, never blocks. The
  `assigned-sweep` default has no label to check — its selector is bot
  assignment — so this guardrail doesn't apply to it.
- **Where to manage them.** **Web**: the Schedules page has a **Default
  jobs** tab alongside **My schedules** — default rows carry a lock marker
  on the baked prompt, with Reset and Clone actions (no separate `DEFAULT`
  badge; the tab header already says so); a default enabled on several repos
  shows as one expandable summary row over its per-repo schedules, the same
  grouped look a multi-repo [custom
  schedule](#running-a-schedule-on-several-repos) gets. **CLI**: `uzi
  schedule catalog list` shows the catalog
  and how many of your repos already run each entry; `uzi schedule catalog
  enable <slug> --repo <id>` enables one (repeatable `--repo` for several
  repos at once); `uzi schedule reset <id>` and `uzi schedule clone <id>`
  work as described above — see [the CLI reference](./cli.md#commands) for
  the full flag list.

### Self-improvement

The `self-improve` job is a **promptless** catalog entry: rather than a baked
prompt or label selector, its directive is baked into the worker, so the
catalog only carries a cadence (every two days by default) and a model. Any
user can enable it on a repo they own — no admin gate. By default it is
**generic**: "review this project's codebase and pick one top improvement."
Each cycle:

- Audits the target repo. Files or reuses a `uzi-self-improve`-labelled
  tracking issue on the repo, and runs against it.
- Branches **fresh off current main** for that cycle
  (`uzi/self-improve/<run-id>`) rather than reusing one long-lived branch, so
  the agent always plans against the current tree instead of an
  ever-more-stale base, and opens its own merge request for the cycle.
- If the repo already has other open self-improvement merge requests, the
  run sees what they propose and is instructed to pick a non-overlapping
  improvement, so concurrent cycles don't duplicate or race each other.

**Concurrent-open-MR cap.** At most **2** self-improvement merge requests can
be open on a repo at once (a fixed default, not per-schedule configurable).
Once that many are open, further cycles are skipped — with a notification —
until you merge or close one; nothing piles onto an existing branch and
nothing is silently dropped.

**uzi dogfooding is an explicit, per-repo capability**, not automatic
behavior tied to any particular repo. An owner can flag a repo to fold their
own accumulated ["improve uzi"](./judge.md) recommendations from the run
judge into the cycle (a recommendation picked up this way is marked
addressed, and an owner with none still gets a plain codebase review) and to
run uzi's own trusted directive and test-suite checks instead of the generic
ones. This is off by default for a new repo; if you already had a
self-improve schedule before this capability existed, it was turned on for
you automatically so nothing changed for you on upgrade. Because the
"improve uzi" backlog belongs to you, the owner, and not to a specific
repo, flag **exactly one** repo for it — flagging two would have them share
(and race to mark addressed) the same backlog.

**Upgrading from the old fixed-branch model?** If your repo has a
pre-existing, long-lived `uzi/self-improve` merge request from before this
change, close it once its work has been superseded by later cycles — an
old, still-open MR occupies one of your two cap slots until you do.

Same guardrails as any other run: `main` is never written, and a human
always reviews and merges the MR. See
[ADR-686](../adr/0686-generalize-self-improve.md) for the design rationale
behind the capability flag and the branch/cap model.

### Sweep-label guardrail

A label-sweep schedule only matches issues carrying its selector label, so a
sweep whose label doesn't exist on the repo silently matches nothing. Enabling
a sweep default, or creating or editing a sweep schedule's label selector,
checks the target repo's labels first: a missing one is flagged (a `WARNING`
on the CLI, a confirm prompt in the web) with the option to create it on the
forge on the spot. The schedule is created or saved either way — the check
never blocks it, including when the check itself can't reach the forge (an
expired token, a rate limit, an outage) — it just means the sweep won't match
anything until the label exists.

## Restarts and missed fires

A recurring schedule survives an api restart: its next fire time is stored,
not held in memory. A fire missed while the process was down (or a slow
tick) still fires **once**, promptly, on the next wake — never a backfill of
every cadence missed. A one-time schedule left overdue by a restart fires
once, the same way, then goes terminal.

## Auto-provisioning a worker for an unmet capability

This is a different kind of automatic than the rest of this page — it isn't
about *when* a run fires, it's about a queued run that can't find a worker to
claim it at all. See [Capability-aware scheduling](./capability-scheduling.md)
for the "no eligible worker" mechanics this builds on.

Normally, if a run needs a capability (`docker` or `jvm`) that none of your
online workers has, it stays `queued` — not failed — waiting for an eligible
worker to claim it, surfaced with a "no eligible worker" health reason (see
[Capability-aware scheduling](./capability-scheduling.md#match-what-a-worker-advertises)).
It claims as soon as you start, or already have, a capable worker online.
With this on, uzi instead spins up a throwaway worker just for that one run.

**Turning it on** takes two switches, both off by default: an admin enables
the feature instance-wide from **Admin → Settings**, then you opt in from
your own **Workers page** (Settings → Workers), in the hosted-worker
section — that per-user toggle only appears there once the admin switch is
on. A per-user cap also bounds how many throwaway workers you can have
running at once, so one busy stretch can't spin up an unbounded fleet.

**What you'll see:** the run keeps showing its existing "no eligible worker"
[health reason](./run-health.md) while the throwaway worker cold-starts.
Once it's online it claims the run — and only that run — works it like any
other worker, then disappears: the worker is dropped and its pod is gone on
the next controller poll. While it exists, it's marked with an `ephemeral`
badge in your fleet list on the Workers page, so you can tell it apart from
a hosted worker you provisioned yourself.

**Caveats, honestly:**

- **Cold-start latency.** A fresh worker has to provision its volumes and
  boot before it can claim anything, so the run can sit for several minutes
  before it actually starts — this is not instant capacity.
- **Cost.** Every throwaway worker pays that same fixed cold-start cost, no
  matter how small the run turns out to be. That's why this is opt-in and
  capped instead of on by default; whether a small pool of already-warm
  workers would suit you better is a tradeoff left to you for now.
- **A real worker of yours can win the race.** If one of your own eligible
  workers comes online while the throwaway one is still cold-starting, it
  can claim the run first. That's harmless — the now-idle throwaway worker
  gets cleaned up shortly after.
