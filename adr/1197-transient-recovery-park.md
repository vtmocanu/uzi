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

A statusless 2xx response is not a positive park acknowledgment. If the
ownership probe still reports `running`, retain and retry the live execution;
do not delete its session or release it on the strength of `applied` alone.
Owner cancellation, authoritative terminal status and worker shutdown remain
explicit exit paths. Verified with real HTTP 204 responses on 2026-09-08.

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

## Amendment 2026-09-16 — PRD #1392: a pre-clone park is the exception to "verified capture first"

**Status**: Accepted (PRD #1392 M1-M4 committed on this branch).
**Issue**: [vtmocanu/uzi#1392](https://github.com/vtmocanu/uzi/issues/1392)
**PRD**: [prds/1392-forge-unreachable-preclone-park.md](../prds/1392-forge-unreachable-preclone-park.md)

"Verified capture before a promotable park" above assumes a clone already
exists to inspect, WIP-commit and fetch. That assumption does not hold for
a transient forge failure (DNS, connect, or a 5xx) hit *while cloning* —
a healthy resume with no retained source can need the same forge refresh,
so this exposure is not limited to first dispatch. Retained-source discovery
now precedes forge refresh and disk preflight; the #2512 amendment below
covers claims that already have local source.

On the healthy path with no retained source discovered, when `ensureClone`
exhausts its retry schedule with a transient verdict, there is no runner
clone, no worktree, no branch and no recovery journal
yet (`phaseClone` reports `running` and starts steering before any of those
exist). There is therefore **nothing to capture, verify, or retain a source
for**: the worker reports `recovery_wait` with cause `forge_unreachable`
directly, with no capture attempt and no retry-while-owned loop, because
there is no local work a retry could lose. This is the one park path in
this ADR's scope that enters `recovery_wait` without a verified capture,
and it does so precisely because "verified" and "unverified" both presume
a capture target that does not exist here.

Everything else stands: a permanent forge error still fails immediately
(no retry can fix bad credentials or a missing repo), an owner cancel
during the retries still ends `cancelled`, and the promotion cadence is
the same `recovery_wait` sweeper this ADR already describes. What differs
is only the *entry* condition and a forge-only lifetime cap this cause
adds on top (`RUN_FORGE_UNREACHABLE_MAX_PARKS`) — the empty-turn park
above keeps its uncapped lifetime, unchanged. See
[adr/1296-durable-run-recovery.md](1296-durable-run-recovery.md) (the
2026-09-16 PRD #1392 amendment) for the matching custody-side exception —
a generation that never adopted a source releases its hold without the
forge proof this ADR's sibling amendments otherwise require — and
[adr/1392-forge-unreachable-preclone-park.md](1392-forge-unreachable-preclone-park.md)
for the full decision record.

## Amendment 2026-10-08 — #2512: source-bound retained recovery

Retained-source discovery runs inside an acquired claim before forge refresh
or disk preflight. The #1392 no-local-source forge park remains valid on the
healthy path where discovery found no retained source. A retained source
instead enters a durable episode bound to its identity, reserving work
before capture: **3 total iterations**, blocked and nonblocked together,
with a **five-minute deadline from the first reservation**. Retries use the
existing exponential delay capped at 16 times the base. Crash, reclaim,
capture, publication and successor handoff preserve the reservation and
deadline. Reset requires a trusted, successfully settled real model turn.

A permanent blocker or exhaustion reports terminal `failed` with
`keepCustody`; it neither auto-reclaims nor exposes failed-run Resume.
The reason distinguishes missing source, invalid clock, preservation or
adoption failure, unavailable/unverifiable needed prerequisites, and a
fallback still over the size cap. `worker_residue_blocked` is reserved here
for actual quiescence failure. Transient capture, forge and disk failures
spend the same episode budget. The healthy provider/empty-turn park budgets
and owner worker-recovery-exhaustion hold are separate and unchanged.

The guarded producer's actual verified thin bundle, under the cap and with
needed prerequisites locally available and verified, permits local adoption
and model execution despite unknown remote publication. Nonempty
prerequisites alone are not a blocker. Content verification reads only the
reachable object closure and verifies object hashes within the recovery
deadline. A decoded history above 1 GiB fails closed with custody retained,
even when its thin archive is below 64 MiB; the recorded reason is visible
in `uzi run recovery` and the worker log. Local proof establishes neither
independent recovery nor remote durability and grants no custody release. Predecessor
sources, pins, journals and descriptors remain until the existing verified
final disposition or explicit discard. Guarded prerequisite-free
archive/verified-empty-inventory release conditions remain unchanged.

Recovery seeds a fresh successor path and model session even on an unwired
worker, avoiding execution in a predecessor while its recovery evidence
must remain. Ordinary unwired keys that never entered retained recovery
retain same-path continuity. External restarts require a genuinely fenced
successor; local storage loss remains outside the protection. See the
matching [ADR-1783 amendment](1783-run-quiescence-and-attempt-clone-paths.md#amendment-2026-10-08--2512-retained-recovery-on-unwired-workers).

Healthy unknown publication continues within the same generation, preserving
the tip, owed roots and publication time gate; confirmed durability requires
positive proof. No API/schema, hold-rebinding, custody-cap or #2486 behavior
changes are included; [ADR-1751](1751-continuation-custody-admission.md)'s soft cap
is unchanged.

A failed source-only run needs operator recovery from retained storage.
Export remains limited to a manifest-bound available archive, with no new
download API or failed-run Resume. Downgrade during pending recovery is
unsupported: older workers may drop the durable recovery fields.

## Amendment 2026-10-09 — #2512: bounded reachable integrity verification

The human review authorizes a 1 GiB delivered decoded-history limit for
local recovery verification, separate from the 64 MiB encoded archive cap.
Exceeding it fails closed and retains custody, with a visible recorded reason
in `uzi run recovery` and the worker log. Verify reachable object contents
and hashes under the existing deadline; unrelated cache objects do not
participate in that proof. Overflow, timeout, corruption and interruption
never authorize custody release or source cleanup.

The 1 GiB budget counts delivered object contents cumulatively within a
recovery operation, including repeated verification reads. The delivered-byte
cap does **not** bound Git-internal delta decompression memory. The deadline limits only duration, and the shared worker cgroup
does not isolate this verifier from sibling runs. The resulting residual
resource-exhaustion risk is deferred scope, as authorized by the human
review on 2026-10-09 (#2512).
