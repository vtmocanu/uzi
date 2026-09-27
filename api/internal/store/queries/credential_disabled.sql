-- Credential disablement parks an UNDELIVERED claim (status 'claimed', exact worker and
-- generation) without consuming any pending owner pause or changing its session, phase,
-- custody, or recovery state. A running flight is never parked (D3): its claim finishes.
--
-- The payload was never handed to the worker, so there is no flight to fence: the D19
-- released-incarnation pair (released_worker_id / released_worker_nonce) is deliberately
-- NOT written. Writing it would bar the parking worker's own live incarnation from ever
-- reclaiming the run after promotion (a single-worker deployment would strand it queued). worker_id is kept as
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

-- The chat lane's twin of ParkCredentialDisabledRun (PRD #1732 D2/D14). ClaimChatRun has no
-- claim generation, capability or custody hold, so the fence is the claimed chat row this
-- worker just claimed. Chat is not bindable: it parks only when the owner's default resolves to
-- a disabled row or every Anthropic token is disabled (no default), and the promoter queues it
-- again once the slot has an enabled default. worker_id is kept as resume affinity.
-- name: ParkCredentialDisabledChatRun :execrows
UPDATE runs SET
    status = 'paused',
    status_since = now(),
    hold_reason = 'credential_disabled',
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.kind = 'chat'
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
  -- Untimed runs (chat, judge, interactive) have no wall (RequestWallParks' own exclusions),
  -- so the spent-budget guard applies only to a timed run.
  AND (kind IN ('chat', 'judge') OR interactive OR started_at IS NULL OR
       (COALESCE(budget_wall_seconds, @global_timeout_seconds::int)
          + budget_extension_seconds + budget_finalize_seconds)
       - (GREATEST(0, EXTRACT(EPOCH FROM (status_since - started_at))::int)
          - budget_paused_seconds) > 0)
RETURNING id, user_id, status;

-- An owner reassignment of an existing credential hold that can go straight back to the queue
-- (no pending pause, budget left): the override and the promotion in one statement. The caller
-- holds the user lock and the run row. On a held row this matches 0 rows only for a pending
-- pause or a spent budget; the caller then writes the override and settles the hold into that
-- pause or budget_exhausted, as the promoter does. The whole reassignment is one transaction,
-- so a refusal leaves no override behind.
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
  -- Untimed runs (chat, judge, interactive) have no wall (RequestWallParks' own exclusions),
  -- so the spent-budget guard applies only to a timed run.
  AND (kind IN ('chat', 'judge') OR interactive OR started_at IS NULL OR
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
SELECT id, user_id, kind, harness, status, hold_reason, pause_requested_at, pause_mode,
       credential_override_mode, credential_override_secret_id, codex_secret_id, worker_id
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

-- A held run that also carries a PENDING pause request converges into that pause once its
-- credential requirement is met (PRD #1732 D14: the promoter never bypasses an owner pause).
-- A pending request survives the involuntary parks by design (SetRunPaused's comment), so a
-- run whose worker died after CreatePauseInput is requeued, reclaimed and parked here still
-- carrying it. Promoting it to queued would run it past the pause the owner asked for, and
-- leaving it held would strand it: ResumePausedRun refuses this hold and CancelPauseInput
-- only matches a running row. So the request is CONSUMED exactly as the park it asked for
-- would consume it: an owner 'milestone'/'now' request lands in the ordinary owner pause
-- (hold_reason NULL, resumed by the owner through ResumePausedRun); a system 'wall' request
-- lands in the budget_exhausted hold the wall park writes (resumed by extend). The status
-- stays 'paused', so status_since is kept: the resume that later leaves the pause banks the
-- whole parked interval, credential hold included, exactly once. The pause's unapplied
-- steering inputs are settled so a resumed flight is never handed a stale pause, and the
-- same requirement guards as the promotion apply.
--
-- Both arms clear claim_released_at, so the owner's later ResumePausedRun (owner arm) or
-- ExtendAndResumeWallPark (wall arm) keeps worker_id as resume affinity exactly as it does for
-- an ordinary owner pause (SetRunPaused never sets it) or a worker-side wall park (SetRunWallPark
-- never sets it either); D14 keeps the existing affinity rules. Both resumes read a set
-- claim_released_at as a server park and drop the worker, which would be wrong here: the
-- undelivered claim ParkCredentialDisabledRun fenced was never handed to a worker and its
-- capability is already revoked (codex_cap_hash NULL); the next ClaimRun bumps
-- claim_generation, and a paused row takes no running report.
-- The caller serializes this with every requirement writer (user lock, then run row).
-- name: SettleCredentialDisabledPause :one
WITH settled_inputs AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    WHERE u.run_id = @id AND u.kind = 'pause' AND u.applied_at IS NULL
      AND EXISTS (SELECT 1 FROM runs r WHERE r.id = @id AND r.user_id = @user_id
                  AND r.status = 'paused' AND r.hold_reason = 'credential_disabled'
                  AND r.pause_requested_at IS NOT NULL)
)
UPDATE runs SET
    hold_reason = CASE WHEN runs.pause_mode = 'wall' THEN 'budget_exhausted' ELSE NULL END,
    claim_released_at = NULL,
    pause_requested_at = NULL,
    pause_mode = NULL,
    pause_after_count = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status = 'paused' AND runs.hold_reason = 'credential_disabled'
  AND runs.pause_requested_at IS NOT NULL
  AND runs.credential_override_mode IS NOT DISTINCT FROM sqlc.narg('expected_override_mode')::text
  AND runs.credential_override_secret_id IS NOT DISTINCT FROM sqlc.narg('expected_override_secret_id')::uuid
  AND runs.codex_secret_id IS NOT DISTINCT FROM sqlc.narg('expected_codex_secret_id')::uuid
  AND runs.worker_id IS NOT DISTINCT FROM sqlc.narg('expected_worker_id')::uuid
RETURNING runs.id, runs.status, runs.hold_reason;

-- A held TIMED run whose requirement is met but whose active budget is already spent (no
-- pending pause: SettleCredentialDisabledPause owns that case) cannot be promoted, since
-- PromoteCredentialDisabledRun's budget guard refuses it, and must not stay on
-- credential_disabled with nothing left to wait for. It settles into the budget_exhausted hold
-- the wall park writes, so the owner's Extend (ExtendAndResumeWallPark) resumes it, the same
-- place SettleCredentialDisabledPause's wall arm lands (PRD #1732 D14: the promoter never
-- bypasses budget exhaustion). The status stays 'paused' and status_since is kept, so Extend
-- measures the active time at the park and banks the whole parked interval. claim_released_at
-- is cleared, like SettleCredentialDisabledPause's arms, so Extend keeps worker_id as resume
-- affinity (the fenced claim was never delivered). A hold_captured_head a resumed
-- completion hold left behind is not a wall capture, so it is cleared, as ParkRunsAtWall does.
-- The exemptions and the budget expression are exactly PromoteCredentialDisabledRun's, negated,
-- so the two never both match. The same requirement guards apply.
-- name: SettleCredentialDisabledSpentBudget :one
UPDATE runs SET
    hold_reason = 'budget_exhausted',
    hold_captured_head = NULL,
    claim_released_at = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'paused' AND hold_reason = 'credential_disabled'
  AND pause_requested_at IS NULL
  AND credential_override_mode IS NOT DISTINCT FROM sqlc.narg('expected_override_mode')::text
  AND credential_override_secret_id IS NOT DISTINCT FROM sqlc.narg('expected_override_secret_id')::uuid
  AND codex_secret_id IS NOT DISTINCT FROM sqlc.narg('expected_codex_secret_id')::uuid
  AND worker_id IS NOT DISTINCT FROM sqlc.narg('expected_worker_id')::uuid
  AND kind NOT IN ('chat', 'judge') AND NOT interactive AND started_at IS NOT NULL
  AND (COALESCE(budget_wall_seconds, @global_timeout_seconds::int)
         + budget_extension_seconds + budget_finalize_seconds)
      - (GREATEST(0, EXTRACT(EPOCH FROM (status_since - started_at))::int)
         - budget_paused_seconds) <= 0
RETURNING id, user_id, status;

-- A one-time schedule held on credential_disabled (PRD #1732 D2) records why it waits: only
-- last_fire is written. next_fire_at, status and last_fired_at are untouched, so the row stays
-- due and fires once the credential is enabled. Guarded to a still-active one-time row so a
-- concurrent edit, disable or fire is never overwritten with a stale hold.
-- name: RecordScheduleHeldFire :execrows
UPDATE run_schedules
SET last_fire = @last_fire, updated_at = now()
WHERE id = @id AND timing = 'once' AND status = 'active';
