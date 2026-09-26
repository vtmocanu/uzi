-- Credential disablement parks an existing worker flight without consuming any
-- pending owner pause or changing its session, phase, custody, or recovery state.
-- name: ParkCredentialDisabledRun :execrows
UPDATE runs SET
    status = 'paused',
    status_since = now(),
    hold_reason = 'credential_disabled',
    claim_released_at = now(),
    released_worker_id = runs.worker_id,
    credential_disable_released_worker_id = runs.worker_id,
    released_worker_nonce = CASE WHEN runs.claimed_worker_nonce IS NOT NULL
        THEN NULLIF(runs.claimed_worker_nonce, '')
        ELSE (SELECT w.snapshot_register_nonce FROM workers w WHERE w.id = runs.worker_id) END,
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.claim_generation = @claim_generation
  AND runs.claim_released_at IS NULL
  AND runs.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND runs.hold_reason IS NULL;

-- The M1 partial index narrows this owner-scoped page to held runs. The caller
-- re-evaluates the current credential requirement before promoting each row.
-- name: ListCredentialDisabledRuns :many
SELECT id, user_id, status_since FROM runs
WHERE user_id = @user_id AND status = 'paused' AND hold_reason = 'credential_disabled'
  AND (sqlc.narg('after_status_since')::timestamptz IS NULL
       OR (status_since, id) > (sqlc.narg('after_status_since')::timestamptz,
                                sqlc.narg('after_id')::uuid))
ORDER BY status_since ASC, id ASC
LIMIT @page_size::int;

-- Promotion banks only the parked interval. The active time already spent before
-- parking must still fit the frozen budget; the hold interval is excluded.
-- name: PromoteCredentialDisabledRun :one
UPDATE runs SET
    status = 'queued',
    status_since = now(),
    budget_paused_seconds = budget_paused_seconds
        + GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int),
    hold_reason = NULL,
    worker_id = CASE WHEN claim_released_at IS NOT NULL THEN NULL ELSE worker_id END,
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'paused' AND hold_reason = 'credential_disabled'
  AND pause_requested_at IS NULL
  AND (started_at IS NULL OR
       (COALESCE(budget_wall_seconds, @global_timeout_seconds::int)
          + budget_extension_seconds + budget_finalize_seconds)
       - (GREATEST(0, EXTRACT(EPOCH FROM (status_since - started_at))::int)
          - budget_paused_seconds) > 0)
RETURNING id, user_id, status;

-- An owner reassignment of an existing credential hold is all-or-nothing. A
-- refused budget/state guard must not leave a new override behind after a 409.
-- name: ReassignCredentialDisabledRun :one
UPDATE runs SET
    credential_override_mode = @mode,
    credential_override_secret_id = @secret_id,
    status = 'queued',
    status_since = now(),
    budget_paused_seconds = budget_paused_seconds
        + GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int),
    hold_reason = NULL,
    worker_id = CASE WHEN claim_released_at IS NOT NULL THEN NULL ELSE worker_id END,
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'paused' AND hold_reason = 'credential_disabled'
  AND pause_requested_at IS NULL
  AND (started_at IS NULL OR
       (COALESCE(budget_wall_seconds, @global_timeout_seconds::int)
          + budget_extension_seconds + budget_finalize_seconds)
       - (GREATEST(0, EXTRACT(EPOCH FROM (status_since - started_at))::int)
          - budget_paused_seconds) > 0)
RETURNING id, user_id, status;
