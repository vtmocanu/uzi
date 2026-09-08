# ADR-1197: verified capture before an automatic transient-recovery park

**Status**: Accepted for issue #1197; provider-error classification is deferred to #1088.
**Date**: 2026-09-08
**Issue**: [vtmocanu/uzi#1197](https://github.com/vtmocanu/uzi/issues/1197)

## Context

The failed run reported a zero-turn SDK result with no model activity.
The previous executor discarded that metadata and treated the missing plan
as a terminal failure. The upstream reason for the empty result was not
established. Separately, autopilot plans were not durably persisted, so a
resume could re-plan instead of continuing the approved work.

The existing usage-limit park provides a preserve, park, promote, reclaim
lifecycle. Empty-result recovery needs a distinct status: it is not a token
limit and must not alter credential rate-limit accounting.

## Decision

### Durable autopilot plans

Persist autopilot plan text through a dedicated worker-owned, running-state,
provenance-guarded update. Require positive acknowledgment before continuing
implementation. Re-delivery is idempotent; a stale plan report must not
overwrite a preserved plan or unpark a run.

### Bounded retry with accurate classification

Only positively empty SDK results enter bounded in-process retries:
zero reported turns, no model activity, and no plan, question or completion.
Missing metrics and nonempty turns retain their existing outcomes.
Carry forward any SDK session ID returned by an empty attempt, including
when the first attempt started a new session.

Retry backoff consumes the turn's wall budget. Genuine wall/idle timeout,
typed pause and cancellation remain higher-priority outcomes; budget
exhaustion is not itself a reason to enter recovery.

### Verified capture before a promotable park

After retries exhaust, reap the agent tree before inspecting its clone.
Read the dirty/clean state as the runner identity, require a successful WIP
commit for dirty work, fetch into the worker-owned bare repository, and
positively verify that its tracking ref covers the current clone HEAD.
Only then report `recovery_wait`. Remote checkpoint publication remains
best-effort and is reported separately from local verification.

A failed or unverified capture keeps the execution and steering poller
active, preserves the source clone and session, and retries with bounded,
cancellation-aware delays. A failed park acknowledgment is also retried:
a live worker heartbeat does not cause an abandoned running row to be
requeued. Cancellation is reported through the existing terminal protocol;
work is not discarded merely because a cancellation report failed.

Before model execution, record clone ownership in worker-owned bare Git
configuration. Retain that record with an unverified clone on shutdown.
A later same-run claim recaptures it before reseeding; a different run or
malformed ownership record cannot authorize deletion. Clone retention never
guards secret eviction, poller shutdown or registry cleanup.

Serialize duplicate claims for the same run through factory setup and all
final cleanup. After the prior batcher closes, refresh a queued claim's
message cursor through every remaining page before starting its batcher.
This prevents delayed park acknowledgments from causing overlapping clone
cleanup or dropping late messages through stale sequence reuse.

### Server-owned backoff

`SetRunRecoveryWait` admits only the owning worker's running, non-judge run.
A repeated report is an idempotent no-op; acceptance depends on the returned
status, not `applied` alone. `PromoteRecoveryWaitRuns` returns due rows to
`queued` while retaining their plan/session/checkpoint fields. Stale
approval reports cannot exit the park.

The server uses a capped exponential backoff, from `RUN_RECOVERY_PARK_BASE`
(default 1m) to `RUN_RECOVERY_MAX_PARK` (default 30m), with jitter. There is
no lifetime park cap or terminal branch caused solely by repeated empty
results. Normal running watchdogs and owner cancellation still apply.
The exponent is bounded to avoid duration overflow.

## Consequences

An empty result no longer immediately destroys resumable work. A persistent
capture failure consumes an active worker slot and storage while retrying;
a parked run retains its issue slot and recovery state. A different worker
can recover only a successfully published checkpoint. Local verification
does not protect against loss of the original persistent volume.

`recovery_wait` is the shared recovery primitive intended for #1088's later
provider-error classifier, not a second implementation of that classifier.

**Verification correction, 2026-09-08:** the initial implementation claimed
a prior checkpoint made it safe to park after failed capture. Real-Git
regressions with failed WIP commits and fetches disproved that claim: the
only current work copy could be deleted. The decision above requires
verified capture or retained-source retry instead. Additional red/green
regressions cover cancellation reporting, restart recovery, duplicate-claim
cleanup serialization and message-cursor refresh.
