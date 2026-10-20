---
title: Completion holds and the structural interlock
order: 112
audience: user
---

# Completion holds and the structural interlock

Some issue runs carry a **structural completion interlock**: a frozen checklist
of the milestones the run's plan was approved with, checked against what the
lead actually declared complete before it can open a closing pull/merge
request. This page explains what a hold looks like when the interlock catches
something, and what you can do about it.

This is **on by default for a new, unseeded issue run using either the Claude
or Codex harness**. An admin can turn it off from **Admin → Instance →
Completion check** (`completion_interlock_rollout`). An explicit off is the
admin kill-switch for new runs. A cold settings read without a valid cache
snapshot also leaves that new run unstamped; later runs can be stamped once
the setting is readable. A run started from a **seeded plan**
(`uzi run create --plan-file`) is never checked either: it
never goes through the plan-bearing report this interlock hooks into, so it
is never interlocked in the first place. And a run created before this
default changed is unaffected either way: the switch is read once, when the
run is created, never retroactively against a run already in flight.

This default also gates who can pick up the run: a worker must advertise the
`completion_interlock_v1` protocol capability to claim a stamped run at all.
For a Codex run, the worker must also advertise `codex_completion_interlock_v1`
so it can run the Codex completion-attempt loop.
If none of the run owner's online workers has the required protocol
capabilities, the run stays queued. Health can report an earlier placement
blocker, such as a missing repo capability. Once those checks pass, it names
a missing shared `completion_interlock_v1` protocol before a missing
`codex_completion_interlock_v1` protocol; a custom Codex model can have its
own capability gap. Upgrade the owner's workers to this release to clear it (a worker from v0.83.0 onward can claim an interlocked
Claude run, but an older one than this release may still pause it at finalize;
see the CHANGELOG). Turning Completion check off does **not**
release a run already queued this way, since the switch is only read when a
run is created: the stuck run needs a capable worker, or cancel it and create
it again.

## Why a run holds

Before an interlocked run can finalize, the server checks that every required
frozen milestone is declared complete, formally deferred by the owner, or
covered by an explicit owner accept-as-is decision. Accept-as-is leaves the
criterion unmet and records a warning; it is not evidence that acceptance ran.
A completion attempt with unmet milestones does not immediately fail or open
a pull/merge request:

1. The lead checkpoints its work and the server checks the frozen contract.
   If the lead requested an owner decision with `signal_done` (see below),
   the first authoritative unmet result opens the **Completion blocked**
   question directly, with a window to answer before the run parks.
2. Without that request, missing milestones are named back to the lead
   **in the same session** — no re-planning, no new run. Three identical
   consecutive completion attempts (same unmet IDs, branch head and worktree
   fingerprint) open the same question.
3. If that window passes with no answer, the run parks in a **completion
   hold** — a `paused` run, but held for this structural reason rather than
   because you asked it to pause.

A completion hold does not authorize publication. It preserves work for a
later decision. Normal capture verifies the Git work before parking. The
pre-push guard also has a bounded degraded-capture path: after capture
exhaustion it can hold with an empty captured head, retaining the clone and
session without claiming a verified recovery copy. See
[Recovering unpublished work](run-recovery.md#completion-publication-holds).

## Requesting an owner decision

On an interlocked, noninteractive issue run, both Claude and Codex advertise
this optional `signal_done` input:

```js
signal_done({ owner_decision_requested: { reason: "The remaining milestone needs an owner scope decision." } })
```

The request object accepts only a string `reason`: after trimming, it must
contain 1–500 Unicode code points. Invalid values and extra keys (including
scope or decision keys) are refused, not truncated. This input is not
advertised outside those eligible runs.

This is a model request for a decision, not a scope change. It opens the
window only when the server finds unmet criteria; if the criteria are met,
the request does not block normal finalization. Requesting a decision does
not reset the iteration or wall budget, extend the deadline, or bypass
cancellation, pause or completion-budget bounds. Leads needing an owner
scope decision should use this request rather than `park_run`, retries
without progress, or an announcement of publication.

## Planning work and pending acceptance

Put owner or deployment acceptance in a separate final milestone when it
must happen outside the worker. Implementation, documentation, tests,
mandatory gates, security checks and review remain worker work. Approval
or auto-approval of that plan does not waive them.

Formal owner partial is the scope-reduction authority: it records the exact
kept and deferred milestone IDs and a reason. This applies to ordinary
coding work too, not just acceptance milestones. An ordinary answer or a
non-closing declaration grants no waiver.

On a partial delivery, the pending checklist keeps each deferred entry
unchecked and includes its ID, frozen title and reason, explicitly marked
**not delivered** or **not verified** before owner closure. Checking a box
grants no completion authority; a refresh restores it unchecked. Put
actionable next steps in the ordinary summary outside the worker-owned
completion block.

## What you'll see

The run view and `uzi run get`/`uzi run list --json` distinguish three states
for an interlocked run, all server-computed so the web UI and the CLI never
disagree about which one applies:

- **Checking completion** — the run is running, has made at least one
  completion attempt, and nothing is currently unmet (it's re-verifying before
  finalizing).
- **Reworking unmet milestones** — the run is running, has made at least one
  attempt, and named milestones are still unmet. The run page shows the
  bounded list of unmet milestone IDs and the attempt count.
- **Completion blocked** — the run is either asking you the completion
  question live, or has parked in the completion hold. Same label either way,
  since both mean the same thing: the run needs a decision.

The completion panels show the bounded server-provided unmet-milestone list,
the attempt count and the hold reason. The durable question feed also shows
an explicit lead request's reason, escaped and labelled **Lead reason
(untrusted display)**. That display text supplies no decision authority; the
unmet IDs come from the server, not the lead's reason.

## Answering the completion question

When a run asks its **Completion blocked** question, you have a fixed window
— the instance's `completion_hold_window_seconds`, defaulting to **900
seconds (15 minutes)** — to answer while the lead session is still alive and
can act on your answer immediately, with no restart. If the window elapses
with no answer, the run parks in the hold instead; it never fails outright
for this reason.

An ordinary answer in the question box (or `uzi run answer <id>`) continues
with guidance only: it defers or waives nothing. Repository docs and
`non_closing` on `signal_done` likewise cannot change the frozen scope.
Only the owner decision endpoints apply formal completion decisions.

## Deciding on a held run

The owner has three formal decisions through the run view or
`uzi run decide <id>` (exactly one is required). Each is available both in
the **live completion-question window** and in the existing
`completion_blocked` hold. An ordinary clarification or plain owner pause
is ineligible; requesting a decision does not widen that admission:

**Continue** — continue with guidance, keeping the frozen contract unchanged:

```
uzi run decide <id> --continue [--guidance "<text>"]
```

- Answered inside the live question window, this resumes the run in place —
  no new claim, no re-planning.
- Answered after the run has already parked in the hold, this resumes the run
  through `queued` like [an ordinary resume](run-pause.md#resuming), carrying
  your guidance to the run's next session as a follow-up.

Guidance is optional: `uzi run decide <id> --continue` with no `--guidance`
simply tells the run to carry on with the required work.

**Partial** — reduce scope by naming the exact milestone ids to keep, with a
required reason:

```
uzi run decide <id> --partial m1,m2 --reason "<why>"
```

The named milestones remain required; the rest are deferred with your
reason. If publication succeeds, the `[partial]` pull/merge request is
**non-closing** and lists the deferred milestone IDs and reason, so the
remaining work stays visible for a later run.

**Accept** — accept exact unmet criterion IDs as-is, with a required reason:

```
uzi run decide <id> --accept m2.c1,m2.c3 --reason "<why>"
```

Use the exact criterion IDs from the run view or `uzi run get`. The named
criteria remain unmet in the publication warning, with your reason. This
accept-as-is decision permits proceeding despite them; it does not turn
unperformed acceptance into a passed check. It is refused when all milestones
are already done. Closing remains possible
only if the run is otherwise eligible; a decision does not promise
publication or override the remaining completion and publication checks.

Partial and accept each require `--reason` and send the run's current contract
revision, which the server fences: if someone has already recorded a decision
(the revision moved), your command is refused with a conflict rather than
silently overwriting theirs. `--guidance` is valid only with `--continue`.

After any answer, the worker supplies a neutral follow-up asking the lead to
call `signal_done` again for a server-authoritative completion recheck. It
does not require the old unmet list or infer a formal decision from prose.

## Mixed-version limits

A new API includes `completion_scope` in each granted worker permit, derived
from the same locked run revision that authorized the permit. It is `{}`
when there is no scope decision. A new worker uses this permit scope for
partial/accept rendering, title, body, `Closes` and finalization guards.

With a new worker and an old API, an absent field falls back to claim scope
only when the permit is at exactly the claim revision (default 1 when absent).
If the permit revision differs and the field is missing, finalization fails
closed into the existing hold without opening a pull/merge request. An old worker
ignores the new API's additive field and retains its previous behavior:
live-window decisions can still render claim-time scope. Upgrade **both API
and worker** for the permit-scope guarantee. The permit-scope addition itself
adds no capability, state, endpoint, migration or claim field and does not expand decision admission. Durable non-closing
intent has separate version requirements below.

## The honest limitation: same-worker-only context

**Be aware that a completion hold's conversation continuity is not durable
across workers.** Verified captures can recover Git work on another worker.
A degraded pre-push hold retains local work without verifying a recovery copy, so losing
that worker can lose the latest work. The run's provider session (the live
conversation state a same-worker resume continues without re-planning) survives only as
long as the same worker that held it is still around to reclaim it, bounded
by the same worker-affinity ceiling that already governs every other parked
run (2 hours by default; see
[ARCHITECTURE.md's Run lifecycle](../ARCHITECTURE.md#run-lifecycle)). A hold
that outlives that ceiling, or whose worker is gone, still resumes — but as a
fresh session against the recovered Git work, the same as any cross-worker
resume, not a continued conversation.

The run page and `uzi run get --json` surface this plainly as
`hold_context: unavailable(same_worker_only)` rather than implying the hold
is durably recoverable everywhere. Durable cross-worker conversation context
is a separate, later piece of work — this page will be updated when it ships.

## Closing directives and the completion block

Every interlocked run's merge/pull request has a completion block that uzi
alone owns and rewrites. On a verified full delivery the worker writes its
`Closes #N` line there. These checks protect the worker-owned body even when
publication fails or a human has touched the request in between:

- **On a verified head, if the `Closes` write can't be confirmed** — the
  write itself fails, the read-back to confirm it fails, or the PR's head
  moved in between — uzi strips `Closes` back out. The run holds once that
  strip is confirmed, or once the request turns out to be unreadable and the
  strip falls back to writing a non-closing body blind (with no read-back,
  but no closing directive by construction). Only when the strip write itself
  is refused, or is written but a confirming read-back shows it didn't land,
  does uzi skip the hold: it fails the run closed instead, since a hold is a
  nominally non-closing parked state and uzi can't guarantee that here.
- **On a non-closing run** — a hold, an owner-partial delivery, or one
  scope-capped mid-flight — uzi scans the whole resulting request body,
  not just its own completion block, for a closing directive aimed at the
  issue. If it finds one anywhere it doesn't own (a human typed `Closes #N`
  into the description, or the request is an older one uzi adopted that
  already carried one), it rewrites the whole body to a non-closing form.
  If that rewrite can't be *written*, or is written but the read-back
  shows it did not land, the run fails closed rather than leaving a
  possibly-closing request open; if the request was unreadable to begin
  with, a blind rewrite that succeeds is still accepted, even though
  nothing read the body back to confirm it. A read-back that errors
  outright after the rewrite is written plays out differently by run
  kind: on a legacy (non-interlocked: seeded, kill-switched or pre-rollout) run,
  that failure has no further fallback
  and the run fails closed there and then; on an interlocked run, that
  same failure falls through to the same blind whole-body rewrite the
  unreadable case uses, and is accepted on the same terms. This scan and
  rewrite apply to legacy runs too, not only interlocked ones.
- **On a delivery the lead declared non-closing** (via `non_closing` on
  `signal_done`), uzi also guards title and commit messages against closing
  the source issue. On interlocked issue runs the commit guard also covers
  owner-partial and Stop-capped delivery.
  Before pushing, it checks the messages of every commit the push would
  publish (relative to the default branch, merge commits included, and again
  on the exact commit about to be pushed) with the same matcher as the
  body. On a hit it stops publication (no further push, no request opened or
  updated, no worker history rewrite). An interlocked run holds, naming up
  to five offending commit SHAs and the remaining count. Unreadable messages
  (including a non-UTF-8 encoding header) refuse publication without naming
  a SHA. A legacy declared run still fails at this guard. The
  committed work is preserved for recovery, and fixing the commits needs a
  history rewrite left to the owner. A new
  request's title is the issue title with any closing keyword for the issue
  neutralised (a zero-width space after the keyword's first letter, or the
  fixed `Work on issue #N` if that isn't enough). After the request is
  created or adopted, uzi reads its actual title and retitles it if it
  closes the issue. If the title can't be read, can't be updated, or still
  closes after re-reading, the run fails closed; the request stays open and
  must not be merged as-is. Owner-partial and scope-capped deliveries get the
  same title neutralisation and fail-closed title confirmation, because they
  are intended to keep their issue open. Interlocked owner-partial and Stop
  runs now also refuse earlier-flight closing commits before pushing,
  reversing the declared-only exception from #2815. Legacy scope-capped runs
  still guard title/body without the added commit guard, so a commit can
  close their issue. Full and accept-closing deliveries keep the normal path.

## Fixing a publication hold

For a declared or owner-partial run whose offending commits are still
unpublished, choose **Continue** and give guidance naming the logged SHAs:
ask the lead to reword its own unpublished commits in the next turn. The
worker does not rewrite history or force-push. Partial and accept decisions
cannot fix commit history.

If the history was already published, or the run has a Stop directive,
cancel and manually deliver the preserved work through
[run recovery](run-recovery.md#completion-publication-holds). A Stop resumes
directly into finalization without another lead turn.

Partial and accept irreversibly change this frozen contract. A separately
approved, explicit milestone reconciliation reset can replace it and clear
scope ceilings and completion decisions; stored non-closing intent survives
that reset. See [the CLI reconciliation procedure](cli.md#commands).
Stop is an owner directive on a running run. Wall parking follows #1497;
provider-policy parking under #3068 is planned, not shipped.

## Durable non-closing intent and remaining limits

On interlocked issue runs, a supported worker records the first non-closing
reason with a completion attempt and restores it on reclaim. The reason is
NUL-stripped, trimmed and required to contain 1–500 Unicode code points;
invalid values are refused, not truncated. It survives resume, holds,
owner decisions and milestone reconciliation. It conservatively restricts
publication; it does not waive scope or satisfy a criterion.

Both harnesses carry this flight's latched declaration on every completion
attempt. The lead must still redeclare it on each `signal_done`. If a
`signal_done` is re-prompted before its attempt (for example by secret
remediation), the declaration is not durable until the next recorded
attempt. A park, reclaim or hold before that attempt can lose it.

Upgrade API and workers together for `completion_non_closing_v1`. Without
that API feature a new declared pre-push refusal remains terminal rather
than holding; existing executor and post-push holds are unchanged and can
lose the declaration. An old worker ignores restored claim intent. See
[Worker versions and upgrades](worker-upgrades.md#completion-publication-compatibility).

Before pushing an interlocked issue run, the worker obtains one running
publication-start acknowledgement after prechecks. Its strict revision and
deferred-ID projection selects the guard; it is not publication authority.
Unknown running projections hold without exposing commit text; a non-running
acknowledgement quietly stops this flight. Transport errors keep existing
finalization-failure handling. After pushing, the normal exact-head permit
must match that acknowledged revision and deferred-ID set.

The commit scans cover default-branch..tip, including earlier flights,
alignment bridge commits and an adopted own branch, and repeat on the exact
candidate before push. They cannot unpublish earlier history or prevent an
already-open request from being merged before a resumed guard runs. Claim
loss accepted between acknowledgement and push remains possible: the API
does not fence the Git push itself. Corruption outside supported writers is
not detected before push; the later permit fails closed.

MR rework (`mr_rework`) and branch-adopting CI fix (`ci_fix`) are excluded.
Forgejo/Gitea can close an issue on any branch push; the non-branch checkpoint
refs `refs/uzi-checkpoints` and held refs `refs/uzi-held` are outside that
branch-push behavior. These two residuals share the recorded finding handoff
#3b56e6e5-4352-4694-ae1c-4999d5719e9c (a finding ID, not a forge issue).

This remains structural enforcement. #1230 D4 and #1231 remain open; there
is no plan-time preauthorization or active semantic completion audit.
Live deployment acceptance has not been performed; mocked tests or an
existing PR do not establish it.

## Not the same as an owner pause

A completion hold and [an owner-requested pause](run-pause.md) both leave a
run sitting in `paused`, but for a different reason. An owner pause is
something you asked for; a completion hold is something the run's own
structural check produced for unmet milestones, following either a lead
request or repeated attempts without progress. The **live** completion
question (before the run has actually parked) is not a `paused` run at all,
so the ordinary **Resume** control
doesn't apply to it. An ordinary answer continues with guidance; use the run
view or `uzi run decide <id>` for a formal continue, partial or accept
decision. Once a run has parked with `hold_reason: completion_blocked`, plain
`uzi run resume` is refused. Use `uzi run decide <id>`; an eligible
`uzi run set-token` switch can also resume it. Neither a credential switch
nor ordinary guidance changes the frozen completion scope.

Related: [Pausing and resuming a run](run-pause.md) ·
[Autopilot](autopilot.md) · [CLI](cli.md)
