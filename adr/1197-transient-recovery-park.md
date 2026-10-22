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

### Retained object access (issue #3059, 2026-10-18)

After settlement and predecessor quiescence, before any retained-source Git
read, a credential-free supervised helper runs as command UID 10003. It adds
only runner-group read/traverse access inside the attributed clone's validated
`.git/objects`: command-owned GID-10002 directories use `mode | 0050`
(`2700 → 2750`); regular files use `mode | 0040` (`0400 → 0440`).
Ownership, bytes, refs and the existing worker-side pack fetch stay unchanged.
Pre-existing worker/runner objects and regular-file no-ops are never chmodded;
multiply-linked command-owned regular mutation candidates are refused.

Repair is nontransactional: refusal can leave a prefix group-readable.
Unsuccessful recovery preserves the sources and custody. The helper does not
follow alternate targets, and readiness does not replace closure integrity
verification. The producer of the restrictive modes remains unknown.

Every retained iteration failure is diagnosed before terminal retry branches.
Git stderr stays private until whole-input redaction, followed by sanitization
and bounded prefix/suffix selection. Oversized unsplit input is omitted at
64 KiB; cause/context limits are 1024/256 characters, within 1536 total free-text
characters including truncation markers and the constant warning. Helper
failures use fixed codes; persisted blockers and failure reasons carry no raw
transport text.

### Retained readiness cleanup (issue #3059, 2026-10-18)

Unconfirmed cleanup of the supervised retained-source permission helper,
including startup cleanup, terminates that run's recovery with the typed
`readiness_cleanup_unconfirmed` blocker before another attempt or successor
preparation. Disposal throwing is also unconfirmed cleanup. The clone,
sources, refs, journal/pins and custody stay retained. Verified startup cleanup
and ordinary helper refusals keep their existing retry classification.

This is a run-local terminal failure, not a worker quarantine. Cancellation,
shutdown, credential switching and ownership changes retain their existing
precedence. **Known limit:** if one of those controls intervenes before the
failure persists its blocker, no terminal record may be written; a later
claim may encounter recovery again. Existing blocker-persistence failures
remain reported as unavailable. No pre-launch marker or command-UID process
scan is added, and the worker quarantine policy is unchanged.

### Terminal-maintenance writer exclusion (issue #3031, 2026-10-19)

The model-free terminal producer's private preparation runs only while the
worker proves no untrusted writer exists, in both uid modes. It closes the
existing claim gate under its own owner, requires an idle worker with no
in-flight claims, and runs a deny-only process scan (worker-local, never
signalling). That scan admits only the worker, its ancestors, the subtrees
of roots the producer slot itself recorded, zombies, and the exact bootstrap
set below; every other process, of any uid, readable or not, refuses. A
proc mount with a hidepid restriction, or more than one mount at `/proc`,
refuses. When Docker is wired, any running container refuses.

The bootstrap set is taken once, at the first statement of `main()`: zero or
one direct child of the worker, pinned by pid, start time, parent pid and
the `exe` realpath of tsx's own native esbuild. It is never re-taken; a later
descendant of an enrolled pid, PID reuse or an unreadable identity refuses.

**Trust limit.** On single-uid workers, an enrolled esbuild, like the worker,
its tsx ancestors and `/app`, is inside the documented same-uid trust limit
(ADR-1786, ADR-2213). Exclusive-idle provides temporal closure only; the uid
split is the boundary.

**Staged.** The producer ships disabled: `TERMINAL_ORIGIN_ADMISSION_ENABLED`
in `agent/src/recovery.ts` stops new-origin admission after saved-evidence
replay, so production never charges the producer ledger, closes the exclusion
gate or captures WIP. The bootstrap snapshot above is still taken at boot.
Guard removal belongs to a separate follow-up PR after the complete producer
pipeline and composed measurement pass. That PR also requires the real-image
PVC roll, live-producer process tree, writer exclusion and process-level
shutdown checks on its branch-built image. This delivery keeps the guard off;
merging ordinary recovery fixes does not enable the terminal producer.

Reachable now, independently of the guard: after a run ends failed or
blocked, the runner starts a pass that retries that run's saved custody
FINAL under the existing replay proofs, and live re-drive waits while it
holds the pass set. Each tracked pass owns an async-local signal combining
its caller's cancellation with recovery shutdown. The Git recovery scope
first adopts any inherited caller budget; pass ownership then isolates it
in a child with the same remaining clock. Scope ordering prevents late
adoption from attaching pass shutdown to the caller's controller. That
signal reaches its Git operations and lock waits, its cancellation budget
(including a budget activated later), and every recovery HTTP attempt. Shutdown closes pass
admission, aborts those owned operations and joins actual settlement; a Git
child is reaped before its lock is released. An HTTP request interrupted
after send retains the existing uncertain outcome and request identity;
shutdown invents no ACK, release or cleanup proof.

This scope does not cancel foreground or run-lane preservation. Caller-owned
cancellation, the graceful checkpoint/credential allowance and bounded
imports keep their existing deadlines and joins. Process-level production
image shutdown latency remains a separate maintainer acceptance check.

After an ordinary guarded archive upload is accepted and read back from its
authenticated journal, the runner also notifies a separate saved-evidence-only
pass once its execution tail and exclusion have been released. Notification
requires actual process, command, steering and auxiliary settlement; an
executor replacement or new execution invalidates prior settlement receipts.
Unconfirmed settlement, rejected uploads and shutdown suppress notification.
This pass uses the existing FINAL and ACK proofs, admits no producer, charges
no maintenance slot and adds no timer. Saved evidence remains available to the
ordinary boot/live replay paths when notification is suppressed.

### Terminal-producer delivery hold (issue #3031, 2026-10-20)

The W0-H capture redesign and its dependent producer pipeline are not
implemented in this delivery. The frozen design requires the clone's index
identity and trailing checksum, pinned immediately after `add -A`, to remain
unchanged before and after `commit` and through the post-capture proof.
Trusted same-UID fixtures on Git 2.54.0 instead observed a legitimate
cache-tree rebuild during commit, despite both `--no-optional-locks` and
`GIT_OPTIONAL_LOCKS=0`. Dirty tracked/untracked, staged-only, unstaged and
racy same-size cases all produced the expected single-parent commit and a
clean worktree, but replaced the pinned index. In the ordinary dirty case,
its size changed from 271 to 309 bytes and its `TREE` payload from 15 to 53
bytes; inode and checksum changed while index entries stayed identical.
The postcommit index remained stable through the later read-only checks.

This is a material conflict with the required capture success path, not a
capacity measurement or a protected over-cap success. Accepting a new
postcommit pin or changing the capture commands needs an explicit contract
revision. No such workaround, delta-transfer contract, producer freeze,
archive handoff or durable producer rediscovery is shipped here. The
composed resource gate remains unrun; capacity projections are unverified.
Existing capture/transfer helpers and their tests remain in place.

The intended W0-H proof hashes tracked and non-ignored untracked bytes,
checks undeclared worktree/index differences and the exact final net delta,
and retains in-clone, runner-UID, final-worktree `add -A` semantics. The
stat-cache-only alternative was rejected because forged clean metadata can
hide modified bytes. Conversion attributes remain a planned protected
refusal. These are undelivered requirements, not guarantees of this release.

The planned attempt-v1 lineage and table-v2 provenance writer are also
undelivered. The maintenance table and HMAC stay at v1; no provenance write
or initialization is added at attempt seeding. The accepted shortfall of
the planned provenance scheme covers legacy/pre-upgrade or orphaned
sources, canonical reseeds, adopted sources and seeds with unrecorded
provenance: those categories have no new restart-preservation guarantee
and retain their existing protected exits. The in-memory rediscovery aid,
continuation phase table and boot/live materialization-order change remain
unimplemented. No ledger history is translated, reset or exported.

The existing dormant maintenance limits remain unchanged:

| Dimension | Existing limit |
|---|---|
| Decoded bytes | Configured C, default 8 GiB, ceiling 64 GiB; 3 × C per cycle |
| Slots and time | At most 3 × 120 s; 360 s aggregate; at least 180 s spacing; fixed 24 h eligibility window |
| Closure/source entries | 65,536 each per slot |
| Administrative entries | 4,096 per slot |
| Source/artifact/metadata bytes | 256 MiB each per slot |
| Sources / launches / RPC attempts | 128 / 512 / 32 per slot |
| Discovery | 10 s, 32 MiB metadata, 4,096 administrative entries, 512 launches, 16 RPC attempts, 4 candidates |
| Durable table / marker / index capacity | 16 MiB / 1 MiB / 4,096 entries |

Existing authenticated ledger history is required for production allowance;
missing, partial, tampered or wrong-key history grants none. Whole-root
removal retains the existing first-initialization residual. This delivery
adds no ledger backup/restore artifact or historical-reader format change,
and does not expand ADR-1786 authority. Production-image/PVC and three-forge
compose/k8s acceptance remain maintainer-owned and unperformed.

### Terminal-producer hold validation (issue #3031, 2026-10-22)

The current approved W0 P1 still pins the post-`add -A` index's device,
inode, size, mtime, ctime and trailing checksum before and after commit and
through P5. A fresh isolated reproduction on Git 2.54.0 with
`--no-optional-locks` and `GIT_OPTIONAL_LOCKS=0`: ordinary add then commit
changed index size from 174 to 193 bytes, replaced its inode, and changed
the trailing checksum from `88f9ac4900f5211559744ad9f7bcfdc3493c11d1` to
`2d4d8e449815a6fcd5b4e9fa951d903164e746ed`. The `ls-files --stage`
entries were identical and status was clean. This independently reproduces
the existing index conflict above on the merged candidate.

There is also a content-policy conflict. W0 P3 replaces the old 64 MiB /
16,384-entry envelope with frozen slot caps "with no hidden lower cap";
W0 P4 requires `add -A` to commit the final worktree for all of T union U
(tracked and non-ignored untracked paths). The current
[requirements record](../specs/human.md) at line 755 prohibits WIP auto-save
of core dumps and files over 8 MiB. The
[WIP policy amendment](0759-protect-run-work-usage-limit-park.md#amendment-2026-10-21-issue-3239-the-wip-auto-save-refuses-core-dumps-and-oversized-files-and-vets-its-own-marker)
explicitly includes `captureMaintenanceWip`.
The [maintenance capture tests](../agent/test/git-maintenance-wip-capture.test.ts)
at lines 271–317 preserve refusal before commit for new/changed oversized
content and core files, success at exactly 8 MiB, and exemption for unchanged
core/oversized content already in HEAD. The literal all-content/no-hidden-lower-cap
contract conflicts with that policy.

Under R3, disagreement with a higher source or an impossible required
acceptance means finish independent work, introduce no workaround, and
leave dependent milestones incomplete. Re-pinning after commit, weakening
the identity proof, omitting protected content or hiding the 8 MiB refusal
inside slot accounting would each change the contract. A material revision
must explicitly reconcile P1/P5 with legitimate index replacement and
P3/P4 with the core/8 MiB policy, then specify the revised acceptance and
repeat the required reviews and gates. No such revision is approved here.

The seed's reachable changes are S (owned recovery-pass shutdown) and N
(saved-evidence replay notification); both remain pending independent
reviews and the lead's exact validation results. New-origin production
remains disabled. W0, U4b, U4c, U4d, U5, F, M3 and M4 remain incomplete;
MO is unperformed. No delta transfer, provenance P, boot/live ordering
change or U5 delivery is claimed. The existing helpers, test ports and
accounting stay unchanged. The named accepted provenance shortfall and
existing dormant limits above remain the boundary, not new guarantees.
See the [delivery report](../docs/issue-3031-delivery.md) for the requirement
map and outstanding evidence.

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
with a finite **five-minute deadline window**. The original first-reservation
episode deadline policy is replaced by the #2958 amendment below. Retries use the
existing exponential delay capped at 16 times the base. Capture, publication and
successor handoff preserve the reservation count. Reset of that count requires
a trusted, successfully settled real model turn.

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
deadline. Under the original #2512 policy (superseded by the
[2026-10-11 amendment](#amendment-2026-10-11--2743) below), decoded history above
1 GiB failed closed with custody retained,
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

**Historical cap policy:** the 1 GiB limit in this section is superseded by the
[2026-10-11 amendment](#amendment-2026-10-11--2743). The integrity, custody and
residual-risk boundaries remain in force.

The human review authorizes a 1 GiB delivered decoded-history limit for
local recovery verification, separate from the 64 MiB encoded archive cap.
Exceeding it fails closed and retains custody, with a visible recorded reason
in `uzi run recovery` and the worker log. Verify reachable object contents
and hashes under the existing deadline; unrelated cache objects do not
participate in that proof. Overflow, timeout, corruption and interruption
never authorize custody release or source cleanup.

The decoded budget (limit amended by #2743) counts delivered object contents
cumulatively within a recovery operation. The 2026-10-14 cache amendment below
supersedes the original repeated-verification-read accounting. The delivered-byte
cap does **not** bound Git-internal delta decompression memory. The deadline limits only duration, and the shared worker cgroup
does not isolate this verifier from sibling runs. The resulting residual
resource-exhaustion risk is deferred scope, as authorized by the human
review on 2026-10-09 (#2512).

## Amendment 2026-10-10 — #2613: pre-clone discovery error classification

Pre-clone retained discovery used to fail the claim on any error that was not
a foreign-retained rejection, so an operation deadline while waiting on the
bare lock failed a run whose local work was intact. It now retries only
confirmed operational errors: the operation deadline, identified by an elapsed
absolute attempt deadline plus positive deadline evidence (the lock-wait
abort message or a "recovery deadline exhausted" error in the cause chain), and the resource
errnos EAGAIN, EMFILE, ENFILE and ENOMEM. A bare abort without that evidence
is terminal. Typed errors and known corruption markers are rejected before
any deadline-text match, and every other error is terminal unless positively
identified as retryable; all of them except a foreign-retained rejection
keep custody. Git spawn exhaustion is
wrapped without a code, so it stays terminal.

The allowance is at most 2 retries per claim, shared across both discovery
callers (pre-clone and the pending-capture rediscovery), using the existing
exponential recovery delay. It is in-memory and separate from the persisted
episode budget, which it never reads, resets or replaces. It never parks and
never mints a hold or generation. Exhaustion fails the claim with "local
recovery storage busy or unavailable after bounded retries", keeping local
work and custody like every other terminal discovery failure.

The extra allowance is about 2 x codexBoundaryDeadlineMs plus 3 x
recoveryRetryMs of scheduled time (about 63 s at defaults). It is not a
wall-clock guarantee, since I/O settlement can add to it.

## Amendment 2026-10-11 — #2743

The delivered decoded-history cap is now worker-configurable through
`UZI_RECOVERY_DECODED_LIMIT_BYTES`, with a default of **8589934592 bytes
(8 GiB)** and a fixed ceiling of **68719476736 bytes (64 GiB)**. This replaces
the historical 1 GiB policy in the #2512 sections above. The finite ceiling is
eight times the default: it permits larger histories while retaining exact
safe-integer arithmetic and existing deadlines, without promising a memory
bound.

### Reported offline measurement

The following measurement was reported **OFFLINE on 2026-10-11** in the
[issue #2743 measurement report](https://github.com/vtmocanu/uzi/issues/2743),
not reproduced on a hosted worker as part of this change:

| History or verification sequence | Decoded object bytes |
|---|---:|
| Base `4d28d412` | 1,469,170,456 |
| Tip `ca44b5c1` | 1,477,164,071 |
| Union of both actual archives, unique objects | 1,477,164,741 |
| Base + failed archive, cumulative reads | 2,946,334,838 |
| Previous archive + base + failed archive, cumulative reads | 4,423,499,268 |

The old 1 GiB cap could not fit even one unique pass. The new 8 GiB default
fits this reported sequence by decoded bytes; that is not a hosted success or
deadline guarantee. Repeated reads remain chargeable and can still exhaust
the cap.

### Configuration and failure boundary

The worker parses the variable once in `loadConfig` at startup and injects it
through the main GitCache factory into a readonly instance setting. An unset
variable selects the default. A set value is trimmed and must contain decimal
digits representing a positive safe integer in `1..68719476736`; leading
zeroes are accepted. Blank, malformed, fractional, exponent, signed, zero,
negative, nonfinite, unsafe-integer and above-ceiling values reject worker
startup. The error names the variable and range without echoing the raw value.
Changing the setting requires restarting the worker.

This is worker/operator configuration, not server, API, repository or model
input. Inject it into a standalone container or an explicit operator container
environment override; setting it in `.env`, the API or controller alone does
not set the worker variable. The bundled hosted Helm chart and docker-compose
do **not** expose this environment variable yet and use the 8 GiB default.
This issue changes neither deployment manifest. See
[operator configuration](../docs/configuration.md#recovery-decoded-history-limit).

Outer recovery operations and closure checks without an operation context use
the configured cap; nested recovery operations share their parent's cumulative
delivered-byte counter. Every verification still walks the **full reachable
history**, including prerequisites, without history exclusion. The 2026-10-14
cache amendment below supersedes the original repeated-read accounting and
states the explicitly accepted change to corruption detection.
Cancellation and existing deadlines remain in force. Cap exhaustion fails
closed, preserving local work and custody; encoded archive limits and storage
quotas are unchanged. Local proof still grants neither remote durability nor
custody release.

Cap errors report exact bytes, adding GiB when the cap is at least 1 GiB.
A journal already blocked for decoded-history exhaustion stays blocked after
restart or a cap increase. Its reason reports previous decoded verification
exhaustion and the current cap, with "prior cap not recorded"; it does not
attribute that earlier failure to the new cap or automatically retry it.

### Memory and residual risk

Object contents stream through a pipe chunk and bounded header; content buffers
do not grow in proportion to the configured cap. Metadata listings still have
the 64 MiB output cap (`GIT_MAX_BUFFER`), and the object arrays remain bounded
by that existing listing. The delivered-byte cap is **not a RAM limit**:
Git-internal delta decompression is not bounded by it. Existing deadlines limit
duration, and the shared worker cgroup does not isolate sibling runs. The
residual worker resource-exhaustion risk accepted in #2512 on 2026-10-09 remains
deferred; this amendment does not claim to solve it.

## Amendment 2026-10-14: #2958, one deadline window per fenced claim

A retained-recovery episode may outlive a park or an interrupted long model turn.
Its cumulative reservation count stays bounded at three, but the five-minute
window now belongs to a positively verified claim generation, rather than the
whole episode. The acquired server claim's positive generation fences the first
window; its reservation is durably charged before ownership probes, preserving
#2512's outage and cancellation accounting. Renewing an existing episode instead
requires a current `running` ownership proof with the exact positive generation
before rebasing its window. A lost claim stops that renewal without changing or
blocking the predecessor episode. If that renewal proof instead fails transiently
(transport, timeout, retryable HTTP status), the flight logs a retryable diagnostic
and stops without a terminal report, charge, rebase or blocker. The existing
missing-flight sweeper governs requeue and its finite worker-loss allowance; it
may hold exhaustion for an owner decision. This is not an uncharged capture retry:
no recovery operation was authorized. First-window and same-window ownership
outages retain #2512's charged, three-reservation retry handling. The journal persists
`claimGeneration` beside `startedAt` and `deadline` (`startedAt + 300000`), under
the existing source binding and bare lock. A higher verified generation rebases
those timestamps once. Same-generation retry and process recreation do not renew
it; a missing or older fence cannot refresh a bound window.

Legacy episode records have no claim identity. They remain readable and can bind
once to a positively verified current claim, with a fresh window and the existing
count, backoff and source attribution. No predecessor attempt id is interpreted
as a claim fence. An unfenced legacy caller keeps the original deadline policy.
Clock rollback and invalid clocks still refuse; three charged reservations still
refuse a fourth even across fresh claims. Only trusted settled model processing
clears the episode and its cumulative count.

The entry reservation wrapper permits ownership verification and this authorized
rebase before applying the persisted window. Failure classification uses the
current claim's window, never a superseded deadline. Already-blocked records stay
blocked with their original reason, including decoded-history exhaustion; neither
a reclaim nor a deployment upgrade revives them. Source protection, custody,
publication proof, closure verification limits and retry backoff are unchanged.
No claim fence, timer renewal or successful local verification authorizes deletion
or release of retained work.

## Amendment 2026-10-14: #2979, per-operation verified-object reuse

**User decision, 2026-10-14: option A.** Retained recovery and retained credential
switches may reuse successfully hashed objects within the same recovery
operation instead of repeatedly spending the decoded budget on shared history.
The independent reviewer dissented, preferring refusal of reuse when
`objects/info/alternates` exists plus per-pack identity checks. That alternative
would still not catch in-place corruption or replacement of a loose object.
The user chose option A with the broader risk wording below. This supersedes
#2512's repeated-verification-read charging and #2743's explicit no-deduplication
deferral. Nested operations share both the counter and cache by
reference; standalone calls have a private cache, and new operations start empty.

Every call still performs the complete `rev-list --objects --missing=error` walk
and batch metadata checks. Cache hits must retain their verified type and size.
Only misses contribute to the declared-size precheck, content hashing and decoded
charges. Entries publish only after all requested frames and hashes verify, the
producer exits cleanly, and the bare identity is unchanged. A failed or partial
batch publishes no entries; its already-delivered bytes remain charged.

Identity is `barePath` plus `lstat dev:ino` for the bare and its `objects/`
directory, checked before the walk and after producer settlement (also on an
all-hit check). The total cache entry count across the whole operation is capped
at `floor(GIT_MAX_BUFFER / 41)`. At capacity, new objects remain uncached and are
still hashed and charged. No cache state persists to disk or crosses operations.

Validity relies on immutable Git object storage owned by the worker. Automatic
bare maintenance is disabled; the worker does not repack or prune its bare, and
clone materialization's repack is separate. Deletion is still refused by the full
missing-object walk. Cache reuse adds a distinct risk: loose objects or packs
modified in place or renamed over, or an added alternate object store, can evade
later content verification when the bare and object-directory inodes and cached
type/size metadata remain unchanged. Previously, each later closure check hashed
those bytes again. **This is not today's guarantee and is not a technical
equivalence to the existing verification-to-consumer race.** The user explicitly
accepts this additional risk within one operation. Directory replacement, missing
objects and cached type/size disagreement still refuse. A fresh operation hashes
objects again. Cache reuse never grants custody release or source deletion.

The finite 8 GiB default and configurable ceiling remain unchanged. Deduplication
removes repeated shared-history charging, not the single-pass growth risk: a
closure whose unique decoded content exceeds the cap still fails closed. History
was measured at roughly 1.5 GiB in October 2026; continued growth can eventually
require another policy decision. Deadline, cancellation, archive and storage
quota boundaries remain unchanged. The decoded cap still does not bound
Git-internal decompression memory.
