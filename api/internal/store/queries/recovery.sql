-- PRD #1296 M1 (D2/D4/D5): the durable-recovery store contract. These queries are the
-- COMPLETE store-query layer M2 (upload/owner API), M3 (worker capture/retry) and M5
-- (owner UX) consume, frozen here so those parallel milestones never edit this file.
-- Custody holds are opened atomically with the claim (runtime.sql ClaimRun); everything
-- below operates on the already-open hold and its captures.

-- name: BindCaptureManifest :one
-- D2/D4: compare-and-set the byte manifest ONCE. The first bind (manifest_bound=false)
-- always wins; a retry with the SAME byte_size+checksum is idempotent (the second
-- disjunct matches and re-stamps updated_at); a DIFFERENT manifest under the same
-- capture_id matches neither disjunct and returns zero rows (a conflict the caller must
-- surface, never an overwrite of bound bytes). Manifest size/checksum/prerequisites need
-- not exist at reserve time; this binds them before chunks are accepted.
UPDATE recovery_captures
SET manifest_bound = true,
    byte_size = @byte_size,
    checksum = @checksum,
    chunk_count = @chunk_count,
    prerequisite_shas = @prerequisite_shas,
    updated_at = now()
WHERE id = @id
  AND (manifest_bound = false OR (byte_size = @byte_size AND checksum = @checksum))
RETURNING *;

-- name: InsertCaptureChunk :exec
-- D4: one ordered, AAD-sealed byte chunk. Inserted inside the single bounded upload
-- transaction M2 owns, alongside the ready transition, so chunks and ready become durable
-- together. sealed is the ciphertext; length is the plaintext byte count for that chunk.
INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed)
VALUES (@capture_id, @chunk_index, @length, @sealed);

-- name: MarkCaptureReady :one
-- D4: the atomic ready transition to 'available', stamping the expiry that begins at
-- durable capture (never at the earlier reservation). Gated on a bound manifest so a
-- capture can never be advertised ready without its verified byte identity (returns zero
-- rows otherwise).
UPDATE recovery_captures
SET state = 'available', expires_at = @expires_at, updated_at = now()
WHERE id = @id AND manifest_bound = true
RETURNING *;

-- name: MarkCaptureState :one
-- D4: move a capture to a non-ready lifecycle state (uploading / needs_action /
-- discarded) with an optional bounded reason. Used to record a bounded, sanitized
-- oversized/quota/transport/integrity reason on needs_action, or to mark an upload in
-- progress. reason is nullable (sqlc.narg) so a state with no reason clears it.
UPDATE recovery_captures
SET state = @state, reason = sqlc.narg('reason'), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: GetCaptureForOwner :one
-- D6: strict owner-scoped by-id read of one capture. The (id, run_id, user_id) triple is
-- the owner-authorization seam every metadata/download path funnels through — an admin
-- viewing a foreign run is refused because user_id will not match.
SELECT * FROM recovery_captures
WHERE id = @id AND run_id = @run_id AND user_id = @user_id;

-- name: ListCapturesForRunOwner :many
-- D7: every retained capture for one run, owner-scoped, oldest-first. Feeds the run's
-- Recovery archives section (all states shown, not gated on availability).
SELECT * FROM recovery_captures
WHERE run_id = @run_id AND user_id = @user_id
ORDER BY created_at;

-- name: ListCaptureChunks :many
-- D4: the ordered chunk inventory for a download stream. The caller reads chunks in
-- chunk_index order, decrypts each with its AAD, and streams the plaintext without
-- buffering the whole bundle. Owner authorization is enforced upstream by
-- GetCaptureForOwner before this read.
SELECT capture_id, chunk_index, length, sealed
FROM recovery_capture_chunks
WHERE capture_id = @capture_id
ORDER BY chunk_index;

-- name: GetRecoverySummaryForRun :one
-- D7: the per-run recovery aggregate, returning ONE row even when the run has zero
-- captures (aggregate query over recovery_captures = single group). `supported` is true
-- iff any custody hold ever existed for the run (a hold means a recovery-capable worker
-- claimed it; its absence is the legacy/unsupported case M5 renders honestly).
-- `has_open_hold` is the pending-custody signal. The per-state counts let M5 render the
-- section without gating on captures.length>=1. The EXISTS results are cast to boolean so
-- sqlc types them as usable bools (an uncast EXISTS types as interface{}).
SELECT
    (EXISTS (SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = @run_id AND h.user_id = @user_id))::boolean AS supported,
    (EXISTS (SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = @run_id AND h.user_id = @user_id AND h.state = 'open'))::boolean AS has_open_hold,
    count(c.id) AS capture_count,
    count(*) FILTER (WHERE c.state = 'preparing') AS preparing_count,
    count(*) FILTER (WHERE c.state = 'uploading') AS uploading_count,
    count(*) FILTER (WHERE c.state = 'available') AS available_count,
    count(*) FILTER (WHERE c.state = 'needs_action') AS needs_action_count,
    count(*) FILTER (WHERE c.state = 'expired') AS expired_count,
    count(*) FILTER (WHERE c.state = 'discarded') AS discarded_count
FROM recovery_captures c
WHERE c.run_id = @run_id AND c.user_id = @user_id;

-- name: RunHasAvailableCapture :one
-- issue #1418: does this run (owner-scoped) have a recovery capture ready to export? The
-- needs_landing derivation's capture half; keyed on the run's OWNER, riding
-- idx_recovery_captures_run_owner (run_id, user_id).
SELECT EXISTS (
    SELECT 1 FROM recovery_captures c
    WHERE c.run_id = @run_id AND c.user_id = @user_id AND c.state = 'available'
)::boolean;

-- name: ReleaseCustodyHold :execrows
-- D3: the PER-HOLD RELEASE mechanism. Nulls both live FKs (dropping the ON DELETE RESTRICT
-- that blocks worker/run teardown), flips state to 'released' and stamps released_at, for the
-- ONE hold named by @id. Idempotent: a second call moves zero rows (the hold is no longer
-- open). Captures survive (hold_id -> capture is ON DELETE RESTRICT, and this never deletes
-- the hold). The immutable provenance columns are untouched. This is the reconciler's release:
-- it releases EXACTLY the hold ListReleasableCustodyHolds qualified, so a sibling
-- older-generation orphan hold on the same run is never collaterally released (the multi-hold
-- hazard documented on ListReleasableCustodyHolds).
--
-- PRD #1392 M1 (D3): release_evidence records WHY this release was warranted — the
-- reconciler passes the per-hold class ListReleasableCustodyHolds now computes ('publication'
-- for a completed-run backstop, 'archive' for a ready capture). CHECK-constrained to the five
-- classes (migration 00232).
UPDATE recovery_custody_holds
SET live_worker_id = NULL, live_run_id = NULL, state = 'released',
    release_evidence = @release_evidence,
    released_at = now(), updated_at = now()
WHERE id = @id AND state = 'open';

-- name: DiscardCaptureForOwner :execrows
-- D7: owner-initiated explicit discard of one capture. Deletes its byte chunks (freeing
-- storage) and marks the capture 'discarded' in ONE statement — the data-modifying del CTE
-- always runs to completion, and :execrows reports the capture UPDATE's row count (1 when
-- owned, 0 for a foreign/absent id). The capture row is retained (audit), only its bytes go.
WITH owned AS (
    SELECT rc.id AS capture_id FROM recovery_captures rc
    WHERE rc.id = @id AND rc.run_id = @run_id AND rc.user_id = @user_id
),
del AS (
    DELETE FROM recovery_capture_chunks
    WHERE capture_id IN (SELECT owned.capture_id FROM owned)
)
UPDATE recovery_captures c
SET state = 'discarded', updated_at = now()
WHERE c.id = @id AND c.run_id = @run_id AND c.user_id = @user_id;

-- name: ExpireReadyCaptures :execrows
-- D4: the retention-enforcement sweep. Moves 'available' captures past their expires_at to
-- 'expired' AND reclaims their bytes — the encrypted chunk rows of the expired captures are
-- deleted in the SAME statement, so a capture past its retention window no longer costs the
-- instance/owner byte storage. Scoped to available+expired-at-past so it NEVER touches an open
-- hold, a pending (preparing/uploading) capture, or a needs_action capture — a ready
-- artifact's TTL must not expire uncaptured custody. The capture METADATA row is kept in state
-- 'expired' (audit + an honest owner-visible state); only its bytes go.
--
-- Ordered exactly like DiscardCaptureForOwner so the whole thing is ONE atomic statement:
-- `expiring` SELECTs the target ids under one snapshot, `del` deletes their chunks, and the
-- final UPDATE flips exactly those ids to 'expired'. Because every CTE reads that same
-- snapshot, a partially-expired capture can never leave orphan chunks or a chunk-less
-- 'available' row, and :execrows reports the capture UPDATE's row count (not the chunk
-- deletes'). Custody is NEVER touched — expiry is a capture-artifact operation, never a custody
-- release (an expired capture whose hold is still open stays retained per D3).
WITH expiring AS (
    SELECT rc.id AS capture_id FROM recovery_captures rc
    WHERE rc.state = 'available' AND rc.expires_at IS NOT NULL AND rc.expires_at < @now
),
del AS (
    DELETE FROM recovery_capture_chunks
    WHERE capture_id IN (SELECT expiring.capture_id FROM expiring)
)
UPDATE recovery_captures c
SET state = 'expired', updated_at = now()
WHERE c.id IN (SELECT expiring.capture_id FROM expiring);

-- name: ExpireStalledUploads :execrows
-- PRD #1296 D3/D4: the upload-retry-window sweep — the LIVE consumer of
-- UZI_RECOVERY_UPLOAD_RETRY_WINDOW. A healthy upload advances a reserved capture from
-- 'preparing' to 'available' within seconds, so a capture still in a NON-TERMINAL upload
-- state (preparing/uploading) whose reserve time is older than the operator's window has
-- genuinely stalled. created_at is the reserve anchor (the upload never restamps it), so a
-- capture unreserved-to-ready for longer than the window is the stall this surfaces. It flips
-- the capture to 'needs_action' with a specific reason so the owner sees a retain-and-decide
-- artifact, stamping updated_at.
--
-- RETAINS THE SOURCE (D3): needs_action is a NON-RELEASING state and this query NEVER touches
-- the custody hold, so the last copy is held until capture succeeds or the owner explicitly
-- discards — ListReleasableCustodyHolds does not qualify a needs_action-only hold. Scoped to
-- the two non-terminal upload states so it NEVER touches an available/expired/discarded/
-- already-needs_action capture. The caller DISABLES this pass when the window is non-positive
-- (the sweep's >0 guard); the query itself is always time-bounded by @retry_window.
UPDATE recovery_captures
SET state = 'needs_action', reason = 'upload_retry_window_exhausted', updated_at = now()
WHERE state IN ('preparing', 'uploading')
  AND created_at < now() - @retry_window::interval;

-- name: ListReleasableCustodyHolds :many
-- PRD #1296 M4 (D3): the custody-release RECONCILER's candidate set — OPEN holds whose
-- release is now WARRANTED but was never applied (e.g. the best-effort terminal release in
-- SetState failed after a full publication, leaving the live FKs set and blocking teardown).
-- Release is warranted ONLY when the recorded disposition proves THIS hold's work is durable.
--
-- MULTI-HOLD HAZARD (why this is PER-HOLD and generation-aware): a run can carry MORE THAN
-- ONE open hold. A cross-worker re-claim after a transient worker loss opens a second hold
-- (generation 2, a different live_worker_id) while the crashed worker's generation-1 hold
-- stays open and never released. Generation 2 typically reseeds from the default branch, so
-- generation 1's committed work is NOT in generation 2's tree — the generation-1 hold
-- protects the ONLY copy of that work. So a per-hold candidate set must never qualify an
-- OLDER-generation orphan on the strength of a NEWER generation's disposition:
--   (a) the hold's run is 'completed' AND h.generation = r.claim_generation — only the
--       generation that actually completed/published published its head (full publication,
--       D3). runs.claim_generation is the LATEST generation; an older orphaned hold on the
--       same completed run has h.generation < claim_generation and is NOT releasable via this
--       path (its uncaptured committed work would otherwise be dropped); or
--   (b) a READY capture ('available') exists for THIS hold (c.hold_id = h.id) — the source
--       covered by THIS hold is durably archived, which releases THIS hold's custody (D3).
--       This is already a strict per-hold test, so a sibling hold's ready capture never
--       qualifies it.
-- It NEVER infers success from an arbitrary terminal status: a 'failed'/'cancelled'/future
-- 'partial' run with no ready capture, and a 'running'/'queued' run, are excluded (a failed
-- run retains custody for capture/discard). Returns oldest-first for stable reconcile order;
-- the reconciler releases the SPECIFIC selected hold by id (ReleaseCustodyHold), so selection
-- and release agree per-hold and a sibling hold is never collaterally released.
--
-- PRD #1392 M1 (D3): `reason` is the per-hold RELEASE-EVIDENCE class the reconciler stamps
-- when it releases this hold, so the stored evidence matches the qualifier that selected it:
-- 'publication' when the completed-run backstop (a) qualifies it (the generation that
-- published its head), else 'archive' (its source is durably captured). The CASE mirrors the
-- WHERE's two disjuncts and prefers publication when both hold; a selected hold always
-- satisfies at least one disjunct, so `reason` is never a spurious 'archive' on a hold that
-- only the completed backstop qualified.
SELECT h.*,
    CASE
        WHEN EXISTS (SELECT 1 FROM runs r
                       WHERE r.id = h.run_id
                         AND r.status = 'completed'
                         AND h.generation = r.claim_generation)
            THEN 'publication'
        ELSE 'archive'
    END::text AS reason
FROM recovery_custody_holds h
WHERE h.state = 'open'
  AND (
      EXISTS (SELECT 1 FROM runs r
                WHERE r.id = h.run_id
                  AND r.status = 'completed'
                  AND h.generation = r.claim_generation)
      OR EXISTS (SELECT 1 FROM recovery_captures c
                   WHERE c.hold_id = h.id AND c.state = 'available')
  )
ORDER BY h.created_at ASC;

-- name: CountOpenCustodyHoldsForWorker :one
-- PRD #1296 M4 (D3): the DeleteWorker custody guard's predicate — how many OPEN custody
-- holds name this worker as their LIVE holder. Non-zero refuses the delete (deleting the
-- worker cascades hosted_worker_tokens, and a preauthorized upload may still need that
-- token to authenticate), so the caller surfaces the count of affected captures and
-- requires an explicit discard decision before the source's last authenticated retry path
-- is destroyed. Reads the same live_worker_id FK the M4 teardown DELETE skip predicate does.
-- OWNER-SCOPED (@user_id): the hold carries user_id, and DeleteWorker runs this BEFORE the
-- owner-scoped DeleteWorkerForUser, so without the owner filter a FOREIGN owner's held worker
-- would return a non-zero count and leak its existence + hold count as a 409 instead of the
-- 404 every other sibling ownership check yields. A non-owner therefore counts 0 and falls
-- through to the 404 (ErrWorkerNotFound) path.
SELECT count(*) FROM recovery_custody_holds
WHERE live_worker_id = @worker_id::uuid AND user_id = @user_id AND state = 'open';

-- ════════════════════════════════════════════════════════════════════════════════════════
-- PRD #1349 M1: the ADDITIVE exact-generation and owner-disposition store contract. These
-- queries are the COMPLETE store-query layer M2 (park capture/post-clone evidence), M4
-- (server generation-safe lifecycle), M5 (owner hold API/CLI disposition) and M6 (web
-- surface + owner Slack episode alert) consume. They were introduced strictly ADDITIVELY by
-- M1 (existing queries UNTOUCHED, so M1 compiled standalone). M4 has since flipped every
-- capture-reserve/custody-release call site onto these exact-generation names and DELETED the
-- superseded generation-blind ReserveCapture / ReleaseCustodyForRunWorker queries, so those no
-- longer exist above (ReleaseCustodyHold, the reconciler's per-hold release, remains).
-- ════════════════════════════════════════════════════════════════════════════════════════

-- name: ReserveCaptureExact :one
-- PRD #1349 M1 (D1/D2): the GENERATION-EXACT reserve. Identical to ReserveCapture except the
-- hold-selection WHERE ALSO matches @generation, so a capture is bound to the ONE hold this
-- worker took at that exact claim generation (never a newer same-worker hold on the run). No
-- open hold owned by @original_worker_id AT @generation -> zero rows -> pgx.ErrNoRows -> not
-- authorized (fail closed), enforcing the D1 exact-identity invariant in-SQL. The ON CONFLICT
-- idempotency is unchanged: retrying the SAME (hold_id, idempotency_key) re-stamps updated_at
-- and returns the existing row, so a lost ACK re-reserves the same capture and a changed
-- source_sha under the same key never overwrites the frozen source. M2/M4 flip capture
-- reservation onto this in place of the generation-blind newest-hold ReserveCapture.
INSERT INTO recovery_captures
    (hold_id, run_id, user_id, original_worker_id, original_worker_identity,
     source_sha, attempted_head_sha, idempotency_key, state)
SELECT h.id, @run_id, @user_id, @original_worker_id, @original_worker_identity::text,
       @source_sha, sqlc.narg('attempted_head_sha'), @idempotency_key, 'preparing'
FROM recovery_custody_holds h
WHERE h.run_id = @run_id
  AND h.user_id = @user_id
  AND h.original_worker_id = @original_worker_id
  AND h.generation = @generation
  AND h.state = 'open'
ORDER BY h.created_at DESC
LIMIT 1
ON CONFLICT (hold_id, idempotency_key) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: ReleaseCustodyHoldExact :execrows
-- PRD #1349 M1 (D1/D2/D3): the GENERATION-EXACT worker release. Nulls both live FKs (dropping
-- the ON DELETE RESTRICT that blocks worker/run teardown), flips state to 'released' and
-- stamps released_at, for the ONE open hold on @run_id at @generation held live by
-- @worker_id. Unlike ReleaseCustodyForRunWorker (which matches EVERY open hold on the run for
-- the worker), this settles EXACTLY the named generation — so a newer same-worker generation
-- can never release an older generation whose source it did not inherit (the D2 hazard). The
-- caller must have already established this generation's durable evidence (published head,
-- available archive, or verified no-output). Idempotent: a second call moves zero rows.
-- Captures survive; the immutable provenance columns are untouched.
--
-- PRD #1392 M1 (D3): release_evidence records WHY this release was warranted. The caller
-- supplies the class: the terminal-completion release passes 'publication'; the worker
-- Release endpoint passes an allowlisted request value ('publication' or 'forge_no_output');
-- the forge pre-clone park passes 'no_adopted_source' (a generation that never adopted a
-- source has nothing to prove against the forge). CHECK-constrained to the five classes
-- (migration 00232).
UPDATE recovery_custody_holds
SET live_worker_id = NULL, live_run_id = NULL, state = 'released',
    release_evidence = @release_evidence,
    released_at = now(), updated_at = now()
WHERE run_id = @run_id
  AND generation = @generation
  AND live_worker_id = @worker_id::uuid
  AND state = 'open';

-- name: GetCustodyHoldForSettle :one
-- Issue #1582 M1: the exact hold the predecessor-settle endpoint names, scoped to its run so a
-- hold id from another run never resolves. The service checks generation / original worker /
-- state against the request and, for an already-released hold, compares the stored ancestry
-- audit identity for the idempotent acknowledgement. Read-only; the release itself is the
-- guarded single-statement ReleasePredecessorCustodyHoldByAncestry below.
SELECT * FROM recovery_custody_holds
WHERE id = @hold_id AND run_id = @run_id;

-- name: ReleasePredecessorCustodyHoldByAncestry :execrows
-- Issue #1582 M1: release ONE older-generation hold on a COMPLETED run whose work the api has
-- PROVEN (via the forge compare API, never the worker's opinion) is contained in the completed
-- branch head. Every guard is re-asserted in this one statement so a change between the proof
-- and the write moves ZERO rows (the caller then re-reads and answers state_changed):
--   * the hold: exact id + run + predecessor generation, taken by the caller worker, still open,
--     and strictly older than the successor generation;
--   * the run: still 'completed', still at the successor claim generation, still held by the
--     caller worker, on the SAME branch and the SAME completion instant (status_since) the
--     service captured before it asked the forge.
-- Stamps release_evidence='ancestry' with all six audit columns (migration 00247's CHECK
-- refuses an 'ancestry' row missing any of them). Nulls both live FKs like every release.
-- Never touches a sibling hold: the WHERE names exactly one id.
UPDATE recovery_custody_holds h
SET state = 'released',
    live_worker_id = NULL,
    live_run_id = NULL,
    release_evidence = 'ancestry',
    released_at = now(),
    updated_at = now(),
    release_pushed_sha = @pushed_sha::text,
    release_source_sha = @source_sha::text,
    release_adopted_sha = @adopted_sha::text,
    release_final_head_sha = @final_head_sha::text,
    release_successor_generation = @successor_generation::bigint,
    release_branch = @branch::text
WHERE h.id = @hold_id
  AND h.run_id = @run_id
  AND h.generation = @predecessor_generation::bigint
  AND h.original_worker_id = @worker_id::uuid
  AND h.state = 'open'
  AND h.generation < @successor_generation::bigint
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.id = h.run_id
        AND r.status = 'completed'
        AND r.claim_generation = @successor_generation::bigint
        AND r.worker_id = @worker_id::uuid
        AND r.branch = @branch::text
        AND r.status_since = @completed_since::timestamptz
  )
  -- The server-held candidate binding (issue #1582 M1 rework), re-asserted here so a
  -- capture registered under the hold DURING the proof is honoured too: when any capture
  -- exists under this hold, one of them must carry the source_sha being stamped.
  AND (
      NOT EXISTS (SELECT 1 FROM recovery_captures c WHERE c.hold_id = h.id)
      OR EXISTS (SELECT 1 FROM recovery_captures c WHERE c.hold_id = h.id AND c.source_sha = @source_sha::text)
  );

-- name: ListCaptureSourceShasForHold :many
-- Issue #1582 M1 rework: the source_sha values of every recovery capture registered under ONE
-- hold — the server-held facts the predecessor-settle request's source_sha must match (a
-- capture's source_sha is the predecessor's committed head H, recorded when the capture was
-- reserved). Empty when the hold never captured; the service then has nothing to bind to.
SELECT DISTINCT source_sha FROM recovery_captures
WHERE hold_id = @hold_id
ORDER BY source_sha;

-- name: GetSettleCompletionPermitHead :one
-- Issue #1582 M1 rework: the head of the completion permit an INTERLOCKED run's completion
-- consumed — the server-held fact the predecessor-settle request's pushed_sha must match.
-- completeRunWithPermit consumes exactly one permit for (run, the LOCKED row's
-- contract_revision, head) in the same transaction that writes 'completed', fenced to the
-- completing worker. InvalidatePriorCompletionPermits also stamps consumed_at, but only on
-- revisions BELOW the one a decision bumped to, so at the run's current revision every
-- consumed permit was consumed by a completion; the newest is the final completion's.
-- pgx.ErrNoRows when none exists (a non-interlocked run never has one).
SELECT head FROM run_completion_permits
WHERE run_id = @run_id
  AND contract_revision = @contract_revision
  AND issued_by_worker_id = @worker_id::uuid
  AND consumed_at IS NOT NULL
ORDER BY consumed_at DESC, id DESC
LIMIT 1;

-- name: ListCustodyHoldsForWorkerRun :many
-- PRD #1349 M1 (D3): the caller worker's OWN open holds on a run, for the worker-facing
-- post-clone generation-exact inventory (M2). Scoped to holds this worker ORIGINALLY took
-- (original_worker_id = @worker_id) so a cross-worker reclaim never sees the crashed worker's
-- holds. Returns the hold id + generation plus a per-hold capture summary: has_available_capture
-- (a ready archive already covers this hold's source) and capture_state (the latest capture's
-- lifecycle state, '' when the hold has no capture yet). The worker uses this to decide, per
-- generation, whether its source is already durable before it re-attempts capture/release.
SELECT
    h.id,
    h.generation,
    (EXISTS (SELECT 1 FROM recovery_captures c
        WHERE c.hold_id = h.id AND c.state = 'available'))::boolean AS has_available_capture,
    COALESCE((SELECT c.state FROM recovery_captures c
        WHERE c.hold_id = h.id
        ORDER BY c.created_at DESC, c.id DESC
        LIMIT 1), '')::text AS capture_state
FROM recovery_custody_holds h
WHERE h.run_id = @run_id
  AND h.original_worker_id = @worker_id::uuid
  AND h.state = 'open'
ORDER BY h.created_at ASC;

-- name: ListCustodyHoldsForOwner :many
-- PRD #1349 M1 (D7): the owner-scoped, bounded hold list the web Workers surface and the
-- `uzi run recovery` CLI render (M5/M6). OWNER-scoped by @user_id, with OPTIONAL run/worker/
-- state narg filters (a NULL narg disables that filter). It returns the exact hold identity
-- (id, run_id, generation), lifecycle state, timestamps, and the OPAQUE original_worker_id
-- UUID, plus a BOUNDED owner-safe worker display name LEFT-JOINed from workers.name (NULL ->
-- '' when the worker row is gone). It NEVER returns original_worker_identity (raw provenance,
-- D7). The per-hold capture summary (has_available_capture, latest capture_state) lets the
-- surface classify a hold (active protection / capture in progress / archive available /
-- needs attention) without a second query.
--
-- run_status is the hold's run's live status (M5, D6/D8), LEFT-JOINed from runs (NULL -> ''
-- when the run row is gone). It is consumed ONLY by the Go attention derivation in
-- ListHoldsForOwner to tell `active` (an OPEN hold whose run is still running/claimed —
-- healthy protection, no owner decision) from `source_only` (an OPEN hold whose run is
-- terminal and carries no capture — a source needing an owner decision). It is NOT added to
-- the frozen RecoveryCustodyHoldDTO wire shape; it never reaches the SPA/CLI JSON. Ordered
-- oldest-first for a stable list.
SELECT
    h.id,
    h.run_id,
    h.generation,
    h.state,
    h.created_at,
    h.updated_at,
    h.released_at,
    h.original_worker_id,
    COALESCE(w.name, '')::text AS worker_name,
    (EXISTS (SELECT 1 FROM recovery_captures c
        WHERE c.hold_id = h.id AND c.state = 'available'))::boolean AS has_available_capture,
    COALESCE((SELECT c.state FROM recovery_captures c
        WHERE c.hold_id = h.id
        ORDER BY c.created_at DESC, c.id DESC
        LIMIT 1), '')::text AS capture_state,
    COALESCE(r.status, '')::text AS run_status
FROM recovery_custody_holds h
LEFT JOIN workers w ON w.id = h.original_worker_id AND w.user_id = h.user_id
LEFT JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
WHERE h.user_id = @user_id
  AND (sqlc.narg('run_id')::uuid IS NULL OR h.run_id = sqlc.narg('run_id')::uuid)
  AND (sqlc.narg('worker_id')::uuid IS NULL OR h.original_worker_id = sqlc.narg('worker_id')::uuid)
  AND (sqlc.narg('state')::text IS NULL OR h.state = sqlc.narg('state')::text)
ORDER BY h.created_at ASC;

-- name: GetCustodyAggregateForOwner :one
-- PRD #1349 M1 (D6/D10): the owner-level custody aggregate the board alert and one-per-episode
-- Slack DM read (M6). open_holds is the owner's UNRESOLVED (state='open') hold count — the SAME
-- admission signal ClaimRun blocks on. blocked_runs is the count of the owner's QUEUED
-- code-publishing runs currently blocked by the custody-admission predicate: it mirrors the
-- reasonCustodyLimit predicate in workersvc/health.go — a run stays queued for custody ONLY when
-- the owner is AT/OVER the limit — so it is 0 unless open_holds >= @custody_hold_limit (and a
-- non-positive @custody_hold_limit DISABLES the gate exactly like the claim path, yielding 0).
-- The code-publishing kinds match ClaimRun's custody-hold CTE (issue/ci_fix/self_improve/prompt/
-- task/mr_rework). Both columns are cast ::bigint so sqlc types them as int64, never interface{}.
-- Every column is table-qualified and @user_id carries an explicit ::uuid cast: this is a
-- top-level SELECT with no FROM, so sqlc's param-type inference cannot pick a single relation
-- for an untyped @user_id when both recovery_custody_holds and runs expose a user_id column
-- (it reports "column reference user_id is ambiguous"). The cast types the param directly.
SELECT
    (SELECT count(*) FROM recovery_custody_holds h
        WHERE h.user_id = @user_id::uuid AND h.state = 'open')::bigint AS open_holds,
    (CASE
        WHEN @custody_hold_limit::int > 0
             AND (SELECT count(*) FROM recovery_custody_holds h2
                    WHERE h2.user_id = @user_id::uuid AND h2.state = 'open') >= @custody_hold_limit::int
        THEN (SELECT count(*) FROM runs r
                WHERE r.user_id = @user_id::uuid
                  AND r.status = 'queued'
                  AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework'))
        ELSE 0
     END)::bigint AS blocked_runs;

-- name: DiscardCustodyHoldForOwner :execrows
-- PRD #1349 M1 (D7): owner-initiated EXACT hold discard. Marks the ONE named open hold
-- 'discarded' and nulls its live worker/run FKs (dropping the ON DELETE RESTRICT so ordinary
-- teardown can proceed), leaving the immutable provenance columns intact for audit. OWNER
-- SCOPE IS VERIFIED IN THE SQL (user_id = @user_id) in ADDITION to the handler's owner-or-404
-- gate, and the run/hold identity (run_id + id) is matched, so a foreign owner or a wrong
-- run/hold id settles zero rows. state='open' makes discard terminal + idempotent: a second
-- call, or a hold already released/discarded, moves zero rows and can never revive it.
-- Captures are settled separately (DiscardNonReadyCapturesForHold); an available archive is
-- NEVER touched here (archive deletion is a distinct owner choice, D7). Sibling holds and
-- generations are left untouched.
--
-- PRD #1392 M1 (D3): release_evidence is stamped 'owner_discard' — the class recording that
-- the owner explicitly discarded this hold's custody. The caller passes it as @release_evidence;
-- CHECK-constrained to the five classes (migration 00232).
UPDATE recovery_custody_holds
SET state = 'discarded', live_worker_id = NULL, live_run_id = NULL,
    release_evidence = @release_evidence, updated_at = now()
WHERE id = @hold_id AND run_id = @run_id AND user_id = @user_id AND state = 'open';

-- name: DiscardNonReadyCapturesForHold :execrows
-- PRD #1349 M1 (D7): the capture-settlement half of an owner hold discard. For ONE hold, marks
-- its preparing/uploading/needs_action captures 'discarded' and deletes their partial byte
-- chunks, NEVER touching an 'available' (ready) capture — a ready archive survives a hold
-- discard so the owner can still export it (D7/D9). Ordered exactly like DiscardCaptureForOwner
-- so the whole thing is ONE atomic statement: `targets` SELECTs the non-ready capture ids under
-- one snapshot, `del` deletes their chunks, and the final UPDATE flips exactly those ids to
-- 'discarded'. Because every CTE reads that same snapshot, a partially-settled capture can never
-- leave orphan chunks or a chunk-less non-terminal row, and :execrows reports the capture
-- UPDATE's row count (not the chunk deletes'). The caller runs this in the SAME transaction as
-- DiscardCustodyHoldForOwner, locking hold then captures in upload order (D7).
WITH targets AS (
    SELECT rc.id AS capture_id FROM recovery_captures rc
    WHERE rc.hold_id = @hold_id
      AND rc.state IN ('preparing', 'uploading', 'needs_action')
),
del AS (
    DELETE FROM recovery_capture_chunks
    WHERE capture_id IN (SELECT targets.capture_id FROM targets)
)
UPDATE recovery_captures c
SET state = 'discarded', updated_at = now()
WHERE c.id IN (SELECT targets.capture_id FROM targets);

-- name: ClaimCustodyEpisodeNotice :one
-- PRD #1349 M1 (D10): atomically claim the one-per-episode owner Slack DM slot, mirroring
-- ClaimVaultLockNotice. Backed by the additive custody_episode_notices table (migration 00226):
-- the FIRST caller to observe a blocked-custody episode for a user INSERTs the row and gets it
-- back; a concurrent second caller conflicts on the user_id PK, DO NOTHING returns no row
-- (pgx.ErrNoRows), so N api pods send EXACTLY ONE DM per episode (at-most-once dedup, the mark
-- is set before Notify runs). ClearCustodyEpisodeNotice re-arms it when the owner drops below
-- the limit, so a later crossing can notify afresh.
INSERT INTO custody_episode_notices (user_id, notified_at)
VALUES (@user_id, now())
ON CONFLICT (user_id) DO NOTHING
RETURNING user_id;

-- name: ClearCustodyEpisodeNotice :exec
-- PRD #1349 M1 (D10): re-arm the owner custody-episode notice by dropping the mark, mirroring
-- ClearVaultLockNotice. Called when the owner falls back below the custody-admission limit
-- (the episode closes), so a later re-crossing sends a fresh DM. Idempotent: a delete of a
-- missing row is a no-op.
DELETE FROM custody_episode_notices WHERE user_id = @user_id;

-- ════════════════════════════════════════════════════════════════════════════════════════
-- PRD #1349 M6: the owner custody-episode reconciler's find-owners reads. The one-per-episode
-- owner Slack DM (slacksvc.CustodyEpisodeReconciler, D10) coalesces the blocked-custody crossing
-- by owner, so it needs (a) the owners currently AT/OVER the admission limit to notify and
-- (b) the already-notified owners who have dropped BELOW it, to re-arm (clear) their episode
-- notice for a later crossing. Both key off the SAME open-hold admission signal ClaimRun and
-- GetCustodyAggregateForOwner gate on. Added strictly ADDITIVELY (M1/M4/M5 queries UNTOUCHED).
-- ════════════════════════════════════════════════════════════════════════════════════════

-- name: ListOwnersOverCustodyLimit :many
-- PRD #1349 M6 (D10): the owners whose OPEN (unresolved) custody-hold count is AT/OVER the
-- admission limit — the crossing set the episode reconciler considers for a one-per-episode DM.
-- Grouped over the partial idx_recovery_custody_holds_owner_open index; HAVING count(*) >=
-- @custody_hold_limit is the SAME predicate ClaimRun's custody-admission clause blocks on, so a
-- notified owner is exactly one whose runs are (or can be) blocked. The reconciler then claims
-- at-most-once and reads GetCustodyAggregateForOwner for the exact DM facts, so this returns only
-- the user_id. The caller guards a non-positive @custody_hold_limit (the admission gate is then
-- disabled), so this is never called with one — a non-positive limit here would match every owner.
SELECT h.user_id
FROM recovery_custody_holds h
WHERE h.state = 'open'
GROUP BY h.user_id
HAVING count(*) >= @custody_hold_limit::int;

-- name: ListOwnersWithClearedCustodyEpisode :many
-- PRD #1349 M6 (D10): the already-notified owners whose OPEN custody-hold count has dropped
-- BELOW the admission limit — the episode has closed, so the reconciler clears their notice
-- (ClearCustodyEpisodeNotice) to re-arm a later re-crossing. A row in custody_episode_notices
-- means "already DM'd for this episode"; the correlated open-hold count mirrors the same
-- admission signal, so this returns exactly the owners whose episode should re-arm. Returns only
-- the user_id; the reconciler clears each.
SELECT n.user_id
FROM custody_episode_notices n
WHERE (SELECT count(*) FROM recovery_custody_holds h
       WHERE h.user_id = n.user_id AND h.state = 'open') < @custody_hold_limit::int;
