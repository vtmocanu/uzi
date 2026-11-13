---
title: Pausing and resuming a run
order: 110
audience: user
---

# Pausing and resuming a run

A run's owner can park it on demand — not because anything went wrong, just
because it's an inconvenient time to keep spending — and pick it back up
later from its preserved session or published checkpoint. No model turns run
while it is paused. The run's lead can also request an ordinary pause while
waiting on a dependency during approved issue implementation. Both wait for
owner resume. System holds also use `paused`, with their own next steps, such
as the [weekly usage reserve](#paused-by-the-weekly-usage-reserve).

## Reading the pause label

The run detail in the CLI, TUI and web can distinguish the lead-park path
(**paused by the run's lead**) from the owner path (**paused by its owner**
in `run get`, **paused by you** in the TUI and web). `park_run` and the
owner's `/park` answer share the lead-park execution path; `uzi run pause`
uses the owner path. An owner-requested `/park` can therefore display
**paused by the run's lead**. These labels describe the execution path;
they do not identify the requester, a particular person or why the run paused.

When current pause evidence is unavailable, the label stays **paused**.
Older workers, missing messages, a read failure, replay gaps or a status
refresh can leave it neutral temporarily. The TUI and web use messages
already loaded for the run; they do not search older history just to name
the path. A pause time can still appear without enough evidence to name it.
Follow notices can also remain neutral. Use the activity feed for context
and **Resume** when ready; a neutral label does not change how an ordinary
pause resumes. System holds keep their hold-specific labels and remedies.

## Requesting a pause

From the run view's **‖ Pause ▾** menu (beside **Stop run**), or
`uzi run pause <id>`, with two modes:

- **After current milestone** (the default). The run finishes the milestone
  — or the turn, on a run with no frozen milestone list — it's already in,
  pushes a checkpoint, then parks. The run stays `running` until then: the
  page shows an info chip, **Pause requested · after M4** (or **· after the
  current step**), with **Cancel pause** and **Pause now** beside it.
- **Pause now** (`--now`). Interrupts the turn in flight and attempts to
  checkpoint the current work before parking. Preservation can fail or the
  published checkpoint can be older than the latest local work; check the
  activity feed rather than assuming that every edit reached the forge.
  A **Pause now** sent while a resumed run is still recovering kept work
  (before any model turn) waits for that recovery to finish, then parks with
  a checkpoint before the run starts new work. A recovery failure or
  `recovery_wait` hold wins over the waiting pause, and a hold leaves the
  request pending. The park can still be refused (`pause_failed`); the run is
  then not paused and is left for the normal requeue path. If the worker never
  hears the reply to its `paused` report, check the run page: the server may
  already show it paused.

A pending request is a flag on the still-running run, not a state of its
own — withdraw it any time before the boundary with **Cancel pause** (CLI:
`uzi run pause <id> --cancel`), or escalate it to `--now`. None of this is
synchronous: the park lands on the worker's next report, so watch the run
page or `uzi run get <id>`.

**Only these kinds accept an owner pause**: issue, non-interactive task, prompt, and
self-improve runs. A chat run already parks between turns, an interactive
task parks after every turn, and judge/MR-rework/CI-fix runs do not accept
owner pause requests — pausing any of those, or a run already sitting at a
gate or an involuntary park, is refused (409) with the reason. Owner-only.

## Waiting on a dependency during implementation

On an approved, non-interactive **issue** run, answer the lead's current
implementation clarification with `/park` to request a pause directly:

```sh
uzi run answer <id> --message /park
```

This command is recognized after the owner answer has been validated and
consumed for that question. Answers are trimmed and empty entries ignored;
there must be at least one non-empty entry, and each must equal `/park`
exactly. A batch mixing `/park` with other text is ordinary guidance.

With API and worker support, ordinary prose goes back to the lead:
continue if it gives actionable work; if implementation must wait on a
dependency and no actionable work remains, call `park_run` and end the turn.
That tool is lead-only, during approved issue implementation, including
clarification follow-ups. Planning,
subagents, interactive runs and other run kinds cannot use it.

Both the API and worker must support this path. With older components, use
the existing `uzi run pause <id> --now` when the run is running. A successful
park uses the ordinary `paused` state, stops the wall clock, frees the run
slot, and waits for owner resume from the session or published checkpoint.
There is no dependency polling or automatic resume when a dependency lands,
and no extra custody or retention guarantee beyond an ordinary pause.
Local-only edits and session state still depend on the retained worker;
checkpoint and recovery data follow the existing
[retention rules](./run-recovery.md), rather than a separate dependency archive.

If checkpoint publication fails, the ordinary `pause_failed` safeguard
clears the request without declaring a successful pause. This dependency
path then asks one worker-authored question about what to do next. It does
not restart a paid/provider turn or retry parking until an explicit owner
answer: `/park` authorizes one more attempt; prose returns to the lead;
cancel ends the run. The fallback is ordinary `awaiting_input`: question
wait time is banked out of the wall budget, but the worker slot stays held.
It draws on the run's shared `QUESTION_TIMEOUT_SECONDS` parked-time budget (when
enabled); a re-parked retry continues the same interval and another question gets
only the remaining budget on that worker. An unattended
run that needs this owner decision stops with a trusted refusal instead.

## What's durable, and where

For an ordinary owner pause, parking works like the checkpoint the run
already keeps pushing during normal work: the worker publishes its checkpoint to the forge *before*
reporting itself paused, so the parked run's progress lives on the branch,
not just on one worker's disk. The run's session and its HOME (skills, git
credentials, in-progress state) stay on the worker that was running it,
freeing its slot for other work in the meantime.

**If the checkpoint can't be published, an ordinary owner pause does not park.** The
pending request is cleared without changing the status to `paused`, and the
run's activity feed carries the reason (*"Could not pause: the checkpoint
could not be published. The run is still running and has restarted the
interrupted step."*). An ordinary owner pause restarts work; the dependency
path above waits for an answer instead. The `pause_failed` report itself sends no Slack
DM because it is not a status transition; its reason is in the activity feed.

## Resuming

**Resume** on the run page, or `uzi run resume <id>`, moves the run from
`paused` back to `queued` with its worker pin kept:

- If that same worker is still alive, it picks the run back up and
  **continues the same session where it stopped — no re-planning.**
- If the worker is gone, another worker claims the run, **recovers the
  branch from the pushed checkpoint, and re-plans only the milestones that
  weren't finished yet** — the ones already done stay done, from the plan
  you already approved.

A `completion_blocked` hold refuses plain `uzi run resume` and
`uzi run resume-now`. Use `uzi run decide` or an eligible `uzi run set-token`
switch; see [Completion holds](run-completion-hold.md).

`uzi run resume-now` also resumes an ordinary owner-paused run (it's the same endpoint that
already resumes a run held for a pooled token).

## The clock stops

No model turns run while a run is paused. The time it spends
parked is banked separately and doesn't count against its budget, so the
remaining budget on resume is exactly what it was when the ordinary pause
landed — unlike a usage-limit wait, whose resume gives the run a fresh full
clock.

## Held after provider outages

A run can also pause without an owner request after a provider-outage
streak reaches its elapsed bound (default 24h): `paused` with
`hold_reason: provider_unavailable`. This policy stops automatic retry
without failing the run. **Resume** queues a fresh attempt and clears the
streak; **Cancel** ends it. Eligible **Stop** sets the scope ceiling first,
then requires **Resume** after success, rather than an atomic Stop endpoint.
It requires a non-interactive milestone issue run with at least one
completed milestone; the surfaces explain unavailable actions.

This hold retains the claim, worker affinity and the issue lock for issue
runs, and satisfies and clears pending pause requests. The worker keeps
clone and HOME; a verified capture may still be unpublished. The owner-pause
publication rule above does not guarantee publication for this involuntary
hold. Losing the worker's storage can lose unpublished work; another worker
needs published work or an available verified recovery copy. See
[Provider outage streaks](./run-recovery-wait.md#provider-outage-streaks)
for progress resets, detection timing and compatibility.

## Paused by the weekly usage reserve

If you set a [weekly usage reserve](rate-limits.md#weekly-usage-reserve) with
**Also pause running runs** on (the default once a threshold is set), uzi can
pause a running run for you when the credential it spends reads at or above
your threshold on the 7-day window. This is a system pause with its own cause,
not one you or the run's lead asked for: the pending request has
`pause_mode: reserve` and the parked run has `status: paused` with
`hold_reason: weekly_reserve`. To the worker it is an ordinary milestone pause.

- **Which runs pause.** Running, non-interactive issue, task, prompt and
  self-improve runs, the same kinds you can pause by hand. Other kinds (CI-fix,
  MR-rework, job, judge, cross-check, interactive tasks) are held at admission
  only, and one already running finishes.
- **When.** Only a fresh reading at or above the threshold on the credential the
  run recorded as spending requests a pause. A missing, stale or partial
  reading holds *new* work but never pauses a running one, and a run with no
  recorded credential is left alone. The run finishes its current milestone
  (or the current step, with no frozen milestones), checkpoints, then parks.
  `uzi run get` shows the pending request as `PAUSE_REQUESTED  after M3 (weekly
  usage reserve)`. A pending pause you or the wall clock
  requested is never overwritten by the reserve. If the checkpoint fails, the
  run keeps going and the reserve asks again at the next boundary.
- **It releases itself.** The sweeper resumes a `weekly_reserve` pause when a
  fresh reading is below the threshold, and on its next pass whenever you clear
  the threshold or turn **Also pause running runs** off, whatever the freshness
  of any reading. Resuming this way keeps the run's remaining budget exactly as
  an owner resume does. The same three conditions withdraw a request that has
  not parked yet. The sweeper never releases an owner pause, a wall park, a
  usage-limit wait or any other hold.
- **Resuming by hand overrides the reserve.** `uzi run resume <id>` (or
  **Resume** on the run page) on a `weekly_reserve` pause queues the run
  immediately and records an override for the credential it spent, so it is not
  held again at admission and does not re-pause at once. The override ends at
  the first of: the observed 7-day reset time (read from your current meter when
  you resume, if that reading is fresh and at or above the threshold); a later
  reading that shows the week reset early; or, when no such reset time was
  recorded, the first fresh reading below the threshold. It
  never lasts longer than seven days. Its end time is the run's
  `reserve_exempt_until`. If you have already cleared the threshold there is
  nothing to override, and a plain resume applies. A resume that is not of a
  reserve pause records no override. A run that spans a reset is protected
  again in the following week.
- **You cannot cancel a reserve request.** `uzi run pause <id> --cancel`
  on a request the reserve filed is refused with a 409:
  raise or clear the threshold in your usage settings to withdraw it, or resume
  the run once it pauses.
- **Your own pause wins.** Requesting a pause yourself (after the milestone, or
  **Pause now**) while a reserve request is pending replaces it with yours.
  The run then parks as an ordinary owner pause and waits for you. The run's
  lead parking over a pending reserve request does the same: it becomes an
  ordinary pause that needs an owner decision, and the reserve does not release
  it or ask again.
- **With the toggle off, started runs continue.** Turning **Also pause running
  runs** off releases reserve pauses and lets previously started runs continue
  above the threshold; new work stays held.
- **Rolling back.** Reserve-paused runs are released only by this version's
  sweeper. After a downgrade, resume them by hand; nothing releases them
  automatically. The schema rollback also refuses to run while any run still
  holds a pending `reserve` pause request, so resume or drain those first.

The reserve is best effort, not a cap; see [what it does not
guarantee](rate-limits.md#it-is-best-effort). Slack pause notices for a reserve
pause name the weekly usage reserve rather than an owner pause.

## Held on a Codex policy refusal

When the Codex provider refuses a root request under its usage policy, an
eligible run pauses instead of failing: `status: paused` with
`hold_reason: codex_policy_hold`. Eligible runs are non-interactive issue, task,
prompt and self-improve runs. The run's work and recovery custody are kept, and
**Cancel** stays available. Policy **Stop** and revising a turn at
`awaiting_approval` are not available for this hold.

The run DTO carries a `policy_hold` object (`hold_cause`, `resume_modes`,
`pending_fresh`, `outcome_id`); the web run page and `uzi run get` show the same
fields. **Resume** is an owner action that names one mode from
`policy_hold.resume_modes` and the exact episode (`expected_episode_id`, the
hold's `outcome_id`). The CLI form is in
[Codex policy hold](cli.md#codex-policy-hold).

| Mode | Offered on | What it does |
|---|---|---|
| `fresh_from_saved_work` | attempt-path lanes | Starts a fresh conversation from the saved work and resolves that refusal. Later pickups behave as ordinary pickups, with no repeated hold for that old refusal; a new refusal needs new consent. It also ends any earlier requirement to preserve the old conversation for the rest of the run, until a new refusal. |
| `preserve` | compatible lanes | Continues the same conversation with your guidance. If the conversation is missing or rejected, the run is held again and never starts a new conversation. |
| `fresh_one_claim` | compatible lanes | Exactly the next pickup may start one new conversation. Later pickups preserve again. A one-claim start that is never confirmed is held again as `fresh_start_uncertain` and is not repeated. |

Either fresh mode sent for the other continuation kind is refused without
changing the run.

- **Consent is bound to the episode.** An exact replay (same mode, episode and
  guidance) returns 200 with `replay: true`. The same request with different
  guidance is a 409 and the new guidance is not recorded. Consent for an old
  episode cannot resolve a newer one. A credential switch (`set-token`) cannot
  consent.
- **Causes.** The hold shows why it was held: `refused` (the provider refused a
  request), `attempt_path` (this pickup cannot continue the old conversation on
  this lane), `saved_work_missing`, `session_missing`, `transcript_unknown`,
  `provider_resume_rejected`, `pending_conversion` (an earlier refusal recorded
  on the worker was converted into this hold) and `fresh_start_uncertain`. A
  historical hold with no recorded cause reads "held (cause not recorded)".
  The exact owner-facing wording is in `fixtures/policy-hold/wording.json`.
- **Saved-work check.** When the hold records a required saved-work head, the
  worker proves the recovered clone contains it before any provider work.
  Otherwise the run is held again as `saved_work_missing` with nothing started.
  After `fresh_from_saved_work` the requirement persists until the claiming
  worker verifies it at the current claim generation.

### Limits

- Server enforcement covers applied refusals. Local pending evidence protects
  only its surviving worker.
- An absent worker or journal beyond the existing pending lease, or the
  documented wall-release race before delivery, can leave another claim
  unconstrained.
- Retained custody still counts toward the per-owner custody admission limit
  (`custody_hold_limit`, default 8). There is no force-clear or capacity
  exception.
- A held, assigned worker can defer idle-only image rolls indefinitely (#3103).
- Policy journals survive a worker process crash, not storage or power loss.
- Clearing the saved-work requirement after `fresh_from_saved_work` relies on
  the claiming worker's local proof, not on publication.
- Roll the API first, then capable workers. A released worker image that
  advertises `provider_policy_hold_v1` but predates `fresh_one_claim` support
  would consume a one-claim grant and hold again as `session_missing` without
  starting a conversation.

### Rolling back

Rollback is drain-only. Stop owner policy traffic, drain or cordon workers, and
wait until no `codex_policy_hold_pending` outcome, no live consumed-unconfirmed
Fresh grant, no claimed, running or waiting row carrying a required saved-work head or a recorded verification,
and no claimed, running or waiting run still preserving after an owner Resume
(preserve or fresh_one_claim) remains. Otherwise stay on the new version and keep undrained worker volumes.
Then stop all API replicas and run goose down exactly one version at a time
from 00421:

1. 00421 restores the Fresh shape check as `NOT VALID`.
2. 00420 is guarded: it refuses live or orphan rows and converts paused or
   queued active rows to failed.
3. 00419 is guarded the same way.

Never go below 00418. After any step or crash, rediscover the version with
`SELECT max(version_id) FROM goose_db_version WHERE is_applied`. Start the
earlier API image only at 00418; at an intermediate version either run the next
Down or roll forward by restarting the new image.

The Downs are destructive. 00419's Down permanently drops the recorded hold
cause and recovery identity columns for all rows, so take a backup or `COPY`
first. They refuse a run in any other live state (claimed, running, `recovery_wait`,
`limit_wait`, `pool_wait`, `awaiting_*`) that still carries an active policy
requirement; drain or cancel those first. Runs in those states without policy
state do not block. Each Down is one
transaction, so a `RAISE` rolls it back fully, and holds `ACCESS EXCLUSIVE` on
`runs` while it runs. Design rationale:
[ADR-3607](../adr/3607-compact-policy-hold-m1-isolation.md#consent-and-continuity-m2m3).

## Paused after an uncertain token switch

If you switch a running run's token with `uzi run set-token` and the worker
cannot prove whether the switch took effect (the confirmation was lost, or the
token can no longer be handed out), the run **pauses for you**. It is an
ordinary pause: `hold_reason` is empty, the worker, session and open question
are kept, and the feed shows a `credential_switch_uncertain` message. Neither
the old nor the new token is used again until you act.

To continue, **Resume** it (`uzi run resume <id>`, or the run page's
**Resume**). Resume re-resolves the token from the run's current choice, so the
token you last set is the one the run spends; to pick a different one first,
run `uzi run set-token <id> <label>`. Cancel works as always.

This is different from a held switch (`credential_switch_settling` then
`credential_switch_unsettled`), which means the worker could **not prove its
processes stopped** and nothing was applied; see
[Credential-switch holds](run-recovery.md#credential-switch-holds). Settling
lifts only on the worker's acknowledgement, a stale heartbeat, worker removal
or re-registration, never on elapsed time. A worker without in-place switching
keeps the release-and-reclaim path and does not produce this pause.
Design: [ADR-4201](../adr/4201-in-place-credential-switch.md).

## Not the same as a completion hold

A `paused` run is not always a pause **you** asked for. A run with the
[structural completion interlock](run-completion-hold.md) turned on can also
land in `paused` on its own for unmet milestones, a closing-commit refusal
or an unknown completion projection — a **completion hold**, not an owner
pause. The two
look alike (both a parked `paused` run) but resolve differently: an owner
pause only ever resumes when you ask (this page), while a completion hold
resumes through `uzi run decide <id> --continue`. The run page tells the two
apart.

## Not the same as a usage-limit wait

An ordinary pause is requested by you or the run's lead; [a usage-limit
wait](run-limit-wait.md) happens to the run because a
provider usage window was exhausted (Claude or Codex subscription). They look
similar — both are non-terminal parks that keep the run's session and worker affinity — but
resolve differently: a usage-limit wait resumes on its own after the reset or
bounded fallback and gives the run a fresh clock, while a pause only ever resumes when
you ask it to and hands back exactly the budget that was left. If you pause
a run and it then happens to hit a usage limit before reaching your
requested boundary, your pause request survives that wait and takes effect
at the first boundary once the run is running again — you don't have to ask
twice.

## Paused at the iteration limit

On both Claude and Codex, an eligible run that completes its invocation's
finite implement ⇄ review allowance before any completion attempt can park
as `paused` with `hold_reason: iteration_budget_exhausted`. The eligible
kinds are exactly `issue`, `mr_rework`, `ci_fix`, `task`, `prompt` and
`self_improve`, non-interactive, with no review target and no isolated fetch.
This needs API support advertised as `iteration_park_version: 1`; absent or
unknown support keeps legacy failure handling, and old workers ignore the
additive field. Eligibility requires an exact persisted
`completion_attempts = 0`, not a missing count or a local assumption that no
attempt happened. Runs with prior attempts keep their existing dispositions;
idle stops and the [worker-initiated stall attempt](./run-completion-hold.md#what-youll-see)
(#3139) are unchanged.

1. Inspect the run and its capture notices. The worker makes a bounded,
   best-effort capture and preserves clone, HOME, session and custody.
   Failed checkpoint publication or degraded capture can still lead to a
   park. Only a matching acknowledgement confirms `paused`: a refusal or
   uncertain report stops the invocation retaining local material, without
   claiming the server paused it.
2. When ready, the owner explicitly chooses **Resume**
   (`uzi run resume <id>`). Each resumed invocation gets a fresh finite
   iteration allowance; completed iterations in that invocation, rather
   than the lifetime iteration counter, exhaust it. Reaching it again
   requires another owner Resume. Worker/session affinity is kept where
   resolvable; a compatible session continues, otherwise the existing
   recovery disclosure applies.
3. Resume adds no wall time. If the wall budget is spent, **Extend**, then
   **Resume**. Extend alone does not resume this iteration hold. An explicit
   owner set-token-and-resume is also allowed under the same remaining-wall
   guard; credential availability alone never resumes it. **Cancel** remains
   available. This hold grants neither **Stop** nor publication authority.

The hold waits without expiry and holds its lane and run-bound worker.
Retained local material is not a durable backup: unpublished work still
depends on that worker's storage. Check the published checkpoint and
[recovery evidence](./run-recovery.md) before relying on another worker.
This is distinct from an owner pause's publication requirement, a
`completion_blocked` decision, and the wall park below.

## Not the same as running out of time

A run that reaches its wall-clock time limit also parks instead of failing
for the clock alone, with no expiry. Before a completion attempt, the wall
park uses `hold_reason: budget_exhausted`. From the CLI: **extend** it
(`uzi run extend <id> --by 2h`, which adds time and resumes that wall park in
one step), **stop** it (`uzi run stop <id>`, milestone issue runs only —
finalizes the milestones already done into a merge request), or **cancel**
it (`uzi run cancel <id>`).

On an interlocked run with a confirmed earlier completion attempt, a live
worker instead routes the time limit to `completion_blocked`, including
across worker flights. This is a [completion hold](run-completion-hold.md#deciding-on-a-held-run)
and needs a completion decision; ordinary resume or extension alone does
not resume it. Extend the budget before choosing Continue, or the run
immediately re-holds. Paused time grants no extra working budget. See the
[mixed-version limits](run-completion-hold.md#mixed-version-limits)
for rollout details.

## The CLI

```
uzi run pause <id> [--now|--cancel]
uzi run resume <id>
```

`--now` and `--cancel` are mutually exclusive. See [uzi CLI](cli.md) for the
full command reference.

Related: [Completion holds and the structural interlock](run-completion-hold.md)
· [Paused on a usage limit](run-limit-wait.md) · [Run activity
pane](run-activity.md) · [CLI](cli.md)
