---
title: Recovery storage diagnosis
order: 69
audience: operator
---

# Recovery storage diagnosis

Use this read-only procedure for [#2544](https://github.com/vtmocanu/uzi/issues/2544):
a quota-refused archive can leave source in custody and contribute to the
owner's admission pressure. It does not establish how much of the owner's
held work is quota-related. See [Recovering unpublished work](./run-recovery.md)
for owner actions and [Admin health](./admin-health.md#recovery-storage)
for the current-state warning.

The operator attributes the refusals to the default 1 GiB per-owner quota,
pending byte-accurate reproduction, with roughly sixteen large self-contained
archives retained. Exact usage, reservations and the refused bundle's size
still need reproduction. A subsequent per-owner override to 4 GiB and an
unchanged 4 GiB instance quota are operator-reported mitigation facts.
Those reports do not identify the rejecting branch: the shared budget,
other owners, reservations, capture counts and bind-time checks also matter.
Admission-counted `needs_action` quota holds can coexist with `source_only`
holds; the exact share remains unknown.

This diagnosis changes no quota, expiry, custody or bundle policy.
[#2625](https://github.com/vtmocanu/uzi/issues/2625) tracks early expiry separately;
[#2623](https://github.com/vtmocanu/uzi/issues/2623) tracks the capacity-share
warning separately (including a proposed 25% warning).
[#2507](https://github.com/vtmocanu/uzi/issues/2507) covers source-only custody.
None is claimed as implemented by #2544.

## 1. Preserve the identities and time

Record the owner, run, hold, capture, original worker and hold generation.
A run's current `claim_generation` can differ from the held generation.
Record the request route (reserve, upload or retained FINAL), timestamp,
API/worker versions, typed response and worker journal/log outcome.
Use `uzi run recovery <run-id>` to join holds and captures and
`uzi admin health --json` for the health evidence.

The surfaces carry different evidence:

| Surface | Evidence and boundary |
|---|---|
| Web recovery archive / owner archive API | Capture `reason` can be the natural-language `storage quota exceeded`. Read the archive, not just the hold's attention. |
| Worker API | `mapRecoveryError` returns HTTP 507 with typed `reason: quota`; both capture-count reserve and byte refusal use it. |
| Worker journal / outcome | `classifyUploadFailure` preserves typed quota as transient `storage_quota_exceeded`, eligible for retained live retry and backoff. Source, bytes and identity remain retained; a successful upload clears the diagnostic. Untyped/unknown 507 stays generic transient, without a quota diagnosis. 401 retains credential precedence. |
| CLI `run recovery`, human and JSON | `recoveryHoldCapture` projects id, state, source SHA, byte size and creation time; it omits capture reason. The hold DTO supplied by `custodyHoldToDTO` has no capture reason either. Absence here is not absence in storage. |
| Admin Health | Counts current persisted quota markers, independently of custody; it is not a failure history. |

Implementation: [error mapping](../api/internal/handler/recovery.go),
[worker classification and retry](../agent/src/recovery.ts),
[CLI projection](../api/cmd/uzi/run_recovery.go) and
[hold conversion](../api/internal/recovery/convert.go).

Collect **effective settings at the incident time** and current effective
settings separately, including the API replica handling the route. Record
retention and upload-retry windows as well as byte and capture-count limits.
Environment/deployment history is needed for the former; today's rows and
health limits cannot prove historical settings or usage.
Use exact integer byte counts from the authenticated manifest and, for bind,
the size/checksum-verified stream; a rounded “large archive” is insufficient.

Current defaults come from [API configuration](../api/internal/config/config.go):

| Environment variable | Default |
|---|---|
| `UZI_RECOVERY_READY_PAYLOAD_PER_OWNER_BYTES` | `1 << 30` bytes (1 GiB) |
| `UZI_RECOVERY_INSTANCE_BYTES` | `4 << 30` bytes (4 GiB) |
| `UZI_STORED_FILES_BUDGET_BYTES` | `4 << 30` bytes (4 GiB) |
| `UZI_RECOVERY_MAX_BUNDLE_BYTES` | `64 << 20` bytes (64 MiB) |
| `UZI_RECOVERY_MAX_CAPTURES_PER_CLAIM` | 16 |
| `UZI_RECOVERY_MAX_CAPTURES_PER_OWNER` | 256 |
| `UZI_RECOVERY_READY_RETENTION` | 7 days |

## 2. Read one consistent accounting snapshot

Connect using your deployment's read-only PostgreSQL access; see
[Installation](./installation.md) for the deployment layout.
Paste the blocks below into the **same psql session and transaction**.
Replace the three UUID placeholders and heartbeat cutoff before starting;
set sizes and ceilings to the effective values being investigated.
`declared_bytes` and `verified_bytes` below are examples, not measurements.
Use the server's heartbeat cutoff for custody admission and the snapshot's
`as_of` for current TTL/reclaim eligibility. For a historical incident use a
historical snapshot and its recorded clock, not today's rows with an old clock.

```sql
\set ON_ERROR_STOP on
\set owner_id 'REPLACE_OWNER_UUID'
\set run_id 'REPLACE_RUN_UUID'
\set capture_id 'REPLACE_CAPTURE_UUID'
\set heartbeat_cutoff 'REPLACE_SERVER_HEARTBEAT_CUTOFF'
\set declared_bytes 67108864
\set verified_bytes 67108864
\set owner_limit 1073741824
\set instance_limit 4294967296
\set shared_budget 4294967296
\set claim_limit 16
\set owner_capture_limit 256

BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT now() AS as_of \gset
SELECT :'as_of'::timestamptz AS as_of,
       c.id AS capture_id, c.user_id AS owner_id, c.run_id, c.hold_id,
       h.generation, h.inventory_guarded, h.state AS hold_state,
       h.original_worker_id, h.live_worker_id,
       r.claim_generation AS current_run_generation,
       c.state, c.reason, c.byte_size, c.reserved_bytes,
       c.manifest_bound, c.prerequisite_shas,
       c.expires_at, c.local_replica_worker_id,
       c.ready_retention_seconds, c.created_at, c.updated_at
FROM recovery_captures c
JOIN recovery_custody_holds h ON h.id = c.hold_id
LEFT JOIN runs r ON r.id = c.run_id
WHERE c.user_id = :'owner_id'::uuid AND c.run_id = :'run_id'::uuid
  AND c.id = :'capture_id'::uuid;

-- Raw per-owner/state metadata: retained values need not still consume quota.
SELECT user_id AS owner_id, state, count(*) AS captures,
       COALESCE(sum(byte_size), 0) AS byte_size,
       COALESCE(sum(reserved_bytes), 0) AS reserved_bytes
FROM recovery_captures GROUP BY user_id, state ORDER BY user_id, state;

SELECT user_id AS owner_id, state, count(*) AS files,
       COALESCE(sum(byte_size), 0) AS byte_size
FROM job_files GROUP BY user_id, state ORDER BY user_id, state;

-- Raw stored chunks, separately from logical accounting; no decryption.
SELECT 'recovery' AS store, count(*) AS chunks,
       COALESCE(sum(length), 0) AS plaintext_bytes,
       COALESCE(sum(octet_length(sealed)), 0) AS ciphertext_bytes
FROM recovery_capture_chunks
UNION ALL
SELECT 'jobs', count(*), COALESCE(sum(length), 0),
       COALESCE(sum(octet_length(sealed)), 0)
FROM job_file_chunks;
```

No identity row means stop and resolve the IDs; do not infer a byte refusal.
The raw grouped values include tombstone metadata. The admission totals below
use the service's state predicates and **exclude the capture being retried**.
A refused `needs_action` capture contributes zero even when its metadata
retains size values. Chunk plaintext and ciphertext totals are separate:
neither is PostgreSQL/PVC physical capacity (indexes, WAL, tuple overhead,
replication and free space are outside this accounting).
Column definitions are in [migration 00223](../api/internal/store/migrations/00223_recovery_archive.sql),
[migration 00277](../api/internal/store/migrations/00277_job_files.sql) and
[migration 00305](../api/internal/store/migrations/00305_recovery_inventory.sql).

```sql
WITH rec AS (
  SELECT user_id, state, COALESCE(byte_size, 0) AS ready_bytes,
         CASE WHEN state = 'available' THEN COALESCE(byte_size, 0)
              ELSE COALESCE(reserved_bytes, 0) END AS admission_bytes
  FROM recovery_captures
  WHERE state IN ('available', 'preparing', 'uploading')
    AND id <> :'capture_id'::uuid
)
SELECT
  COALESCE((SELECT sum(admission_bytes) FROM rec
            WHERE user_id = :'owner_id'::uuid), 0) AS owner_recovery,
  COALESCE((SELECT sum(admission_bytes) FROM rec), 0) AS instance_recovery,
  COALESCE((SELECT sum(byte_size) FROM job_files
            WHERE state <> 'expired'), 0) AS jobs,
  COALESCE((SELECT sum(ready_bytes) FROM rec WHERE state = 'available'), 0)
    + COALESCE((SELECT sum(byte_size) FROM job_files
                WHERE state NOT IN ('expired', 'reserved')), 0) AS committed,
  COALESCE((SELECT sum(byte_size) FROM job_files
            WHERE state = 'reserved'), 0) AS reserved_jobs,
  COALESCE((SELECT sum(byte_size) FROM job_files
            WHERE (state IN ('unattached', 'available')
                   AND expires_at IS NOT NULL
                   AND expires_at < :'as_of'::timestamptz)
               OR state = 'available'), 0) AS reclaimable
\gset acc_

SELECT :'acc_owner_recovery'::bigint AS owner_recovery_bytes,
       :'acc_instance_recovery'::bigint AS global_recovery_bytes,
       :'acc_jobs'::bigint AS global_nonexpired_job_bytes,
       :'acc_instance_recovery'::bigint + :'acc_jobs'::bigint
         AS shared_admission_bytes,
       :'acc_committed'::bigint AS committed_shared_bytes,
       :'acc_reserved_jobs'::bigint AS reserved_job_bytes,
       :'acc_reclaimable'::bigint AS reclaimable_job_bytes;

SELECT CASE
  WHEN :owner_limit::bigint > 0
   AND :'acc_owner_recovery'::bigint + :declared_bytes::bigint > :owner_limit::bigint
    THEN 'owner byte refusal'
  WHEN :instance_limit::bigint > 0
   AND :'acc_instance_recovery'::bigint + :declared_bytes::bigint > :instance_limit::bigint
    THEN 'instance byte refusal'
  WHEN :shared_budget::bigint > 0
   AND :'acc_instance_recovery'::bigint + :'acc_jobs'::bigint
       + :declared_bytes::bigint - :shared_budget::bigint > :'acc_reclaimable'::bigint
    THEN 'shared byte refusal after reclaim allowance'
  ELSE 'byte admission fits this snapshot'
END AS admission;
```

[Service.admit](../api/internal/recovery/service.go) checks owner → instance →
shared budget in that order. Byte refusal is strict `>`: equality fits;
each byte ceiling is disabled when `<= 0`. Shared admission permits an
excess covered by `SumReclaimableJobFileBytes`, but deletes nothing.
[SumStoredFileBytes / SumCommittedSharedBytes](../api/internal/store/queries/job_files.sql)
define the sums. This snapshot does not take the service's advisory locks;
it models observed inputs, not permission to upload or proof of a past decision.

## 3. Simulate bind-time reclaim without deleting

The verified upload uses `Service.reclaimForBudget`.
Reclaim need is `max(committed + verified - budget, 0)`, **excluding reserved
jobs and other recovery reservations**. The final check includes reserved
jobs: `committed_after_reclaim + reserved_jobs + verified <= budget`.
A bind-time refusal rolls back chunks and reclaim with the transaction.
The following SELECTs model the ordered whole-file selection, including
possible overshoot, without running `ReclaimJobFilesForRecovery`.

```sql
SELECT CASE WHEN :shared_budget::bigint <= 0 THEN 0::numeric
            ELSE GREATEST(:'acc_committed'::numeric + :verified_bytes::numeric
                          - :shared_budget::numeric, 0) END AS need
\gset bind_

WITH candidates AS (
  SELECT id, user_id, state, byte_size, expires_at, created_at,
    (state IN ('unattached', 'available') AND expires_at IS NOT NULL
     AND expires_at < :'as_of'::timestamptz) AS expired_first
  FROM job_files
  WHERE (state IN ('unattached', 'available') AND expires_at IS NOT NULL
         AND expires_at < :'as_of'::timestamptz) OR state = 'available'
), ordered AS (
  SELECT *, sum(byte_size) OVER (ORDER BY expired_first DESC, created_at, id)
              - byte_size AS bytes_before
  FROM candidates
)
SELECT *, bytes_before < :'bind_need'::numeric AS would_reclaim
FROM ordered ORDER BY expired_first DESC, created_at, id;

WITH candidates AS (
  SELECT id, byte_size, created_at,
    (state IN ('unattached', 'available') AND expires_at IS NOT NULL
     AND expires_at < :'as_of'::timestamptz) AS expired_first
  FROM job_files
  WHERE (state IN ('unattached', 'available') AND expires_at IS NOT NULL
         AND expires_at < :'as_of'::timestamptz) OR state = 'available'
), ordered AS (
  SELECT *, sum(byte_size) OVER (ORDER BY expired_first DESC, created_at, id)
              - byte_size AS bytes_before
  FROM candidates
)
SELECT COALESCE(sum(byte_size) FILTER
         (WHERE bytes_before < :'bind_need'::numeric), 0) AS freed
FROM ordered \gset bind_

SELECT :'bind_need'::numeric AS reclaim_need,
       :'bind_freed'::numeric AS whole_file_reclaim_bytes,
       :'acc_committed'::numeric + :'acc_reserved_jobs'::numeric
         + :verified_bytes::numeric AS bind_check_bytes_before_reclaim,
       :'acc_committed'::numeric - :'bind_freed'::numeric
         + :'acc_reserved_jobs'::numeric + :verified_bytes::numeric
         AS bind_check_bytes,
       CASE WHEN :shared_budget::bigint <= 0 THEN 'shared check disabled'
            WHEN :'acc_committed'::numeric - :'bind_freed'::numeric
                   + :'acc_reserved_jobs'::numeric + :verified_bytes::numeric
                 > :shared_budget::numeric THEN 'bind quota refusal'
            ELSE 'bind fits this snapshot' END AS bind_result;
```

Selection is past-expiry unattached/available first, then available files,
ordered by `created_at, id` within those groups. Attached, reserved and
unattached-inside-TTL files do not qualify. The predicates and ordering above
match [ReclaimJobFilesForRecovery](../api/internal/store/queries/job_files.sql).
Changing the reclaim need to include reserved jobs would misrepresent the
implementation.

## 4. Separate capture counts, expiry and custody

A new `Service.Reserve` refuses at `count >= ceiling` (unlike byte equality):
per-claim counts captures under the exact hold, including tombstones;
per-owner counts captures whose state is not `discarded`, including expired.
An existing idempotency key bypasses these new-capture checks. Nonpositive
ceilings disable them. Count refusal also returns 507 `quota`, potentially
before any capture row is created; it is absent from `recovery.storage`
unless a current persisted upload-refusal marker exists.

```sql
SELECT h.id AS hold_id, h.generation, count(c.id) AS per_claim_count,
       (:claim_limit::int > 0 AND count(c.id) >= :claim_limit::int)
         AS new_capture_count_refusal
FROM recovery_custody_holds h
LEFT JOIN recovery_captures c ON c.hold_id = h.id
WHERE h.user_id = :'owner_id'::uuid AND h.run_id = :'run_id'::uuid
GROUP BY h.id, h.generation ORDER BY h.generation, h.id;

SELECT count(*) AS per_owner_count,
       (:owner_capture_limit::int > 0 AND count(*) >= :owner_capture_limit::int)
         AS new_capture_count_refusal
FROM recovery_captures
WHERE user_id = :'owner_id'::uuid AND state <> 'discarded';

SELECT id, run_id, hold_id, state, expires_at, local_replica_worker_id,
       (state = 'available' AND expires_at IS NOT NULL
        AND expires_at < :'as_of'::timestamptz
        AND local_replica_worker_id IS NULL) AS eligible_ttl_expiry
FROM recovery_captures WHERE user_id = :'owner_id'::uuid
ORDER BY expires_at NULLS LAST, id;

SELECT count(*) AS total_open,
       fn_custody_admission_count(:'owner_id'::uuid,
                                 :'heartbeat_cutoff'::timestamptz)
         AS admission_counted
FROM recovery_custody_holds
WHERE user_id = :'owner_id'::uuid AND state = 'open';

-- Exact quota-linked open holds; this is not a list of generic failures.
SELECT h.id AS hold_id, h.run_id, h.generation, h.inventory_guarded,
       h.original_worker_id, h.live_worker_id,
       c.id AS capture_id, c.state, c.reason
FROM recovery_custody_holds h
JOIN recovery_captures c ON c.hold_id = h.id AND c.user_id = h.user_id
WHERE h.user_id = :'owner_id'::uuid AND h.state = 'open'
  AND c.state = 'needs_action' AND c.reason = 'storage quota exceeded'
ORDER BY h.run_id, h.generation, h.id, c.id;

-- Enumerate other holds too, using the server attention classification.
-- A hold can have several captures; EXISTS avoids counting it several times.
SELECT h.id AS hold_id, h.run_id, h.generation,
       fn_custody_attention(h.state,
         EXISTS (SELECT 1 FROM recovery_captures c
                 WHERE c.hold_id = h.id AND c.state = 'available'),
         h.inventory_guarded,
         COALESCE((SELECT c.state FROM recovery_captures c
                   WHERE c.hold_id = h.id
                   ORDER BY c.created_at DESC, c.id DESC LIMIT 1), ''),
         r.status, r.recovery_wait_cause) AS attention,
       EXISTS (SELECT 1 FROM recovery_captures c
               WHERE c.hold_id = h.id AND c.state = 'needs_action'
                 AND c.reason = 'storage quota exceeded') AS quota_linked
FROM recovery_custody_holds h
JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
WHERE h.user_id = :'owner_id'::uuid AND h.state = 'open'
ORDER BY h.run_id, h.generation, h.id;
ROLLBACK;
```

`total_open` is custody, not the admission count.
[GetCustodyAggregateForOwner](../api/internal/store/queries/recovery.sql)
uses `fn_custody_admission_count(owner, heartbeat_cutoff)`; the
[function](../api/internal/store/migrations/00310_custody_admission_count.sql)
discounts at most one qualifying healthy current-claim hold per run,
with no owner-decision attention. Unknown evidence remains counted.
Quota-linked rows do not establish which individual holds are discounted or
the exact proportion of admission pressure caused by quota.

[ExpireReadyCaptures](../api/internal/store/queries/recovery.sql) requires
available state, strictly past `expires_at` and no local-replica marker.
Selected final guarded archives keep that marker while their local worker
exists; physical worker deletion renews the window and clears it.
Hold discard preserves available archive bytes. Explicit archive discard
and eligible TTL expiry delete chunks; custody and local-replica blockers
are unchanged by this documentation change.

Guarded production attempts a self-contained full bundle first; below the
effective 64 MiB default cap that is the expected result. Only oversize
permits a thin fallback using an eligible direct cached default-branch ref
and verified merge bases. Invalid/absent/unrelated cache keeps the oversized
outcome. See [GitCache.produceRecoveryBundle](../agent/src/git.ts) and
[guarded producer tests](../agent/test/recovery-guarded-producer.test.ts).
Capture metadata alone cannot establish a persistent-versus-ephemeral
worker cause or reconstruct the producer's historical cache/ref inputs.

## 5. Ready-to-post issue comment

The following comment is prepared for #2544; it has **not been posted**.
Replace the incident placeholders with measured evidence before attributing
a specific limit. The test results are recorded implementation evidence,
not a reproduction on the operator's deployment.

> The default 1 GiB per-owner quota is the operator-reported attribution,
> pending byte-accurate reproduction. Roughly sixteen large self-contained
> archives were reportedly retained; consumers to enumerate are available
> archives and preparing/uploading reservations for this owner and globally,
> alongside shared job-file bytes. A later 4 GiB owner override was applied;
> the instance quota reportedly remained 4 GiB. Exact historical usage, reservations,
> route/time, effective settings and byte-accurate bundle size remain
> unverified. Current rows cannot prove the historical rejecting branch.
>
> #2544 preserves typed worker API quota as transient
> `storage_quota_exceeded` with retained bytes/source/identity and retry;
> untyped 507 stays generic. Admin Health now exposes `recovery.storage`,
> warning on current persisted `needs_action` / `storage quota exceeded`
> captures, independently of custody. Logical byte evidence and bounded
> owner examples support diagnosis; this is not physical-capacity telemetry
> or a refusal history. Count refusals without a capture row are not covered.
>
> Regression evidence: U1's same
> [live recovery](https://github.com/vtmocanu/uzi/blob/4adc57f5f2148de389652f3a2566321e1a550aa0/agent/test/issue-1995-live-recovery.test.ts) and
> [inventory](https://github.com/vtmocanu/uzi/blob/4adc57f5f2148de389652f3a2566321e1a550aa0/agent/test/recovery-inventory.test.ts) selection produced
> 6 behavioral failures / 301 passes with the fix absent (compile passed),
> and 307 passes after restoration.
> [Guarded producer](https://github.com/vtmocanu/uzi/blob/4adc57f5f2148de389652f3a2566321e1a550aa0/agent/test/recovery-guarded-producer.test.ts)
> controls separately passed 12 tests.
> U2's
> [TestRecoveryStorageAccountingLiveDB and TestRecoveryStorageUploadRefusalHealthLiveDB](https://github.com/vtmocanu/uzi/blob/c7a7bdac9237f800ee79adae1a264181231ae2fc/api/internal/handler/recovery_storage_health_livedb_test.go)
> establish a real HTTP 507 and persisted marker before testing warning and
> byte accounting.
> [TestAdminHealthAuthLiveDB](https://github.com/vtmocanu/uzi/blob/c7a7bdac9237f800ee79adae1a264181231ae2fc/api/internal/handler/health_admin_livedb_test.go)
> and
> [TestRecoveryInventoryExpiryHandoffLiveDB](https://github.com/vtmocanu/uzi/blob/c7a7bdac9237f800ee79adae1a264181231ae2fc/api/internal/recovery/inventory_livedb_test.go)
> provide auth and retention controls. Changing only the warning branch to
> `sevOK` compiled (exit 0) but failed the warning assertions (exit 1);
> restoration passed the identical 13 named RUN entries, zero skips,
> exit 0. These controls do not prove the operator's historical failure.
>
> Fixes:
> [U1 4adc57f5](https://github.com/vtmocanu/uzi/commit/4adc57f5),
> [comment clarification 7164233](https://github.com/vtmocanu/uzi/commit/7164233),
> [U2 c7a7bdac](https://github.com/vtmocanu/uzi/commit/c7a7bdac),
> [wiring/fake follow-up 553f5e19](https://github.com/vtmocanu/uzi/commit/553f5e19)
> and [web fixture 2bb61ce3](https://github.com/vtmocanu/uzi/commit/2bb61ce3).
> The lead reports agent/API/web gates and the full LiveDB sweep passed
> (2,492 passes, zero skips, seven packages); U3 has not rerun those gates.
>
> Incident evidence to add: owner/run/hold/generation/capture/worker IDs
> [pending]; reserve versus upload versus bind/FINAL route and time
> [pending]; historical/current effective owner, instance, shared and
> count limits [pending]; excluded-capture owner/global recovery bytes,
> job reservations, reclaim allowance and verified bundle bytes [pending];
> total-open versus admission-counted holds and quota-linked subset
> [pending]. Quota `needs_action` and source-only custody coexist;
> their exact share is unknown.
>
> No quota increase, expiry/custody/bundle policy change or 25% capacity
> warning is included. Early expiry
> [#2625](https://github.com/vtmocanu/uzi/issues/2625), capacity-share warning
> [#2623](https://github.com/vtmocanu/uzi/issues/2623) and source-only custody
> [#2507](https://github.com/vtmocanu/uzi/issues/2507) remain separate.
