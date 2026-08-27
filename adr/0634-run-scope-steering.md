# ADR-634: Run scope steering — one ordered ceiling, honored at the loop top

**Status**: Accepted (PRD #634, all milestones M1–M7 landed)
**Date**: 2026-08-23
**Deciders**: architect (design), team lead, coders (M1–M7)
**PRD**: [prds/done/634-run-scope-steering.md](../prds/done/634-run-scope-steering.md) (GitHub issue [vtmocanu/uzi#634](https://github.com/vtmocanu/uzi/issues/634)) — the PRD carries the milestones, the evidence run, and the full Decision Log (D1–D6); this ADR carries the durable design shape and its rationale.

## Decision (summary)

A mid-run operator directive to stop or narrow a milestone-structured `issue` run's scope now writes a **single nullable column**, `runs.scope_ceiling INT` — the count of milestones permitted over the **immutable** `milestones_frozen` list (NULL = unbounded). `uzi run stop` resolves server-side to `scope_ceiling = completed_count`; `uzi run scope --through N` clamps to `[completed_count, len(frozen)]`. Both ride the per-iteration `reportIteration` ACK (re-read every loop, self-healing) and the claim payload (durability across requeue). The worker honors the ceiling at the **top of the implement loop**, not at the cooperative checkpoint: when `completed_count >= scope_ceiling`, it starts no further milestone and takes PRD #517's graceful finalize (push + MR iff `open_mr`), reporting `completed` with `stop_kind='scope_capped'`. The disposition (`applied`/`declined`/`superseded`) is settled server-side and surfaced through `uzi run inputs`, the web steer-queue card, a `steer_ack` feed message, and (surface-only) the run's Slack thread.

## Context

An operator who sends `uzi run follow-up` to bound or redirect an approved, in-flight run has no reliable lever: the message is delivered, `uzi run inputs` shows it `consumed_at != null`, and the lead runs the approved plan to completion anyway. "Consumed" reads as "landed"; it means only "handed to the worker." A recorded run (issue #602, Auto mode) shows the concrete failure: four follow-ups narrowing scope, including a stop and then a correction reversing it, all consumed, none honored — the lead committed milestones the operator had explicitly asked it to defer. Four structural facts underlie this: a follow-up is untrusted advisory prompt text the model may ignore; the implement loop has no operator-driven honor-point, only a cooperative checkpoint the lead may under-fire; `stop` is rejected outright on non-interactive issue runs (`ErrStopNotInteractive`); and `run_user_inputs` carries no applied/declined disposition, only a delivery timestamp. This PRD closes the not-honored and not-surfaced-honestly gaps together.

## The decisions

### D1 — One ordered field, not two racing signals

All scope control — a bare STOP and a forward ceiling alike — writes the same column, `runs.scope_ceiling`. This is the load-bearing choice. The rejected alternative was a **two-signal model**: a sticky `stopRequested` flag riding the fast, consume-on-read `/inputs` poll for STOP, plus a `scope_ceiling` column on the slow per-iteration ACK for the forward case. The two signals would carry no shared ordering — a worker could not tell which of "STOP at M4" and "actually finish M5" arrived last, and the fast flag would race ahead of the slow ACK and finalize at M4 before the raise to M5 was ever read. That is exactly the evidence run's failure shape, and back-to-back directives are the *common* case in Auto mode, not an edge case. Collapsing both actions onto one column makes supersede genuinely **last-writer-wins**: `scope_ceiling = 4` then `= 5` is two writes to the same field, and the later one simply overwrites the earlier — no cross-channel race to reason about. `stop_kind='scope_capped'` is a *derived* finalize disposition stamped at completion, never a control signal in its own right.

### D2 — Delivery: the existing per-iteration ACK, plus the claim payload

The claim payload is read once at claim time, so it cannot carry a change to an already-running worker. Delivery instead rides the `reportIteration` ACK the worker already makes every loop iteration, extended to carry the current `scope_ceiling` and the server's `completed_count`. Re-reading both every iteration means a mid-run change lands on the next loop and self-heals if a single ACK is dropped. The claim payload *also* carries `scope_ceiling`, so a requeued or re-claimed worker (death, `limit_wait` resume) honors a ceiling set while it was gone — the same cross-claim durability PRD #517/#552 already gave the stop flag. `completed_count` is always the server-supplied `milestones_completed` union, never a worker-local tally, because a local tally does not survive resume.

### D3 — Honor point: the loop top, not the cooperative checkpoint

The gate sits after `reportIteration` returns the fresh ceiling and before the turn dispatch, so it fires **every iteration** regardless of whether the lead cooperatively checkpointed. When `completed_count >= scope_ceiling`, the run takes the graceful-finalize path and starts nothing further. STOP is immune to PRD #390's report-under-firing risk by construction: the ceiling equals the count already done, so the very next loop-top finalizes without needing any future index update. A **forward** ceiling (a count above the current progress) is only as robust as the lead's mid-run progress reporting — accepted as a named risk, not solved here, and the reason STOP-alone remains a clean fallback cut if forward-ceiling robustness proves fragile in practice.

### D4 — The frozen milestone set stays immutable

`milestones_frozen` is written once at run-start and never re-applied. `scope_ceiling` is a separate truncation control layered over it, never a rewrite of the approved record. "What was approved" and "how far the operator let it run" stay two distinct, independently auditable facts — the frozen list answers the first, the ceiling (and its audit trail) answers the second.

### D5 — The control is the column; the steer-queue row is audit-only

Each scope-set additionally inserts a `kind='scope'` `run_user_inputs` row in the same statement as the column write, for surfacing — but the row carries no control authority, and the worker never routes it. This sidesteps a fragile alternative: a brand-new *control* steer kind would be silently dropped by `SteeringChannel.route`'s `default:` arm on any worker predating this change, indistinguishable from "never answered." Because the ceiling travels as durable `runs` state on the ACK/claim path instead, the audit row's kind is free to be ignored by an older worker with no correctness consequence. STOP likewise stops riding a dedicated `stop` control wire on an issue run; it is one more write to `scope_ceiling`.

### D6 — Disposition is settled deterministically, server-side

Each scope-directive row settles to exactly one of `applied` / `declined` / `superseded`. `superseded` is folded into the same CTE that writes a later `scope_ceiling`, using the MVCC snapshot property that a not-yet-committed earlier write is still visible to be marked superseded when the later one lands; `applied`/`declined` are settled from the worker's outcome report at completion. Settling is idempotent — a repeat settle is a no-op, and `superseded` can never overwrite an `applied` row that actually fired. This closes the "consumed means delivered, not applied" confidence gap: `uzi run inputs`, the web steer-queue card, and a `steer_ack` feed message all surface the real outcome, not just delivery.

### D7 — Judge eligibility is preserved, with context

A scope-capped run (`status=completed`, `kind=issue`) stays judge-eligible; its `stop_kind='scope_capped'` is carried into the judge input as explicit context. Without this, the judge's retrospective pass would score an operator-directed partial completion as an incomplete implementation — a false defect against the operator's own instruction.

### D8 — Slack: surface-only, pinned

A scope-capped completion is rendered in the run's Slack thread (narrowed-scope label plus the N-of-M milestones actually completed), matching how PRD #517's stop already surfaces there. This PRD does **not** add a set-scope or stop control from Slack — the CLI (`uzi run stop`, `uzi run scope --through N`) and the web run view are the write surfaces. A Slack-originated control is left for a future PRD if the run-control-card pattern (PRD #322) is extended to this action.

### D9 — Guardrails are unchanged

`SubmitInput` stays owner-scoped through the existing `GetRunByIDForUser` → 404 check; no new trust boundary is introduced. `main` is never touched — finalize is the run's ordinary push-and-open-MR path. A scope directive is honored in Auto mode: it is a distinct, explicit operator action, not the plan-approval gate that `auto_approve` short-circuits, so autopilot runs remain steerable mid-flight.

### Zero-slice behavior

A STOP delivered before any milestone has produced committed work completes report-only, with **no MR opened** — not an empty MR. The graceful-finalize path only opens a merge request when there is a committed slice to hand a reviewer.

## Consequences

- **Two writers, one column.** `uzi run stop` and `uzi run scope --through N` on a milestone-structured issue run are now the same mechanism at different resolved values; a reader auditing scope behavior only ever needs to reason about `scope_ceiling`.
- **A forward ceiling inherits PRD #390's reporting risk; STOP does not.** This asymmetry is deliberate and named, not a gap to silently close — see the PRD's Risks section and the Decision Log's STOP-alone fallback.
- **The steer-queue audit row can never desynchronize the worker**, because the worker structurally never reads it — only the column does.
- **Slack is read-only for this control today.** An operator directing scope narrowly must use the CLI or web; Slack shows the outcome after the fact.
- **The judge's retrospective pass is no longer penalizing intended partial completions**, but only for runs that reach `scope_capped` — an ordinary hard `cancel` still discards work and is unaffected by this ADR.
