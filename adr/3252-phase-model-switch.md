# ADR-3252: Claude planning model and effort, switched at plan approval

**Status**: Implemented on branch agent/issue-3252 (2026-11-21); exact-head CI pending
**Date**: 2026-11-21
**Related**: Issue #3252; [PRD #3252](../prds/done/3252-claude-planning-model.md) (the Decision Log carries D1-D19); Q-spike #3246 (mechanism evidence); epic #1703; [PRD #3253](../prds/done/3253-codex-planning-model.md) (Codex integration; evidence record maintained separately)

## Decision (summary)

A user can set an optional Claude **planning model** and **planning effort**. For Claude lead-lane runs, lead turns before plan approval use that pair; after approval the run switches to its own model and effort in the same session and conversation. Left unset, nothing changes.

- **Switch only at plan approval.** The pair is bound to the executor phase, not to a latch: a re-plan on a later claim uses the planning pair again with no special case (PRD D2).
- **The pair is fixed per claim.** Both pairs resolve when a worker claims the run and nothing is re-read at approval; a new claim re-resolves (D4). A model chosen at run start or pinned by a schedule, and an effort chosen at run start, independently win in both phases (D5).
- **Same-conversation evidence (D1).** Q-spike showed that resuming the plan session with another model and effort works on the pinned SDK/CLI: switch, reclaim and switch back all completed with the history intact. This is provider and runtime compatibility evidence. It does not establish that a switch took effect.
- **Never implement on the planning pair.** An unverifiable switch holds the run; it does not continue on the planning model, fall back to a fresh conversation, or substitute a value (D8, D9).
- **A settings barrier guards the switch (D18).** Before the first turn after approval the worker withholds the prompt and reads the runtime's applied model and effort (`getSettings`). The prompt is released only on a match with an expectation uzi derives itself.

The original Claude decisions below remain in force; the Codex integration is
recorded in the final section. Not shipped: a CLI setter for planning settings,
a fresh-conversation phase-switch fallback, and real-gateway or
subscription-auth validation of the Claude alias table. This ADR does not
claim completion of exact-head CI, hosted acceptance or the PRD evidence update.

## Context

Before this feature, a run resolved one model and one effort at claim and used them for every lead turn. A maintainer wants a strong pair to plan and a cheaper one to implement. Changing worker defaults between plan approval and implementation does nothing for a running session, because its claim already carried the old pair.

Two facts shaped the design. First, the SDK init frame arrives only after the prompt is consumed, and aborting on init still let one provider request through, so init cannot be a guard. Second, the pinned CLI answers a `get_settings` control request while the prompt is withheld, and its applied model and effort matched the first wire request in every characterized shape.

## Decisions

### Which runs and which workers

Lead-lane Claude kinds (issue, ci_fix, self_improve, prompt, mr_rework, task) carry a planning pair; review, chat, judge, cross_check and job runs, and egress-bound runs, do not. Only workers advertising `claude_phase_models_v1` claim a run that carries one (D6). This is a claim requirement mirrored in every claimability query and health path, not a degrade: a missing capability waits, never substitutes. The predicate over-approximates to keep every mirror simple (D7), so a run can be pinned to a custodian that cannot claim it after a capability requeue; that residual is accepted and documented for users.

Hosted placement provisions a current worker, and the ephemeral demand gate admits runs that need only the phase capability. Placement uses an updated admitting-workers function shipped by migration.

### The barrier and its expectation table

The worker compares `applied.model` and `applied.effort` with an expectation uzi resolves independently, never one taken from the runtime's own answer.

- **Characterized aliases only.** The expectation table is `CLAUDE_CANONICAL_MODELS` (alias to canonical id) and `CLAUDE_OMITTED_EFFORT` (omitted-effort default) in `agent/src/claude-canonical-models.ts`, characterized against the pinned CLI and re-checked on every SDK or CLI bump. At decision time it mapped `opus`, `sonnet`, `haiku` and `fable` to `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-haiku-5-5` and `claude-fable-5-1` (defaults `medium`, `medium`, `medium`, `high`); since #4619 (SDK 0.3.325) `fable` maps to `claude-fable-5-2` with default `medium`. Read the file for current values. An alias outside the table cannot be verified and holds.
- **Full ids compared verbatim.** A full model id, or a provider-mapped id (`ANTHROPIC_DEFAULT_<ALIAS>_MODEL` taken verbatim), is compared as given.
- **Omitted effort.** The Q-spike finding is that an omitted effort reverts to the model default on resume, so an explicit planning effort with a Runtime-default implementation effort is allowed; the combination rule in the original PRD text is inactive (D19). The expected default must still be established independently, or the run holds: with no effort sent, it comes from the model the runtime reports, and that model must be one of the four characterized ids. A provider run with no implementation effort therefore holds unless the provider maps the alias to one of those ids.
- **Missing method, timeout or malformed answer holds.** `getSettings` is present at runtime but absent from the public typing, so a pin-sensitivity characterization test guards every SDK or CLI bump.
- **Init reports, never gates.** A later contradictory init stops the query and holds; it cannot undo a request already admitted, and the docs do not claim it can.

Rejected: abort-on-init (lets a request through), an unverified switch, and a fresh-conversation fallback while the barrier holds.

### Unsupported configuration

A planning model with no implementation model (no worker model and no template model) is refused before any turn as an unsupported phase configuration naming both remedies. The refusal does not depend on approval state: a fresh pre-approved start with no session is refused too, because with no implementation model there is no model key to switch back with, and no start or resume is left to run under the planning model (`agent/src/sdk-executor.ts`). A resumed run at its gate is refused even if the owner would just approve. An empty worker-model lane with the builtin lead template (`opus`) implements on the template model. The refusal is skipped on provider runs, because the provider map always yields an explicit model or fails with `provider_model_unmapped`.

### The hold

A run whose switch cannot be confirmed parks `paused` with `hold_reason: phase_switch_unavailable`, owner-resumable, through a generation-fenced active-run park. The hold request names the phase, which only decides whether the plan-approval check applies; nothing stores it. The server admits a plan-phase hold without an approval check and checks plan approval only for the implement phase, answering `409` with reason `plan_not_approved` for an unapproved run.

- **Resume** queues a new claim whose phase follows server state (`resumePhaseFor` in `api/internal/workersvc/claim_assembly.go`). The hold can be taken in the plan phase on an unapproved run, so Resume does not always continue to implementation. An approved run implements directly and re-verifies the switch. An unapproved run with a submitted plan and session shows the plan gate again, and a revise re-plans and re-verifies. A run with no plan plans again. **Cancel** is the ordinary path. Nothing promotes or extends out of the hold on a timer.
- **Outcome classification.** The worker classifies the server's answer: applied, already applied for this generation (a lost-ACK retry), refused with the run's real status, or no longer this worker's. An implement-to-plan re-post is a distinct classified outcome, not an error. An owner Cancel during an unconfirmed hold reports cancelled.
- **Readopt exclusion.** A held run is not readopted as ordinary running work.
- **Late-report guards.** In both `paused` and `queued`, late worker reports that would move the run on are refused: AwaitingInput, AwaitingFollowup, AwaitingApproval, Completed, Failed, FailedPlanRejected and SupersedeRunByWorker. CancelRunByWorker is exempt because it carries owner intent. RecordCompletionAttempt is left unguarded, like the base-attestation hold.
- **Lost-ACK and Resume race.** If the hold was applied but the worker never saw the answer, the worker's retry is classified by the server's answer for that generation (already applied, or refused with the run's real status), so an owner Resume that raced the lost ACK is not undone by the retry.
- **Unknown outcome.** When reads fail and the outcome stays unknown, the worker keeps the quiesced flight registered and keeps retrying, ownership read first, with no timeout of its own (a wall-clock bound would end a flight whose row may still be running and hand it to the missing-from-snapshot path). It stops only on a definitive answer (held, owner-resumed, a `plan_not_approved` re-post for the plan phase, claim gone, confirmed cancel), on worker shutdown, or on a run already paused at this claim with another hold reason and the claim kept (a defensive arm; no server-only producer is known), so the flight ends without posting and the feed says "The server paused the run first (<reason>); this worker keeps the work." The wall-clock sweep's park (ParkRunsAtWall) releases the claim, so it ends as the claim-gone case; the sweep skips interactive and awaiting-approval rows, so an unconfirmed hold on those retries until shutdown, cancel or a claim change (accepted). The shutdown case keeps its copy of the work and posts a feed line that says only what is known. When the worker shuts down or dies with the hold unapplied, its heartbeat snapshot no longer lists the run, and the server's missing-from-snapshot handling takes over, but only if the hold was not applied (the run is still `running` or `awaiting_*`): on the first heartbeat that omits the run once its last status change (`status_since`) is older than the stale window plus one heartbeat interval, it is requeued while under `RUN_MAX_REQUEUES`. Over the cap it parks in `recovery_wait` (cause `worker_requeue_exhausted`) when recovery evidence exists (checkpoint tip, available capture, publication enrollment, or uncertain or unknown evidence; `classifyWorkerExhaustion` in `api/internal/store/worker_exhaustion.go`), the usual case for a held run, which keeps its clone and custody. Only without evidence is it failed with "worker lost the execution; exceeded re-queue budget". If the hold was applied but never confirmed, the run is `paused` on `phase_switch_unavailable` and stays held for the owner (Resume or Cancel). The stale-worker sweep applies only when the worker itself is gone.
- **Process death** is not a new route. A worker crash before the hold is recorded follows the existing worker-lost policy and the existing stale-worker handling.

Presentation is added to the shared hold-reason set (web texts, CLI renderers, agent protocol unions) beside `run_override_unavailable`, without reusing its park path.

### Settings, migration and compatibility

Two nullable user columns store the pair; the settings API stays tri-state and the PUT remains cookie-only (D15: no CLI setter in this change). Planning effort accepts the five levels and no runtime token. The migration head moved while this was built, so the migration numbers are renumbered at landing. The vocabulary golden is untouched.

Deploy the API before relying on the capability. A worker deployed first against an older API keeps legacy behavior because the API never sends it a planning pair.

## Consequences and residual risks

- **Gateway rewriting.** A provider gateway that rewrites model ids can make a correct switch look like a mismatch and hold the run.
- **Alias stability.** The alias table was characterized on the pinned CLI with a dummy key; stability under subscription auth is not separately proven.
- **Provider check is create-time only.** The provider-lane check is made at creation, not re-checked at approval.
- **Late effort re-derivation.** The effort expectation is re-derived late, when the switch is verified; see PRD D19 for the boundary.
- **Cost.** Task (handoff) runs plan first, so a strong planning model raises their cost.
- **Over-approximation** can leave a run pinned to a custodian that cannot claim it, for a short resume-affinity window and, rarely, longer; cancel and restart clears it.
- **Pin sensitivity.** Any SDK or CLI bump must pass the barrier characterization first; if the barrier stops holding, a fresh-conversation fallback needs its own design and review pass.

## Codex

### Settings, scope and precedence (#3253)

Codex adds nullable `planning_codex_model` and `planning_codex_effort` beside
the Claude pair. Null means **Same as the run**; absent PUT fields are unchanged.
The cookie-only settings API and the existing Planning card expose both harnesses,
with one shared **Save planning defaults** action for changed fields. Planning
effort accepts the five explicit levels, not `runtime`. Safe custom model IDs
are allowed at worker/planning roots; Claude aliases are rejected in the Codex
lane. This does not widen run-start, schedule, role, review or summary vocabularies,
or change the dedicated cross-check pin contract.

The phase pair applies to Codex lead kinds issue, ci_fix, self_improve, prompt,
mr_rework and task. Review, chat, judge, cross_check, job and egress-bound lanes
retain their prior behavior. `resolvePlanPhase` in
`api/internal/workersvc/claim_phase.go` resolves each field independently: a
frozen run or schedule model wins in both phases, as does a frozen run effort;
otherwise the planning field overrides the implementation field before approval.
A later claim resolves its pair again, including a claim that re-plans.

API rollout precedes workers. The claim/placement requirement is
`codex_phase_models_v1`; a custom effective planning root additionally needs
`codex_custom_model_v1`. Curated planning and effort-only choices do not need
the extra custom capability. A curated frozen run model suppresses the custom
planning-root requirement because that frozen model wins in both phases.
Incapable workers leave the run queued with the existing phase/custom health
reason rather than silently dropping a setting.

### Cold same-conversation switch and guard

Approval resumes the plan thread in a cold provider epoch with the intended
implementation pair. Local replacement construction may precede teardown, but
old-harness disposal and proof of cleanup precede fresh provider preparation
and publication (`codex-executor.ts`, approval handoff). The closed harness
rejects late usage callbacks. The implementation model is latched within an
epoch: a later request changing it is refused before `turn/start`.

`CodexHarness.verifyPreparedPhaseModel` checks the sanitized canonical
`thread/resume` response model against the intended implementation model before
an implementation turn. A mismatch, missing field or unsafe value raises
`PhaseSwitchUnverifiedError`, using the existing `phase_switch_unavailable`
hold. Resume starts a new claim and verifies again; Cancel ends the run.
An unconfirmed phase transition cannot fall back to a fresh thread.

Explicit planning effort with Runtime-default implementation effort is accepted
in either save order. On pinned Codex 0.167.0 the cold resume reports effort
present as `null`; implementation sends no level and reporting invents no numeric
effort. This is the cold mechanism's allowance, not permission for a warm switch
that may retain an override. The model guard does not establish an independently
measured numeric effective effort.

### Accounting handoff and failure boundary

The integration reuses #4580's accountant and `beginModelSegment`; it does not
introduce another billing fold. A claim-shared pending transition survives
provider replacements. Verification binds to the new harness instance, so a
replacement must verify for itself before it can consume that transition.

In `CodexHarness.recordUsageNote`, buffered historical replay is recorded into
the planning segment before the single cut at the first matching root
`turn_started`. Preparing a thread is not a cut. Completion without a matching
start is not a cut and leaves the boundary unproved. The old harness's closed
fence prevents its late notes crossing into implementation. Child threads keep
their own registrations; each adopted response's `usage_record` carries its
segment model.

The accountant's seven-column monotone admission preserves proved claim deltas;
quarantine and `unreported` remain sticky and retain proven tokens. An empty
segment can rebind under #4580's rules. Reconciliation and pricing use each
segment's own model and response evidence; see
[ADR-2555](./2555-run-usage-attribution.md).

A process crash loses the accountant, unflushed frames and pending cut. A later
claim creates a new accountant registered to its current phase and baselines
proved replay history. It does not reconstruct missing spend. Delivered frames
remain idempotent and additive per leg through the existing server fold; neither
this switch nor its accounting claims fsync durability or storage-crash recovery.
Runtime-pin compatibility, live/hosted acceptance and exact-head checks need their
own evidence; this documentation update supplies none.
