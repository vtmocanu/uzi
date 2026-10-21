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
- **CI watch** (`ci_watch`) — no issue and no prompt of your own. At fire
  time it looks at the repo's **default-branch** CI and, when a workflow is
  red, starts one repair run. It is the target behind the `ci-repair`
  [default job](#default-branch-ci-repair); see that section for what a fire
  does and which limits apply.

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
bot-assigned is filtered out before the scan window and is never a
candidate; the fire counts it in `ineligible_matched` (see
[Fire outcomes](#fire-outcomes)). The `assigned-sweep` default sidesteps
this pairing by construction: its selector *is* the eligibility signal, so
every candidate it matches is already eligible. The **ad-hoc prompt** target
is the deliberate exception: with no issue to gate on, it bypasses the
eligibility requirement by design.

Each schedule also has its own **auto-approve** toggle, **on by default**,
including the assigned-sweep default. Issue-target fires also assess the
issue author and comments before honoring that request — see [Issue input
and the approval gate](#issue-input-and-the-approval-gate). Turn the toggle
off to have runs wait for a human, same as a manual start. For an owner who
opted in to [Plan cross-check](./cross-check.md), an eligible auto-approved
schedule run must pass that gate before implementing. A Claude lead proceeds
after a Codex APPROVE, and a Codex lead after a Claude APPROVE, of the latest exact plan; eligible changes requested
receive bounded automatic revisions. Exhaustion, blockers and check failures
normally park for a human decision even with auto-approve on. A Codex lead on
a worker without `cross_check_codex_lead_v1` parks for a human too.
Irrecoverable delivery losses fail the run; see
[Cross-check](./cross-check.md#terminal-delivery-failures). A semantic run
(dormant until #1231; production runs are not enrolled) also needs complete
source coverage and no maintainer criteria, or an auto-approved schedule run
parks for a human instead; see
[Autopilot](./autopilot.md#semantic-runs-park-instead-of-auto-approving-dormant).
The plan is
recorded either way, and a human still merges the resulting MR; the
[main-branch guardrails](../ARCHITECTURE.md#guardrail-layers-the-primary-directive)
apply independently of schedule approval.

A schedule also has its own **wait-on-limit** toggle, **on by default** for a
new schedule: a fired run parks until the Anthropic usage window reopens
instead of failing — the right behavior for something unattended, typically
firing off-hours. This takes effect for auto-approve schedules too (the
default case), not just the ones that stop at the plan gate. Turn it off for
a given schedule to have a fired run fail outright on a usage limit instead
of waiting. An existing schedule keeps whatever it already had — this is a
create-time default, not a retroactive change.

## Issue input and the approval gate

Pinned-issue, label-sweep, and assigned-sweep fires requesting auto-approve
start with a server-side author assessment. An issue author below the
repository input threshold, withheld comments, or unknown permissions
(including a comments-fetch failure) disables auto-approve **for that run**.
The run still queues and plans normally, then parks at `awaiting_approval`.
Known, wholly eligible input keeps auto-approve. This does not change the
schedule's saved toggle or its ON default.

The CLI and web run detail show creation-time assessment history, with the
applicable `auto_approve_blocked_reasons`: `author_not_eligible`,
`permission_unknown`, or both. These are approval reasons, not run failures,
health alerts, or recovery holds. Later comments do not flip the stored
approval flag; live reads still filter comments and can show eligible late
comments. Explicit auto-approve OFF, manual starts, autopilot, MR rework,
self-improve, CI fixes, prompt schedules, and job approval flags retain
their existing behavior.

The input thresholds use stable numeric identities: GitHub effective
Triage/Push/Maintain/Admin from a complete all-affiliations collaborator
list; GitLab active inherited membership at Reporter (20) or above, excluding
Planner (15) and custom Guest roles below 20; Forgejo proven personal owners,
direct collaborators including Read, or effective organization-team
Write/Admin/Owner, including the Owners team. Forgejo uses the existing
token; a Write bot forbidden to look up an arbitrary user's effective access
produces unknown. Custom-role names, author associations, team-member lists,
and assignees do not establish authorization, and no new scopes are needed.

Each capture/live read has a 30-second child deadline starting before the
issue fetch and a 200-distinct-author budget with an operation-local cache.
Missing identity, errors, incomplete evidence, deadline or budget exhaustion
produce unknown. A valid raw capture can persist after child expiry under a
valid parent, with auto-approval disabled; no raw capture means no new run.
The complete comment thread is assessed before its newest 200 / 32 KiB body
tail is retained oldest-first. Older withheld comments still affect approval.
Only uzi's own bot is excluded by stable ID (GitLab system notes are already
dropped); other bots face the author threshold. An unknown bot ID omits
comments and disables requested scheduled auto-approval. Withheld bodies become fixed
author/time/reason placeholders, not newly stored or delivered raw comments.

Every issue run saves its target title/body once, regardless of author
access; owner guidance stays separate from the raw-field digest. Resume and
reclaim retain those fields, and a new run captures current fields. Target
`get_issue` reads and matching list titles keep the saved fields even after
an edit, promotion, or approval. Changes produce a fixed changed-content
note, not changed text or a diff; unavailable comparison or current metadata
does not replace saved fields. Other-issue live fields require an eligible
current author, which **does not establish eligible editors**: GitLab
assignees and Planner users can edit title/body, and editor attribution is
not implemented. Other list summaries are unchanged.

Use `uzi run revise` at the plan gate and `uzi run follow-up` during
implementation (read on the next ordinary turn) to steer the run.
Nonce fences resist delimiter/header spoofing, but eligible comments or a
frozen outsider's task can still steer a plan; human review cannot guarantee
injection detection or prevent planning-time exfiltration. Until the API
and workers are upgraded, turn auto-approve **OFF** for public-repository
issue-target schedules. Upgrades do not retract old transcript text or
rewrite existing approvals/flags; cancel and start a new run for clean
context. The time/author limits do not bound decoded memory: oversized or
chunked GitHub/GitLab responses remain deferred hardening. See the
[architecture boundary](../ARCHITECTURE.md#issue-comments-as-untrusted-worker-input-prd-381)
for prompt compatibility, forge evidence, and egress residuals.

## Sweep cap

A label sweep also has a **max issues per fire**, applied oldest issue first
(lowest issue number). Candidates are filtered down to **eligible** issues —
carrying the configured `uzi` label, or assigned to the uzi-bot account —
*before* the cap is applied, so an ineligible selector match (a `bug`-labeled
issue with neither the `uzi` label nor a bot assignment) is never fetched and
never occupies a slot; it is not a candidate at all, and the sweep's whole
selector backlog is unaffected by how many of those precede the eligible
ones. The cap itself still counts runs **started**, not candidates matched:
when an eligible candidate can't start — already mid-run from a previous
fire, or a transient fetch error — the fire flags it (see [Fire
outcomes](#fire-outcomes)) and walks on to the next eligible issue, so a
stale issue at the head of the eligible backlog no longer wastes a slot or
blocks newer work. That walk is bounded by a **scan window** — the cap plus a
fixed headroom — so one fire's cost stays predictable even when the eligible
backlog is thin; if the window runs out before the cap is filled the fire
under-fills, and each skipped issue stays flagged for you to fix. A new
sweep defaults to **10**, so one fire can't fan out across an entire label's
backlog at once; raise it, or in the web modal blank the field for unlimited
(today's original behavior — the CLI always sends a cap, defaulting to 10, so
an unlimited sweep is web-only). An existing sweep created before this cap
existed stays unbounded until you set one.

### Remove the selector label on dispatch

A recurring **single-label sweep** can opt in to
`remove_label_on_dispatch` (off by default for custom schedules and unchanged
on existing schedules; the `ondeck-sweep` catalog default enables it).
Once an issue's run is **created**, uzi removes the selector label on the
forge, then applies an **atomic label delta** to the current cached issue
row. That cache write preserves unrelated labels and non-label fields,
including assignments, rather than replacing them from an older snapshot.
A skipped candidate keeps its label; removal doesn't wait for the run to
finish. The eligibility gate, active-run dedup, assignment handling and
cache-backed candidate scan windows are unchanged.

Removal requires exactly one non-blank selector label after trimming.
Pinned issues, prompt targets, one-time schedules, assigned-selector sweeps,
empty selectors and multi-label selectors cannot enable it. The configured
`uzi` eligibility label is protected: create/edit refuses it as a removal
selector, and the scheduler rechecks the **live** setting after creating
each run. If an admin has made the selector the eligibility label since the
schedule was saved, that fire leaves the label in place without recording
a removal failure.

**Best effort, not strict once-only dispatch.** A removal error keeps the
started run and records `label_remove_failed`; a successful forge and cache
write records `label_removed` (see [Fire outcomes](#fire-outcomes)). Stopping
between run creation, the forge write and the cache write, or a failed
write, can leave the selector on the forge or in the cache. A later
authoritative forge sync can restore a label in the cache, and re-adding the
same label can make the issue a candidate again. There is no strict
once-only ledger or automatic label-removal retry; active-run dedup still
protects an issue while its run is live. While the selector remains absent,
a failed issue run stays out of that sweep; there is no automatic failed-issue
retry. The owner **re-adds the selector label to retry it**. If removal
failed and the label remains, remove it by hand to prevent a later fire
from picking the issue again.

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

Guidance affects one dormant detector. On a semantic run (not enrolled in
production until #1231), source-drift observations compare the current forge
issue with the frozen issue text; a scheduled run whose frozen text was composed
with guidance is compared with the raw issue body saved at creation instead, so
it shows **unchanged** until the issue is edited. Drift only displays and never holds the
run; see [Completion holds](./run-completion-hold.md#source-drift-is-a-detector-only).

## Proposal output: MR or issues

A prompt-target schedule that *proposes* work — a brainstorm or a survey that
suggests something a maintainer might pick up, like feature bingo, the
refactor scout or the CI scout — has an **output mode**: `mr` or `issues`. Without a
catalog output setting, the default is `mr`; feature bingo, refactor scout and
CI scout ship with `issues`. It is **prompt-target only**: a pinned issue or a label
sweep runs against an issue that already exists, so there is no proposal to
file, and the setting is
rejected there (HTTP 422 / a CLI usage error) rather than silently ignored.

In **`mr` mode** — the existing delivery behavior — a proposal run writes an
idea file and opens a merge request for it (feature bingo's `bingo: <feature>` MR,
the refactor scout's and CI scout's proposal files). Set `--output mr` to keep this
path for any of these catalog jobs; their prompt bodies still contain the idea-file
and MR instructions used by this mode.

In **`issues` mode**, the run produces the proposal as structured content and,
when it completes, uzi files it as a **forge issue** on the owner's connection.
The api does the writing — the same trust model as [findings](./findings.md)
and recommendation filing, so the agent never gains a forge-write tool. The
filed issue carries **exactly one** label, the scoped marker `proposal::<slug>`
(`proposal::feature-bingo` for a catalog job, a bare `proposal` for a custom
prompt schedule), and **nothing else** — never `uzi`, never a sweep selector
(`Planned`/`bug`), never a bot assignment. Because
[eligibility](./admin-settings.md#run-eligibility) is built from that label set
alone, a freshly filed proposal is **never sweep-eligible at creation**: an
unattended 02:00 sweep cannot pick it up. Promoting a proposal to real work is
a deliberate human act — add the `uzi` label (or a sweep selector), the same
promote flow any issue uses, and it becomes runnable like anything else.

**Dedup at fire time.** An issues-mode schedule does not re-file a proposal it
already filed. Each time it fires, uzi lists that schedule's existing
`proposal::<slug>` issues — open **and** closed, newest first, capped — and
appends a compact digest of them to the rendered prompt, so the run sees what
has already been proposed (and declined) and is told not to repeat it unless
the evidence materially changed.

Feature bingo, refactor scout and CI scout ship with `issues` as their catalog
output; prompt jobs without a catalog output setting fall back to `mr`. A
schedule with a stored `mr` or `issues` mode keeps that choice. An existing
catalog schedule whose stored mode is NULL inherits the new catalog value on
its next fire, with no migration of stored rows. Reset restores a catalog
job's current baseline, so resetting any of these proposal jobs adopts `issues`.
Choose `--output mr` on the CLI (`uzi schedule create`/`edit`, see [the CLI
reference](./cli.md#commands)) or in the web schedule modal to retain
idea-file and MR delivery.

## Managing schedules

- **Web**: the **Schedules** page lists your schedules, and a "Schedule…"
  entry point on an issue opens the create modal pre-pinned to it. The modal
  offers cadence presets (weekdays, every day, every week, every N hours,
  every N minutes) plus an
  advanced raw-cron field, and a live "next fires" preview. Pause, resume,
  and run-now are per-schedule row actions on the list.
- **CLI**: `uzi schedule create | edit | list | get | pause | resume | run-now |
  delete` — see [the CLI reference](./cli.md#commands) for the full flag list.

### Every N minutes

The **Every N minutes** preset accepts **1, 2, 3, 4, 5, 6, 10, 12, 15, 20,
30** and produces `*/N * * * *`. Those values divide 60, giving evenly spaced
fires across the hour boundary. These cron expressions reopen as the same
minutes preset. Other minute steps stay **Custom (cron)**: `*/40 * * * *`,
for example, fires at minutes 0 and 40, alternating 40- and 20-minute gaps.

### Configuring dispatch label removal

Use `uzi schedule create --sweep --label Planned --remove-label-on-dispatch`
with the usual repo and recurring timing flags, or
`uzi schedule edit <id> --remove-label-on-dispatch` to enable it on an
existing single-label sweep. Pass `--remove-label-on-dispatch=false` to turn
it off. Unrelated edits preserve the setting. `uzi schedule get` prints
`REMOVE_LABEL_ON_DISPATCH`, and both `get` and `run-now` describe removal on
each started run (see [Fire outcomes](#fire-outcomes)).

The API exposes `remove_label_on_dispatch` as a boolean on the schedule DTO
and create/PATCH requests. Omission on create means false; omission on PATCH
preserves the stored value, and explicit false disables it. Enabling an
unsupported configuration returns HTTP 400. This is also an owner-editable
field on a default-origin schedule whose effective catalog selector meets
the same single-label constraints. Clone and Add repo copy it; Reset restores
the catalog's removal value (`true` for `ondeck-sweep`, `false` for older
catalog entries). The catalog-owned selector itself stays read-only.

### Which harness a schedule runs on

A schedule either **pins** a harness (Claude or Codex) or leaves it
**unpinned**. Pin it with the modal's **Harness (optional)** picker (shown
only when you hold a usable credential for both harnesses) or with
`uzi schedule create/edit --harness claude|codex`; `--harness ""` clears the
pin. Its default option is "Use my default" — leave it there to resolve the
harness automatically at fire time.

For **automatic issue starts**, exact forge labels `harness::codex` and
`harness::claude` select the harness from the fresh issue-input capture,
not the cached candidate. This applies to recurring and one-time issue
schedules, schedule run-now, and every issue selector (`bug`, `Planned`,
`on-deck`, assigned and custom sweeps), as well as
[autopilot](./autopilot.md#which-harness-autopilot-uses). Manual web/CLI run
creation and chat `start_run` keep their existing selection; prompt and
self-improve schedules do not consume issue harness labels.

The precedence inside an issue fire is:

| Fresh issue labels | Schedule harness pin | Result |
|---|---|---|
| No `harness::*` label | None | Existing automatic resolution (D11): usable user default, otherwise a usable harness, Claude if both. |
| No `harness::*` label | Claude or Codex | Use the stored pin, without cross-harness fallback. |
| Exactly `harness::claude` or `harness::codex` | None | Explicitly select that harness, without credential fallback to the other. |
| One recognized label | Matching pin | Use the pin. |
| One recognized label | Differing pin | Start nothing; `harness_label_conflict`. |
| Both recognized labels, or any other `harness::*` label | Any or none | Start nothing; `harness_label_invalid`, before pin comparison. |

Repeated copies of the same recognized label are allowed. Label selection
is per candidate and does not save a schedule pin. A stored Anthropic
credential override with label-selected Codex gives
`codex_override_conflict`; label selection does not gain the actual-pin
exemption. The existing stored-model compatibility fallback (D6) is unchanged:
harness selection does not merge the separate Claude and Codex model defaults.

**The one-time stored-credential precheck comes first.** Before fetching
forge input, it checks the stored token override and schedule harness pin,
not a label-selected candidate. A disabled stored choice can therefore hold
the schedule before labels are fetched or validated. If creation reaches
the authoritative stored-override guard, a disabled override retains its
existing precedence.

On an **unpinned sweep**, a label-selected harness with disabled credentials
records `credential_disabled` for that candidate; a missing or unusable
selected credential records `no_usable_credential`. The sweep continues
within its existing scan window and start cap, without cross-harness
fallback. A disabled actual schedule pin or stored token override retains
the existing one-time hold before any start, or stop-and-advance after a
partial start. If resolution refuses before the stored-override insert
check, the scheduler rechecks the stored token alone to preserve that
classification when it was disabled after preflight.

For a **one-time single issue**, a disabled label-selected harness holds
and resumes on re-enable like a pin. Merely missing or unusable credentials
create no new disable hold: explicit `no_credential_for_harness` remains a
hard refusal retried each tick; implicit `no_usable_credential` remains a
benign advancing skip. Recurring credential semantics are unchanged.
A single-issue or prompt fire explicitly selecting a harness with a missing
credential retries without recording an outcome; an actual pinned sweep
buckets that refusal as `fetch_failed` and continues.

Invalid or conflicting labels are advancing skips on tick-driven one-time
fires; sweeps continue to the next candidate. Run-now reports skips without
persisting cadence, last-fire or recent-fire changes. Each started run
records its actual harness. See [Fire outcomes](#fire-outcomes) for the
labels and repair hints shown in web and CLI.

### Running a schedule on several repos

Creating a custom schedule can target more than one repo at once: the "New
schedule" modal's repo picker is a multi-select, and picking N repos creates
**N independent schedules** — "N repos → N schedules" — one per repo, each
with its own cadence, pause/resume, and run history. Editing an existing
schedule still targets exactly one repo, unchanged; the multi-select is a
create-time affordance only.

Schedules created together this way share a **display-only** group id
server-side, but the **Schedules** tab renders each sibling as its own
independent row — no expandable summary, no sub-rows — the same as a
default enabled on several repos (see [Default jobs](#default-jobs)
below). The list does not group siblings: it orders rows by status and
next fire, so siblings sit together only when they share a fire time. For
a default job, a catalog card's "Enabled on N repos" link filters the list
to every repo that job runs on. Custom siblings have no job filter; the
Repo select narrows the list to one repo.

**The grouping is data only, not a linked job.** Each sibling is its own
independent schedule row: editing, pausing, or removing one never touches
the others, and an edit on one never propagates to the rest of the
group.

**Add to another repo**, a row's More actions menu item, extends that same
job onto one more repo you own as a new sibling, without reopening the
create modal and re-entering everything. If the schedule already has a
sibling on that repo, this is a clean no-op rather than an error. A
multi-repo create also runs the [sweep-label
guardrail](#sweep-label-guardrail) once per selected repo, same as any
other sweep schedule.

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
<id>` is the CLI twin of "Add to another repo" — see [the CLI
reference](./cli.md#commands) for the full flag list.

### Pausing everything at once

A single **user-level** switch pauses every schedule you own, on every
repo, catalog defaults and your own alike — the "tonight I want nothing to
run" case, without hand-pausing each row and remembering which ones were on
when you resume. It takes an optional **auto-resume instant**: pass
`--until` (a preset in the web picker, or the CLI flag) and the pause lifts
itself at that time with no manual action and no background job — an
expired `until` simply reads as not-paused the next time anything checks
it. Omit an end time (`--until never`, or the "Until I resume" preset) to
stay paused until you resume by hand.

A **recurring** schedule due while paused records a benign
[`schedules_paused` skip](#fire-outcomes) and **advances its cadence
normally**, exactly as if it had fired — so resuming never replays a burst
of missed fires, and its `Last run` shows why nothing started instead of
looking dead. A **one-time** schedule due while paused is left alone: it
stays overdue and fires once, on the first tick after the pause ends, the
same as a [restart-missed one-time schedule](#restarts-and-missed-fires).
**`Run now` bypasses the switch** — a manual fire is an explicit act, not
an unattended one — and a run already in flight when you pause is never
touched. Pausing and resuming never write any schedule's own `enabled`
value, so resuming restores the **exact** set of schedules that were on
before.

Three surfaces read and drive the same state: the **Schedules** page (a
"Pause all" control, an inline picker for the presets, and a paused banner
while it's active), the **CLI** (`uzi schedule pause-all --until <when>` /
`resume-all` / `pause-status` — see [the CLI
reference](./cli.md#commands)), and a paused fire's **Last fire** record
(and a **Recent fires** entry),
which carries the `schedules_paused` skip reason.

## Fire outcomes

A schedule can fire right on time and still start **zero** runs — every
candidate can be benign-skipped by the same gate a manual start goes
through. The motivating case: a `bug` label sweep whose oldest matches all
lack the `uzi` label and aren't assigned to the uzi-bot — those matches are
never eligible, so they are filtered out before the scan window is even
cut and simply never become candidates; [backfill](#sweep-cap) then walks
the eligible backlog to start any runnable issue in reach. When there is no
eligible issue at all, the fire runs every night, `Last run` keeps
advancing, and nothing ever starts. Without a fire outcome, that looks
identical to a healthy schedule — which is why a label sweep also reports
`ineligible_matched` (below): how many of the selector's matches were
filtered out, so a run of `uzi`-less issues is visible instead of silent.

Each fire records how many candidates it **examined** (attempted — this can
exceed `max_issues` once backfill walks past a skip), which ones
**started** (paired with the run they produced), and which were
**skipped**, each with a typed reason — never free text:

- `not_eligible` — the candidate carried neither the `uzi` label nor a
  bot assignment when uzi went to start its run, so the eligibility gate
  refused it. On a label sweep this is now rare: an
  ineligible selector match is filtered out before it's ever a candidate
  (see [Sweep cap](#sweep-cap)), so this reason only fires on a race —
  eligibility changed between candidate selection and run creation — or on
  a pinned-issue fire, where no upfront filter applies. It's benign, and
  the schedule advances normally.
- `already_running` — an active run already exists for that issue, or a
  CI-fix run (or, once its MR has closed, an MR-rework run) is working its
  branch (or, for the schedule itself, a dedup at fire time). For a
  one-time issue schedule, a CI-fix or MR-rework run on the branch holds
  the fire instead: no run starts and nothing is recorded, the schedule
  stays active and due, and it fires once that run releases the branch.
- `description_too_large` — the composed run instruction (issue body
  plus any [guidance](#guidance)) exceeds the size limit.
- `fetch_failed` — a transient forge or database error while checking
  a sweep candidate. The same underlying error is handled differently
  by target: on a **pinned issue**, it's transient and the fire retries
  next tick with nothing recorded; on a **sweep**, one bad candidate
  can't stall the rest of the fan-out, so that candidate is bucketed
  `fetch_failed` and the sweep continues.
- `schedules_paused` — the owner's user-level [pause
  all](#pausing-everything-at-once) was active when this recurring fire
  came due, so nothing started. It's benign and self-resolving: the
  schedule's cadence advances normally and re-fires on schedule next time,
  and nothing replays on resume.
- `harness_label_invalid` — both recognized harness labels, or an unknown
  `harness::*` value, appeared in the fresh issue capture. Keep one of
  `harness::codex` or `harness::claude` and remove other harness labels.
  This validation precedes pin comparison inside the fire.
- `harness_label_conflict` — one valid issue harness label differs from
  the schedule pin. Match the label to the pin, or change/clear the pin.
  Both label skips start nothing and advance tick-driven fires, including
  one-time schedules; sweeps continue.
- `no_usable_credential` — implicit automatic resolution found neither
  harness usable, or an unpinned sweep's label-selected harness has a
  missing or unusable credential. The web label is "no usable credential".
  It's benign: the schedule advances; repair credentials for the selected
  harness when a label chose it, since there is no cross-harness fallback. This
  reason is recorded (and the schedule advances) for issue, sweep, and
  prompt fires; the `self-improve` [default job](#default-jobs) is the one
  exception — there, the same condition instead leaves the fire unrecorded
  and non-advancing, retrying each tick, the same as any other pinned
  harness with no usable credential (see [Which harness a schedule runs
  on](#which-harness-a-schedule-runs-on)).
- `codex_override_conflict` — the schedule is unpinned but carries a stored
  Anthropic credential override, and automatic resolution or the issue's
  `harness::codex` label selects Codex. The override can't ride a Codex run
  and there's no cross-harness fallback, so the fire starts nothing and leaves the stored override
  untouched. Benign: the schedule advances and re-fires next cadence, and
  may resolve back to Claude (or the override may be changed) by then.
- `credential_disabled` — a stored token is
  [disabled](anthropic-token.md#disabling-a-token), or the selected harness
  has credentials but none enabled. The affected candidate starts nothing;
  uzi keeps the stored choice without credential or cross-harness fallback.
  Recurring schedules advance and re-fire next cadence. A one-time schedule
  with a disabled stored pin/override is **held** before any start: due,
  un-advanced, with the skip recorded as its last fire, until re-enable or
  a changed stored choice. A disable after a partial one-time sweep start
  stops the scan and advances instead. A one-time single issue whose label
  selects a disabled harness holds like a pin. On an unpinned label-selected
  sweep, harness disable is a per-candidate skip and scanning continues
  within the caps; stored-pin/override rules still apply. Missing credentials
  do not acquire a disable hold. The web shows "pinned credential is disabled";
  `uzi schedule get` prints the same label with a Settings hint.
- `config_not_supported` — a removal-enabled schedule's effective target,
  timing or selector no longer supports removal (for example, a catalog
  selector changed to multiple labels). The fire starts nothing; an invalid
  selector is rejected before listing candidates.

- `ci_green`, `ci_already_handled`, `ci_halted_no_progress`,
  `ci_halted_attempt_cap`, `ci_watched_not_seen`, `ci_not_failed`,
  `ci_unsupported`, `ci_capacity`, `ci_evidence_limited`, `ci_config_changed`
  and `no_default_branch` — the outcomes of a [CI-watch
  fire](#default-branch-ci-repair); each is defined in [its skip
  reasons](#ci-watch-skip-reasons).

Started entries can also carry **label-removal outcomes**; these are not
skip reasons, since the run already exists:

- `label_removed: true` — the selector was removed on the forge and from the
  cached issue.
- `label_remove_failed: true` — removal failed, but the run still started
  and counts toward the cap. The sweep continues to later candidates within
  its existing scan window.

These fields appear on `last_fire.started[]` and the run-now response's
`started[]`, with false values omitted. An optional `selector_label` stores
the **fire-time selector snapshot**, so later schedule edits do not rename
historical removal outcomes. `uzi schedule get` and `run-now` use that
snapshot for messages such as "Planned removed" or "Planned could not be
removed (the run started; remove it by hand)". Legacy entries without a
snapshot use the generic word "label", rather than the schedule's current
selector. Absence of both removal flags makes no claim that removal
succeeded; for example, the live eligibility-label protection leaves both
unset.

`examined == started + skipped` always holds — every candidate the fire
reaches lands in exactly one bucket, so the tally never silently drops one.

The outcome surfaces everywhere a schedule's status does: the
Schedules page's `Last run` cell (an outcome badge — started work vs.
started nothing) with an expandable panel giving the per-issue breakdown
(labelled **Recent fires** once the schedule has recent fires, **Last fire**
otherwise; see [Recent fires](#recent-fires)); `uzi schedule
get`'s **Last fire** and **Recent fires** blocks (and their `--json`
`.last_fire` and `.recent_fires`); and `uzi schedule run-now`'s
per-candidate summary. In the web per-issue breakdown, each row's `#<iid>` links to
the issue on the forge for the issues the fire actually fetched
(started rows, and a sweep's post-fetch skip); a candidate skipped
before it was fetched (e.g. `already_running`) shows a plain number
instead — the CLI's `Last fire` block always prints a plain number. A
capped sweep fire with more **eligible** issues than its [scan
window](#sweep-cap) reached that still started nothing also carries a hint
that newer eligible issues weren't reached, pointing back at the skip
reasons above for why the candidates ahead of them were skipped — it no
longer suggests raising `max_issues`: the fix is to clear those skips, and a
higher cap also raises how many runs one fire can start.

A label sweep's fire also reports `ineligible_matched`: the number of open
issues that match the sweep's selector but aren't eligible (neither
carrying the configured `uzi` label nor assigned to the bot), counted over
the sweep's **whole** selector backlog, not just the scan window that was
reached. This is the diagnostic for the case above: it tells you a fire
that started nothing (or fewer than expected) is being starved by
`uzi`-less matches, and roughly how many there are, without waiting for
[backfill](#sweep-cap) to walk past each one individually. It's shown in
the Schedules list's **Last run** badge when the fire started nothing
("0 started · N not eligible", with the skip count between them when the
fire also skipped candidates), and the Schedules page's panel, but only
while the schedule has no recent fires or when an entry in `recent_fires`
itself carries the count: the web shows `recent_fires[0]` whenever the list
is non-empty, and a fire whose selector matched only ineligible issues has
`matched` 0, so it is never added to that list. It is also in `uzi schedule get`'s **Last fire** block, `uzi schedule run-now`'s
summary, and `--json` (`.last_fire.ineligible_matched` and `run-now`'s
`ineligible_matched`). It's **absent, not zero, on a fire from before this
count existed and on an assigned-selector sweep** (an assigned candidate is
eligible by construction, so the count doesn't apply) — read a missing key
as unknown, never as "no ineligible matches."

Two records are kept, and both hold only *scheduled* fires:
`last_fire` is written on the same path that advances the schedule, so
a **parked** schedule (bad repo or config) or a fire that hit a
transient error (retried next tick, see `fetch_failed` above) leaves
`last_fire` untouched — it shows whatever fired before, or nothing. The
one exception is a one-time schedule held on a disabled credential (see
`credential_disabled` above, including a label-selected single issue,
but not a per-candidate unpinned sweep skip): it records that skip as its
last fire without advancing, so it can show a `last_fire` while still due. A
`run-now` fire reports its own outcome in the response without
touching `last_fire` at all, since a manual fire must not disturb the
cadence. A schedule that has never fired and was never held that way
reads `last_fire: null`.

### Recent fires

Alongside `last_fire`, a schedule keeps `recent_fires`: the last 10
scheduled fires that **did something**, newest first, each in the same
shape as `last_fire`. "Did something" means `matched > 0`, so the fire
started a run or recorded a skip. It is never appended for a
capacity-blocked tick (the "waiting for room" tick, matched 0), a tick
that examined 0 candidates, or a fire whose summary failed to serialize.
`last_fire` is unchanged: it is still the latest persisted tick, so it can
differ from `recent_fires[0]` (for example, when the latest tick was
blocked on capacity, or matched 0 because nothing was eligible). The web
shows only a blocked latest tick (the "Waiting for room at" line and a
panel note), not an examined-0 one. The parked and transient paths write nothing to
either field, and `run-now` records nothing in either. A one-time schedule
held on a disabled credential appends its hold once, not once per tick,
because the hold only writes when the stored summary differs.

Skips count, so a schedule-wide skip also takes a slot: the `schedules_paused`
skip recorded on every tick while all schedules are paused, or a persistent
`already_running` skip, can push older dispatches out of the 10 over a
long pause. Existing schedules were backfilled on upgrade with their
current `last_fire` when it matched > 0.

`GET` on a schedule returns `recent_fires` as an always-present array
(`[]` when none); an older server lacks the field. On the Schedules page,
a non-empty `recent_fires` makes the **Last run** cell show the newest
recent fire's outcome badge and stamp instead of `last_fire`'s, and, when
the latest tick was capacity-blocked, a warning-coloured
"Waiting for room at `<stamp>`" line. The **Recent fires** panel lists entries newest
first: the newest is expanded with the per-issue breakdown, older ones are
collapsed headers (stamp, outcome badge, examined/started/skipped counts)
that expand to the same breakdown, and a blocked latest tick adds a note
first naming its time and its in-flight/limit/room numbers. With no
recent fires, the panel is the plain **Last fire** panel. `uzi schedule
get` prints a **Recent fires** block after **Last fire**
([CLI reference](./cli.md#commands)).

## Default jobs

uzi ships a small **catalog of built-in default jobs** — twelve generic,
repo-agnostic schedules covering the standing automations most projects want
from day one: a weekly test-improvement pass, a weekly docs-hygiene sweep, a
deep bug-hunt audit, a feature-brainstorm prompt, a biweekly propose-only
refactoring scout that surveys for one structural refactor and files a
proposal issue by default (never implementing it, `refactor-scout`), a
weekly [CI scout](#ci-scout) that reads the repo's recent CI history and files
one measured CI improvement proposal (`ci-scout`), daily
sweeps over the `bug` and `Planned` labels and over issues **assigned to the uzi-bot
account** (`assigned-sweep`), an On-deck backlog drain every 10 minutes
(`ondeck-sweep`), and self-improvement — an autonomous audit of
the enabled repo's own codebase that picks one top improvement, and a
default-branch CI repair job (`ci-repair`, hourly, covered in
[Default-branch CI repair](#default-branch-ci-repair)). Each has a
baked cron cadence and, for the six prompt
jobs, a baked prompt; three of the four sweeps instead carry a baked label
selector, and the fourth (`assigned-sweep`) carries the non-label "assigned"
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
  model, and the run options (auto-approve, wait-on-limit, max issues,
  capacity limit and room needed, selector-label removal, and
  whether the model is also applied to agents — the "apply model also to
  agents" toggle, `override_subagent_model`) are yours to edit like any
  schedule — as is owner **guidance** on a prompt-target or sweep-target
  default (on a sweep default it is an overlay composed onto the read-only
  baked catalog guidance); a **Reset** action (shown only on a customized row)
  restores cron, timezone, model,
  auto-approve, wait-on-limit, max issues, capacity limit and room needed,
  selector-label removal and output mode to the catalog
  values in one step, sets `override_subagent_model` back to its catalog
  baseline of `false`, and clears MR rework, owner guidance, the harness pin
  and the credential override back to inherit. It also re-activates a parked
  schedule on its catalog cadence (a schedule you had paused stays paused).
  The catalog-owned prompt and labels are never touched by Reset.
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
- **Auto-approve, on by default (except `ci-repair`).** Like any new
  schedule, a default is created with auto-approve and wait-on-limit both on
  — the point of a default is that it runs unattended off-hours. The one
  exception is `ci-repair`, which is created with auto-approve **off** (see
  [Default-branch CI repair](#default-branch-ci-repair)); every other default
  keeps it on. Issue-target defaults,
  including assigned-sweep, still apply the [issue-input approval
  gate](#issue-input-and-the-approval-gate) per run; prompt defaults keep
  their existing approval behavior. Nothing a default job does merges on
  its own. In their default `issues` mode, feature bingo, refactor scout
  and CI scout file proposal issues; `--output mr` retains their idea-file
  and MR path. The other prompt jobs open merge requests when they produce a
  change: docs hygiene lands its mechanical fixes (broken links, stale refs,
  frontmatter only — never prose rewrites, `CLAUDE.md`, or anything under
  `.claude/`), weekly test improvement lands new tests (test files only, no
  production code), and bug hunt lands one focused fix for its single
  highest-confidence bug. An MR-mode run that commits nothing opens no branch
  or empty MR either (issue #341), so a quiet week produces no off-hours MR
  noise, and each job can report when it has nothing worth landing.
- **Sweep-label guardrail.** Enabling one of the three label-selector sweep
  defaults (or creating or editing a label-selector sweep schedule) checks
  whether its selector label actually exists on the target repo, and offers
  to create it if not — see [the sweep-label guardrail
  below](#sweep-label-guardrail). It's advisory: it warns, never blocks. The
  `assigned-sweep` default has no label to check — its selector is bot
  assignment — so this guardrail doesn't apply to it.
- **Where to manage them.** **Web**: the Schedules page has two tabs,
  **Schedules** and **Job catalog**. Every schedule you have, default and
  custom alike, is one flat row on the **Schedules** tab — default rows
  carry a lock marker on the baked prompt, plus inline Run now, Edit, a
  Reset action (only when the row has been customized) and a More actions
  menu (Clone, Enable on another repo, Remove; a custom row offers Add to
  another repo in place of Enable); nothing to expand to act on
  a row, even for a default enabled on several repos — each repo is its own
  row (see [Running a schedule on several
  repos](#running-a-schedule-on-several-repos) above). **Job catalog** is for discovery: one card per built-in job,
  each with its own "Enable on…" repo picker and a status line ("Not
  enabled", or "Enabled on N repos" linking back to the Schedules tab
  filtered to that job). **CLI**: `uzi schedule catalog list` shows the
  catalog and how many of your repos already run each entry; `uzi schedule
  catalog enable <slug> --repo <id>` enables one (repeatable `--repo` for
  several repos at once); `uzi schedule reset <id>` and `uzi schedule clone
  <id>` work as described above — see [the CLI
  reference](./cli.md#commands) for the full flag list.

### On-deck sweep

**On-deck sweep** (`ondeck-sweep`) drains a triaged backlog selected by the
`on-deck` label. Its catalog baseline is:

| Setting | Value |
|---|---|
| Selector label | `on-deck` |
| Cron | `*/10 * * * *` (every 10 minutes) |
| Timezone | `UTC` |
| `max_issues` (N) | `1` |
| `capacity_limit` (C) | `4` |
| `capacity_room_needed` (K) | `2` |
| `remove_label_on_dispatch` | `true` |

Enable it with `uzi schedule catalog enable ondeck-sweep --repo <id>`, or
from its **Job catalog** card. The `on-deck` label selects work; issues still
need the configured `uzi` label or bot assignment to pass
[run eligibility](./admin-settings.md#run-eligibility). A missing `on-deck`
label triggers the advisory [sweep-label guardrail](#sweep-label-guardrail),
with an option to create it; it does not block enabling the schedule.

Each fire waits for at least two slots in the owner's unfinished work under
the limit of four, then starts at most one eligible issue, oldest first (see
[When to send issues](#when-to-send-issues)). After run creation it consumes
`on-deck` on the forge and in the cache, with the
[best-effort removal guarantees](#remove-the-selector-label-on-dispatch)
above. A failed run whose label was removed stays out of the backlog until
the owner re-adds `on-deck`; a removal failure keeps the started run and is
reported for manual cleanup.

Enable and Reset **persist** the catalog capacity pair and removal flag on
the schedule, just as they persist `max_issues`. Older catalog entries have
no gate (`capacity_limit` and `capacity_room_needed` are null) and removal
is false. Existing schedules keep their stored settings. Editable gate and
removal values are not overlaid from the catalog at fire time.

Changing C (for example, `uzi schedule edit <id> --capacity-limit 5 --room-needed 2`)
marks the default **customized**. An API PATCH of C alone keeps the stored K. Restoring the exact
catalog values clears that divergence; the customized marker clears when
no editable field diverges. `uzi schedule reset <id>` restores the current
catalog baseline, including C=4, K=2 and removal on for On-deck sweep.

### CI scout

The **CI scout** (`ci-scout`, weekly, Monday 06:00 UTC) proposes one CI
improvement per cycle and never implements it. When it fires, uzi reads the
repo's recent CI history from the forge and appends a bounded snapshot to the
run's prompt, because the run itself has no tool to read historical CI timings,
attempts or logs:

- **Window**: runs created in the last 14 days, up to 50 default-branch runs and
  20 merge/pull request runs, each with every attempt's jobs where the forge can
  return them (GitHub and GitLab; Forgejo returns only the latest attempt, so
  retries within a run are reported as unknown there).
- **What it summarizes**: per-job p50/p90 duration, the latest week against the
  week before, time to the first failure, suspected flakes (a job that failed and
  then passed on the same commit, within a run or across runs of one workflow)
  with their failed and rerun execution, cancelled and overlapping runs, an
  evidence list of the runs and jobs behind every figure, and up to four log
  tails of failed jobs. Only timestamps the forge reported are used; a figure
  missing some durations is shown as a lower bound with its unknown count.
- **Bounds**: collection stops at a 15-second deadline, a cap on forge calls or
  the shared CI read budget, and the snapshot then says what is partial. It is
  capped at 48 KiB; anything trimmed leaves a marker. Each fire collects
  synchronously, so one scheduler tick collects for at most two CI scouts and
  holds any further due scout until a later tick.
- **The proposal**: one issue (`proposal::ci-scout`) with a short summary and an
  implementation spec ready for a run, picked from duration regressions, slow
  feedback, slow jobs, suspected flakes, infrastructure failures and
  cancellation waste. It never proposes removing or weakening a test or check,
  skipping checks by changed paths without a proven dependency model, raising a
  timeout or narrowing a matrix; every proposal requires a before/after
  inventory showing nothing that runs was dropped, and says whether it edits CI
  workflow files a bot token may not be allowed to push. "Nothing worth
  proposing" is a valid outcome. Promote a proposal with the `uzi` label as
  usual.

The snapshot is tied to the catalog job: a **clone** of the CI scout keeps the
prompt but detaches from the catalog, so it fires without CI history.

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
Once that many are open, further cycles are skipped until you merge or close
one; nothing piles onto an existing branch. You get a Slack DM for the skip
if you've linked your account, and either way the Schedules page always
shows the skip reason on the schedule's last-fire outcome, so nothing is
silently dropped even without Slack.

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

## Default-branch CI repair

The `ci-repair` [default job](#default-jobs) watches a repo's **default
branch** CI and, when a workflow goes red, starts one run that diagnoses the
failure and opens a merge request with the fix (or finishes with a report
when it can't fix it). It spends nothing while CI is green. It is the lane
for the default branch: [automatic CI fixes](./ci-autofix.md) only touch
your agent MR branches and never the default branch.

Under the hood `ci-repair` is a schedule with the **`ci_watch`** target. Its
catalog entry runs hourly (`0 * * * *`, UTC). Enable it like any default
(`uzi schedule catalog enable ci-repair --repo <id>`), or create your own
with `uzi schedule create --ci-watch` — see [the CLI
reference](./cli.md#commands) for the flags. A custom `ci_watch` schedule
stores its instruction in its prompt, which defaults to a copy of the
`ci-repair` body made at create time; `guidance` isn't offered there. The
default's instruction is read-only and resolved from the catalog at fire
time, like any prompt default, and takes an owner **guidance** overlay.

### Auto-approve is off by default

Unlike every other default, `ci-repair` is created with **auto-approve
off**, so each repair run stops at the plan gate for your approval. The
run's only input is CI output — workflow and job names, log text — and that
text can come from dependencies, tests or outside contributors, so an
instruction-shaped line in a log could otherwise steer an unattended run.
The run treats the evidence as untrusted (it sits inside a fenced block
under a heading that says so), but a fence marks the boundary and doesn't
neutralise the text; the plan gate is what puts a human in the loop. The web
create and edit dialog and the schedule detail say so ("Off by default: this
job reads CI logs, which can contain text from dependencies, tests or outside
contributors. Review the plan before the run changes code. You can turn it
on."), and `uzi schedule get` prints the same reason in its own words for
every CI-watch schedule. You can turn auto-approve on for a
schedule; **Reset** puts it back to off, and a clone of a default keeps the
value the source has at the time. Every other default still enables with
auto-approve on.

### What a fire does

Each fire looks at the **newest completed execution of each watched
workflow** on the default branch:

- **Nothing red: nothing starts.** The fire records `ci_green` and starts no
  run.
- **A red workflow that isn't handled and isn't halted: one `prompt` run.**
  At most one run starts per fire. The run is titled `CI repair: <workflow>
  #<number>` and its description holds the instruction, any guidance, then
  the failure evidence (workflow, execution URL, head SHA, failed jobs with
  bounded log tails) inside a fenced untrusted block. It works on a new
  `uzi/prompt-<id>` branch and opens an MR, or finishes report-only. The
  default branch is never written; a human merges. The fix MR's own CI is
  covered by [automatic CI fixes](./ci-autofix.md) like any scheduled-run MR.
- **Several red workflows:** the one whose newest red execution is oldest
  goes first (ties by workflow key). The fire builds failure evidence for at
  most 3 candidates and stops at the first run it starts.
- **A run of this schedule is still active:** no second run; the red
  workflows that aren't halted record `already_running`. Once that run ends, an execution that
  is still the newest records `ci_already_handled`.

A scheduled (cron) fire stores its outcome in `last_fire` (and
`recent_fires`) like any other target, so an all-green fire shows `started 0`
and `ci_green`. `uzi schedule run-now` returns the same outcome in its
response only and never writes `last_fire`.

**Which executions count.** Only completed executions on the default branch
are considered. Pull-request and merge-request events are dropped (a fork PR
from a branch named like the default branch reports that branch on some
forges), and so are executions triggered by another workflow (GitHub
`workflow_run`), so workflows that only run that way aren't watched.
Failure and timeout (`failure`, `failed`, `error`, `timed_out`,
`startup_failure`) are red; `success` is green; anything else completed
(cancelled, skipped, neutral, action required) is neither, records
`ci_not_failed`, and neither starts a run nor resets history.

**The window is one page.** A fire reads one page of default-branch
executions, newest first: **100** on GitHub and GitLab, **50** on Forgejo
(the Forgejo driver's fixed page size). It does not page further, so a
workflow whose latest execution is older than that window isn't seen.

**Choosing what to watch.** The watched selector is a list of up to 20
entries (1 to 200 bytes each, unique, no control characters). An entry
matches a workflow's key **or** its display name exactly, so a name shared by
two GitHub workflows watches both; use the key to pick one. With no selector
(`NULL`, the default) every workflow seen in the window is watched. Set it
with the `--workflow` flag on `schedule create`/`edit` (or in the web
dialog); `--clear-workflows` goes back to all.

**Coverage is never reported as green.** A watched entry with no completed
execution in the window records `ci_watched_not_seen` for that entry and the
fire is never reported as `ci_green`. With no selector, "green" means every
workflow seen in the window (100 executions, 50 on Forgejo) is green, and a
window with no completed execution at all records `ci_watched_not_seen`,
not green.

**Workflow identity depends on the forge.** Episodes (below) key on it:

| Forge | Workflow key | Display name |
|---|---|---|
| GitHub | the numeric workflow id | the workflow's name |
| Forgejo | the workflow file path | the same path |
| GitLab | the pipeline name when set, else the ref | the same value |

GitLab exposes no stable workflow id, so **renaming a GitLab pipeline starts
a new episode** (the old one stays in the history). On GitHub two workflows
with the same display name stay distinct.

### CI-watch skip reasons

Every one is benign: the schedule advances and tries again at the next
cadence. Each is recorded with the workflow it concerns where there is one.

- `ci_green` — every watched workflow's newest completed execution
  succeeded.
- `ci_already_handled` — that execution was already acted on (a run started
  for it, or the episode halted on it), or a concurrent fire or a newer
  state recorded since this fire read it made the observation stale.
- `ci_halted_no_progress` — the [loop guard](#loop-guard-and-episode-state)
  stopped the workflow: it failed again with the same failure signature as
  the last attempt.
- `ci_halted_attempt_cap` — the loop guard stopped the workflow at
  `CI_AUTOFIX_MAX_ATTEMPTS` attempts (2 by default).
- `ci_watched_not_seen` — a watched workflow had no completed execution in
  the window (see above).
- `ci_not_failed` — the newest completed execution was neither red nor
  green (cancelled, skipped, neutral, action required).
- `ci_unsupported` — the forge has no Actions API (a Forgejo version
  without it).
- `ci_capacity` — the schedule already tracks 1024 workflows and this one is
  new (see [admission](#loop-guard-and-episode-state)).
- `ci_evidence_limited` — a discovery or evidence read hit one of the
  [evidence limits](#evidence-limits), or the workflow listing was malformed
  (for example a workflow key over 200 bytes). No run starts and the limited
  workflow's episode is not written, but resets or halts already written
  earlier in the same fire stand; the next fire tries again.
- `ci_config_changed` — the schedule or repo was edited (or deleted) while
  the fire ran. The episode write that detected the change was not made, but
  resets or halts already written earlier in the same fire stand, and the
  fire records only this reason; the next fire uses the new configuration.
- `no_default_branch` — the repo has no recorded default branch.

A CI-watch fire can also record the existing `already_running`,
`credential_disabled`, `no_usable_credential`,
`description_too_large` (the instruction and guidance leave no room for even
the smallest evidence block; the instruction is never shortened to make room)
and `codex_override_conflict`. Other forge errors are transient: the fire
retries on the next tick.

### Loop guard and episode state

Per watched workflow the schedule keeps an **episode**: the last execution it
handled, how many repair attempts it has started, the failure signature of
the last attempt, and whether the workflow is halted. The signature
fingerprints the failed jobs' names and stages plus the last lines of their
log tails, so a re-run of the identical failure matches and a different
failure doesn't.

- **No progress.** If the workflow fails again after an attempt with the
  **same signature**, the episode halts as `no_progress` and no run starts.
  A different signature is a new attempt.
- **Attempt cap.** At `CI_AUTOFIX_MAX_ATTEMPTS` attempts (2 by default,
  shared with [automatic CI fixes](./ci-autofix.md)) the episode halts as
  `attempt_cap`. The cap is checked first, so a workflow at the cap reports
  the cap.
- **A halted workflow stays halted** and skips with its halt reason until its
  newest completed execution is **green**, which resets attempts, signature
  and halt. A stale or repeated green observation doesn't clear a newer
  halt.
- **Re-runs.** A GitHub re-run reuses the execution id with a later update
  time. A re-run that turns green resets the episode; one that stays red
  with a new signature counts as a new attempt; with the same signature
  after an attempt it halts as `no_progress`. An older execution that
  finishes late never outranks a newer one (GitHub and GitLab order by
  creation time then id; Forgejo by execution id alone).
- **Atomic and serialized.** The run and its attempt are written in one
  transaction under the schedule's lock, rechecking the episode state, so two
  concurrent fires on the same red execution start one run, and a fire that
  read stale state can't start an attempt past the cap.

**Admission and paging.** A schedule tracks at most **1024 workflow keys over
its lifetime**. There is no eviction: at capacity, workflows already tracked
keep working and a new one is refused with `ci_capacity`. A green workflow
never takes a slot (history is only created when a run starts or a halt is
recorded). State is read in pages of up to 100 (the default and the
maximum), ordered by workflow key bytes with an exclusive cursor;
`uzi schedule get` and the schedule detail show the first page and a next
cursor, and `GET /api/schedules/{id}/ci-episodes?cursor=&limit=` reads
further pages (see [the CLI reference](./cli.md#commands)).

**History survives edits.** Editing the watched selector or converting a
schedule's target does not delete episodes, and they are dropped only when
the schedule is deleted. Because the keys belong to one repo's workflows,
**repointing a schedule to another repo is refused (HTTP 409) while it has
episode history**; delete and recreate it against the new repo.

### Evidence limits

The evidence a fire reads is bounded, and everything read from the forge is
treated as untrusted:

- **Per job and per execution:** at most `CI_FIX_MAX_JOBS` failed jobs
  (10 by default) and the last `CI_FIX_LOG_TAIL_BYTES` bytes of each log
  (32 KiB by default) — the same settings [automatic CI
  fixes](./ci-autofix.md) use, see [Configuration](./configuration.md).
  The composed description is further shortened to fit the run description
  limit (log tails first, then whole jobs from the last one), always with an
  explicit omission marker.
- **Per fire:** at most 3 failure snapshots, and one list call of 100 rows
  (50 on Forgejo).
- **Bytes:** each CI JSON response is limited to **4 MiB** and all of one
  fire's responses to **16 MiB**. Both the compressed bytes on the wire and
  the decompressed bytes are counted, each against those limits. Log tails
  and other forge reads don't draw on this budget (their own tail size
  bounds apply).
- **Job discovery** reads at most 10 pages and 1000 jobs per execution.

Exceeding a byte or job-discovery limit records `ci_evidence_limited`.

### Artifact repair evidence

In the CI-watch dialog, **Artifact names or globs (optional)** takes one
artifact name or glob per line, up to **20**. Default `ci-repair` entries and
user-created CI-watch schedules use the same field. Leave it blank to fetch
**no artifacts**; this differs from a blank workflow selector, which watches
all workflows in the listed window. Names match artifact names, not paths
inside an archive. Entries are trimmed, unique, valid UTF-8, 1–200 bytes,
without control or format characters; globs use Go `path.Match` syntax
(for example `e2e-rundir-*`).

The CLI uses repeatable `--artifact` on `schedule create --ci-watch` or
`schedule edit`. Editing with `--clear-artifacts` fetches none and leaves
the workflow selector unchanged; it is mutually exclusive with `--artifact`.
Omitting both preserves the saved artifacts. For API edits, omitted
`ci_artifacts` preserves the value; `null` or `[]` clears it.

**GitHub is the implemented artifact provider.** GitLab and Forgejo repairs
remain logs-only; their artifact support is deferred (M2). Collection binds
evidence to the observed execution, repository, workflow, commit and branch,
then revalidates that execution before creating the run. GitHub's run-scoped
artifact API cannot prove which attempt produced an artifact after a rerun:
an unknown attempt or any attempt other than the first stays logs-only.

The api downloads and extracts selected archives, admitting **UTF-8 text
only**, up to **50 files / 16 MiB per repair run / 4 MiB per file**.
Administrators can lower the run limits in
[Configuration](./configuration.md#job-files-and-product-skill-sets-prd-1909).
Archive paths, links, inflated bytes and download work are bounded before
worker delivery. Content is scrubbed **before hashing and storage**.
The bounded, untrusted manifest identifies kept files and refusal reasons;
it does not promise a row for each archive member. Omission counts are
reported when known, and stay unknown when content was not inspected.
Missing, expired, unsupported or budget-refused evidence leaves the repair
able to proceed with log tails; if some files were admitted, the manifest
records the partial evidence.

Inputs reuse sealed job-file storage and its per-owner, instance and shared
stored-files quotas. They expire `UZI_JOB_FILES_RETENTION` after the repair
run ends (default **168h**). On the worker, evidence stays outside the clone
in a private worker-owned **0700** directory with **0400** files. Both
harnesses receive read-only access through mediated evidence tools, including
on resume; workers receive neither artifact URLs nor forge tokens for
collection.

This describes the source at M1's dispatched head, not completed release
acceptance. Required final gates, real-database composition, worker-UID/runtime
and maintainer packaged proof remain pending; see
[PRD #2866](../prds/2866-ci-artifacts-repair-evidence.md#dispatched-progress-and-evidence).

### The CI-config guard

A repair run **never pushes a change to CI configuration**. The guard is
unconditional for this lane: it applies to every `ci-repair` run, including
one whose plan you approved (unlike automatic CI fixes, which exempt
human-approved runs). Approving a plan doesn't give the worker's token
permission to rewrite CI, so a needed CI-config change is reported for you
to make, not pushed.

Protected paths are `.gitlab-ci.yml`, `.gitlab/**`, `**/*.gitlab-ci.yml`,
`.github/workflows/**`, `.forgejo/workflows/**` and `.gitea/workflows/**`,
plus anything in `CI_AUTOFIX_CONFIG_PATHS` and the project's own configured
CI config path. If that project path can't be looked up, the fire creates no
run and retries.

The worker refuses at four points:

- an **early check** on the finished branch, which fails the run;
- **every checkpoint publish**, which is skipped (the work stays local);
- **every held-work publication**, also skipped;
- the **final push**, rechecked on the exact commit immediately before every
  push attempt, which fails the run.

It **fails closed**: if the diff can't be computed, it refuses. It matches the
**raw git paths** (no trimming or normalising), lists both sides of a rename,
and diffs against **every merge base** with the default branch, so a merge
can't carry a protected file in through a second parent. A path that matches the
current default branch tip (for example a workflow copied in from the default
branch when the branch is brought up to date) doesn't count as the run's change.

The server repeats the check on checkpoint and held-work publications by
comparing the candidate's tree with the fetched default branch tip, and
refuses (the publication is skipped, never passed) when a protected path
differs or when it can't prove it doesn't: an invalid glob, an unknown or
unfetched default branch, a tree walk that reaches **100,000** entries, a
single name or path over 4096 bytes, or more than 64 MiB of path bytes in one
walk, or a differing subtree the worker's pack omits (the server can't prove
it unchanged). So once the default branch has moved under a directory since
the run started, the run's checkpoint and held-work publications are skipped
(held work falls back to the recovery archive); the worker's own check still
decides the final branch push.

A refusal fails the run with a reason such as `CI-config guard: this run
changes CI configuration: <up to 5 paths, and N more>. CI-config changes in
this lane are reported, not pushed.` (or, when the diff couldn't be computed,
`could not compute the diff to verify it does not change CI configuration
(failing closed)`). The run's diff is attached as the **preserved patch**
only when its secret scan is trusted and clean, after redacting the run's own
secrets; otherwise the patch is withheld, the reason says so, and points at
`uzi run export`.

### Rolling it out

The guard needs a worker that enforces it, so a guarded run is gated by a
worker capability, `ci_config_guard_v1`:

- **Upgrade the api first.** Workers that predate the guard don't report the
  capability, so they can't claim a repair run. The run stays `queued`, and
  its [health reason](./run-health.md) says no online worker enforces the
  CI-config push guard (`ci_config_guard_v1`). With
  [auto-provisioning](#auto-provisioning-a-worker-for-an-unmet-capability) on,
  such a queued run counts as unmet demand. Roll the workers and it claims.
- **Workers first also works:** a new worker reports the capability, an older
  api ignores it, and nothing changes until the api ships.

File-bearing repair runs also require `ci_artifacts_v1` **in addition to**
`ci_config_guard_v1`. Roll out workers advertising both before expecting
those runs to claim. Repairs without artifact files retain the guard
capability requirement.

### Not covered

- Artifact fetching on GitLab and Forgejo is deferred; those repairs use
  job log tails. MR-branch `ci_fix` runs do not receive these artifact inputs.
- A fire reads one page; a workflow outside the window is reported, not
  paged to.
- One red execution is enough to start a repair, and the attempt cap is the
  instance-wide `CI_AUTOFIX_MAX_ATTEMPTS`, not a per-schedule setting.
- Detection runs on the schedule's cadence only, not on a CI event.
- A halt isn't announced by Slack or an issue comment; it shows in the
  schedule's last fire and its episode state.
- Workflows triggered only by another workflow aren't watched.
- Live acceptance on a real repo (a red nightly yielding one run at the plan
  gate and one MR once approved) follows the release and hasn't happened yet.

## Automatic fixes on scheduled-run MRs

The merge requests a schedule opens — **MR-mode prompt** jobs (docs
hygiene, test improvement, bug hunt, feature bingo, refactor scout or CI scout
with `--output mr`, and custom prompt schedules in `mr` mode) and the
[self-improvement](#self-improvement) job —
participate in uzi's two unattended autofix lanes, exactly like the MRs your
issue runs open:

- **[MR review rework](./mr-review-watcher.md)** folds new review comments on a
  green, still-open MR back onto the branch. It is **on by default** with a
  four-layer opt-out; the per-schedule **"Auto-rework MR review comments"**
  control (a three-way Inherit/On/Off choice in the schedule modal, or
  `--mr-rework` / `--mr-rework=false` on `uzi schedule create`/`edit` and
  `--clear-mr-rework` to return to Inherit) is where you turn it off for a given
  schedule's runs. See [that page's Enablement
  section](./mr-review-watcher.md#enablement) for the full resolution order.
- **[Automatic CI fixes](./ci-autofix.md)** react to a failed head pipeline on a
  scheduled-run MR branch. This lane is **on by default**, on the same
  account-wide **Settings → Automatic CI fixes** toggle your issue runs use —
  a scheduled run inherits the owner's setting; there is no separate
  per-schedule switch for it. An instance-wide admin kill-switch can turn it
  off for everyone regardless of any owner's own setting; see [Automatic CI
  fixes](./ci-autofix.md#1-opt-out) for both.

Both lanes act only on a **still-open** MR and spend the schedule owner's own
Anthropic token, the same as any other run. A prompt schedule's MR has no
backing issue, so the notices these lanes post as an issue comment on an
issue-run MR (ci_autofix's halt; mr_rework's per-MR cap halt) arrive as a
**Slack DM only** for a prompt MR; a self-improvement MR's shared tracking
issue is likewise left untouched.

## Restarts and missed fires

A recurring schedule survives an api restart: its next fire time is stored,
not held in memory. A fire missed while the process was down (or a slow
tick) still fires **once**, promptly, on the next wake — never a backfill of
every cadence missed. A one-time schedule left overdue by a restart fires
once, the same way, then goes terminal.

A user-level [pause all](#pausing-everything-at-once) leaves a one-time
schedule overdue in the same way a restart does: it fires once, on the
first tick after the pause ends.

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
With this on, uzi instead spins up a throwaway worker for that run. It also
provisions when every capable worker stays busy (the saturation trigger),
after the saturation debounce.

**Turning it on** takes two switches, both off by default: an admin enables
the feature instance-wide from **Admin → Instance**, then you opt in from
your own **Workers page** (Settings → Workers), in the hosted-worker
section — that per-user toggle only appears there once the admin switch is
on. A per-user cap also bounds how many throwaway workers you can have
running at once, so one busy stretch can't spin up an unbounded fleet.

**Docker-capable ephemeral workers.** When the instance offers the Docker
tier, the **Ephemeral workers** subsection also shows a **Docker-capable**
checkbox beside **Auto-provision on demand**. It saves separately and keeps
its value while auto-provision is off. For both capability-gap and saturation
provisioning, the preference adds Docker only for ordinary runs with a
repository explicitly in the admin's `docker_repo_allowlist`. It does not
apply to jobs, isolated-lane runs or repo-less runs, including judges. If the
allowlist read fails, any returned values are discarded and membership is
treated as empty: no preference-driven Docker. A run's explicit Docker
capability requirement and the independent worker-claim fence are unchanged.

**What you'll see:** the run keeps its existing
[health reason](./run-health.md) while the throwaway worker starts.
Once online it claims the run and works it like any other worker. Afterwards
it may stay warm for up to 2 hours (unless your admin disables the lease),
so an eligible same-owner, same-repository, same-branch follow-up run can reuse
it. It still counts toward your ephemeral cap while warm. At the cap, an idle,
releasable lease may be evicted early to make room; the cap still bounds the
fleet. When its lease ends, the worker and pod are removed. While it exists,
it has an `ephemeral` badge in your fleet list. See
[Ephemeral worker lease](./hosted-workers.md#ephemeral-worker-lease) for
eligible run kinds and early removal rules.

**Warm Docker reuse.** A plain warm worker steps aside when the Docker preference
applies to the run's allowlisted repository, so it gets Docker-capable capacity
instead. A capability-free run with no claimable online worker becomes an
immediate capability-gap candidate under that same policy. A busy eligible
persistent worker still keeps the run on the saturation debounce. Unallowlisted
repositories, an empty allowlist and failed allowlist reads keep plain warm
reuse; returned values alongside an error are discarded. Persistent placement
and already-bound runs are unchanged.

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

## When to send issues

Recurring label-selected sweeps can use an optional capacity gate. Set a limit on
unfinished runs and the room needed before sending another batch. Multiple selector
labels are supported; an empty custom selector still uses the configured uzi label.
Assigned sweeps, one-time schedules and other targets do not support this gate.

The gate counts your unfinished work across all repos and origins, including job
runs and queued, waiting and parked runs. Chat and judge runs and terminal runs are
excluded. Room is the limit minus this count, clamped to zero. When room is below
the required threshold, the fire records “Waiting for room”, starts nothing and
advances to its next recurring check without contacting the forge. Otherwise it
sends up to the smaller of room and **issues to send at a time** (the existing
`max_issues` setting); an unlimited batch is still bounded by room.

This is an advisory snapshot: other schedules or manual starts can exceed the
limit. Run now uses the same check, including when a schedule is paused, and does
not replace its recorded last fire. An automatic fire respects pause-all first.

Use `uzi schedule create --sweep --capacity-limit 4 --room-needed 2` with the usual
repo and recurring timing flags. Edit accepts the same paired flags, or
`--clear-capacity` to turn the gate off. Unrelated edits preserve it. API PATCH
may change one integer using the stored other value; clearing requires both
`capacity_limit` and `capacity_room_needed` explicitly set to null. Enabling or
resetting a default persists its catalog gate: C=4 and K=2 for `ondeck-sweep`,
both null for older catalog entries. Owner edits use the stored values, without
a runtime catalog overlay; restoring the exact baseline clears gate divergence
(see [On-deck sweep](#on-deck-sweep)). Clone and Add repo copy the gate.
