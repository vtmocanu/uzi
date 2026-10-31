-- M1 persistence only: no query grants custody release.
-- S2 MUST execute separate statements in this order, in one short transaction:
-- LockPrerequisiteRun (KEY SHARE), LockPrerequisiteHold (NO KEY UPDATE),
-- LockPrerequisiteCapture (UPDATE), LockPrerequisiteClosure (UPDATE).
-- Recheck eligibility and the S1 binding, then MarkPrerequisiteInvoked; commit
-- before calling the forge. A missing run/closed hold uses RefusePrerequisitePreparation
-- with archive_unavailable. Outcomes lock capture then closure. No forge call in a transaction.
-- Preparation: reserve BEFORE S1; attempts is the durable proof generation.
-- No proof caching. A before-S1 budget deferral must not reserve an attempt.

-- name: InsertPrerequisiteClosure :one
INSERT INTO recovery_prerequisite_closures
    (capture_id,hold_id,run_id,user_id,repo_id,checksum,prerequisite_shas,pin_ref,next_attempt_at)
SELECT c.id,c.hold_id,c.run_id,c.user_id,h.repo_id,c.checksum,c.prerequisite_shas,
    'refs/uzi-prereq/'||c.id::text,now()
FROM recovery_captures c JOIN recovery_custody_holds h ON h.id=c.hold_id
WHERE c.id = @capture_id AND c.state='available' AND c.manifest_bound
    AND cardinality(c.prerequisite_shas) BETWEEN 1 AND 64
    AND h.state='open' AND h.inventory_guarded AND h.run_id=c.run_id AND h.user_id=c.user_id
    AND c.original_worker_id IS NOT DISTINCT FROM h.original_worker_id
    AND c.original_worker_identity=h.original_worker_identity
ON CONFLICT (capture_id) DO NOTHING
RETURNING *;

-- name: GetPrerequisiteClosure :one
SELECT * FROM recovery_prerequisite_closures WHERE capture_id = @capture_id AND user_id = @user_id;

-- name: LockPrerequisiteRun :one
SELECT * FROM runs WHERE id = @run_id FOR KEY SHARE;

-- name: LockPrerequisiteHold :one
SELECT * FROM recovery_custody_holds WHERE id = @hold_id FOR NO KEY UPDATE;

-- name: LockPrerequisiteCapture :one
SELECT * FROM recovery_captures WHERE id = @capture_id FOR UPDATE;

-- name: LockPrerequisiteClosure :one
SELECT * FROM recovery_prerequisite_closures WHERE id = @id FOR UPDATE;

-- Lane cursors are exclusive (due timestamp,id). Start/wrap with '-infinity'
-- and the zero UUID; when a page is empty wrap once, within the caller's pass cap.
-- Every page is bounded independently; callers retain a cursor per lane.
-- name: ListPrerequisitePendingDue :many
SELECT * FROM recovery_prerequisite_closures
WHERE state='pending' AND next_attempt_at<= @due_at::timestamptz
    AND (next_attempt_at,id) > (@after_due_at::timestamptz,@after_id::uuid)
ORDER BY next_attempt_at,id LIMIT @lim::int;

-- name: ListPrerequisiteStaleInvokedDue :many
SELECT * FROM recovery_prerequisite_closures
WHERE state='invoked' AND invoked_deadline<= @due_at::timestamptz
    AND (invoked_deadline,id) > (@after_due_at::timestamptz,@after_id::uuid)
ORDER BY invoked_deadline,id LIMIT @lim::int;

-- name: ListPrerequisiteReconcileDue :many
SELECT * FROM recovery_prerequisite_closures
WHERE state='create_unknown' AND next_attempt_at<= @due_at::timestamptz
    AND (next_attempt_at,id) > (@after_due_at::timestamptz,@after_id::uuid)
ORDER BY next_attempt_at,id LIMIT @lim::int;

-- name: ListPrerequisiteCleanupDue :many
SELECT * FROM recovery_prerequisite_closures
WHERE state='pinned' AND EXISTS (
    SELECT 1 FROM recovery_captures c
    WHERE c.id=recovery_prerequisite_closures.capture_id AND c.state IN ('expired','discarded')
) AND next_attempt_at<= @due_at::timestamptz
    AND (next_attempt_at,id) > (@after_due_at::timestamptz,@after_id::uuid)
ORDER BY next_attempt_at,id LIMIT @lim::int;

-- name: ListPrerequisiteAuditDue :many
SELECT * FROM recovery_prerequisite_closures
WHERE state='pinned' AND audit_next_at<= @due_at::timestamptz
    AND (audit_next_at,id) > (@after_due_at::timestamptz,@after_id::uuid)
ORDER BY audit_next_at,id LIMIT @lim::int;

-- name: CountLivePrerequisiteClosuresForRun :one
SELECT count(*) FROM recovery_prerequisite_closures p
JOIN runs r ON r.id=p.live_run_id JOIN repos repo ON repo.id=r.repo_id
JOIN forge_connections f ON f.id=repo.connection_id
WHERE p.live_run_id = @run_id AND p.user_id = @user_id AND f.user_id = @user_id;

-- name: CountLivePrerequisiteClosuresForRepo :one
SELECT count(*) FROM recovery_prerequisite_closures p
JOIN runs r ON r.id=p.live_run_id JOIN repos repo ON repo.id=r.repo_id
JOIN forge_connections f ON f.id=repo.connection_id
WHERE p.live_run_id IS NOT NULL AND p.repo_id = @repo_id AND p.user_id = @user_id AND f.user_id = @user_id;

-- name: CountLivePrerequisiteClosuresForConnection :one
SELECT count(*) FROM recovery_prerequisite_closures p
JOIN runs r ON r.id=p.live_run_id JOIN repos repo ON repo.id=r.repo_id
JOIN forge_connections f ON f.id=repo.connection_id
WHERE f.id = @connection_id AND p.user_id = @user_id AND f.user_id = @user_id;

-- name: ReservePrerequisitePreparation :one
UPDATE recovery_prerequisite_closures
SET attempts=attempts+1, attempt_deadline=now()+make_interval(secs => @lease_seconds::float8),
    next_attempt_at=now()+make_interval(secs => @lease_seconds::float8)
        +make_interval(secs => LEAST(3600,30*(1<<attempts))),
    updated_at=now()
WHERE id = @id AND state='pending' AND attempts<8 AND next_attempt_at<=now()
    AND (attempt_deadline IS NULL OR attempt_deadline<=now())
    AND @lease_seconds::float8>0
RETURNING *;

-- name: RecordPrerequisitePreparationRetry :execrows
-- Incomplete ancestry or positive proofs leaving insufficient send budget.
-- The reservation already persisted positive backoff. Attempt 8 is terminal.
UPDATE recovery_prerequisite_closures
SET state=CASE WHEN attempts=8 THEN 'refused' ELSE 'pending' END,
    reason=CASE WHEN @budget_shortfall::boolean THEN
        CASE WHEN attempts=8 THEN 'prerequisite_prepare_budget_exhausted' ELSE '' END
        ELSE 'prerequisite_ancestry_unknown' END,
    last_error=left(@last_error::text,512), attempt_deadline=NULL, updated_at=now()
WHERE id = @id AND state='pending' AND attempts = @generation::int
    AND attempt_deadline>now();

-- name: RefusePrerequisitePreparation :execrows
UPDATE recovery_prerequisite_closures SET state='refused',reason = @reason::text,updated_at=now()
WHERE id = @id AND state='pending' AND attempts = @generation::int AND attempt_deadline>now()
    AND @reason::text IN ('closure_unsupported','prerequisite_not_on_default','archive_unavailable','pin_foreign');

-- name: ExpirePrerequisitePreparation :execrows
-- Dedicated interrupted-attempt-eight CAS: deliberately NO valid-deadline/attempts<8 predicate.
UPDATE recovery_prerequisite_closures
SET state='refused',reason='prerequisite_ancestry_unknown',updated_at=now()
WHERE id = @id AND state='pending' AND attempts=8 AND attempts = @generation::int AND attempt_deadline<=now();

-- name: MarkPrerequisiteInvoked :one
-- Caller has completed the four separate ordered locks above; trigger rechecks binding/eligibility.
UPDATE recovery_prerequisite_closures p
SET state='invoked',live_run_id=p.run_id,invoked_at=now(),
    invoked_deadline=now()+make_interval(secs => @send_seconds::float8),
    pin_tip = @pin_tip,connection_id = @connection_id,forge_kind = @forge_kind,
    normalized_base_url = @normalized_base_url,project_id = @project_id,default_branch = @default_branch,
    reason='',updated_at=now()
WHERE p.id = @id AND p.state='pending' AND p.attempts = @generation::int
    AND p.attempt_deadline>now() AND @send_seconds::float8>0
    AND EXISTS (SELECT 1 FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id
      JOIN recovery_captures c ON c.hold_id=h.id JOIN repos repo ON repo.id=r.repo_id
      JOIN forge_connections f ON f.id=repo.connection_id
      WHERE r.id=p.run_id AND r.user_id=p.user_id AND h.id=p.hold_id AND h.state='open'
        AND h.inventory_guarded AND c.id=p.capture_id AND c.state='available' AND c.manifest_bound
        AND c.checksum=p.checksum AND c.prerequisite_shas=p.prerequisite_shas
        AND repo.id=p.repo_id AND f.user_id=p.user_id
        AND f.id = @connection_id AND f.forge_type = @forge_kind
        AND rtrim(f.base_url,'/') = @normalized_base_url AND repo.forge_project_id = @project_id
        AND repo.default_branch = @default_branch)
RETURNING p.*;

-- name: MarkPrerequisiteCreateUnknown :execrows
UPDATE recovery_prerequisite_closures
SET state='create_unknown',reason='pin_unknown',next_attempt_at=now()+interval '30 seconds',updated_at=now()
WHERE id = @id AND state='invoked' AND attempts = @generation::int;

-- name: SettleStalePrerequisiteInvocation :execrows
UPDATE recovery_prerequisite_closures
SET state='create_unknown',reason='pin_unknown',next_attempt_at=now(),updated_at=now()
WHERE id = @id AND state='invoked' AND invoked_deadline<=now();

-- name: RecordPrerequisitePinned :execrows
UPDATE recovery_prerequisite_closures
SET state='pinned',reason='',audit_next_at=now()+interval '24 hours',next_attempt_at=now(),updated_at=now()
WHERE id = @id AND state IN ('invoked','create_unknown') AND attempts = @generation::int;

-- name: RecordPrerequisiteCreateRefused :execrows
-- Only a definitive first-send refusal with absent readback; unknown never becomes refused.
UPDATE recovery_prerequisite_closures
SET state='refused',live_run_id=NULL,reason='pin_refused',updated_at=now()
WHERE id = @id AND state='invoked' AND attempts = @generation::int;

-- name: RecordPrerequisiteForeign :execrows
UPDATE recovery_prerequisite_closures
SET state='foreign',live_run_id=NULL,reason='pin_foreign',cleanup_exhausted_at=NULL,absent_since=NULL,absence_pass_id=NULL,updated_at=now()
WHERE id = @id AND state IN ('invoked','create_unknown') AND attempts = @generation::int;

-- name: RecordPrerequisitePinnedForeign :execrows
-- A pinned listing is fenced independently from preparation. It can finish a
-- cleanup listing while its lease is live, or an audit after that lease expired.
UPDATE recovery_prerequisite_closures
SET state='foreign',live_run_id=NULL,reason='pin_foreign',cleanup_exhausted_at=NULL,
    absent_since=NULL,absence_pass_id=NULL,updated_at=now()
WHERE id = @id AND state='pinned' AND cleanup_attempts = @generation::int
    AND last_audit_pass_id = @pass_id;

-- name: DeferPrerequisiteReconciliation :execrows
UPDATE recovery_prerequisite_closures
SET next_attempt_at=now()+make_interval(secs => LEAST(3600,30*(1<<reconciliation_attempts))),
    reconciliation_attempts=LEAST(7,reconciliation_attempts+1),
    last_error=left(@last_error::text,512),updated_at=now()
WHERE id = @id AND state='create_unknown' AND attempts = @generation::int
    AND reconciliation_attempts = @reconciliation_generation::int AND next_attempt_at<=now();

-- name: ReservePrerequisiteAudit :one
-- A cleanup listing is also reservable when its own schedule is due, including
-- after delete exhaustion. Reserving a listing never authorizes a delete.
UPDATE recovery_prerequisite_closures p
SET last_audit_pass_id = @pass_id,audit_next_at=now()+interval '24 hours',updated_at=now()
WHERE p.id = @id AND p.state='pinned'
    AND (p.cleanup_deadline IS NULL OR p.cleanup_deadline<=now())
    AND (p.audit_next_at<=now() OR (p.next_attempt_at<=now() AND EXISTS (
        SELECT 1 FROM recovery_captures c WHERE c.id=p.capture_id AND c.state IN ('expired','discarded'))))
    AND p.last_audit_pass_id IS DISTINCT FROM @pass_id AND @pass_id::uuid IS NOT NULL
RETURNING p.*;

-- name: RecordPrerequisiteAbsence :execrows
-- FINAL must reserve a listing pass before the external listing too.
-- All pinned listings carry both the audit reservation and cleanup generation.
UPDATE recovery_prerequisite_closures
SET absent_since=now(),absence_pass_id = @pass_id,last_audit_pass_id = @pass_id,
    audit_next_at=now()+interval '10 minutes',reason='pin_absence_unconfirmed',updated_at=now()
WHERE id = @id AND state='pinned' AND absent_since IS NULL
    AND last_audit_pass_id = @pass_id AND cleanup_attempts = @generation::int
    AND (cleanup_deadline IS NULL OR cleanup_deadline<=now());

-- name: ClearPrerequisiteAbsence :execrows
UPDATE recovery_prerequisite_closures
SET absent_since=NULL,absence_pass_id=NULL,last_audit_pass_id = @pass_id,
    audit_next_at=now()+interval '24 hours',
    reason=CASE WHEN cleanup_exhausted_at IS NULL THEN '' ELSE 'pin_cleanup_exhausted' END,updated_at=now()
WHERE id = @id AND state='pinned' AND absent_since IS NOT NULL AND absence_pass_id <> @pass_id AND last_audit_pass_id = @pass_id
    AND cleanup_attempts = @generation::int AND (cleanup_deadline IS NULL OR cleanup_deadline<=now());

-- name: ConfirmPrerequisiteLost :execrows
UPDATE recovery_prerequisite_closures
SET state='lost',live_run_id=NULL,cleanup_exhausted_at=NULL,reason='pin_lost',absent_since=NULL,absence_pass_id=NULL,
    last_audit_pass_id = @pass_id,updated_at=now()
WHERE id = @id AND state='pinned' AND absent_since+interval '10 minutes'<=now() AND absence_pass_id <> @pass_id AND last_audit_pass_id = @pass_id
    AND cleanup_attempts = @generation::int AND (cleanup_deadline IS NULL OR cleanup_deadline<=now());

-- name: ReservePrerequisiteCleanup :one
-- Durable generation BEFORE a delete; cleanup exhaustion retains the live pointer for listing only.
UPDATE recovery_prerequisite_closures p
SET last_audit_pass_id = @pass_id,cleanup_attempts=cleanup_attempts+1,cleanup_deadline=now()+make_interval(secs => @lease_seconds::float8),
    next_attempt_at=now()+make_interval(secs => @lease_seconds::float8)
        +make_interval(secs => LEAST(3600,30*(1<<cleanup_attempts))),updated_at=now()
WHERE p.id = @id AND p.state='pinned' AND p.cleanup_attempts<8 AND p.cleanup_exhausted_at IS NULL
    AND p.next_attempt_at<=now() AND (p.cleanup_deadline IS NULL OR p.cleanup_deadline<=now())
    AND @lease_seconds::float8>0 AND @pass_id::uuid IS NOT NULL
    AND EXISTS (SELECT 1 FROM recovery_captures c WHERE c.id=p.capture_id AND c.state IN ('expired','discarded'))
RETURNING p.*;

-- name: RecordPrerequisiteDeleted :execrows
UPDATE recovery_prerequisite_closures p
SET state='deleted',live_run_id=NULL,reason='',absent_since=NULL,absence_pass_id=NULL,
    cleanup_exhausted_at=NULL,updated_at=now()
WHERE p.id = @id AND p.state='pinned' AND p.cleanup_attempts = @generation::int
    AND p.cleanup_attempts>0 AND p.cleanup_deadline>now() AND p.last_audit_pass_id = @pass_id
    AND EXISTS (SELECT 1 FROM recovery_captures c WHERE c.id=p.capture_id AND c.state IN ('expired','discarded'));

-- name: RecordPrerequisiteCleanupRetry :execrows
UPDATE recovery_prerequisite_closures
SET last_error=left(@last_error::text,512),cleanup_deadline=NULL,
    cleanup_exhausted_at=CASE WHEN cleanup_attempts=8 THEN now() ELSE NULL END,
    reason=CASE WHEN cleanup_attempts=8 THEN 'pin_cleanup_exhausted' ELSE reason END,updated_at=now()
WHERE id = @id AND state='pinned' AND cleanup_attempts = @generation::int
    AND cleanup_attempts>0 AND cleanup_deadline>now() AND last_audit_pass_id = @pass_id;

-- name: ExpirePrerequisiteCleanup :execrows
UPDATE recovery_prerequisite_closures
SET cleanup_exhausted_at=now(),reason='pin_cleanup_exhausted',updated_at=now()
WHERE id = @id AND state='pinned' AND cleanup_attempts=8 AND cleanup_attempts = @generation::int
    AND cleanup_deadline<=now() AND cleanup_exhausted_at IS NULL AND last_audit_pass_id = @pass_id;

-- name: ConfirmPrerequisiteCleanupAbsent :execrows
-- A later listing after an ambiguous delete can settle even an exhausted row,
-- without authorizing another delete. The audit pass and cleanup generation fence it.
UPDATE recovery_prerequisite_closures p
SET state='deleted',live_run_id=NULL,reason='',absent_since=NULL,absence_pass_id=NULL,
    cleanup_exhausted_at=NULL,updated_at=now()
WHERE p.id = @id AND p.state='pinned' AND p.cleanup_attempts = @generation::int
    AND p.last_audit_pass_id = @pass_id::uuid
    AND (p.cleanup_deadline IS NULL OR p.cleanup_deadline<=now())
    AND EXISTS (SELECT 1 FROM recovery_captures c WHERE c.id=p.capture_id AND c.state IN ('expired','discarded'));

-- name: DeferPrerequisiteCleanupListing :execrows
-- Failed/positive listing without a delete (including exhausted rows): bounded
-- backoff, without resetting or spending a delete attempt.
UPDATE recovery_prerequisite_closures p
SET next_attempt_at=CASE WHEN EXISTS (SELECT 1 FROM recovery_captures c
        WHERE c.id=p.capture_id AND c.state IN ('expired','discarded'))
        THEN now()+make_interval(secs => LEAST(3600,30*(1<<cleanup_attempts))) ELSE p.next_attempt_at END,
    audit_next_at=CASE WHEN p.absent_since IS NOT NULL THEN now()+interval '10 minutes'
        ELSE p.audit_next_at END,
    last_error=left(@last_error::text,512),updated_at=now()
WHERE p.id = @id AND p.state='pinned' AND p.cleanup_attempts = @generation::int
    AND p.last_audit_pass_id = @pass_id
    AND (p.cleanup_deadline IS NULL OR p.cleanup_deadline<=now());

-- name: DeferPrerequisitePreparation :execrows
-- Before S1 only: no slot/full-item budget means no attempt and no terminal refusal.
UPDATE recovery_prerequisite_closures
SET next_attempt_at=now()+interval '30 seconds',updated_at=now()
WHERE id = @id AND state='pending' AND attempts = @generation::int AND attempts<8
    AND next_attempt_at<=now() AND (attempt_deadline IS NULL OR attempt_deadline<=now());
