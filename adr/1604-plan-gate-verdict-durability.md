# ADR-1604: A plan-gate verdict is applied only when its effect is durable, and a replayed one is judged against the plan the owner saw

**Status**: Accepted
**Date**: 2026-09-26
**Deciders**: architect (design), team lead.
**Issue**: GitHub issue [vtmocanu/uzi#1604](https://github.com/vtmocanu/uzi/issues/1604).
There is no PRD file; the issue carries the report. This ADR carries the decision, the
invariants a later change must keep, and the alternatives a future reader is most likely to
reach for.
**Related**: builds on the two-phase input receipts of issue
[vtmocanu/uzi#1673](https://github.com/vtmocanu/uzi/issues/1673) (ACK on receipt, APPLIED once
acted on). That work has no ADR of its own; its contract lives in
`api/internal/workersvc/input_receipts.go` and `agent/src/steering.ts`. The plan-revision epoch
this ADR extends is PRD #41 (Decisions 2 and 3, described in the `agent/src/steering.ts` header).

## Decision (summary)

The owner's plan-gate verdicts (`approve_plan`, `reject_plan`, `revise_plan`) could be lost or
misapplied when the claim holding the gate was interrupted: a credential switch, a worker
shutdown or eviction, or a resumed claim. Two faults caused it. The worker marked a verdict
applied before its effect was durable, so an interruption in between dropped it. And a
resumed claim replayed a verdict against whatever plan it offered next, which need not be the
plan the owner answered.

1. **Receipts extend to gate inputs.** A gate verdict is APPLIED only once its effect persists:
   - a **revise** once the revised plan is persisted (the awaiting_approval report that
     carries it is acknowledged as applied, `ack.applied === true` with status
     `awaiting_approval`). The executors pass the revise's input id to that gate as
     `settles` (`agent/src/executor.ts`, `agent/src/sdk-executor.ts`,
     `agent/src/codex/codex-executor.ts`; settled in `agent/src/runner.ts`);
   - a **reject** in the same statement as the run's `plan_rejected` failed transition
     (`SetRunFailedPlanRejected` in `api/internal/store/queries/runtime.sql`, called from
     `SetState` in `api/internal/workersvc/service.go`). A declined transition settles no input;
   - an **approve** only when a gate takes it.

   Until then the verdict stays unapplied and is replayed to the next claim. On the Claude SDK
   executor (`resumesAtGate = true`, `agent/src/sdk-executor.ts`) a replayed revise re-runs the
   revision with the owner's original feedback and does not re-offer the superseded plan.
   Codex has no resume-at-gate: a replayed approve, reject or revise is ignored with a notice
   (superseded ones as in point 6), and a fresh plan is shown (see residuals).
2. **A claim says when its plan was shown.** A claim carrying an unapproved persisted plan
   carries `resume_plan_at` (`ResumePlanAt` in `api/internal/workersvc/claim.go`, filled in
   `claim_assembly.go` via `LatestPersistedPlanFrameAtForRun`). It is the `created_at` of the
   latest `plan` frame whose payload `plan_md` equals the persisted `runs.plan_md`. The worker
   discards any replayed verdict created before that instant, or with an absent or
   unparseable `created_at` (`setReplayCutoff` / `disposedOnArrival` in `agent/src/steering.ts`).
3. **Fail closed without it.** When `resume_plan_at` is absent or does not parse, the worker
   treats every replayed approve or reject as stale until both of these hold: the replayed
   backlog is fully read (a GET page shorter than the server's LIMIT) and the claim's first
   gate is shown. On the SDK executor a replayed revise still acts, because its result is
   gated for a human again; on Codex it goes epoch-stale (see the Codex residual). The field
   is absent when the lookup failed, when the frame was tombstoned or redacted, or when the api
   is older than this change.
4. **No gate before the backlog is read.** A resumed claim with an unapproved plan reads its
   whole replayed backlog before it offers any gate. This holds for every executor, SDK and
   Codex (the `gatePlan` closure and `takeResumedGateEvent` in `agent/src/runner.ts`). An SDK
   claim then acts on the first pending event before it re-presents the plan: a cancel ends
   the run, a reject fails it, and a revise revises the submitted plan. A transient read
   give-up parks the run in `recovery_wait` through the existing `TransientRecoveryError`
   path without showing the plan. A definitive protocol failure fails the run.
5. **A discarded approve is settled without counting as approval.** The server treats any
   applied `approve_plan` as human approval (`human_plan_approved` in `GetRunClaimContext`,
   and `SetRunRunning`'s awaiting_approval guard). So an approve the worker judges stale,
   superseded or after-close is never APPLIED. The worker settles it through
   `POST /api/worker/runs/{id}/inputs/discarded` (`WorkerRunInputsDiscarded` →
   `DiscardInputs` → `DiscardRunInputRows`), which sets `applied_at` with disposition
   `superseded`. Both approval readers exclude that disposition. The row leaves the capped
   replay list without ever reading as approval. The route accepts only `approve_plan` rows
   (any other kind is a 400). An APPLIED receipt for a discarded row is a 409. An older api
   returns 404 for the route; the worker then logs once and leaves those approves unapplied.
6. **The feed names almost every ignored verdict.** A disposal emits at most one notice
   (`staleNotice` in `agent/src/steering.ts`). The reasons are: the verdict was written
   against an older plan version of this claim ("Approval ignored — the plan changed; re-send
   if you still want it.", "Rejection ignored — the plan changed; re-send if you still want
   it.", "Feedback ignored — it was written against an older plan version; re-send it.");
   it was sent before this plan was shown; which plan it was for could not be confirmed; the
   plan is already approved (cancel to stop the run); the gate had already closed; an older
   server already recorded the approval, which could not be withdrawn; or a buffered reject
   was replaced by a newer verdict ("an earlier plan rejection was superseded by a newer
   verdict"). Two disposals are silent: an approve replaced in the buffer by a newer verdict,
   and a repeat approve after an approve closed the gate. An empty revise is dropped (only
   logged) when it is routed and never reaches a disposal.
7. **A pending credential switch accepts only revise-only APPLIED batches.** A revise the
   worker settles while the switch is pending has a final disposition, so the server accepts
   it (`applyUnderSwitch` in `input_receipts.go`). A mixed batch, or one on a released or
   stale claim, is still refused. If the switch refuses an approve's APPLIED, the run takes
   the credential-switch path rather than reporting `running`
   (`approveRefusedBySwitchPending`).

## Context

- **The gate epoch cannot judge a replay.** PRD #41 stamps each buffered verdict with the
  gate epoch it arrived under, so a verdict sent against an older revision goes stale. The
  epoch is per flight and restarts at 0 on every claim. A verdict replayed onto a new claim
  therefore always looks current, even when the owner sent it against an earlier plan. Only
  a server-side fact that outlives the claim can judge it, and `resume_plan_at` is that fact.
- **The frame precedes the persistence.** The worker emits and flushes the `plan` frame
  before its awaiting_approval report persists `plan_md`. A declined report, or a worker that
  died in between, leaves a newer frame for a plan that was never persisted. That is why
  `resume_plan_at` matches the frame to the persisted `plan_md` and never falls back to the
  latest frame.
- **The replay list is capped.** `ListReplayRunInputs` returns unapplied rows oldest first,
  `LIMIT 1000`. Any row that stays unapplied forever eventually starves every later cancel,
  follow-up, answer or pause behind it. That rules out leaving discarded approves unapplied.
- **"Fully read" depends on the page size.** The worker counts the backlog as drained only
  on a page shorter than `REPLAY_PAGE_LIMIT` (`agent/src/steering.ts`). If the server's LIMIT
  dropped below that constant, a full server page would read as short. A gate could then
  appear with replayed verdicts still unread.
  `api/internal/store/replay_page_limit_test.go` pins the two values to each other.

## Invariants (a later change must not break these)

1. **No applied `approve_plan` unless a gate took it on the plan the owner saw.** Every path
   that disposes of an approve (stale, superseded, gate closed, unjudged) goes through the
   discard lane, never APPLIED.
2. **A verdict's receipt never precedes its durable effect.** Revise: after the revised plan
   persists. Reject: in the same statement as the failed transition. Approve: at the gate
   that takes it. Persistence and APPLIED are separate transactions for a revise, so an
   interruption between them can repeat a revision. Delivery is at-least-once, never
   exactly-once.
3. **Every reader of "approved" must exclude disposition `superseded`.** Today those readers
   are `human_plan_approved` in `GetRunClaimContext` and the approve clause of
   `SetRunRunning`. A new query that treats an applied `approve_plan` as approval without that
   exclusion reopens the fault.
4. **The worker's `REPLAY_PAGE_LIMIT` equals the server's `ListReplayRunInputs` LIMIT.**
   `replay_page_limit_test.go` enforces this.
5. **`resume_plan_at` never falls back to an unmatched frame.** It is absent when no `plan`
   frame's `plan_md` matches the persisted plan, and absence means fail closed.

## Alternatives considered

- **Settle a verdict on ACK.** Rejected. An interruption between ACK and the durable effect
  loses the verdict, which is the original fault.
- **Judge replays by the per-flight epoch alone.** Rejected. The epoch restarts at 0 on each
  claim, so a replayed verdict always matches the new claim's first gate.
- **Leave discarded approves unapplied.** Rejected. They would stay on the replay list
  forever and fill the `LIMIT 1000` window, starving later inputs.
- **Apply a discarded approve as a normal APPLIED.** Rejected. The server counts any applied
  `approve_plan` as human approval. A new disposition that the approval readers exclude
  settles the row without granting approval.
- **Fall back to the latest plan frame for `resume_plan_at`.** Rejected. That frame can
  belong to a plan that was never persisted, and it would move the cutoff past verdicts the
  owner sent for the plan actually on the run.

## Consequences and residuals

- **Older consume-on-read api.** That api applies every row as soon as it returns it. A stale
  approve read that way already counts as approval. The worker ignores it and says on the
  feed that it could not be withdrawn and that cancelling stops the run.
- **1000 or more pending revises.** A backlog that never yields a short page keeps the claim
  from counting as delivered, so it cycles through the recovery park. Recorded as a uzi
  incidental finding during the #1604 run.
- **A compromised worker holding the claim** can discard its own run's current approve. The
  impact is a stall (the owner re-approves), not an approval.
- **Pre-existing, recorded as uzi incidental findings during the #1604 run:** an approve
  freezes milestones and budget at submit time; a stale reject's `stop_kind` handling.
- **Codex does not resume at the gate** (out of #1604's scope). A resumed Codex claim always
  gates a fresh plan (the runner puts the run in `gatedRuns`, so its first gate bumps the
  epoch). A carried-over approve, reject or revise is ignored with a notice asking to re-send
  it: on arrival when it predates the shown plan or has no comparable `created_at` ("… sent
  before this plan was shown …") or the claim cannot say when the plan was shown ("Could not
  confirm which plan …", approve and reject only; an older api that consumed an approve on
  read says it was recorded instead), otherwise as epoch-stale at that first gate ("Approval
  ignored …", "Rejection ignored …", "Feedback ignored …"). The owner must re-send it: a
  reject sent before the interruption does not stop a Codex run. As in point 6, a verdict
  superseded by a later carried-over approve or reject gets no re-send notice (an approve is
  dropped silently, a reject gets "an earlier plan rejection was superseded by a newer
  verdict"), and an empty revise is only logged.
- **An api from before this change** sends no `resume_plan_at` (every replayed approve or
  reject fails closed as unjudged) and has no discard route (404). Those approves stay
  unapplied and pending, and count toward the replay `LIMIT 1000` window until the api is
  upgraded.
- **Resumed-claim cost.** A resumed claim with an unapproved plan waits for one full backlog
  read before its gate appears. A read that fails transiently parks the run instead of
  guessing.
