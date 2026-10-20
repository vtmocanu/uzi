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
owner resume. System holds also use `paused`, with their own next steps.

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

A pending request is a flag on the still-running run, not a state of its
own — withdraw it any time before the boundary with **Cancel pause** (CLI:
`uzi run pause <id> --cancel`), or escalate it to `--now`. None of this is
synchronous: the park lands on the worker's next report, so watch the run
page or `uzi run get <id>`.

**Only these kinds can be paused**: issue, non-interactive task, prompt, and
self-improve runs. A chat run already parks between turns, an interactive
task parks after every turn, and judge/MR-rework/CI-fix runs are short and
finish on their own — pausing any of those, or a run already sitting at a
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
It shares the run's absolute `QUESTION_TIMEOUT_SECONDS` deadline (when enabled);
another question or retry does not reset it on that worker. An unattended
run that needs this owner decision stops with a trusted refusal instead.

## What's durable, and where

Parking works like the checkpoint the run already keeps pushing during
normal work: the worker publishes its checkpoint to the forge *before*
reporting itself paused, so the parked run's progress lives on the branch,
not just on one worker's disk. The run's session and its HOME (skills, git
credentials, in-progress state) stay on the worker that was running it,
freeing its slot for other work in the meantime.

**If the checkpoint can't be published, the run does not park.** The
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

## Not the same as running out of time

A run that reaches its wall-clock time limit also parks — `hold_reason:
budget_exhausted` — instead of failing; it is never failed for the clock
alone, and this park has no expiry either. It isn't the pause on this page
(nobody asked for it) and it isn't the completion hold above (nothing is
unfinished by the plan's own criteria — the run simply ran out of budget).
From the CLI: **extend** it (`uzi run extend <id> --by 2h`, which adds time
and resumes it in one step), **stop** it (`uzi run stop <id>`, milestone
issue runs only — finalizes the milestones already done into a merge
request), or **cancel** it (`uzi run cancel <id>`).

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
