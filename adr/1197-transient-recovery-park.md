# ADR-1197: `recovery_wait` is a shared transient-recovery park primitive

**Status**: Accepted (issue #1197 — implements only the empty-SDK-result trigger; a provider-error trigger is deferred, see Consequences)
**Date**: 2026-09-08
**Deciders**: architect, coders, reviewers
**Issue**: GitHub issue [vtmocanu/uzi#1197](https://github.com/vtmocanu/uzi/issues/1197)

## Decision (summary)

A resumed SDK turn that comes back **positively empty** (zero turns, no model activity, no plan/questions/done) is retried a bounded number of times in-process, and only if it is *still* empty does the run enter recovery: the worker best-effort captures its local restore point **before** reporting the park (the causal fence — a server promotion or reseed can never race an in-flight capture), then always reports `status: "recovery_wait"`. The server parks the run on a **capped exponential backoff** — doubling from a short base, clamped at a ceiling, jittered to spread a promoted wave — and a sweep auto-promotes it back to `queued` once that backoff elapses, preserving `plan_md`, `session_id` and the worker's on-disk checkpoint exactly as a resume needs them. There is **no lifetime park cap and no terminal branch**: `recovery_wait` always becomes promotable again, so a run keeps recovering until it succeeds or the owner cancels it (`CancelRunServerSide`'s negative admit-set already covers a parked run for free).

`recovery_wait` is deliberately built as a **reusable primitive**, not a one-off fix: any transient cause can report it and reuse the same preserve→park→promote→reclaim lifecycle. This issue implements only the trigger for a positively-empty SDK result; issue #1088's provider-error classifier is expected to adopt the same status and lifecycle later, without building a competing mechanism.

## Context

Before this change, a resumed SDK turn that returned zero turns and no model activity was thrown as `REASON_NO_PLAN` and terminally failed the run — destroying resumable session and checkpoint state over what is frequently a transient blip (the model's own empty response to a resumed turn), not a genuine defect in the run. There was no non-terminal outcome for "the model gave us nothing, try again later" distinct from a genuine wall/idle/cancel trip or a turn that legitimately ran but produced no plan.

The existing park precedent, `limit_wait` ([ADR-0035](0035-run-limit-retry.md), PRD #35), already proved out the shape — non-terminal status, server-owned backoff, sweep-driven promotion, checkpoint preservation — for a **usage-limit** pause. `recovery_wait` is modelled closely on it, but is a different thing: it is not a usage limit, it does not touch any credential's rate-limit gauge, and (unlike `limit_wait`'s `RUN_LIMIT_MAX_WAITS`) it carries no lifetime park cap at all, because there is no budget being exhausted — only a transient signal that may take an unknown number of attempts to clear.

## The decision, in detail

### Bounded in-process retry, then a truthful park

`driveTurnWithEmptyRecovery` (`agent/src/sdk-executor.ts`) wraps a turn: the common case (any non-empty return, including a turn that ran but did not submit a plan, or one with missing metrics) passes through unchanged. Only a **positively-empty** result — checked by `isPositivelyEmpty`, which excludes missing metrics, a plan, questions, `done`, or any model activity — enters a small number of short, budget-accounted, cancel-safe in-process retries against the *same* `resumeId`, so a resumed turn's SDK session is never torn down mid-recovery. A genuine cancel/idle/wall trip always wins over recovery: `state.tripReason` is polled and thrown first on every attempt, and budget exhaustion is never itself a recovery trigger — the between-turns wait is debited from the wall so the next turn's own `armWall` trips the true `REASON_WALL` outcome rather than being reclassified as a park.

### Capture before report — the causal fence

Only once those bounded retries are exhausted does the worker escalate to `TransientRecoveryError`, handled by `handleRecoveryExhausted` (`agent/src/runner.ts`). Unlike `limit_wait`'s park (which reports first, then captures via the shared `execute()` block), the recovery path captures the local restore point **first** and reports the park **after** — a deliberate ordering so a server-side promotion or reseed can never fire while a capture is still in flight. The capture is a runner-uid dirty/clean split, `commitWipMarker`-on-dirty, `fetchAgentBranch` (which throws on failure, unlike the best-effort `fetchBackBestEffort`), then a positive verification that the bare's tracking ref actually covers the captured tip — because capture *attempt* completion is not capture *success* (`commitWipMarker` returns `false` for both a clean tree and a failed commit). The park always happens regardless of whether the capture verified or not — a local-capture shortfall no longer strands the run behind a `running` status nobody is driving; it always parks, and the feed line states truthfully whether a verified local restore point or a landed origin publish backs the park, or whether the run instead falls back to its last durable checkpoint.

The ack is discriminated the same way `handleLimitReached` discriminates its own ack: only the literal `ack.status === "recovery_wait"` counts as parked, never `applied` alone — a server that refuses or coerces the park still answers 200 with a different status.

### Server-owned capped backoff, no lifetime cap

`setRecoveryWait` (`api/internal/workersvc/recoverywait.go`) computes `retry_not_before = now + recoveryParkFallbackFor(recovery_wait_count) + jitter` and calls `SetRunRecoveryWait` (`api/internal/store/queries/runtime.sql`), whose positive `status = 'running'` source guard makes a re-delivered or out-of-order report a 0-row, idempotent no-op. `recoveryParkFallbackFor` is a capped exponential — doubling per prior park from `RUN_RECOVERY_PARK_BASE` (default 1m), clamped at `RUN_RECOVERY_MAX_PARK` (default 30m) — and, unlike the limit park's `recoveryParkFallbackFor` analogue, **always returns a finite positive duration**: there is no terminal or hold branch, so a park always becomes promotable again. `PromoteRecoveryWaitRuns` (same file) is the sweep's half: a single `UPDATE` that releases every row whose `recovery_retry_not_before` has passed back to `queued`, run every tick beside (never folded into) `PromoteLimitWaitRuns`, because it is a distinct clock-based hold.

### No lifetime cap, no terminal branch — the load-bearing difference from `limit_wait`

`limit_wait` bounds how long a run may hold the one-active-run-per-issue lock (`RunLimitMaxWaits × RunLimitMaxPark`), because a usage limit's exhaustion is itself bounded and a runaway park is a sign something else is wrong. `recovery_wait` has no such bound by design: a transient empty-turn condition (a provider-side blip, a degraded model) may legitimately take an unknown number of cycles to clear, and failing the run after some arbitrary number of parks would throw away a session and checkpoint that a slightly longer wait would have recovered. The guard against a runaway loop instead lives in the exponential backoff's cap (`RUN_RECOVERY_MAX_PARK`) and in the owner's own cancel — never in a lifetime counter. `recoveryParkFallbackMaxShift` bounds the *exponent* only, so an unbounded `recovery_wait_count` can never overflow the backoff duration into a negative (past) timestamp that would spin the sweeper.

## Consequences

**A run that hits a transient empty-turn result now recovers instead of terminal-failing**, preserving its plan, session and checkpoint across as many recovery cycles as it takes. The cost is symmetrical with `limit_wait`: a persistently-recovering run holds its worker's disk and its issue's run slot for as long as it takes, which is the trade this park always makes in exchange for never discarding recoverable work.

**This issue implements only the empty-SDK-result trigger.** No provider-error classifier is built here — issue #1088 is expected to reuse `recovery_wait` and its preserve→park→promote→reclaim lifecycle for a transient provider error, rather than invent a second, competing park status. Reviewing #1088 against this ADR should confirm it reports the same `recovery_wait` literal and reuses `SetRunRecoveryWait`/`PromoteRecoveryWaitRuns` rather than adding a new status or a parallel backoff mechanism.

**Key files**: `api/internal/store/queries/runtime.sql` (`SetRunRecoveryWait`, `PromoteRecoveryWaitRuns`), `api/internal/workersvc/recoverywait.go` (`setRecoveryWait`, `recoveryParkFallbackFor`, `recoveryParkJitter`), `api/internal/workersvc/sweep.go` (the promote pass), `agent/src/sdk-executor.ts` (`driveTurnWithEmptyRecovery`, `isPositivelyEmpty`), `agent/src/runner.ts` (`handleRecoveryExhausted`, `captureRecoveryRestorePoint`).
