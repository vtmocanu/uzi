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
exhaustion is not itself a reason to enter recovery. The #3462 amendment below
adds a narrow Codex zero-model-processing-evidence idle exception to this
historical timeout precedence. The [#3509 amendment](#amendment-2026-10-27-3509-bounded-codex-idle-stall-parks)
qualifies that exception with typed zero-evidence/productive idle parks and an
owner hold; #3506 below routes qualifying retry-associated zero-evidence silence
through provider recovery instead. #3466's human revision driver remains separate.
(AI-synced 2026-10-28)

### Verified capture before a promotable park

The [#3495 amendment](#amendment-2026-10-26-3495-bounded-retained-recovery-parks)
below adds a narrow exception for durably confirmed retained-source budget exhaustion.

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
(including a budget activated later), and every recovery HTTP attempt. Each
owned response registers cleanup at transport fulfillment, before caller
postchecks. Body and reader cleanup share the first cancellation promise;
requested cleanup joins at the RPC boundary before caller locks unwind, and
pass teardown joins any remaining response. Reader ownership lasts through
source settlement. RPC membership uses a separate async-local set, preserving
the original mutable attempt context and clock. This adds no timer or authority.
Shutdown closes pass
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

### Historical terminal-producer delivery hold (issue #3031, 2026-10-20)

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

### Historical terminal-producer hold validation (issue #3031, 2026-10-22)

At this historical capture, the approved W0 P1 pinned the post-`add -A` index's device,
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

The reachable changes are S (owned recovery-pass shutdown, including the
response-handoff cleanup join) and N (saved-evidence replay notification).
Scoped acceptance, regression and mutation results are recorded in the
report below; frozen-gate and live acceptance remain separate. New-origin
production remains disabled. W0, U4b, U4c, U4d, U5, F, M3 and M4 remain
incomplete; MO is unperformed. No delta transfer, provenance P, boot/live
ordering change or U5 delivery is claimed. Existing capture/transfer helpers
and resource accounting stay unchanged. Ordinary LIVE/foreground test cases
are retained case-for-case with controlled automatic-notification timing. The named accepted provenance shortfall and
existing dormant limits above remain the boundary, not new guarantees.
See the [delivery report](../docs/issue-3031-delivery.md) for the requirement
map and outstanding evidence.

### W0-H hash-attested maintenance WIP capture (issue #3349, 2026-10-23)

This revision supersedes the W0 index-transition and content-policy
conflicts recorded in the two subsections above, which stay as history. It
changes `captureMaintenanceWip` only; its signature, result kinds and
`status`/`add`/`commit` callbacks are unchanged.

**Capture.** The capture runs in the clone as the runner UID. Afterwards
clone HEAD is the capture commit W (single parent `sourceSha`) and the
worktree is clean, so existing consumers use it unchanged.

**Hash attestation.**
- P1 proves the clone's own index (identity plus trailing checksum) and
  lists the tracked set T. Unmerged entries, gitlinks, assume-unchanged
  and skip-worktree flags refuse.
- P2 reads the status and `diff-index --cached` manifest of declared
  changes.
- P3 hashes every path in T and the non-ignored untracked set in two stages,
  a metadata census and then the payload, each reserved before it runs and
  never refunded. Every tracked path's worktree oid must equal its index oid
  unless the manifest declares it. An undeclared difference refuses; the stat
  cache is never trusted.
- The expected net delta E is every path whose worktree (mode, oid) differs
  from its HEAD tuple, plus deletions. A manifest and staged set that cancel
  to an empty E refuse protected, as before.

**Plumbing commit.** Porcelain `commit` is no longer used. The capture runs
`add -A`, `write-tree`, a pre-commit `diff-tree` proof that tree X equals E
exactly, then `commit-tree` and `update-ref`. W has deterministic identity
and dates (both the committer date of `sourceSha`), so identical content
reproduces W. `update-ref` is a compare-and-swap on the plain branch ref and
never writes `HEAD`; it refuses if the branch moved, writes no ref and
leaves the competing tip. Before the swap a refusal leaves HEAD at
`sourceSha`; after a successful swap a failed post-capture proof refuses
with the source retained, and no ref is rolled back.

**Index transitions (contract revision).** Exactly two index transitions are
sanctioned, each pinned in the scope of the command that makes it: `add -A`
and `write-tree` (Git rewrites the index there when its cache-tree is
stale). The post-`write-tree` pin is the P5 reference, so the earlier
requirement that P5 equal the post-`add` pin is replaced. Every interval
between pins is checked; any other index change refuses with the source
protected and no ref update.

**Semantics and refusals.**
- Final-worktree semantics as with `add -A`: staged intermediate content is
  not preserved. Tracked-but-ignored paths stay in T and are captured.
- A detached HEAD, or a branch ref that is itself symbolic, refuses.
- `eol`, `ident` and `working-tree-encoding` conversions make the hashed and
  staged oids disagree and refuse, at the latest at the tree proof.
- A source commit whose committer date is outside the accepted canonical
  form (leading-zero timestamp, `-0000`, or a zone outside ±14:59) refuses
  in P0, before any commit-group launch. Independently, W's exact text is
  proved before the branch CAS, so any header Git normalizes refuses with
  the branch unmoved.
- Two states the previous capture committed now refuse with the source
  retained: a tracked file replaced by a directory (a census type change),
  and an intent-to-add index entry (`git add -N`), which the strict index
  and status cross-checks reject.

**Content policy (resolves the earlier content-policy conflict).** The
per-file policy of the [requirements record](../specs/human.md) and the
[WIP policy amendment](0759-protect-run-work-usage-limit-park.md#amendment-2026-10-21-issue-3239-the-wip-auto-save-refuses-core-dumps-and-oversized-files-and-vets-its-own-marker)
applies to the maintenance capture. A path in E whose basename matches the
core pattern (`core`, `core.<digits>`), or whose new content exceeds 8 MiB,
refuses the whole capture with the source retained. Exactly 8 MiB succeeds.
A path whose current tuple equals its HEAD tuple (the staged old side if
staged, else the index entry) is exempt, and a deletion never refuses. The
capture never omits a file and claims complete coverage. This is the only
cap lower than the frozen slot caps and applies in addition to them.

**Accounting.** Slot limits are unchanged. Each Git launch charges one
launch plus its actual output rows as `sourceEntries` and its output bytes
as `metadataBytes`. The census and payload stages are reserved once and
consumed, not re-reserved. Each index pin read charges `metadataBytes`
equal to the index size. The caller's fresh observation pairs cost about 8
`rpcAttempts` per capture (4 mutating dispatches), against the cap of 32.
Representative measurement: see the delivery report.

**Boundary.** The composed gate (C2) is UNRUN: no qualifying measurement exists, and
the diagnostic run shows `sourceEntries` refusals at 65,536 on representative
fixtures, with no archive outcome reached (see the MEASURE section of the delivery
report). No contract change follows. The terminal producer guard stays off. U4b, U4c, U4d, U5, F, M3
and M4 remain undelivered, and MO is unperformed.

### Server-owned backoff

`SetRunRecoveryWait` admits only the owning worker's running, non-judge run.
A repeated report is an idempotent no-op; acceptance depends on the returned
status, not `applied` alone. `PromoteRecoveryWaitRuns` returns due rows to
`queued` while retaining their plan/session/checkpoint fields. Stale
approval reports cannot exit the park.

The server uses a capped exponential backoff, from `RUN_RECOVERY_PARK_BASE`
(default 1m) to `RUN_RECOVERY_MAX_PARK` (default 30m), with jitter. There is
no lifetime park cap or terminal branch caused solely by repeated empty
results. This uncapped policy does not apply to negotiated Codex idle stalls;
see the [#3509 amendment](#amendment-2026-10-27-3509-bounded-codex-idle-stall-parks)
for the cause-specific cap and fresh-wall cost. Normal running watchdogs and
owner cancellation still apply.
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
**PRD**: [prds/done/1392-forge-unreachable-preclone-park.md](../prds/done/1392-forge-unreachable-preclone-park.md)

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
above keeps its uncapped lifetime, except for negotiated Codex idle stalls under
[#3509](#amendment-2026-10-27-3509-bounded-codex-idle-stall-parks). See
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
a trusted, successfully settled real model turn. (The #3330 amendment below
supersedes that sentence: an explicit owner Resume also renews the count.)

The [#3495 amendment](#amendment-2026-10-26-3495-bounded-retained-recovery-parks)
supersedes terminal exhaustion below only for durably confirmed `budget_exhausted` retained source.

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
clears the episode and its cumulative count. (The last two statements are
superseded by the #3330 amendment below: a fresh claim alone still cannot renew,
but an explicit owner Resume can. Clock refusal is unchanged.)

The entry reservation wrapper permits ownership verification and this authorized
rebase before applying the persisted window. Failure classification uses the
current claim's window, never a superseded deadline. Already-blocked records stay
blocked with their original reason, including decoded-history exhaustion; neither
a reclaim nor a deployment upgrade revives them (superseded for
`budget_exhausted` blockers only by the #3330 amendment below; every other
blocker stays permanent). Source protection, custody,
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

Ordinary closure verification still performs the complete
`rev-list --objects --missing=error` walk and batch metadata checks. The #3441
amendment below separately defines the weaker maintenance delta-range proof. Cache hits must retain their verified type and size.
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

## Amendment 2026-10-24, #3179: an event-free Claude stream is a recovery outcome

The decision says that missing metrics and nonempty turns retain their existing
outcomes. This amendment carves out one narrow additional recovery outcome: a
Claude turn whose harness stream ends cleanly with no events at all and no
terminal. Such a stream has no turn count, so it was never positively empty, and
the implement loop folded it into a finished iteration, losing the plan, worker
safety steer, first-turn content and held owner batch the attempt carried.

An event-free attempt is now retried in process through the same bounded retry as
the other outcomes (same retry count, backoff and cancel, idle and wall
precedence), with the original prompt and callbacks:

- The plan, the drained worker safety steer and the held owner batch are retained
  for the retry. No new outer iteration begins. The owner inclusion receipt is
  issued only once an attempt shows model-processing evidence.
- The interrupted-calls note is re-sent only after an event-free attempt, and
  mirrors exactly what the immediately preceding dispatch appended (including
  nothing). An attempt that processed its prompt never has its note repeated.
- The retry preserves the requested resumable session identity because the
  event-free attempt reported no replacement identity.
- Exhaustion throws the recovery park error with an event-free message and
  `providerOrigin` false. Retry notices say the stream ended without any events;
  they never claim zero turns or provider processing.
- Plan phase: an event-free planning turn now retries and then parks, where it
  previously ended as `REASON_NO_PLAN`. A planning turn with a terminal and no plan
  is unchanged.

Known boundaries. A stream that yields only init or rate-limit events and then
ends is not covered. A park after exhausted retries does not carry the drained
steer, as with the positively-empty path today. The Codex lane is out of scope
for #3179; the #3462 amendment below separately covers its zero-evidence idle.
The evidence is a scripted executor reproduction; no real-provider occurrence has
been observed. The ADR-1416 steering bridge is unchanged.

## Amendment 2026-10-25, #3330: an owner Resume renews the retained-source budget

Historical decision, narrowed by the [#3495 amendment](#amendment-2026-10-26-3495-bounded-retained-recovery-parks)
below, including its claim episode projection and stricter rollout boundary.

Maintainer decision: **only** an explicit owner Resume of a run in
`recovery_wait` with cause `worker_requeue_exhausted` renews the retained-source
recovery budget. This supersedes three earlier statements: the #2512 amendment's
"Reset of that count requires a trusted, successfully settled real model turn",
the #2958 amendment's "three charged reservations still refuse a fourth even
across fresh claims", and its "neither a reclaim nor a deployment upgrade revives
them" (for `budget_exhausted` blockers only). A successful settled model turn
still clears the completed episode, unchanged.

Mechanism:

- `runs.worker_recovery_episode` is incremented by exactly 1 only by
  `ResumeWorkerRecoveryEpisode`, reached only from the owner Resume for
  `worker_requeue_exhausted`. Every worker claim payload carries it as
  `worker_recovery_episode`. The worker treats a non-safe-integer or negative
  value as absent.
- The retained source's durable `RecoveryProgress` journal gains an optional
  `episode`, the episode of the last renewal. Absent reads as 0, a malformed
  value is refused, and it is written only when above 0, so a never-resumed
  journal is byte-identical to before.
- `reserveRecoveryIteration` checks, in order: the stale fence, refusal of any
  blocker other than `budget_exhausted`, the clock, then renewal. It renews iff
  the claim's episode is above the stored episode **and** the claim is a new
  generation (a new window). That applies to a partly used, exhausted or
  `budget_exhausted`-blocked source. Renewal charges 1 of the 3 iterations,
  opens a new five-minute window, restarts backoff from its base, clears the
  blocker and stores the episode. The stored episode never decreases, one Resume
  renews at most once, and the cap stays 3 iterations per episode, so a fourth
  reservation is refused again.
- Renewal still takes the new-window ownership proof, and each renewed iteration
  re-proves quiescence before capture.

Never renews: a new claim generation alone, server requeue or reclaim, the
`data_volume_full` park retry, successor handoff, the memory-pressure Resume
(`ResumeMemoryEpisode`), the model-free preservation producer of #3031 (it still
never renews), any other blocker (`clock_invalid`, `decoded_history_limit`,
`readiness_cleanup_unconfirmed`, `capture_failed`, `source_missing`, a corrupt
journal), a same, older or missing episode, a same or missing claim generation
(journal left unchanged), and clock rollback. A claim from an older api carries
no episode and never renews.

Clock-rollback boundary: a rollback on a record already blocked
`budget_exhausted` leaves it byte-identical (not rewritten to `clock_invalid`),
and a renewal on the same claim generation writes nothing. A rollback with a
newer episode and a new generation on an unblocked record still persists
`clock_invalid` as before, and that permanent blocker is never renewed by a later
Resume.

Known overlap: `fail()` records a failure after the deadline as
`budget_exhausted`, so such a record becomes renewable. That is safe because each
renewed iteration proves quiescence again.

Rollout needs no capability flag: either deployment order works. An older api
sends no episode, so nothing renews; an older worker ignores the claim field.
Accepted race: a run resumed before its worker upgraded has a legacy journal
(reads 0), so it gets one renewal on its next claim, still at most one per
Resume. An episode that goes backwards (database restore, a down/up of migration
00309) fails safe: nothing renews until it passes the stored value. Worker
downgrade while recovery is pending stays unsupported: an older worker cannot
parse the new journal field, treats the journal as corrupt and fails closed.

Scope: the `data_volume_full` task-run symptom is not fixed here (#3329).

## Amendment 2026-10-25, #3462: Codex zero-evidence idle enters ordinary recovery

Historical decision: the [#3509 amendment](#amendment-2026-10-27-3509-bounded-codex-idle-stall-parks)
below supersedes the untyped, productive-idle and uncapped-silence claims for
negotiated lead idle recovery. The #3506 note below qualifies provider attribution
for retry-associated zero-evidence silence. The human revision and evidence
boundaries remain. (AI-synced 2026-10-28)

An eligible Codex lead turn that reaches the idle timeout with zero
model-processing evidence now enters ordinary recovery. This is the narrow
exception to the historical genuine wall/idle precedence above; it does not
extend #3179's Claude event-free-stream rule to Codex.

The measurement is `processedEvents === 0` on a real `CodexTurnTripError`
for `REASON_IDLE`, using the existing `evidencesModelProcessing` predicate
in [the harness contract](../agent/src/harness.ts). It is not a raw event
count: lifecycle events can arrive without proving model processing.
Assistant-authored main-origin frames admitted by that predicate and
model-initiated activity count as evidence. Unknown evidence, a synthetic
trip or an ordinary error whose text happens to match the idle reason
does not qualify.

For plain zero-evidence silence,
[CodexExecutor.driveTurnWithWallPark](../agent/src/codex/codex-executor.ts)
persists the session through the existing pre-reaping sink and throws
`TransientRecoveryError` with
the exact message `provider sent no model output within the idle timeout`.
Without the #3506 retry context below, it leaves `providerOrigin` false.
The runner captures and verifies the restore point before recovery; #3509
supersedes the original untyped report when idle-stall typing is negotiated.
That plain-silence route has no `provider_outage` or provider-streak accounting:
silence alone proves neither a provider error nor that the model processed the
prompt. (AI-synced 2026-10-28)

Eligibility covers initial planning, automatic plan revisions and implementation
model turns.
Human plan-revision turns use the separate, negotiated in-flight recovery
path described in the #3466 amendment below, including clarification and
prose-only followups. Without that recovery, they retain the idle failure
contract. Productive idle, callback-based idle suppression, `idleOperation`,
child turns and the Claude lane are unchanged.
The [SDK recovery-error comment](../agent/src/sdk-executor.ts) records the
same Codex exception.

Cancellation, pending pause and spent wall take their existing routes before
recovery. Completion hold is considered before the zero-evidence idle park;
an accepted hold wins. After a declined idle completion hold and after
pre-recovery persistence/capture, the executor rechecks interruption state
and charges the elapsed wait against the remaining wall budget. A late cancel,
pause or spent wall therefore takes its existing route instead of becoming
ordinary recovery. A latched vault lock prevents this conversion; a lock
latched after the idle trip preserves the earlier idle outcome.

Outside negotiated human revision recovery (#3466 below), there is no
in-process retry or transient-retry debit for this idle outcome, including
silence after a #2099 retry. The existing #2099 classified-provider
retry count and same-thread retries remain unchanged; that in-process
backoff stays clamped to the remaining wall budget. A fresh
harness is required after the recovery park: `turn/interrupt` is a
best-effort cancellation request, not proof that the provider turn terminated,
so ending the local stream does not authorize another turn on that harness.
The existing verified-capture and settlement requirements still apply.

Ordinary recovery retains its uncapped lifetime park count and existing
exponential server backoff clamped at the configured maximum, plus jitter.
Repeated silence can recover until the run wall requires an owner park or
the owner cancels. These are historical lifetime claims superseded by #3509;
#3506 below routes qualifying retry-associated silence through the existing
provider-outage bound instead. A resolvable persisted session can resume on the
new harness. If the provider rejects `thread/resume`, the existing fresh-thread
fallback emits `resume_lineage_break` and warns that earlier context is lost and work may
be repeated. (AI-synced 2026-10-28)

**Evidence boundary.** Evidence is limited to scripted app-server/executor
and runner fixtures in [the Codex executor tests](../agent/test/codex-executor.test.ts).
The resume fixture covers a resolvable non-Docker session path, with
retained-source successor selection bypassed; it does not establish general
successor-path or Docker resume coverage. There is no live-provider proof.

### Issue #3506: provider retry followed by zero-evidence silence

Decision record (AI-synced 2026-10-28). This narrow exception qualifies this
#3462 amendment, [#3466 human revision recovery](#issue-3466-amendment--silent-human-revisions)
and [#3509 idle-stall recovery](#amendment-2026-10-27-3509-bounded-codex-idle-stall-parks).
It supersedes #3509's blanket not-provider-outage claim for qualifying
retry-associated zero-evidence idle; other stalls keep their existing policy.

[The Codex harness](../agent/src/codex/codex-harness.ts) retains the latest
`willRetry: true` `codex_error` bound to the active root thread and turn,
normalized by `normalizeCodexErrorInfo`. This closed classification contains no
raw provider text. It resets at each root turn and on emitted
`evidencesModelProcessing` evidence. An active-root `willRetry: false` invalidates
the retry fallback even when its classification is missing or unknown, and
blocks later retry notes from restoring it in that turn. Terminal selection is
unchanged; a retry note is not a terminal failure or completed progress.

At the first actual `REASON_IDLE` trip, the executor snapshots the retained retry
classification. An eligible measured `processedEvents === 0` trip routes directly
to `TransientRecoveryError` with `providerOrigin: true`,
`idleStallKind: "zero_evidence"` and the identity of `originalIdle` retained when
`isCodexTransientClassification` accepts that snapshot: exact `rateLimitExceeded`,
or transport with absent or retryable HTTP status (408/429/5xx). The message is
`silence after a provider-reported rate limit or transport retry`. The reported
retry condition does not prove it caused the silence. Lifecycle/retry liveness
never fabricates model progress. This does not invoke #2099 retries on the
interrupted root or debit its transient retry allowance. Plain zero-evidence and
productive idle keep #3509's policy; child turns and Claude are unchanged.
Cancel, pause, wall, vault, completion and first-trip precedence remain as above.

An accepted human revision, including clarification and prose followups, keeps
both markers and follows #3466: verified disposal before the retry hook, then
source/claim/control-fenced fresh-epoch construction after allowance admission.
This idle error does not take the same-epoch provider-redrive path. Missing or
unsupported hooks retain the `originalIdle` fallback. Exhaustion, owner wait,
source safety and control routing remain unchanged. Generic provider revision
errors keep their existing behavior. Retry and exhaustion notices use the same
retry-associated silence phrase without claiming a proven cause.

For ordinary recovery, provider origin excludes `idleTyped` in
[the runner](../agent/src/runner.ts). Existing provider capability negotiation and
positive, stampable claim-generation wire checks decide whether the report uses
`provider_outage`; older APIs retain the untyped fallback, which promises neither
provider counting nor a provider hold. Verified capture and settlement remain
required. Typed provider parks use the existing streak and
`RUN_PROVIDER_PARK_MAX_DURATION` (default `24h`; `0` disables the hold while
retaining typing/counting). At a qualifying park at or past that duration, the
run can enter owner-resumable `paused` / `provider_unavailable`. Backoff, jitter,
queue/claim delay and the next attempt can delay detection; there is no exact
24-hour deadline or bounded overshoot. Successful completed model turns reset
the streak under the existing progress rules; retry liveness does not.
Owner Resume uses the existing provider-unavailable route, not #3527's worker
owner episode or adoption renewal. Existing lost-ACK and definitive HTTP 400
typed-park-refusal behavior is unchanged and outside this amendment.

This note records the M1 behavior at `a212cd5d`; it adds no live-provider,
rollout or newly executed test evidence.

## Amendment 2026-10-25: #3441 U4b verified delta transfer

C1/U4b implements the objects-only delta transfer with focused acceptance and
coordinator controls recorded in the delivery report. The terminal producer remains
disabled. Independent candidate reviews, mutation proofs and full integration gates
remain required and are reported in the PR; these focused results do not waive them.
C2–C8 and the complete #3031 producer pipeline are separate deliveries.
The guarded terminal producer transfers selected objects absent from an eligible
canonical boundary. TERMINAL_ORIGIN_ADMISSION_ENABLED remains false. The importer writes
objects, never refs. A successful imported or already_present result proves W is
connected relative to the trusted-boundary invariant; neither result grants FINAL,
release, cleanup, tracking/owed or accepted-ledger authority. The coordinator must join
the actual owner and prove final family continuity before a fresh observation and output
indexing. Real coordinator-to-runner-to-importer tests establish a fresh
complete-list/exact pair immediately before canonical import and after the actual owner
join before accepted output indexing, with no intervening metadata Git. A custody
refusal at either dispatch prevents its effect; a post-import range failure retains
imported and unknown bytes and prevents accepted output indexing. A crash after canonical import but
before maintenance-wip-v1 indexing leaves the tip imported in bare, not indexed; C6 owns
reconciliation.

The canonical bare, objects, pack and ancestors are no-follow dev/ino-pinned. Canonical
alternates/http-alternates are refused. Producer admission and post-proof recheck
require absent bare/shallow by lstat (any present entry/non-ENOENT error refuses), and
canonical rev-parse --is-shallow-repository=false. The query uses plain replacement
gitEnv, no GIT_SHALLOW_FILE or private object-directory overrides. Both checks are
charged even on already_present. Existing shallow-source writer behavior remains
unchanged and maintainer-owned.

All canonical ref namespaces depend on complete connected inputs, not only the three
eligible anchors. Fetch negotiation/connectivity excludes existing ref history in any
namespace. The eligible minimization anchors are exactly
`refs/remotes/origin/<defaultBranch>`, `refs/heads/<defaultBranch>` and
`refs/uzi-runner/<branch>`. recovered-source as default refuses. RECOVERY_BUNDLE_REF and
every other namespace are excluded. Candidates from original.baseCommit,
record.sourceSha and eligible tips become boundary only on positive bare-only ancestry.
Empty boundary refuses. A nonempty selection must uniquely include W. Empty selection
requires positive W-to-eligible-anchor ancestry and returns already_present after the
same shallow/identity/settlement checks, without pack/index/import/ref writes.
Non-anchor-reachable empty selection refuses. This resolves the
includes-tip/boundary-candidate conflict.

The clone object store is read in place under private controls, restricted environment
and sole pinned canonical alternate. No clone config, fetch-back, graph/MIDX
acceleration or hardlink trust is used. Every nlink>1 file refuses protected with
custody/source retained. Legitimate hardlinked clones therefore refuse; Git interrupted
between loose-object link() and unlink() can leave a permanent nlink2 refusal. The
bounded walk counts encoded entries without copying or rehashing excluded objects.

Selected object sizes are prechecked against inherited C; pack-objects cannot reuse
encoded objects/deltas. Strict staged indexing admits canonical prerequisites only as an
alternate, and pack membership must equal the selected OID set exactly. Every newly
staged selected body is decoded and hashed, even after canonical cache hits.
Same-operation positive reuse binds unchanged staged pack/index provenance plus
canonical identity; partial/failed/cancelled batches publish nothing. Ordinary
full-closure verification remains a full walk. The selected range is checked in
staged-only proof plus pinned canonical alternate, and again in canonical objects after
strict import. Post-import failure retains all imported and unknown objects,
source/custody, and returns no accepted result; no rollback/deletion or output/FINAL
authority follows.

The rev6 acceptance 4 full-prerequisite claim is conditionally replaced by the weaker
4/4a/4b contract: missing selected-range objects refuse before import; post-import range
failure may leave inert objects; missing/corrupt excluded closure is not proved by the
range walk. A defect may predate and persist across operations with directory identities
unchanged. No unbounded full walk is added to the maintenance slot.

For self-contained archives only, missing/unreadable excluded objects block bundle
creation before pack emission. The archive-backed FINAL proof requires
`selfContained === true` and an empty `prerequisiteShas` array
(`agent/src/recovery.ts`, the archive FINAL predicate); it does not verify pack bodies. Only RecoveryBundleTooLargeError permits guarded thin
fallback; thin never authorizes archive FINAL. Existing independently proved
held-publication/settled FINAL paths remain unchanged. This backstop does not detect
well-formed object bytes inconsistent with their names: pack-objects does not rehash and
FINAL reads bundle header. Same-uid trust remains. No agent core.fsync setting exists;
Git default committed,-loose-object permits host-crash loose-object loss and ref
rollback, with the same backstop limits. If #2506 option A authorizes release on
thin archives, re-review this invariant as authority-bearing.

Owner R8 clarification: read-only Git queries/config acquisition and supervised private
pack/index/decoded proof commands do not independently need remote pairs. They retain
original operation/slot/deadline/cancellation/accounting/pin checks; fixed private
per-file preparation remains deny-only, with no Git/network. Fresh complete-list/exact
pairs remain immediately before canonical strict import, accepted output indexing after
joined owner, producer-origin indexing/publication, journal temp creation/rename and
every other existing listed shared/durable/server-mutating effect. No Git metadata
acquisition occurs after the pair before its effect. All W0 pairs stay unchanged. Final
measured complete-path resource totals and both import/output custody-refusal traces are
required; no cap increase or omission is authorized.

Disposable private scratch is never reused after crash. O_NOFOLLOW/O_EXCL, nlink1,
dev/ino matching, pinned-parent rechecks before rename, full read-completion checks and
finite nonrecursive owned-only disposal remain mandatory. File/directory fsync is not
required by U2. Unknown entries and uncertain close/disposal identities are retained.
Scratch paths are borrowed, never journaled.

**Enabling blocker: decompression amplification.** The decoded counter (8 GiB
aggregate, no per-object cap) meters bodies delivered to the verifier; it does not
bound the memory a Git child uses while inflating an object before that first
delivery. Demonstrated: a roughly 1 MB loose tree object declaring a 1 GiB inflated
size drove the source-environment `rev-list --objects` selection to about 1.08 GB
RSS, and `pack-objects` and both `index-pack --strict` invocations (private staging
and canonical import) also inflate such objects (inferred, not measured; Git may
stream blobs above `core.bigFileThreshold`). The transfer is
unreachable in production today: `terminalOriginAdmissionEnabled()` returns the false
`TERMINAL_ORIGIN_ADMISSION_ENABLED` constant (tests override it). Enabling it is
blocked until the enabling design proves a memory bound before the first inflation
(this amendment prescribes no specific mechanism as sufficient). Until then the
transfer's resource claims cover metered deliveries, not Git-process memory.

**Enabling blocker: split-UID runner tree.** `importMaintenanceTip` pins every
source-path component from the filesystem root through `safeDir`, which rejects the
production split-UID runner root (`worker:runner` 3775, sticky) and the repository
directory under it (worker-owned, group `runner`, 2775), so with the guard enabled
every split-UID transfer would refuse `maintenance_source_layout_unsupported` at the
source pin, before any Git launch. The split-UID test models do not cover those two
directories. Supporting the production layout requires a separately reviewed
layout/admission design that preserves component-replacement protection, plus tests
using the exact entrypoint ownership and modes. No permission broadening is presumed
safe or authorized here.

Current enforcing symbols are `GitCache.importMaintenanceTip` (admission,
`literalBranch`, no-follow pins/walk, canonical shallow checks, eligible anchors,
selection, staging, decoded proof, import/no-op and final rechecks),
`stageMaintenancePack` and `indexMaintenancePack` (private transport), and
`verifyRecoveryClosureBound` (the unchanged ordinary full-closure walk), all in
`agent/src/git.ts`. `RunRunner.withRetainedSourceBoundary` owns the transfer
operation/children in `agent/src/runner.ts`; `RecoveryCoordinator.admitTerminalOrigin`
in `agent/src/recovery.ts` joins that owner before its fresh output pair/index.

The original slot/signal/deadline is retained. Read/private-proof operations add no
remote pair. Immediately before canonical import, a fresh pair follows all Git
metadata acquisition; only local deny/pin checks follow the pair before dispatch.
The real-owner tests cover import custody refusal, output custody refusal,
post-import range failure, clean `already_present`, and fixed shallow diagnostics.
Successful no-op returns after both canonical shallow checks and identity rechecks,
then follows the same owner-join/fresh-output path; it imports no pack and writes no ref.

The producer-only stage parent-mode equality reuses its already charged stat and
leaves the ordinary helper default unchanged. The final pre-rename parent recheck
performs additional charged admin stats. The ordinary public family reader strips
any extra private config-read option, so only the importer supplies that binding.
Literal branch/ref handling preserves valid punctuation, Unicode whitespace and
long names rather than imposing a new ASCII/1024-character cap; input validation
and closed-anchor string capacity are reserved as metadata before allocation.
Late observed regular-file hardlinks retain the fixed alias refusal at both fd and
named rechecks without adding filesystem operations. See the delivery record's
per-site accounting, acceptance, port and measured resource tables.

### Complete-boundary writer induction and per-pinner acquisition chains

Owner decision 9ab05c2f (owner baseline 735d3734) resolves the previously reported R3
prerequisite by audit, not by a writer change. Owners report a composed test through
real worker code at origin/main 735d3734, substituting direct RecoveryCoordinator.pin
for the private runner journaling wrapper: failed fetchAgentBranch leaves an
unreferenced tip commit with missing ancestors; the journal records it; boot coverage
refuses with "recovery coverage proof failed" at its ancestry check, no coverage ref is
pinned and custody is retained. Owners traced the remaining writer chains to successful
fetch connectivity, connected synthesis or existing explicit full walks. No reachable
producer of a root missing its tree/blob was found. This is owner-reported evidence on
735d3734, not a test run by this worker or on its baseline 76cd0a66. Proceed under the
existing non-shallow precondition, record the three named residuals and leave live
writers unchanged; any OTHER unresolved writer gap still triggers R3.

The trusted-boundary invariant is conditional on initially healthy non-shallow
worker-owned storage, normal Git fetch connectivity and the stated same-uid/crash
residuals. It covers **all canonical ref namespaces**, including staging/transient refs;
eligible anchors are selection minimization only. Fetch negotiation/connectivity can
exclude history under every existing ref. Mere presence, journal authenticity, commit
type or ancestry alone is never the acquisition proof. The following acquisition table
is part of this ADR and the delivery/PR record.

Exhaustive tracked production-writer/caller sweeps covered
clone/fetch/update-ref/commit-tree/write-tree/index-pack, other import verbs, canonical
paths and constructed ref namespaces. The SHA/acquisition/quote column preserves
baseline line citations; the added current-pointer column maps unchanged source lines
into this working tree. Paths below are `agent/src/` unless stated; source baseline
76cd0a66 differs from owner test 735d3734.

| Writer / presence-only pinner | Baseline SHA source, acquisition chain and implementing quote | Current working-tree source sites |
|---|---|---|
| Initial cloneBare, heads/tags | `git.ts:15054` uses `["clone", "--bare", "-c",` (not mirror/no depth/filter). Successful ordinary full clone establishes initial population. Owner-required producer non-shallow checks exclude shallow-derived stores without changing this writer. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Warm fetch/origin/checkpoint refs | `git.ts:15085` `["fetch", "--prune", "origin"]`; `:15098` `["fetch", "origin", "+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*"]`; exact task checkpoint `:2770` awaits fetch then verifies advertised SHA. Connectivity is relative to all existing refs, not unreferenced objects. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Publication/workflow/secret-scan/default temporary refs | Fetch writers `git.ts:2991/:11576–11577/:13032/:13258–13259` use `"fetch", "--refmap="` or `"fetch", "--", "origin"`, with explicitly constructed publication-floor/workflow-target/secret-scan-floor/origin-default destinations. Successful transport supplies closure before later semantic validation; abandoned refs remain induction roots. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| fetchAgentBranch → staged incoming → promotion | `git.ts:4853–4854` supplies `"fetch", "--refmap=", "--no-tags",` and `"--no-write-fetch-head"` with file transport and branch-to-stage refspec; stage is `refs/uzi-incoming/` (:4849). Await transport before resolveCommitStrict (:4858); then `promoteTrackingUnderLock` writes runner tracking (:17282, `"update-ref", runnerTrackingRef(branch)`). Thrown transport bypasses incoming pinning; connected staged refs differ from unreferenced residue. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| updateTrackingRef / bridge synthesis | Sole live adoption caller `runner.ts:14987`; before it, :14981–14982 execute `await this.git.scratchPublicationPreflight(barePath, branch, H);` and the same call for bridge. `git.ts:15461` constructs `["-c", "commit.gpgsign=false", "commit-tree", tipTree, "-p", tip]`; floors supply further parents (:15462), from acquired canonical published/checkpoint history. Existing full walk is `"rev-list", "--objects", "--missing=error", "--quiet", candidate` (:2919). Not merely requireOwedCommit. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Workflow wrappers / checkpoint-chain synthesis | `git.ts:10547` `["read-tree", realTip]`, :10565 `["read-tree", "--prefix=.github/workflows",`, :10569 `["write-tree"]`, :10577 `["-c", "commit.gpgsign=false", "commit-tree", newRoot]`, :10578 adds existing connected parents. realTip is imported canonical candidate; workflowSource is freshly fetched default (:10449), previous checkpoint is prior connected published/synthesized state. checkpointPack :9602–9603 explicitly preflights source and transformed candidate before publication, including any claim-provided prior checkpoint input. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| anchorRecoveryHead | Sole caller `runner.ts:17137`. transferRestorePointToTrustedBare awaits fetchTracking (:17102; calls fetchAgentBranch at :14724 and rejects not_updated at :14727), verifies tracking coverage, then :17127 rejects `if (head === null \|\| head !== fetched.candidateSha) {` before anchoring its private clone HEAD. Writer `git.ts:5463` uses `["update-ref", pinRef, sha]`. No arbitrary present-object discovery. | `runner.ts:17141`, `runner.ts:17106`, `runner.ts:14728`, `runner.ts:14731`, `runner.ts:17131`, `git.ts:5463` |
| createQuarantineArchiveRef / uzi-archive run-generation pin | Sole caller `quarantine-archive.ts:124–127`: `const head = await git.trackingTip(barePath, branch);` then archive that exact head. trackingTip resolves runnerTrackingRef's commit (`git.ts:5017`), from successful fetch/promotion or preflighted bridge. Writer :5498 uses `["update-ref", ref, sha, ""]`. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinCodeSnapshot | Capture `runner.ts:11833–11839` checks branch HEAD equality and `await this.requireTrackingOwned(flight, barePath, runnerClone.branch, head);`; defaultCommit comes from originBranchTip. Capture pin=false follows successful fetch (:11979–11982 or :13426–13432); writing call :13442 uses that captured head. Writer `git.ts:5193` `["update-ref", ref, head, ""]`. A previously acquired head may legitimately no longer be current tracking tip. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinOwedUnderLock | All five call sites in `git.ts`: :4870 successfully staged incoming; :17239 displaced observed tracking SHA; :17244 incoming promoted SHA; :4973 conflicting ancestor's observed SHA; :17603 retained current observed SHA. Incoming follows fetch or preflighted bridge; others are exact ref/receipt-derived SHAs. Writer :17049 `["update-ref", ref, sha]`. :16885 `"cat-file", "-t", sha` is type evidence only. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| retainCurrentOwedCandidate | Live callers `runner.ts:5168/:14441` supply trusted owed context. `git.ts:17596–17603` observes tracking under lock, requires matching owner run, and pins that observed SHA. Receipt :17103 checks `receipt.trackingSha === sha` and committed phase; :17107–17108 bind branch/run/generation/context. Attribution binds the ref; it does not certify arbitrary dangling objects. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| clearConflictingAncestorTrackingRefs / uzi-archive branch-SHA pin | Only promotion calls it (`git.ts:17264`). :4968 observes each conflicting tracking ancestor, :4969 refuses unknown observation, :4975 records `sha: observed.sha`; :4973 optionally owed-pins same SHA. :4984 `["update-ref", archive, sha]` precedes :4986 deletion of the old tracking ref. Existing ref closure is copied, not manufactured by commit-type check. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — source | `runner.ts:16814` requires successor seededFrom tracking/checkpoint; :16824 excludes guarded predecessor; :16831 `if (!(await this.git.isAncestorRef(barePath, pred.sourceSha, clone.baseCommit))) continue;`; :16832–16835 pins source/adopted. Source closure follows membership in independently complete adopted history, not journal authentication/ancestry alone. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — adopted | Same call supplies clone.baseCommit. `git.ts:3991–3997/:4022–4054` chooses tracking/owner checkpoint refs; effectiveBase returned at :4502 may peel wrapper/marker to acquired ancestors (:4176/:4215–4233). Recovery override :7328–7329 binds episode pin (:7310–7311), explicitly closure-proved before creation (:7094–7097). | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — pushed | Sole writing caller `runner.ts:16979–16992` uses successfulPushedSha or valid completion body.head. Field set only after awaited pushBranch (:8519–8527) over tracking-owned publication.candidate (:8485). CompletionHead (:9944) derives from same success field (:9400–9402). Scope-capped zero-slice body (:7853–7857) is `report_only`, skipped by journalTerminalReport (:7113), and never reaches this pin. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — published | `runner.ts:2988` maps pinPublished to `{ published: sha }`; `recovery-settlement.ts:820–822` checks source/adopted ancestry then pins. Sole producer `runner.ts:16895` supplies branch successfulPushedSha (:9321/:16913–16915) or checkpoint packed.tipOid only after publication ack AND successful pack-child exit (:16436–16457). checkpointPack full preflights (git.ts:9602–9603) establish transformed tree/parent closure before publication. Shared pin writer `git.ts:6146` uses `"update-ref"`; local commit presence is supplemental. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Recovery episode pin | `git.ts:7094`: `if (!await this.verifyRecoveryClosure(barePath, tip)) throw new Error("captured object closure is invalid");`, before episode update-ref :7097. Explicit full-closure verification supplies proof for later recovery-successor references. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| buildRecoveryCoverage / coverage ref | `recovery.ts:1254–1261` adds originalSourceSha to roots; `git.ts:17781–17782` builds `"commit-tree", tree` over those parents; :17797 requires root ancestry or throws `"recovery coverage proof failed"`, before `"update-ref", coverageRef, sha` (:17804). **Owner audit/test resolves earlier concern:** reachable source chains are connected, or tested missing-ancestor failed-fetch root refuses before pin. This ancestry protection is incidental and NOT a tree/blob closure proof; no reachable producer of a root missing tree/blob was found. Keep the residual, not a claim that the builder independently proves closure. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| RECOVERY_BUNDLE_REF / transient recovery bundle pin | `git.ts:13369` `["update-ref", RECOVERY_BUNDLE_REF, h]` precedes bundle production. Quarantine caller (:150) uses tracking-derived archive head; coordinator `recovery.ts:2771/:3970` supplies record.sourceSha from acquired/proved sources or connected coverage above. Owner failed-transfer composed test refuses before the coverage ref, so that partial root never reaches this transient pin. Never an eligible boundary anchor. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Explicit index-pack writers | `git.ts:5565` `["index-pack", "--stdin", "--strict", "--check-self-contained-and-connected"]` targets temporary bundle-verification bare; :14477 `"index-pack", "--strict"` indexes private staging; :14788 `spawnGitAsRunnerWithStdin(clonePath, ["index-pack", "--stdin"])` targets runner clone. Canonical transport imports are clone/fetch above. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Delete-only transaction | `git.ts:5998–5999` prepares `update-ref --stdin` with `delete`; deletes cannot enlarge closure. Include in exhaustive classification, not as acquisition. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| importMaintenanceTip — objects only, never refs | Nonempty: selected exact-OID/decoded proof + trusted-boundary invariant → canonical import → bare-only range check and identity/control revalidation (`validate()`; the shallow query is checked before boundary formation and at the post-proof recheck preceding import, and is not re-run after import) → scoped imported result. Empty: eligible-anchor ancestry proof and same admission/rechecks → already_present. Owner joins/fresh observation precede accepted output index. Failed post-import residue has no accepted result; successful connected but not-yet-indexed imports are legitimate unnamed objects, not forbidden merely for lacking a ref. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |

**Three named residuals required in ADR-1197 and PR:** (1) the trusted-boundary
invariant relies on fetch connectivity checks and existing explicit walks;
buildRecoveryCoverage's protection against partial roots is incidental (ancestry), not a
tree/blob closure proof; (2) failed agent-branch fetch may leave unreferenced partial
objects in the bare—fetch does not trust unreferenced objects; (3) maintainers track
writer hardening separately and this child changes none of those writers. The owners'
tested failure retained custody; do not generalize it into an independently verified
full-closure guarantee for the builder.

**Residue versus provenance:** failed-fetch unreferenced objects and U4b
post-import-failure residue are not trusted by presence/type or a journal alone and
authorize no pin/output/FINAL/release/cleanup. Successful
fetch-connected/synthesized/full-walk-verified objects can be connected but unnamed
(verified import before its index write, preflighted bridge, prior verified snapshot
head). Current pinners use the acquisition chains above; do not add a rule banning
legitimate unnamed verified objects. A new caller/acquisition path not covered here
needs its own proof; any other unresolved writer gap is still R3. The ordinary-journal →
coverage concern is resolved by owner audit/test evidence, not waived by plan approval
or repaired in this child.

Automatic maintenance: `git.ts:4745–4746` sets `maintenance.auto=false` and `gc.auto=0`;
per-invocation gitEnv pins at :19714–19715 reinforce them. `:4674` `await git(["repack",
"-a", "-d", "-q"]);` is runner-clone materialization only. Tracked searches found no
explicit canonical repack/prune or agent core.fsync setting; no other issue is cited as
establishing the invariant.

### Canonical history-decoration writer audit

Record in audit/ADR: at baseline 76cd0a66 no tracked production writer creates canonical
`info/grafts` or `refs/replace/*`. Tests do create replacements in temporary bare
fixtures (`recovery-guarded-producer.test.ts:277`,
`repo-instructions-at-commit.test.ts:142`), so do not say “no writer” without the
production qualification.

Exhaustive tracked grep: `git grep -n -i -e graft -e 'refs/replace' -e 'refs\\/replace'
-e GIT_NO_REPLACE_OBJECTS -e GIT_REPLACE_REF_BASE -e '"replace"' -e "'replace'"`;
complete hits at `.uzi/scratch/point7-exhaustive-grep.txt`. Destination/plumbing
enumeration: `git grep -n -E
'update-ref|symbolic-ref|pack-refs|fetch.*refspec|refs/\\*|refs/heads/\\*|--mirror|fast-import|unpack-objects'
-- agent/src`; evidence at `.uzi/scratch/point7-writer-hits.txt` and expanded variants
`.uzi/scratch/point7-expanded-hits.txt`.

Matches are defensive env pins (`git.ts:2441` `GIT_GRAFT_FILE: "/dev/null"`, :13556
`GIT_REPLACE_REF_BASE: "refs/disabled/"`), clone-side refusal (:11931), tests/docs or
unrelated replacement. Clone is bare, not mirror (:15054); refspecs are heads→origin
(:15059), checkpoints (:15098), and branch→incoming (:4853–4854). External/same-uid
actors and config remain trust residuals.



Current production-only repeat: `git grep -n -i -e graft -e 'refs/replace' -e
'refs\/replace' -e GIT_NO_REPLACE_OBJECTS -e GIT_REPLACE_REF_BASE -e '"replace"' -e
"'replace'" -- agent/src` returned defensive pins, refusals and comments, with no
production history-decoration writer found. The current hits are
`agent/src/git.ts:1101`, `:2439`, `:2441`, `:2511`, `:2512`, `:5528`, `:5529`, `:5567`,
`:7020`, `:7021`, `:7022`, `:11500`, `:11502`, `:11677`, `:11872`, `:11945`, `:12344`,
`:12658`, `:12664`, `:13683`, `:13684`, `:14516`, `:14517` and `:16547`. Verdict for
each listed site: already accurate defensive configuration, refusal or explanatory
comment; none creates canonical grafts/replacements. The broad historical
plumbing/ref-path/filesystem sweeps are recorded in
`.uzi/scratch/point7-exhaustive-grep.txt`, `point7-writer-hits.txt`,
`point7-expanded-hits.txt`, `point7-ref-path-hits.txt`, `point7-filesystem-writers.txt`
and `point7-other-writers.txt`. Current repeat evidence:
`.uzi/scratch/u4b-history-decoration-production-grep.txt` and
`.uzi/scratch/docs-u4b-current-history-grep.txt`. These scratch files are supporting run
evidence; the findings, acquisition chains and limits are recorded here and in the
delivery record.

The unchanged-writers comparison records `captureMaintenanceWip`, `cloneBare`,
`disableAutoMaintenance`, `fetchAgentBranch`, `updateTrackingRef`, `anchorRecoveryHead`
and `pinSettlementRefs` unchanged against 76cd0a66. No production writer or W0-H change
is part of U4b. Same-uid actors, storage loss, shallow-source writers and separate
maintainer hardening remain outside this change.

### U4b validation limit observed during M2

The required full-gate fixture, including W0 and coordinator effects under its
existing temporary-directory wrapper, exhausted the unchanged adminEntries limit.
A narrowed reproduction refused the next 1-entry scratch-parent stat at 4,096
entries, after proof and during the canonical-import phase's post-import-range
dispatch guard; it used 16 charged RPCs plus 4 discovery attempts. The earlier direct
fixture success at 3,983 admin entries does not establish this wrapped path's
capacity. This over-cap result is R3 material-change evidence, not permission to
raise the cap, shorten the failing path, remove proofs/checks or grant output/cleanup
authority. This is historical failed-gate evidence. The owner has now resolved
the resource-category decision by the C1-only correction and actual wrapped pass
below. The implementation remains guarded off; full-gate and complete mutation-map
obligations still block completion. Detailed counters and fixture repairs appear
in [the delivery record](../docs/issue-3031-delivery.md#m2-owner-diagnosis-and-category-correction).


### U4b owner-directed C1 category correction

Diagnosis precedes redesign: at baseline `6f8f0b7c`, admin charges reconcile to
4096 successful units plus one refused existing-ancestor lstat at `git.ts:13650`
`scratchCheck`, before post-import range dispatch. Strict import had completed;
no accepted output followed. No actual duplicate operation was established.
The owner authorized a category correction preserving operations, units, separate
consume calls, reservation timing, checks, scheduling, caps and timeouts.
This resolves the resource R3 decision, not the historical failed gate.

C1 existing-file stat/fstat/open/pin checks and parent/identity/cleanup reserves
use `sourceEntries`: controls after exclusive creation, retries, initial/repeated
scratch ancestry and staged/renamed entries. `own` retains
depth+(administrative?3:0) and its separate nonadministrative three-unit consume,
with `(depth+3)*6144` metadata. Private mkdir and exclusive HEAD/config creation
remain `adminEntries`. Cleanup stays finite/prepaid after joins, retains uncertain
identities and grants no canonical rollback.

Disposable stage and proof-bound index select source for target and parent stats
and cleanup reservations. Omitted options retain ordinary admin defaults, fsync,
parent policy and environment. New private canonical config acquisition passes
an explicit sixth `runGit` argument selecting source for raw nonempty NUL rows
before filtering. `runGit` and
`MaintenanceResources.consumeMetadataEntriesIfActive` default their optional
category to admin; ordinary callbacks and W0 rows retain their original charges.
Selected/range/index/CRC object rows stay `closureEntries`; metadata, encoded reads,
artifact emission and decoded bodies retain independent charges.

W0-H bytes are preserved (lead-verified hash prefix `77c072d`; exact W0 accounting
test passed). Historical W0 totals remain P0=430, P1=0, P2+P3=380, P4a=426,
P4b=1042, P5=262, aggregate2540. This is preservation, not a newly measured
post-fix W0 subphase ledger.

The measurements used the four-file category correction before its commit,
not the unchanged source at `6f8f0b7c`. Main's actual wrapped eight-case run passed
(`gate-log.Wjznwz`, EXIT=0, zero skips, 154614.593962 ms), with real procfs and
tmpdir-leakguard wrappers, primary agent cwd, unchanged Linux os.tmpdir depth,
only UZI_SQLC_DIR unset. Dirty import measured launches200/512,
metadataBytes32870476/268435456, admin2664/4096, sources126/128,
RPC18/32 +4 discovery=22 physical, sourceEntries13017/65536,
sourceBytes201091/268435456, artifact2046/268435456, closure18/65536,
decoded478/8GiB or tighter inherited C. Eight variants remain under their caps;
index acceptance follows owner join and refusals block their respective effects.
No ref, FINAL or cleanup authority follows.

The delivery record carries the full baseline phase/operation table, each
old-to-new site verdict, 15 new accounting cases and one preserved partial-write
assertion adjustment, plus every slot dimension and physical/discovery RPC total
for each wrapped variant. M2/M3, complete mutation mapping, integration/full gate
and new candidate reviewer/tester/auditor sign-offs remain incomplete and mandatory.
The timed-out full gate is not green. Guard remains false; C2–C8, compose70/72/73
and hosted timing retain their separate unproven boundaries.

## Issue #3466 amendment — silent human revisions

A real Codex idle trip with exactly zero measured model-processing events can
use `plan_revision_retry_v1` during an accepted human revision, including its
clarification and prose followups. The `idleStallKind: "zero_evidence"` marker
retains the original idle trip; `providerOrigin` stays false except for #3506's
qualifying retry-associated silence above, which retains both markers. Silence
alone does not establish a provider error. This path uses `PlanRevisionRecovery`,
without an ordinary recovery park or provider-outage accounting. Missing negotiation,
hook, active revision or presentation identity retains the original terminal
idle outcome. Control and ownership exceptions retain their own routes.
(AI-synced 2026-10-28)

The pre-reaping sink persists the session before verified epoch disposal,
including when the retry allowance is exhausted. A silent retry reconstructs
inside the revision driver, after allowance consumption and the last awaited
disposal. Exact source, claim and owner controls are checked again before
construction and at credential/root admission, including before any capture.
Unproven cleanup, including cleanup of a partially built replacement or a
failed repeated disposal, and residue quarantine refuse continuation. A failed
repeated disposal revokes the earlier capture-only proof for that exact facade.
Token redaction survives replacement
until existing terminal cleanup.

Each admitted reconstruction spends the existing three-redrive allowance
before fresh construction, including an owner replacement after exhaustion.
Locked-vault and unavailable-credential construction failures remain inside
that bounded driver, including an owner replacement revision after exhaustion;
they do not escape into the runner credential hold. Teardown, backoff and
admission consume the unchanged wall budget. Silent-revision backoff slices are
clamped to the remaining wall budget before sleeping; a spent wall returns to
the executor's existing interruption routing. Time awaiting an owner event at
the restored gate remains excluded; ownership checks and gate restoration
between wait ticks remain charged. Both revision-retry entry paths check the
remaining wall after source checks and gate restoration, before each owner-event
wait. A spent wall returns to the executor's wall routing; an extension retains
the exhausted retry state instead of admitting an uncharged reconstruction.
Retry waits for unknown ownership and gate-restoration evidence use that same
remaining budget. A replacement revise already taken from the owner queue stays
pending across a wall unwind until its source proof can complete.
Cancel, pause, spent wall and completion
holds retain their existing precedence. Generic provider-origin revision
retries continue on their existing epoch; #3506's dual-marker idle error keeps
verified disposal before the hook and fenced fresh-epoch construction after
allowance admission. (AI-synced 2026-10-28)

The same failed prompt and clarification/prose budgets survive a silent retry.
Feedback remains pending until source-fenced revised publication succeeds.
Exhaustion restores the original presentation and an operable busy owner wait;
source-bound reject and cancel remain available. An ordinary replacement revise
spends the normal revision cap and receives a fresh allowance, while predecessor
feedback settles only with the successor publication receipt. Retry notices
describe plain silence as no model output. For #3506's dual-marker error they
say `silence after a provider-reported rate limit or transport retry`, without
asserting that the retry condition caused it. (AI-synced 2026-10-28)

Validation uses scripted executor and runner fixtures. No live-provider or
compose-stack proof is claimed for this amendment.

## Amendment 2026-10-26, #3495: bounded retained-recovery parks

Decision record (AI-synced 2026-10-26). This amendment qualifies #2512's
terminal-exhaustion policy, #3330's owner-only renewal and server-requeue
exclusion, and the verified-capture-before-promotable-park prerequisite.
It applies only to an authoritative exact-source journal durably confirmed as
`budget_exhausted`. Other permanent blockers keep the existing terminal fallback
with custody retained. #3462's Codex zero-evidence idle still requires verified
capture; its typed park policy is superseded by #3509 below. This is not a
broader retry for deterministic blockers.
#3181 remains out of scope. There is no linked PRD.

### Retry allowance and renewal

- `RUN_RETAINED_RECOVERY_MAX_PARKS` is nonnegative, default **3**; **0** holds
  immediately. If the prior `retained_park_count` is below the cap, the park
  increments it and schedules a retry using that cause's count: base backoff
  of 1m, 2m, 4m at defaults, clamped by `RUN_RECOVERY_MAX_PARK`, plus jitter.
  `RUN_RECOVERY_PARK_BASE` sets the base. At the cap the count stays unchanged
  and `recovery_retry_not_before` is NULL: the fourth exhaustion is owner-held
  by default. Both forms increment lifetime `recovery_wait_count`, which does
  not set this cause's cadence. Owner Resume preserves the count and cap.
- Automatic parks do not change stored `worker_recovery_episode` or
  `requeue_episode_baseline`. All four claim builders (ordinary, chat, job,
  judge) project `worker_recovery_episode = stored worker_recovery_episode +
  retained_park_count` through `claimRecoveryEpisode` in
  [retainedpark.go](../api/internal/workersvc/retainedpark.go).
  The worker-death allowance and historical `WorkerRecoveryDTO` use stored
  episode evidence; retained retries are not worker-death requeues.
- Renewal requires both a fresh generation and a higher derived episode than
  the journal. It charges exactly one reservation of three in a new five-minute
  window, clears only `budget_exhausted`, and requires quiescence again before
  recovery work. Same generation or same episode cannot renew. Resume of a
  pending timed retry before its fresh claim can advance the derived episode
  by two but still grants one renewal, not two windows.
- Default timer promotion permits up to three new full run wall windows:
  `PromoteRecoveryWaitRuns` sets `started_at = NULL`. Manual Resume instead
  preserves existing wall-remaining semantics and banks held time; the owner
  may need `uzi run extend` first. It does not grant that timer wall reset.
- Existing owner-authorized Resume and Cancel work on timed and held forms.
  Resume clears the retry, increments stored `worker_recovery_episode` by one,
  resets the worker-death baseline, and preserves `retained_park_count`.
  Timer promotion excludes NULL retries. Token/credential immediate promotion
  excludes this cause entirely. See the park, Resume and promotion queries in
  [runtime.sql](../api/internal/store/queries/runtime.sql).

### Worker confirmation and execution fencing

The worker requires a positive claim generation, negotiated
`recovery_cause_retained_recovery_exhausted`, and authoritative exact-source
journal readback confirming durable `budget_exhausted`. Feature absence,
readback or persistence failure, another blocker, or explicit HTTP 400 before
any uncertain request keeps the existing terminal fallback with custody kept.
Transport/server errors and unconfirmed 2xx responses enter bounded
reconciliation and preserve a nonterminal retained stop. Only returned status
and cause confirm the park; stale, cancelled or newer outcomes stop without a
terminal report. Budget exhaustion does not require a new capture it prevents.

An applied park atomically releases the **execution claim** and fences late
same-generation state, completion and snapshot reports. It retains `worker_id`,
session and open custody holds, and does not set a released-worker-incarnation
marker. `ClaimRun` clears execution release with a fresh generation. Execution
release is never a custody release or source-deletion receipt.

Both timed and held forms conservatively protect custody throughout
`recovery_wait`, before archive readiness or capture progress, after existing
discarded/released precedence. After timer promotion or Resume leaves the park,
existing archive/final-disposition proof rules apply again; source-only,
unknown or unproven holds stay. There is no new custody lifetime marker.
Export availability remains independent and an earlier archive may omit latest
work. Same-worker affinity remains; after worker death or the affinity ceiling,
a peer may recover a published checkpoint without the local journal, with
source custody still held and no latest-work guarantee.

### Presentation, risks and rollout

Finite retries, including due timestamps, display as timed retries. NULL means
owner-held with no countdown; omitted timing is unknown. Default CLI wait and
log follow stop for the owner-held form; explicit `--until` is unchanged.
Follow notices include a finite-to-NULL change within the same cause/status.

Retained sources increase storage and custody-quota pressure and can block image
rolls in both forms. Post-deadline failure reclassification as
`budget_exhausted` can include another failure; renewed quiescence remains
required. Deploy the API fully before workers consume the feature. Mixed API
replicas and API rollback are unsupported; an old API omitting the feature uses
the terminal fallback. The existing worker-journal downgrade caution remains:
older workers may reject or drop durable recovery fields. This amendment adds
no operator watcher or lander policy, source-deletion authority or guarantee
that the latest edits have an archive.

## Amendment 2026-10-27, #3509: bounded Codex idle-stall parks

Decision record (AI-synced 2026-10-27). For Codex idle stalls this amendment
explicitly supersedes #3462's earlier claims:

> "Ordinary recovery retains its uncapped lifetime park count"

> "Repeated silence can recover until the run wall requires an owner park"

Promotion resets the wall: silence was not bounded by one original
wall window. Up to the configured cap of automatic parks may grant that many
extra fresh wall windows (three by default), an accepted cost of recovery.
Other causes, #3466 human revision recovery and retained-source policies keep
their separate rules. No live-provider or live-rollout acceptance is claimed.

### Classification and capture boundary

Measured lead initial-planning, automatic-plan-revision and implementation idle
trips use `codex_idle_stall` for both `zero_evidence` and `productive` stalls,
except for #3506's qualifying retry-associated zero-evidence idle, which uses
provider recovery as described above. (AI-synced 2026-10-28)
“Productive” here means observed output before the stall, not a completed turn.
Human revision productive idle keeps failure; zero-evidence silence stays inside
the existing revision driver. Child turns, Claude, callback-based idle suppression
and `idleOperation` remain unchanged. Cancel, pause, spent wall, vault lock,
completion hold and first-trip precedence retain their existing boundaries in
[the Codex executor](../agent/src/codex/codex-executor.ts).

Before capture, typing requires negotiated `recovery_cause_codex_idle_stall_v1`
and a positive, stampable claim generation. Session persistence, executor cleanup,
dirty-work WIP, fetch-back and verified ref coverage precede an accepted park.
These nonqualifying idle errors have `providerOrigin: false` and do not enter
provider-outage counting; #3506's qualifying dual-marker errors instead take the
provider wire checks and accounting above. The remaining rules in this amendment
apply to `codex_idle_stall`. (AI-synced 2026-10-28)
Acceptance requires returned `recovery_wait` and exact `codex_idle_stall` cause,
including when the full worker ACK has no retry timestamp (held).
Without negotiation, zero-evidence idle keeps legacy untyped recovery and
productive idle keeps terminal failure. An advertised feature followed by HTTP
400 or feature loss must not cause an untyped resend; source stays kept.
See [the runner](../agent/src/runner.ts) and [client](../agent/src/client.ts).

### Park-observed accounting and owner controls

`RUN_CODEX_IDLE_STALL_MAX_PARKS` accepts integers `0..2147483646`, default **3**;
malformed, negative or above-bound values fall back to 3. A qualifying park
increments internal `idle_stall_park_count` with int32 saturation, or sets it to
1 when `provider_progress_turns` reports positive completed turns in that flight.
A count above the cap sets `recovery_retry_not_before` to NULL; otherwise the
existing lifetime backoff and jitter produce a finite timestamp. Default parks
1–3 are timed; park 4 is held. **0 holds on the first park**. The counter is not
a public DTO field. See [recoverywait.go](../api/internal/workersvc/recoverywait.go)
and [configuration parsing](../api/internal/config/config.go).

This is a park-observed streak, not an exact unsuccessful-turn count. Output on
the current stalled turn is not completed progress. Another flight's progress
may be unseen if it does not end in an idle park, allowing an earlier hold.
Reclaims, checkpointing and credential parks are neutral for the idle counter.
Positive completed progress on the idle park can also clear a provider streak
under its existing progress rules, without charging a provider outage.

Owner Resume of either timed or held form queues it, clears retry, resets idle
count to 0, sets `started_at = NULL` and `budget_paused_seconds = 0`, and revokes
the Codex capability/advances its epoch. It preserves lifetime recovery backoff,
provider streak, session, worker affinity, worker recovery episode and requeue
baseline. Ordinary claim-release/custody semantics remain. Timer promotion
preserves the idle count and grants a fresh wall window. Timed set-token promotion
is permitted under existing credential behavior; a held set-token request returns
a named 409 requiring Resume. Cancel remains available. These boundaries are in
[Resume and promotion SQL](../api/internal/store/queries/runtime.sql) and
[credential switching](../api/internal/workersvc/run_credential.go).

UI/CLI finite timestamps, including due ones, mean timed recovery. NULL means
owner-held without countdown; omitted public timing means unknown, distinct from
the full worker ACK. Default wait and follow stop for held, continue through
finite/unknown timing, and notice timed-to-held changes within the same
status/cause. Explicit `--until` keeps its chosen stop set.

### Session, source custody and admission limits

Same-session continuity is conditional on a resolvable ordinary unwired
non-Docker path. Retained-source and Docker recovery use fresh paths/threads.
A rejected thread resume retains the `resume_lineage_break` warning and
fresh-thread fallback. Interrupted-turn notes, reconciliation and snapshots
do not prevent duplicate work or guarantee exactly-once external effects.

An execution hold adds no custody-retention policy. Ordinary settlement and
exact-generation ACK guards still govern release; uncaptured, guarded,
unresolved and 507-refused sources remain retained. Open holds still count
toward admission. The cap limits further generation minting for one run; it
does not relieve custody quota, storage or admission pressure. #2969/#2544
remain outside this change. See [custody admission in ClaimRun](../api/internal/store/queries/runtime.sql)
and the runner's existing source-settlement boundaries.

### Deployment and destructive Down

Deploy the API to **all replicas before workers**. Mixed old/new API replicas
and application rollback while idle holds exist are unsupported: the old
token promoter can bypass the held NULL-retry shape. Held runs block worker
image rollout under #3103's existing upgrade-blocker policy. No live rollout
has been executed as evidence for this documentation unit.

[00370_codex_idle_stall.sql](../api/internal/store/migrations/00370_codex_idle_stall.sql)
adds a constant-default counter and `NOT VALID` checks in a short schema
transaction; [00371_validate_codex_idle_stall.sql](../api/internal/store/migrations/00371_validate_codex_idle_stall.sql)
validates in a separately committed migration. The brief ACCESS EXCLUSIVE DDL
lock can wait; separate validation avoids holding that lock through the scan.
Production table size, lock duration and row-update/WAL/replica costs are unmeasured.

Down changes active idle parks (timed or held) to `failed` / `worker_lost`
with the 00370 schema-rollback reason and finish/status/update timestamps, then
clears historical idle cause/retry fields before restoring the old check and
dropping the counter. It does not mutate custody. Counter history and idle-cause
evidence are lost; assess backup/PITR needs first. This destructive downgrade
failure is not normal cap policy or supported application rollback.

The setting is wired into the Compose API environment (including literal 0)
and `.env.example`. Helm uses existing
`api.config.RUN_CODEX_IDLE_STALL_MAX_PARKS` → ConfigMap → API `envFrom`;
no chart mechanism is added. See [operator configuration](../docs/configuration.md#codex-idle-stall-deployment-and-schema-rollback).
