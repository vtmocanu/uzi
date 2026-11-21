# ADR-3252: Claude planning model and effort, switched at plan approval

**Status**: Implemented on branch agent/issue-3252 (2026-11-21); exact-head CI pending
**Date**: 2026-11-21
**Related**: Issue #3252; [PRD #3252](../prds/done/3252-claude-planning-model.md) (the Decision Log carries D1-D19); Q-spike #3246 (mechanism evidence); epic #1703; Q-codex #3253 (the Codex half)

## Decision (summary)

A user can set an optional Claude **planning model** and **planning effort**. For Claude lead-lane runs, lead turns before plan approval use that pair; after approval the run switches to its own model and effort in the same session and conversation. Left unset, nothing changes.

- **Switch only at plan approval.** The pair is bound to the executor phase, not to a latch: a re-plan on a later claim uses the planning pair again with no special case (PRD D2).
- **The pair is fixed per claim.** Both pairs resolve when a worker claims the run and nothing is re-read at approval; a new claim re-resolves (D4). A model or effort chosen at run start or pinned by a schedule wins in both phases (D5).
- **Same-conversation evidence (D1).** Q-spike showed that resuming the plan session with another model and effort works on the pinned SDK/CLI: switch, reclaim and switch back all completed with the history intact. This is provider and runtime compatibility evidence. It does not establish that a switch took effect.
- **Never implement on the planning pair.** An unverifiable switch holds the run; it does not continue on the planning model, fall back to a fresh conversation, or substitute a value (D8, D9).
- **A settings barrier guards the switch (D18).** Before the first turn after approval the worker withholds the prompt and reads the runtime's applied model and effort (`getSettings`). The prompt is released only on a match with an expectation uzi derives itself.

Not shipped: the Codex half (see "Codex: see Q-codex (#3253)" below), a CLI setter for the two settings, a fresh-conversation fallback, and any real-gateway or subscription-auth validation of the alias table.

## Context

Today a run resolves one model and one effort at claim and uses them for every lead turn. A maintainer wants a strong pair to plan and a cheaper one to implement. Changing worker defaults between plan approval and implementation does nothing for a running session, because its claim already carried the old pair.

Two facts shaped the design. First, the SDK init frame arrives only after the prompt is consumed, and aborting on init still let one provider request through, so init cannot be a guard. Second, the pinned CLI answers a `get_settings` control request while the prompt is withheld, and its applied model and effort matched the first wire request in every characterized shape.

## Decisions

### Which runs and which workers

Lead-lane Claude kinds (issue, ci_fix, self_improve, prompt, mr_rework, task) carry a planning pair; review, chat, judge, cross_check and job runs, and egress-bound runs, do not. Only workers advertising `claude_phase_models_v1` claim a run that carries one (D6). This is a claim requirement mirrored in every claimability query and health path, not a degrade: a missing capability waits, never substitutes. The predicate over-approximates to keep every mirror simple (D7), so a run can be pinned to a custodian that cannot claim it after a capability requeue; that residual is accepted and documented for users.

Hosted placement provisions a current worker, and the ephemeral demand gate admits runs that need only the phase capability. Placement uses an updated admitting-workers function shipped by migration.

### The barrier and its expectation table

The worker compares `applied.model` and `applied.effort` with an expectation uzi resolves independently, never one taken from the runtime's own answer.

- **Characterized aliases only.** `opus` to `claude-opus-5-5`, `sonnet` to `claude-sonnet-5-5`, `haiku` to `claude-haiku-5-5`, `fable` to `claude-fable-5-1`. Omitted-effort defaults are `medium`, `medium`, `medium`, `high` respectively. An alias outside the table cannot be verified and holds.
- **Full ids compared verbatim.** A full model id, or a provider-mapped id (`ANTHROPIC_DEFAULT_<ALIAS>_MODEL` taken verbatim), is compared as given.
- **Omitted effort.** The Q-spike finding is that an omitted effort reverts to the model default on resume, so an explicit planning effort with a Runtime-default implementation effort is allowed; the combination rule in the original PRD text is inactive (D19). The expected default must still be established independently, or the run holds. Provider runs with no implementation effort always hold.
- **Missing method, timeout or malformed answer holds.** `getSettings` is present at runtime but absent from the public typing, so a pin-sensitivity characterization test guards every SDK or CLI bump.
- **Init reports, never gates.** A later contradictory init stops the query and holds; it cannot undo a request already admitted, and the docs do not claim it can.

Rejected: abort-on-init (lets a request through), an unverified switch, and a fresh-conversation fallback while the barrier holds.

### Unsupported configuration

A planning model with no implementation model (no worker model and no template model) is refused before any turn as an unsupported phase configuration naming both remedies. A fresh pre-approved start with no session is exempt because it never plans. A resumed run at its gate is refused even if the owner would just approve. An empty worker-model lane with the builtin lead template (`opus`) implements on the template model.

### The hold

A run whose switch cannot be confirmed parks `paused` with `hold_reason: phase_switch_unavailable`, owner-resumable, through a generation-fenced active-run park that records which phase it was verifying. The server admits a plan-phase hold without an approval check and checks plan approval only for the implement phase, answering `409` with reason `plan_not_approved` for an unapproved run.

- **Resume** queues a new claim that is already approved, implements directly and re-verifies. **Cancel** is the ordinary path. Nothing promotes or extends out of the hold on a timer.
- **Outcome classification.** The worker classifies the server's answer: applied, already applied for this generation (a lost-ACK retry), refused with the run's real status, or no longer this worker's. An implement-to-plan re-post is a distinct classified outcome, not an error. An owner Cancel during an unconfirmed hold reports cancelled.
- **Readopt exclusion.** A held run is not readopted as ordinary running work.
- **Late-report guards.** In both `paused` and `queued`, late worker reports that would move the run on are refused: AwaitingInput, AwaitingFollowup, AwaitingApproval, Completed, Failed, FailedPlanRejected and SupersedeRunByWorker. CancelRunByWorker is exempt because it carries owner intent. RecordCompletionAttempt is left unguarded, like the base-attestation hold.
- **Lost-ACK and Resume race.** If the hold was applied but the worker never saw the answer, the worker's retry is classified by the server's answer for that generation (already applied, or refused with the run's real status), so an owner Resume that raced the lost ACK is not undone by the retry.
- **Unknown outcome.** When reads fail and the outcome stays unknown, the worker retries for a bounded 30 minutes and then stops, keeping its copy of the work and posting a feed line that says only what is known. The server's stale-worker sweep then requeues the run within its budget, else it goes to `recovery_wait` or `failed`.
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
- **Over-approximation** can leave a run pinned to a custodian that cannot claim it, for about two minutes of resume affinity and, rarely, longer; cancel and restart clears it.
- **Pin sensitivity.** Any SDK or CLI bump must pass the barrier characterization first; if the barrier stops holding, a fresh-conversation fallback needs its own design and review pass.

## Codex

Codex: see Q-codex (#3253).
