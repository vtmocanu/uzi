# ADR-1795: A plan-gate verdict binds to the server-allocated revision it was sent against, not the worker's per-claim epoch

**Status**: Accepted
**Date**: 2026-09-27
**Deciders**: architect (design), team lead. Reviewed with the Codex peer (3 rounds).
**Issue**: GitHub issue [vtmocanu/uzi#1795](https://github.com/vtmocanu/uzi/issues/1795).
PRD: `prds/1795-gate-revision-bound-verdicts.md` (Decision Log carries the amendments cited
below).
**Related**: builds on the plan-revision loop and gate epochs of PRD #41
(`prds/done/41-plan-revision-gate.md`), the replayable verdict receipts and
`resume_plan_at`/`resume_plan_seq` replay cutoff of
[ADR-1604](1604-plan-gate-verdict-durability.md), the resumed-gate epoch bump for
executors without `resumesAtGate` from issue #1772, and PRD #37 (agent selection on
approve) and PRD #1247 M5 (`resume_plan_seq`).

## Context

A plan-gate verdict (`approve_plan`, `reject_plan`, `revise_plan`) was matched to a gate by
the worker's **per-claim gate epoch**, taken when the verdict was routed, not by the plan the
human actually saw on screen. That produced two concrete faults.

1. **Report-response race.** The worker sends the `awaiting_approval` report and bumps its
   epoch only after that report resolves. The server makes the new gate visible the moment it
   persists the report — before its response reaches the worker — while the worker's steering
   poll loop runs independently. An approve created in that window routed at the OLD epoch,
   then got discarded as epoch-stale once the bump ran, and was settled `superseded` through
   the discard lane. The human had approved the plan on screen and got a stale-approve notice
   instead.
2. **No client-side anchor.** Nothing told the server which plan a web tab, CLI invocation or
   Slack card displayed. A human approving from a tab still showing plan N, while the run had
   already gated plan N+1, could approve an unseen plan whenever the verdict happened to route
   at the new epoch. Slack alone guarded against this, with its own `gate_ts` anchor check.

The epoch is per-claim and restarts at 0 on every new claim, so it cannot serve as the
judge: a verdict routed on a fresh claim always looks current against that claim's own epoch,
regardless of which plan it was written against. Only a fact that outlives the claim — a
revision the server allocates and stamps atomically with publication — can do that job.

## Decision

Bind every plan-gate verdict to a server-allocated **gate revision**, keyed by a
worker-minted **presentation id**, rather than fixing the report-response race by moving the
epoch bump earlier (rejected: still admits an old-plan approve during the report window, and
`bumpEpoch()` also opens the fail-closed replay window, so moving it can end that protection
early with no way to roll back concurrent routing or discards already in flight).

**D1. Gate presentation identity.** The worker mints a presentation id for each gate it
shows. A retry of the same report (lost response), or an SDK resume re-presenting the
persisted unapproved plan at its gate, reuses that id; every other gate (fresh plan,
revision, a stub/Codex resumed gate) mints a new one, even when the plan text is identical —
text or status equality cannot tell these cases apart. The server maps a presentation id to a
monotonically increasing per-run gate revision: a new id allocates `revision + 1` atomically
with persisting the report; a retry or re-presentation succeeds only for the run's current
presentation and only with an unchanged approval-relevant payload (`plan_md`, milestones,
required capabilities, required tools, size class — **not** `plan_changed_files`, which is
advisory worktree evidence recomputed each round and excluded from identity, amendment A1,
2026-09-27). A historical id (any presentation older than current) is refused, so a replayed
old presentation can never overwrite a newer gate.

A refused report — a historical id, a changed-payload conflict on the current id, or a stale
adoption (D4 below) — is a **failed gate report** (amendment A3, 2026-09-27): it never
publishes the refused payload as a new gate, and is never retried under a fresh id. The
worker confirms no revision and takes no verdict; it parks through the existing
transient-recovery path (`recovery_wait`), and the next claim re-presents. Repeated refusals
are bounded: the server counts a refusal once per claim generation, under the same run-row
lock and claim fencing as the report, and the refusal that would push the count past
`RUN_GATE_REFUSAL_MAX` (default 3, `0` = unlimited) fails the run with fail origin
`gate_presentation_refused`. A successful publication resets the count. This applies only to
plan-gated run paths (issue runs and any other kind reaching `awaiting_approval` through the
human gate); chat, judge and autopilot never allocate a gate revision.

**D2. Stamping under the run-row lock.** Every insert of a gate verdict takes the same
run-row lock as gate publication and binds the input to the run's current gate revision
inside that statement — an UPDATE-backed insert obtains it through `RETURNING`, never a
sibling snapshot read — so an insert cannot bind to a stale revision across a concurrent
publication. The `expected_gate_revision` check (D5) runs under the same lock, before any
side effect: a mismatch inserts nothing and changes nothing (no agent selection, milestone
freeze, `revise_count` increment, `stop_kind` stamp, or capability clear).

**D3. Three input bindings, persisted.** A verdict row carries one of:
- **bound(N)**: stamped with revision N; the worker takes it only at the gate whose revision
  is N (exact match), bypassing the epoch and replay-cutoff checks for it (otherwise a
  matching input could be discarded before revision matching even runs);
- **unbound**: created while no gate was visible. An unbound **approve** can never act on a
  later gate — it is disposed of as stale with a feed notice, through the existing discard
  lane. An unbound **reject** or **revise** keeps today's legacy handling, which is
  fail-closed on its own terms (a reject only stops the run, a revise only re-plans and
  re-gates), so a reject sent during initial planning still stops the run at its first gate,
  and pre-gate revise feedback is taken at the first gate (amendment A2, 2026-09-27 — narrowed
  from an earlier draft that would have disposed of every unbound verdict);
- **legacy**: no binding recorded (rows from before the migration, or an api without this
  feature); today's epoch and replay-cutoff behaviour, unchanged.

Absence of the field is legacy; an explicit unbound value is never treated as legacy.

**D4. Worker protocol and compatibility.** A new capability (`gate_revision_v1`) gates the
wire fields; the report ACK carries the allocated revision, and the claim carries the
persisted presentation id and revision for a gate resume. An old worker against a new api:
the api still stamps, the worker ignores the fields, behaviour is today's (epoch matching). A
new worker against an old api: no fields arrive, every input is legacy. A new api receiving
an old worker's report (no presentation id) allocates a new revision unconditionally; the
persisted gate then has a revision but no presentation id. A new worker resuming such a gate
sends an explicit `adopt_gate_revision: N`, never inferred from an unchanged payload (an
identical-text stub/Codex gate would look the same); the server accepts the adoption only
under the run-row lock when the current revision is N, its id is null, the payload matches,
and claim fencing passes — then binds the id to N so a pending bound approve still counts.
Any other state is a stale-adoption refusal, itself a failed gate report under D1's refusal
accounting. The api advertises support for the new report fields in the worker register
response, so an older api's strict request decoding never sees an unknown field from a newer
worker.

**D5. Client-supplied expected revision.** The run DTO exposes `gate_revision`.
`POST /api/runs/{id}/inputs` gains an optional `expected_gate_revision` for the three gate
verdict kinds; present and different from the run's current revision (or the run not at the
gate at all) answers 409 `gate_revision_mismatch` naming `current_gate_revision`. Omitting it
keeps today's behaviour for older clients and scripts. Each consumer binds to the revision it
**displayed**, captured when the human starts the action and retained through any
confirmation, never refetched at submission: the web gate UI and its reject/revise/
capability-override dialogs; the TUI's confirmation capture; the CLI's
`--expected-gate-revision`, defaulting (only while the run is `awaiting_approval`) to the
revision read at invocation, which proves the verdict targets the gate current at that
moment, not a plan read earlier — a mismatch exits 5; Slack's gate cards and text-reply flows,
which persist the originating card's revision alongside the existing `gate_ts` anchor.

## Consequences

- **The report-response race (fault 1) closes.** An approve routed in the gap between report
  persistence and ACK now binds to the revision the persisted report allocated, not to a
  per-claim epoch that has not bumped yet, so it is taken rather than discarded.
- **A stale client surface (fault 2) is refused, not silently misapplied.** Any client naming
  the revision it displayed gets a typed 409 instead of an approval landing on a plan it never
  showed.
- **Rolling upgrade is bounded but safe.** A bound row replayed to an old worker is matched by
  epoch (today's behaviour), so the fix is only as wide as the new-worker fleet; the
  capability makes that state observable rather than silent.
- **A dead-forge-style lifetime cap for refusals**, mirroring `RUN_FORGE_UNREACHABLE_MAX_PARKS`
  and `RUN_LIMIT_MAX_WAITS`: a run stuck re-presenting a gate it cannot durably publish does
  not park forever.
- **The epoch and #1604's replay-cutoff machinery are not removed.** They remain the legacy
  path for rows and peers that predate this capability (D3); a later cleanup can retire them
  once every supported worker advertises `gate_revision_v1`.
- **Lock contention** from run-row locking at both publication and every verdict insert is
  bounded to one run; there is no cross-run lock.

## The seam every future verdict path and gate path must respect

1. **A gate revision only advances on an accepted publication.** Never allocate one for a
   refused report (historical id, payload conflict, stale adoption) — a new verdict path that
   allocates on a refusal reopens B3-style double-allocation risk.
2. **A verdict insert must stamp its binding from the SAME locked read that decides the
   run's current gate state**, never from a sibling snapshot taken before or after the lock. A
   new insertion seam that reads the revision outside the lock (or after releasing it) can bind
   to a revision a concurrent publication has already superseded.
3. **`expected_gate_revision` (or any future client-anchor field) is checked before any side
   effect of the verdict**, under the same lock as the stamp. A future field that authorizes an
   agent-selection write, a milestone freeze, or a capability clear before this check runs
   reopens the exact race this ADR closes.
4. **`plan_changed_files` (and any future purely-advisory, recomputed-per-round field) never
   joins approval-relevant payload identity.** Adding it back would make a same-gate reclaim
   whose tree listing merely changed look like a new presentation (A1).
5. **An unbound approve is always disposed of; an unbound reject or revise always follows the
   pre-existing legacy path for that kind.** A future gate path (a new run kind, a new
   verdict type) that skips this distinction must justify it explicitly — treating every
   unbound verdict uniformly was tried and rejected (A2) because it broke the
   already-fail-closed handling reject/revise had before this feature existed.
6. **A client that displays a plan revision must retain it through confirmation, not refetch
   at submit time.** Refetching defeats the entire client-anchor guarantee of D5: a refetched
   revision always matches whatever is current, which is exactly the unguarded behaviour this
   ADR replaces.
7. **The refusal cap counts once per claim generation, fenced the same way the report itself
   is fenced** (ownership, claim generation, unreleased claim, non-terminal status). A report
   from a claim that fencing has already superseded neither counts toward the cap nor can push
   the run to `gate_presentation_refused`; only a refusal from the run's still-current claim
   counts.

## Alternatives considered

- **Bump the epoch earlier, before the report is sent.** Rejected: still admits an
  old-plan approve during the report window, and moving `bumpEpoch()` (which also gates the
  fail-closed replay window from ADR-1604) forward can end that protection early with no way
  to undo concurrent routing or discards already in flight.
- **Infer adoption of an id-less legacy gate from an unchanged payload.** Rejected: an
  identical-text stub/Codex resumed gate is indistinguishable from a genuine re-presentation
  by payload alone; adoption is an explicit `adopt_gate_revision` operation instead.
  Prevented a real ambiguity — a stub/Codex resumed gate frequently repeats the plan text
  verbatim without being the same presentation.
- **Treat every unbound verdict the same way (disposed of).** Rejected (A2): a reject or
  revise sent while no gate was visible already had fail-closed, useful legacy handling (stop
  the run; re-plan at the first gate); disposing of it as stale would regress behaviour users
  already depend on, for no safety gain — only an unbound approve can ever authorize an unseen
  plan.
- **Include `plan_changed_files` in approval-relevant payload identity.** Rejected (A1): it
  is advisory worktree evidence recomputed every round, not part of what the human is judging
  when they approve; including it would spuriously invalidate a same-gate reclaim whenever the
  worktree listing merely changed.
- **Retry a refused report under a fresh presentation id.** Rejected (A3): would let a
  historical-id or conflicting-payload report eventually publish anyway, defeating the refusal
  itself; a refused report is final for that presentation, and the worker must obtain a fresh
  claim.
