-- Credential disablement parks an UNDELIVERED claim (status 'claimed', exact worker and
-- generation) without consuming any pending owner pause or changing its session, phase,
-- custody, or recovery state. A running flight is never parked (D3): its claim finishes.
--
-- The payload was never handed to the worker, so there is no flight to fence: the D19
-- released-incarnation pair (released_worker_id / released_worker_nonce) and
-- credential_disable_released_worker_id are deliberately NOT written. Writing them would
-- bar the parking worker's own live incarnation from ever reclaiming the run after
-- promotion (a single-worker deployment would strand it queued). worker_id is kept as
-- resume affinity, exactly like RequeueClaimAssemblyExact; the claim capability is revoked
-- (codex_cap_hash NULL, epoch + 1) and claim_released_at rejects any report at this
-- generation until the next ClaimRun clears it.
--
-- hold_reason is overwritten whatever it was. The only annotation a 'claimed' row can carry
-- is 'completion_blocked' from a completion decision that already resumed the run
-- (ResumePausedRun keeps it until SetRunRunning): the owner's decision is already recorded
-- (its follow_up input and audit row are durable), so the credential is now the actionable
-- reason. hold_captured_head is kept for the later SetRunRunning clear. Guarding on
-- hold_reason IS NULL instead would match 0 rows after the custody release, roll the
-- finisher back and leak this claim's custody hold on every ClaimGrace requeue.
-- name: ParkCredentialDisabledRun :execrows
UPDATE runs SET
    status = 'paused',
    status_since = now(),
    hold_reason = 'credential_disabled',
    claim_released_at = now(),
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.claim_generation = @claim_generation
  AND runs.claim_released_at IS NULL
  AND runs.status = 'claimed';

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
    -- Resume affinity is kept: the undelivered-claim park fences no incarnation. Only a row
    -- that carries a D19 released incarnation drops its worker, as ResumePausedRun does.
    worker_id = CASE WHEN released_worker_id IS NOT NULL THEN NULL ELSE worker_id END,
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'paused' AND hold_reason = 'credential_disabled'
  AND pause_requested_at IS NULL
  -- The requirement the promoter evaluated must still be the run's requirement: a
  -- concurrent reassignment or rebind that is not serialized by the caller's locks
  -- makes this match 0 rows instead of promoting on a stale requirement.
  AND credential_override_mode IS NOT DISTINCT FROM sqlc.narg('expected_override_mode')::text
  AND credential_override_secret_id IS NOT DISTINCT FROM sqlc.narg('expected_override_secret_id')::uuid
  AND codex_secret_id IS NOT DISTINCT FROM sqlc.narg('expected_codex_secret_id')::uuid
  AND worker_id IS NOT DISTINCT FROM sqlc.narg('expected_worker_id')::uuid
  AND credential_disable_released_worker_id IS NOT DISTINCT FROM sqlc.narg('expected_released_worker_id')::uuid
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
    -- Resume affinity is kept: the undelivered-claim park fences no incarnation. Only a row
    -- that carries a D19 released incarnation drops its worker, as ResumePausedRun does.
    worker_id = CASE WHEN released_worker_id IS NOT NULL THEN NULL ELSE worker_id END,
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

-- The claim finisher's in-transaction enablement re-check of the credential the payload
-- resolved, taken under the exact-claim run lock and after the Codex alias/account locks.
-- FOR SHARE blocks a concurrent disable (which locks the row FOR UPDATE under the user's
-- secret mutation lock) until the claim decision commits; NOWAIT means the finisher never
-- waits on that writer: a held row returns 55P03 and finishRunClaim retries the whole
-- transaction. Owner-scoped; a missing row is pgx.ErrNoRows.
-- name: LockSecretEnablementForShareNowait :one
SELECT (disabled_at IS NOT NULL)::boolean AS disabled
FROM user_secrets
WHERE id = @id AND user_id = @user_id
FOR SHARE NOWAIT;

-- Sweep fallback worklist: one keyset page of owners with at least one run held on
-- credential_disabled. The partial promoter index serves the DISTINCT scan.
-- name: ListCredentialDisabledUsers :many
SELECT DISTINCT user_id FROM runs
WHERE status = 'paused' AND hold_reason = 'credential_disabled'
  AND (sqlc.narg('after_user_id')::uuid IS NULL OR user_id > sqlc.narg('after_user_id')::uuid)
ORDER BY user_id
LIMIT @page_size::int;

-- Promoter step (2): the candidate run row, locked AFTER the user's secret mutation
-- advisory lock and BEFORE any requirement source or credential row.
-- name: LockCredentialDisabledRunForPromotion :one
SELECT id, user_id, kind, harness, status, hold_reason, pause_requested_at,
       credential_override_mode, credential_override_secret_id, codex_secret_id,
       worker_id, credential_disable_released_worker_id
FROM runs
WHERE id = @id AND user_id = @user_id
FOR UPDATE;

-- Promoter step (3a): the recorded worker's Anthropic binding, the requirement source for an
-- ordinary Claude run without a per-run override. Owner-scoped. holds_affinity mirrors
-- ClaimRun's affinity pin on a queued run's own worker: the row exists and is draining or
-- heartbeat-fresh at @heartbeat_cutoff (now - WORKER_HEARTBEAT_STALE). A worker that does
-- not hold affinity will not be the next claimant, so its binding is not the requirement.
-- name: LockWorkerBindingForShare :one
SELECT anthropic_bind_mode, anthropic_secret_id,
       (draining_since IS NOT NULL
        OR (last_heartbeat_at IS NOT NULL AND last_heartbeat_at >= @heartbeat_cutoff::timestamptz))::boolean
           AS holds_affinity
FROM workers
WHERE id = @id AND user_id = @user_id
FOR SHARE;

-- Promoter step (3b): the owner's Judge binding, the requirement source for judge and
-- self_improve runs.
-- name: LockUserJudgeBindingForShare :one
SELECT judge_anthropic_bind_mode, judge_anthropic_secret_id
FROM users
WHERE id = @id
FOR SHARE;

-- Promoter step (4): the exact requirement credential. Owner-scoped.
-- name: LockSecretForPromotion :one
SELECT (disabled_at IS NULL)::boolean AS enabled
FROM user_secrets
WHERE id = @id AND user_id = @user_id
FOR SHARE;

-- Promoter step (4) for a default requirement: the owner's current Anthropic default.
-- pgx.ErrNoRows means the slot is empty (D4: every default is enabled, and the last
-- disable clears the slot).
-- name: LockDefaultAnthropicSecretForShare :one
SELECT id, (disabled_at IS NULL)::boolean AS enabled
FROM user_secrets
WHERE user_id = @user_id AND kind = 'anthropic_token' AND is_default
FOR SHARE;

-- Promoter step (4) for a Judge/self-improve auto lane: an enabled pooled token or an
-- enabled default can serve it (the #1140 fallback). Read under the user's secret
-- mutation lock, which every enablement and default writer takes.
-- name: HasEnabledAnthropicForJudgeAuto :one
SELECT EXISTS (
    SELECT 1 FROM user_secrets
    WHERE user_id = @user_id AND kind = 'anthropic_token' AND disabled_at IS NULL
      AND (is_default OR auto_eligible)
)::boolean AS available;

-- Promoter step (4) for an ordinary auto lane (a worker bound auto, or a per-run auto
-- override): the lane spends only pooled tokens (secretchoice's pooled-only promise), so it
-- can serve the run only when an ENABLED auto-eligible token exists. The default is not a
-- fallback here (that is the Judge/self-improve rule above). Read under the user's secret
-- mutation lock, which every enablement and pool opt-in writer takes.
-- name: HasEnabledPooledAnthropic :one
SELECT EXISTS (
    SELECT 1 FROM user_secrets
    WHERE user_id = @user_id AND kind = 'anthropic_token' AND disabled_at IS NULL
      AND auto_eligible
)::boolean AS available;
