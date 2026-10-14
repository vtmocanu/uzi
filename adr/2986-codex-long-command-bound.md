# ADR-2986: Explicit Codex lead long-command bound

**Status:** Implemented; validation results are reported with the change.
**Date:** 2026-10-14
**Related:** #2986, ADR-2903, PRD #1497; independent of #2956.

## Decision

An otherwise-authorized Bash command, including its wire alias uzi_bash, may
carry optional `long_running: true` only when the callback's normalized origin
is root and the immutable grant has isRoot=true. Any authorized command is
eligible: the worker does not classify commands by executable or text.
Absent or false retains 3,600,000 ms (60 minutes); true selects exactly
7,200,000 ms (120 minutes). There is no arbitrary model-supplied duration.
Present nonboolean values are bad_args. Child, unknown and nonroot-grant
true requests are denied before launch. Screening and path checks still apply.

The root dynamic schema advertises the optional boolean; the closed child
schema does not. Broker runtime validation remains authoritative even if a
caller bypasses schema validation. Replay fingerprints already bind arguments
and origin, so changing the flag under a used callback identity is changed
reuse, rather than permission to launch again.

SpawnCommandOptions carries the flag into makeDefaultSpawnCommand. Separate
command constants leave DEFAULT_WALL_MS, the run-wall fallback, unchanged.
The existing positional wallMs test seam remains; an optional final longWallMs
supports short eligible-bound fixtures. The selected bound goes to waitChild
and CommandDeadlineError, whose broker reply names the actual bound (120 min
in production). Child command and child-turn deadlines are unchanged.

## Bound rationale

The 120-minute bound provides headroom over reported 44–76-minute unit gate
stages. Those reports are the decision's evidence, not new measurements made
for this clarification. Run-wall limits remain unchanged and take precedence.
We explicitly accept that a hung opted-in command may occupy the lead for up
to two hours, subject to existing interruption limits. Gates needing more than
two hours are unsupported by this command bound.

## Rollout and safe downgrade

Request `long_running: true` only on an upgraded worker that advertises it;
that upgraded worker is required for the two-hour command bound. A legacy
worker that does not advertise the option may ignore the flag and keep its
60-minute command bound; rejection must not be assumed. An ordinary request
without the flag retains the older worker's 60-minute command bound.

At pre-fix base 2b84c993, dynamic-tools.ts advertised
`objectSchema({command:STRING,cwd:STRING},["command"],false)` with no
long_running property. The legacy schema does not advertise the flag, but
schema alone does not prove enforcement: routeToolCall passes raw arguments
to the broker, and no worker schema validation was found. No guaranteed
older-worker bad_args rejection was observed (base `dispatchShell` reads only
command and cwd, so it ignores the flag), and no live rollout validation
is claimed here.

## Ownership and interruption

The command stays in the foreground and owns its root until primary exit,
deadline, abort or the existing output cap. The seam awaits root reap and
output drain before success or a clean deadline failure. Unclean disposal
fails closed and poisons the registry, rather than claiming the tree stopped.
An abort already ready with the deadline, or arriving during deferred reap or
drain, beats command_deadline after clean cleanup. No new cancel API, silence
detector, automatic extension or background-command mechanism is introduced.

Quiet or redirected output does not shorten or extend the selected command
bound. The existing approximately 20-minute health flag is advisory, not a
command kill timer. Checkpoint ticks continue; a lead checkpoint callback
queues behind the active effect and runs only when ownership can settle.
ADR-2903's bounded Read observation lane can read a growing known log while
the command remains active, even with checkpoint or other effects queued.
Read is not a snapshot and does not alter the deadline.

PRD #1497's run wall takes precedence over either command bound: it interrupts
the turn and routes through parkForWall. Owner cancellation, pause-now and turn
abort still stop owned commands. The runner's capture boundary must settle
callbacks and reap roots before the checkpoint/park sink; merely dropping a
transport reply proves no cleanup. This decision neither depends on nor
implements #2956.

## Prompt policy

Shared Codex guidance identifies the root lead as the sole opt-in authority
and explicitly tells child agents they cannot opt in. It states 60/120-minute
finite limits, retains the foreground gate/log/exit-status recipe, and allows
Read observation of a known log. Suspected hangs remain finite and owner/run
interruption remains available. The Claude recipe is unchanged.

## Regression evidence and limits

The independently runnable `#2986 base RED` executor test uses only existing
imports and the original function signature, with an additive options cast.
Its inert supervisor models elapsed=3,600,001 ms: the old bound rejects
immediately, whereas 7,200,000 ms returns success with independent stdout and
stderr. The test was run against an immutable export of the pre-fix base
2b84c993316bb33f3fb4f9cc7fd13674383b7dc6 and failed with
CommandDeadlineError(3600000); the fixed candidate passed.

Focused tests pin absent/false/true waitMs, root/child/unknown/grant mismatch,
malformed flags, canonical/wire aliases and changed replay. Short eligible
bounds exercise deferred cleanup and fail-closed reap; the broker checks the
actual 120 min reply. Abort fixtures cover both-ready and abort during disposal.

The actual executor uses its production seam over an inert supervisor for
held eligible commands under the actual run timer, owner cancel, pause-now,
transport EOF/turn abort and a deadline with disposal held across the run wall.
The capture seam uses the real safety boundary before the park sink. The tests
assert command disposal, callback quiescence, provider retirement and no deadline
reply at the wall. Quiet-output coverage verifies no idle trip while the callback
owns the command. Real-broker ADR-2903 fixtures read a growing known log with
checkpoint and effects queued for both ordinary and eligible commands.

These are hermetic focused checks, not a real supervisor, live Codex, Kubernetes,
full-stack proof. The required full gate is task gate:agent; its result and
measured timings are reported with the implementation. CI critical-path impact
and deployment validation remain unmeasured.
