# ADR-3068: Owner consent governs fresh lead conversations after a Codex policy refusal

**Status**: Accepted
**Date**: 2026-10-17
**Issue**: [#3068](https://github.com/vtmocanu/uzi/issues/3068)
**Plan**: [PRD #3068](../prds/3068-codex-policy-refusal-hold.md), including its Decision Log and three serial milestones.
**Supersedes**: #2321's terminal-root-refusal choice only for the negotiated admission below. Detection, content-free metadata, children, historical records and the rest of the fleet retain their behavior.

## Context

A terminal root refusal prevented the owner from supplying narrower guidance while retaining the run's work. Silently starting a new conversation would discard context after a provider refusal without informed consent. The approved choice is an owner hold with preserving resume, a separately authorized fresh claim, eligible Stop and Cancel. This records the completed M2 worker/server/CLI contract; web, TUI and Slack controls are M3, pending the next milestone.

## Decision

### Admission and preservation

The positive fence is negotiated API feature `provider_policy_hold` and worker capability `provider_policy_hold_v1`, Codex, root origin, noninteractive, and kind `issue`, `task`, `prompt` or `self_improve`. The exact tags are `cyberPolicy` and `misalignmentPolicyViolation`. Other kinds, interactive runs, Claude, children and unnegotiated workers retain their previous behavior. Historical terminal refusals stay terminal. Child refusals return `child_policy_refused`; they do not determine the parent's outcome.

An eligible refusal parks `paused` / `provider_policy_refusal`, with detail `refused`. Capture precedes park delivery: retain clone, HOME, session identity, generation pins, custody and normal worker affinity; attempt guarded capture and checkpoint publication. Failed publication still holds, with local-only or unverified preservation stated truthfully. A verified local source does not establish remote durability or recovery of the latest edits. An accepted or pending hold does not enter normal success publication or generic failure handling. Cancel wins over capture; a pending pause or worker wall trip is satisfied by the negotiated hold.

Root planning refusals during `awaiting_approval` are admitted for a verified current-claim revision turn, with source-bound gate revision checked under lock. Preserve the plan, presentation, gate revision, frozen/candidate milestones and pending feedback. Draft planning output remains unapproved evidence.

### Until-terminal consent and the Docker exception

The durable worker constraint is **nonterminal and `policy_refusal_count > 0`**. Successful continuation, running reports, credential changes, another hold reason and reclaims do not clear it. Each later fresh claim requires explicit fresh owner consent, until terminal. An actual further refusal increments the count once; `resume_rejected` retains the count, timestamp and last refusal metadata.

This is the approved narrow exception to [the Docker fresh-session default in specs/human.md](../specs/human.md#feature-1783--confirmed-run-quiescence-and-attempt-unique-clone-paths) (line 1162 at approval), and [ADR-1783](./1783-run-quiescence-and-attempt-clone-paths.md). Attempt-unique clone paths and quiescence requirements remain. For a constrained run, a changed Docker/retained-recovery attempt path, different worker, absent session id, missing or unknown local transcript, or rejected provider `thread/resume` re-parks `resume_rejected` unless that exact claim has a fresh grant. Attempt isolation must not be weakened to regain conversation continuity. Ordinary unconstrained Docker resumes keep their fresh-session default.

Preserving resume requests the original conversation; it cannot promise provider acceptance. The neutral turn request carries the constraint and exact grant to the harness's `thread/start` boundary. That boundary rejects unauthorized fresh starts, including the executor's resume-error fallback. Prompts, guidance and provider text confer no permission. Preserve #3045's reconciliation predecessor-session suppression (`reconciled_claim_session.go`): a suppressed transcript is not offered to the lead; without fresh consent the constrained claim re-parks.

Fresh retry atomically writes `policy_fresh_claim_generation = claim_generation + 1`, clears the active session identity while retaining its prior value in the audit, persists guidance and queues the run. ClaimRun increments generation normally; equality with the stored grant authorizes that claim. ClaimRun does not consume or refresh the grant. A reclaim advances generation and cannot reuse the fresh grant. It may preserve a durably recorded conversation; without one, it needs new fresh consent, including after a crash before the new thread identity becomes durable.

A constrained continuation with recorded completed work but no recovered delivery tree re-parks before milestone reset or execution, including with fresh consent. Conversation consent does not authorize clearing completion or counting unavailable work as delivered. Reached Stop scope must be verified at the executor boundary; a failed report or incomplete acknowledgement re-parks before a lead turn, keeping completion and ceiling intact. Recovered files, checkpoints, scope ceilings and completion contracts survive fresh retry. Normal preserving resume clears an unconsumed fresh grant.

### Plan source and verdict protection

[ADR-1604](./1604-plan-gate-verdict-durability.md), [ADR-1795](./1795-gate-revision-bound-verdicts.md) and [Feature #1604](../specs/human.md#feature-1604--plan-gate-verdicts-survive-interruptions) (lines 1150–1153 at approval) remain the source/verdict boundary. Policy continuation can reuse only verified persisted revision source and gate replay context: matching persisted plan presentation/time, valid positive gate revision, and replay cutoff judged against the plan the owner saw. The worker reads pending verdicts before presenting a gate. Missing/unmatched source or verdict context is ignored or rejected with a feed notice asking the owner to re-send, subject to the existing superseded/repeat-approve and empty-change exceptions. Outside the verified policy-gate continuation, the ordinary Codex fresh-plan/replay rules remain.

A carried approve never authorizes an unseen plan. Receipt means received, not durably applied; an ignored approve cannot become human approval. A fresh-session grant authorizes a conversation, not a plan. Full submitted plan text remains readable under #3053, with a neutral Full plan title and existing CLI version/diff behavior; visibility is not approval.

### Owner actions and request binding

The [CLI reference](../docs/cli.md#codex-policy-refusal-owner-hold) describes the available M2 controls:

- `run resume <id> [--guidance <text>]` preserves the conversation. Guidance is a `follow_up`, persisted in the resume transaction and included in the first resumed root turn, including planning/revision context.
- `run resume <id> --fresh-session [--guidance <text>]` explicitly discards conversation context and grants one exact fresh claim. It does not discard recovered files or change contract scope.
- `run stop <id>` is available for a noninteractive milestone issue with at least one completed milestone. It atomically sets the completed-scope ceiling and queues finalization. Partial and fully completed scopes both use normal executor execution. A reached authoritative ceiling skips only the early session re-park and reaches the normal scope boundary with no lead turn and no further milestone. Normal result, quiescence, completion-permit and publication guards still apply. Finalization uses recovered completed work; separately bounded delivery summaries grant no implementation authority. Unexpected fresh-thread requests still hit the harness guard. Rejection or fresh retry retains the Stop ceiling.
- `run set-token` routes an otherwise eligible paused policy-context mutation through preserving resume; a credential choice is not fresh consent. Its existing Anthropic-only validator still rejects Codex with 422, so this is not a new Codex credential-switch control.
- `run cancel <id>` terminates; custody disposition is separate. Discard is not a substitute for Resume, Stop or Cancel. Existing pending-outcome cancellation confirmation still applies.

`POST /api/runs/{id}/resume-now` accepts an optional strict object with `mode: preserve|fresh` (default preserve) and string `guidance` (valid UTF-8, no NUL, at most 16000 decoded bytes). Empty-body ordinary callers remain compatible. Policy mutations require `If-Match` from the presented DTO's `policy_refusal.action_if_match`: the quoted token `"policy-<outcome UUID>-<claim generation>"`. Owner/state/token validation, choice, audit, guidance, session/grant and queue transition share the locked transaction. Missing/stale binding is a conflict; clients must present the changed state for a new decision, never silently refetch a token and retry an old intent. Stop and preserving credential promotion retain that binding too. Worker credentials cannot perform owner actions.

Held time is banked; resume adds no wall budget. Warn **extend first** when spent. An ordinary policy hold may queue without remaining time; a retained `budget_exhausted` hold requires extension first. Extension is not fresh-session consent and must preserve the constraint and reviewed action binding.

### Durable delivery and historical authority

`POST /api/worker/runs/{id}/policy-hold` strictly accepts immutable UUID `outcome_id`, `source_generation`, current `claim_generation`, `detail: refused|resume_rejected`, head/publication/latest-content evidence and optional session identity. `refused` requires validated tag, phase and opaque correlation; `resume_rejected` needs no reconstructed refusal metadata. Revision holds include `expected_gate_revision`. Content-free correlation is `pr-` plus 32 lowercase hex digits, a hyphen and a positive decimal safe-integer suffix (maximum 9007199254740991), 37–52 characters total. Raw provider text is excluded.

Journal the immutable outcome before active-flight retirement; advertise it as `terminal_pending` in snapshots/heartbeats and replay the hold endpoint, not a fabricated terminal `/state`. Journal failure retains the blocked flight/source. At claim entry reconcile surviving local records at any generation before execution. A current exact unreleased claim authorizes its worker's own surviving older local evidence conversion without requiring an old custody row. Unapplied evidence re-parks at the current generation with the same outcome id. `already_applied` retires evidence without undoing owner Resume/fresh choice or spending its grant; `resolved_newer_policy` does not resurrect an older episode. Claim supersession alone proves neither application nor safe deletion. Unknown delivery stays pending; definitive terminal/Cancel resolution remains separate from custody cleanup.

Late foreign/historical reporting outside the worker's own surviving-journal conversion under its exact current claim requires the exact run/G custody record with `original_worker_id` matching the authenticated original worker, matching run/hold/worker owner, and state other than `discarded`. Lock that provenance through mutation and check it before idempotency or metadata changes. Current worker identity and `released_worker_*` are insufficient: ClaimRun clears the latter. Missing, discarded or mismatched records return `unauthenticated_historical_outcome` with a content-free notice and record no constraint, count or metadata.

In the exact released server-wall G case retain `budget_exhausted`, record the refusal once, and do not revive G. After G+1 exists, authenticated late G reporting records identity/count/metadata for future reclaims only; it does not change G+1 status, session, generation, grant, scope, gate or execution. Preserve newer applied policy outcomes and owner choices. Server-wall release drops affinity on owner resume; preserving context there will normally re-park rather than transfer a transcript.

### Accepted residuals

1. `TerminalPendingLease` defaults to **1 hour**. If the original worker is absent past it and another worker claims before the journal evidence can be consulted, replay can occur. The other worker cannot read the original local journal. Surviving same-worker evidence closes the local reclaim gap, not a shared-journal/global exactly-once guarantee.
2. The server wall sweep can release G before refusal evidence arrives. The owner can extend/resume and another worker claim G+1 while the count is zero, permitting an unconstrained fresh replay. The normal live/capable worker grace is 600 seconds; stale/incapable backstops may act sooner and arbitrary delivery delay is not bounded by this grace. Authenticated late evidence constrains future reclaims; missing/discarded provenance leaves that protection unavailable. Do not retroactively cancel G+1 or enlarge global wall grace to conceal the residual.

Neither local gates nor fake app-server observations establish that a real provider will accept resume. There is no promise of global exactly-once execution, permanent conversation availability or recovery of unpublished latest edits.

## Rollout, resources and rollback

Deploy API/migration first, then capable workers. Activation requires generation fencing, run snapshots and positive pending-outcome lease protection together. Missing prerequisites keep the feature off. Constrained queued/reclaimed runs require `provider_policy_hold_v1` independently of ordinary capability scheduling or owner override, including after a running report clears the hold reason. Claim, peer placement, ephemeral demand/saturation and health mirrors retain custody, credential, affinity and #3045 reconciliation fences. Old terminal reports are not reinterpreted as holds.

The hold has no expiry or refusal cap, automatic refusal retry or harness switch. It retains the issue lock and normal worker affinity. Open custody counts against admission (default **8**); a nonterminal owned run makes its hosted worker busy. Holds can pin upgrades until drain deadlines, but eviction, roll and storage loss still happen and may destroy conversation/local-only source. The execution slot frees after park/outcome handling permits flight retirement. Cancel and explicit custody discard remain distinct.

API rollback with outstanding holds, pending policy intents/outcomes, or constrained queued/running claims is unsafe. Quiesce workers and resolve delivery before guarded migration Down; this is not a rolling downgrade. [Migration 00340](../api/internal/store/migrations/00340_policy_hold.sql) takes a table lock and refuses affected nonterminal rows outside paused/queued or released `limit_wait`, `pool_wait`, `recovery_wait`, `awaiting_approval`, `awaiting_input`, `awaiting_followup`. Affected means count > 0 or a non-null fresh grant, regardless of hold reason. Protected live/orphaned states refuse before conversion or column removal. Convertible rows, including wall-race budget holds, become terminal `provider_policy_refusal` failures with session and custody preserved; unrelated rows remain. Then its columns are dropped.

## References and validation limits

Web/TUI/Slack owner controls remain pending M3. Deterministic tests establish local safety behavior; real-provider resume acceptance and live-provider E2E remain unobserved.

Implementation anchors: [owner service](../api/internal/workersvc/policy_hold.go), [resume request handler](../api/internal/handler/resume.go), [CLI flags](../api/cmd/uzi/run_limits.go), [worker contract and acceptance plan](../prds/3068-codex-policy-refusal-hold.md). The plan remains the complete acceptance/verification record; this status does not assert provider resume acceptance.
