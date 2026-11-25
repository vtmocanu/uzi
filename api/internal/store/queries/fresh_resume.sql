-- Fresh resume (issue #4628): a run held at an owner-held worker-exhaustion recovery_wait is started on
-- a SEPARATE persistent hosted worker (W2) from durable evidence. run_fresh_resumes is the transfer
-- row; a0_guard_fresh_resume_trg (00535) refuses every runs status / worker_id / claim_generation
-- change while a pending or claimed row exists, except under the transaction-local opt-in below.
-- Callers lock the run (and the transfer row) before any of these writers; the sequence is
-- OptInFreshResume, the guarded writes, then ClearFreshResumeOptIn.

-- name: OptInFreshResume :exec
-- Transaction-local opt-in for exactly one transfer row id. Its own statement (pgx pools reuse
-- sessions), cleared after the last guarded statement by ClearFreshResumeOptIn.
SELECT set_config('uzi.fresh_resume_id', @id::text, true);

-- name: ClearFreshResumeOptIn :exec
SELECT set_config('uzi.fresh_resume_id', '', true);

-- name: InsertFreshResumePending :one
INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind,
                               capture_id, checkpoint_ref, sha, candidate_token)
VALUES (@run_id, @user_id, @request_id, @evidence_generation, @evidence_kind,
        sqlc.narg('capture_id')::uuid, sqlc.narg('checkpoint_ref')::text, @sha, @candidate_token)
RETURNING *;

-- name: GetFreshResumeByRequest :one
SELECT * FROM run_fresh_resumes WHERE run_id = @run_id AND request_id = @request_id;

-- name: GetActiveFreshResumeForRun :one
SELECT * FROM run_fresh_resumes WHERE run_id = @run_id AND state IN ('pending', 'claimed');

-- name: GetActiveFreshResumeForRunForUpdate :one
SELECT * FROM run_fresh_resumes WHERE run_id = @run_id AND state IN ('pending', 'claimed') FOR UPDATE;

-- name: GetFreshResumeForUpdate :one
SELECT * FROM run_fresh_resumes WHERE id = @id FOR UPDATE;

-- name: ListPendingFreshResumeCandidates :many
-- The fresh claim's unlocked candidate read (plan section 4, step 1) for the claimant @worker_id (W2):
-- the owner's pending, unexpired rows, oldest first, that W2 may take. Candidates only: the facts that
-- decide a claim are rechecked under the run and transfer locks, and ClaimRun itself re-evaluates every
-- gate it owns (capability, intake, reserve, forge origin, custody) on the claimed row.
-- Pre-filters here: fn_worker_can_claim, the intake pause, the forge origin admission, the W1-keyed
-- snapshot / terminal_pending / overflow dedupe (ClaimRun's own copies of those are keyed on the run's
-- worker_id, which is W2 once the tuple is prepared, so W1's rows are read here, before that), and the
-- exclusions: W2 is not the run's worker or released worker (W1), holds no open custody hold of the run
-- and is not the excluded predecessor of a seeded or superseded transfer.
-- w1_id is the predecessor the claim must lock: the run's worker, else its released worker; the nil
-- uuid when the run names neither (a deleted worker).
SELECT t.id, t.run_id, COALESCE(r.worker_id, r.released_worker_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS w1_id
FROM run_fresh_resumes t
JOIN runs r ON r.id = t.run_id AND r.user_id = t.user_id
WHERE t.user_id = @user_id AND t.state = 'pending' AND t.admission_deadline > now()
  AND r.status = 'recovery_wait'
  AND (r.worker_id IS NULL OR r.worker_id <> @worker_id::uuid)
  AND (r.released_worker_id IS NULL OR r.released_worker_id <> @worker_id::uuid)
  AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h
                  WHERE h.run_id = r.id AND h.state = 'open'
                    AND @worker_id::uuid IN (h.live_worker_id, h.original_worker_id))
  AND NOT EXISTS (SELECT 1 FROM run_fresh_resumes s
                  WHERE s.run_id = r.id AND s.state IN ('seeded', 'superseded')
                    AND s.predecessor_worker_id = @worker_id::uuid)
  AND fn_worker_can_claim(@is_docker_worker::boolean, @docker_repo_allowlist::uuid[], r.repo_id, r.kind,
                          @worker_caps::text[], r.required_capabilities, @capability_aware::boolean)
  AND (NOT (SELECT fn_instance_intake_paused()) OR fn_run_admitted_during_intake_pause(r))
  AND EXISTS (SELECT 1 FROM workers c WHERE c.id = @worker_id::uuid AND fn_forge_origin_eligible(c, r, now()))
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = r.id AND a.worker_id = r.worker_id
                    AND a.claim_generation = r.claim_generation
                    AND r.claim_released_at IS NULL
                    AND (a.reported_at >= @snapshot_fresh_cutoff::timestamptz
                         OR (a.terminal_pending AND a.terminal_pending_until > now())))
  AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = r.worker_id AND w.pending_overflow_until > now())
  -- ClaimRun's PRD #3522 weekly-reserve admission gate, verbatim (same @reserve_* params, from the claim's
  -- ClaimRunParams): a reserve-held run is not even attempted.
  AND (NOT @reserve_on::boolean
           OR (COALESCE(r.reserve_exempt_until > @reserve_now::timestamptz, false) AND NOT (r.id = ANY(@reserve_override_ended::uuid[])))
           OR (r.first_started_at IS NOT NULL AND NOT @reserve_pause_on::boolean)
           OR NOT (
               CASE
                   WHEN r.harness = 'codex'
                       THEN COALESCE(r.codex_secret_id = ANY(@reserve_blocked_codex::uuid[]), false)
                   WHEN r.kind IN ('judge', 'self_improve')
                       THEN CASE @reserve_judge_mode::text
                           WHEN 'auto' THEN
                               CASE
                                   WHEN EXISTS (SELECT 1 FROM unnest(@reserve_permitted::uuid[]) pp
                                                WHERE NOT COALESCE(r.retry_not_before > @reserve_now::timestamptz AND pp = r.limit_dead_secret_id, false))
                                       THEN false
                                   WHEN EXISTS (SELECT 1 FROM unnest(@reserve_pooled::uuid[]) pp
                                                WHERE NOT COALESCE(r.retry_not_before > @reserve_now::timestamptz AND pp = r.limit_dead_secret_id, false))
                                       THEN true
                                   ELSE @reserve_owner_default_blocked::boolean
                               END
                           ELSE @reserve_judge_static_blocked::boolean
                       END
                   WHEN r.credential_override_mode = 'pinned' AND r.credential_override_secret_id IS NOT NULL
                       THEN COALESCE(r.credential_override_secret_id = ANY(@reserve_blocked_claude::uuid[]), false)
                   WHEN r.credential_override_mode = 'default'
                       THEN @reserve_owner_default_blocked::boolean
                   WHEN r.credential_override_mode = 'auto' OR @reserve_worker_mode::text = 'auto'
                       THEN (EXISTS (SELECT 1 FROM unnest(@reserve_pooled::uuid[]) pp
                                     WHERE NOT COALESCE(r.retry_not_before > @reserve_now::timestamptz AND pp = r.limit_dead_secret_id, false))
                             AND NOT EXISTS (SELECT 1 FROM unnest(@reserve_permitted::uuid[]) pp
                                             WHERE NOT COALESCE(r.retry_not_before > @reserve_now::timestamptz AND pp = r.limit_dead_secret_id, false)))
                   ELSE @reserve_worker_static_blocked::boolean
               END
           ))
ORDER BY t.created_at, t.id
LIMIT @row_limit::int;

-- name: LockFreshClaimTarget :one
-- Plan section 4, step 2.3: the run and the transfer row locked together, SKIP LOCKED (a held row is a
-- skipped candidate, never a wait), with every fact rechecked on the locked versions: the row is still
-- pending and unexpired, the run is still held at recovery_wait and the owner's, and w1_id is the
-- predecessor the caller locked (the caller compares it; the nil uuid, no worker, is accepted only on the
-- deleted branch).
-- user_id is the claimant's owner: the destination worker and the run belong to the same user.
-- A SKIP LOCKED zero-row result can leave r locked (t was the skipped row): the caller relies on its
-- savepoint rollback to release it.
SELECT t.id, t.run_id, t.capture_id, t.evidence_kind, t.evidence_generation, t.sha,
       COALESCE(r.worker_id, r.released_worker_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS w1_id
FROM run_fresh_resumes t
JOIN runs r ON r.id = t.run_id AND r.user_id = t.user_id
WHERE t.id = @id AND t.user_id = @user_id AND t.state = 'pending' AND t.admission_deadline > now()
  AND r.status = 'recovery_wait'
  -- The W1-keyed dedupe of ListPendingFreshResumeCandidates, rechecked on the locked versions (the
  -- caller already holds W1 FOR KEY SHARE): a W1 snapshot or terminal_pending row committed after the
  -- candidate read must skip the candidate.
  AND (r.worker_id IS NULL OR r.worker_id <> @worker_id::uuid)
  AND (r.released_worker_id IS NULL OR r.released_worker_id <> @worker_id::uuid)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                  WHERE a.run_id = r.id AND a.worker_id = r.worker_id
                    AND a.claim_generation = r.claim_generation
                    AND r.claim_released_at IS NULL
                    AND (a.reported_at >= @snapshot_fresh_cutoff::timestamptz
                         OR (a.terminal_pending AND a.terminal_pending_until > now())))
  AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = r.worker_id AND w.pending_overflow_until > now())
FOR UPDATE OF r, t SKIP LOCKED;

-- name: LockCaptureForFreshClaim :one
-- Plan section 4, step 2.4: the transfer's capture FOR SHARE NOWAIT (a concurrent discard or expiry holds
-- it FOR UPDATE: 55P03, the caller skips the candidate), rechecked on the locked version: still
-- available and manifest-bound, unexpired, under the evidence generation's hold, and the same source.
SELECT c.id FROM recovery_captures c
JOIN recovery_custody_holds h ON h.id = c.hold_id
WHERE c.id = @id AND c.run_id = @run_id AND c.user_id = @user_id
  AND c.state = 'available' AND c.manifest_bound AND c.expires_at > now()
  AND h.run_id = c.run_id AND h.user_id = c.user_id AND h.generation = @evidence_generation
  AND c.source_sha = @sha
FOR SHARE OF c NOWAIT;

-- name: LockWorkerKeyShareSkipLocked :one
-- The fresh claim's predecessor lock: FOR KEY SHARE, skipping a worker row a conflicting FOR UPDATE holder
-- has. No row is ambiguous (locked by another, or gone): FreshResumeWorkerExists tells them apart.
SELECT w.id FROM workers w WHERE w.id = @id FOR KEY SHARE SKIP LOCKED;

-- name: FreshResumeWorkerExists :one
SELECT EXISTS (SELECT 1 FROM workers w WHERE w.id = @id)::boolean AS present;

-- name: PrepareFreshResumeClaim :one
-- Saves the run's pre-claim tuple into the pending transfer row, then queues the run for W2 with the
-- paused-time credit and clears of ResumeWorkerRecoveryEpisode (same SET list, except that
-- worker_recovery_episode is untouched: ADR-1197's grant and high-water mark are kept, and
-- codex_cap_hash / codex_claim_epoch stay monotonic for ClaimRun). Requires the opt-in for this row.
WITH cur AS MATERIALIZED (
    -- FOR UPDATE: lock the run before the transfer row (run -> transfer, the order every other
    -- transfer writer uses), and read the saved tuple from the post-lock row version.
    SELECT r.* FROM runs r
    WHERE r.id = @run_id AND r.status = 'recovery_wait'
    FOR UPDATE
), saved AS (
    UPDATE run_fresh_resumes t SET
        saved_worker_id = cur.worker_id,
        saved_tuple = jsonb_build_object(
            'status', cur.status,
            'claimed_at', cur.claimed_at,
            'cross_check_lane', cur.cross_check_lane,
            'checkpoint_contains_latest', cur.checkpoint_contains_latest,
            'reserve_hold_reason', cur.reserve_hold_reason,
            'policy_pending_fresh', cur.policy_pending_fresh,
            'policy_fresh_consumed_generation', cur.policy_fresh_consumed_generation,
            'recovery_wait_cause', cur.recovery_wait_cause,
            'recovery_wait_detail', cur.recovery_wait_detail,
            'recovery_wait_phase', cur.recovery_wait_phase,
            'status_since', cur.status_since,
            'budget_paused_seconds', cur.budget_paused_seconds,
            'requeue_episode_baseline', cur.requeue_episode_baseline,
            'requeue_count', cur.requeue_count,
            'stale_requeue_generation', cur.stale_requeue_generation,
            'claim_released_at', cur.claim_released_at,
            'released_worker_id', cur.released_worker_id,
            'released_worker_nonce', cur.released_worker_nonce,
            'recovery_retry_not_before', cur.recovery_retry_not_before,
            'worker_recovery_evidence', cur.worker_recovery_evidence,
            'forge_admission', cur.forge_admission,
            'hold_reason', cur.hold_reason,
            'hold_captured_head', cur.hold_captured_head,
            'health', cur.health,
            'health_reason', cur.health_reason,
            'health_since', cur.health_since)
        -- jsonb_build_object takes at most 100 arguments: the SetRunRunning-written columns are a second object.
        || jsonb_build_object(
            'plan_cross_check_gate_reason', cur.plan_cross_check_gate_reason,
            'open_question_id', cur.open_question_id,
            'completion_question_at', cur.completion_question_at,
            'started_at', cur.started_at,
            'first_started_at', cur.first_started_at,
            'iteration_count', cur.iteration_count,
            'session_id', cur.session_id,
            'policy_fresh_confirmed_generation', cur.policy_fresh_confirmed_generation,
            'repo_agents', cur.repo_agents,
            'repo_agent_source_commit', cur.repo_agent_source_commit,
            'agent_source', cur.agent_source,
            'agent_exclusions', cur.agent_exclusions,
            'plan_agent_source', cur.plan_agent_source,
            'required_capabilities', cur.required_capabilities,
            'capability_inference_evidence', cur.capability_inference_evidence,
            'required_tools', cur.required_tools,
            'size_class', cur.size_class,
            'milestones_frozen', cur.milestones_frozen,
            'completion_contract', cur.completion_contract,
            'contract_revision', cur.contract_revision,
            'budget_max_iterations', cur.budget_max_iterations,
            'budget_wall_seconds', cur.budget_wall_seconds,
            'milestones_completed', cur.milestones_completed,
            'milestones_in_progress', cur.milestones_in_progress,
            'milestones_agents', cur.milestones_agents)
    FROM cur
    WHERE t.id = @id AND t.run_id = cur.id AND t.state = 'pending'
    RETURNING t.id
)
UPDATE runs SET
    status                = 'queued',
    status_since          = now(),
    budget_paused_seconds = runs.budget_paused_seconds
        + CASE WHEN runs.started_at IS NOT NULL
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int)
               ELSE 0 END,
    worker_id             = @worker_id,
    hold_reason           = NULL,
    hold_captured_head    = NULL,
    requeue_episode_baseline = runs.requeue_count,
    stale_requeue_generation = NULL, worker_recovery_evidence = NULL,
    recovery_wait_cause = NULL, recovery_wait_detail = NULL, recovery_retry_not_before = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at            = now()
WHERE runs.id = @run_id AND runs.status = 'recovery_wait'
  AND EXISTS (SELECT 1 FROM saved)
RETURNING runs.id, runs.user_id, runs.status, runs.worker_id, runs.budget_paused_seconds;

-- name: MarkFreshResumeClaimed :one
-- Records the claim on the pending row: the destination, G' and the transfer deadline. The
-- predecessor is the worker the run was bound to (its worker_id, else its released worker), kept
-- only while that worker row still exists.
UPDATE run_fresh_resumes t SET
    state = 'claimed',
    destination_worker_id = @worker_id,
    claim_generation = @claim_generation,
    claimed_at = now(),
    transfer_deadline = now() + make_interval(secs => @transfer_seconds::int),
    predecessor_worker_id = (SELECT w.id FROM workers w
        WHERE w.id = COALESCE(t.saved_worker_id, NULLIF(t.saved_tuple->>'released_worker_id', '')::uuid))
WHERE t.id = @id AND t.state = 'pending' AND t.saved_tuple IS NOT NULL
RETURNING *;

-- name: RestoreFreshResumeTuple :execrows
-- Puts the saved pre-claim tuple back on the run (worker_id comes from saved_worker_id, which the FK
-- already nulled if that worker was deleted) and bumps fresh_hold_version. The tuple also carries every
-- column W2's allowlisted SetRunRunning advance can write (started_at, first_started_at, session_id,
-- open_question_id, iteration_count, the plan-freeze and budget columns, ...), so a refusal after W2
-- reported running puts back the pre-claim values; the registry test asserts the column coverage. Applies only while the run
-- is still the destination's claim at G'; zero rows means the run moved. It never touches
-- claim_generation, codex_claim_epoch, codex_cap_hash or claim provenance. Requires the opt-in.
UPDATE runs SET
    status = t.saved_tuple->>'status',
    claimed_at = (t.saved_tuple->>'claimed_at')::timestamptz,
    cross_check_lane = COALESCE((t.saved_tuple->>'cross_check_lane')::boolean, false),
    checkpoint_contains_latest = (t.saved_tuple->>'checkpoint_contains_latest')::boolean,
    reserve_hold_reason = t.saved_tuple->>'reserve_hold_reason',
    policy_pending_fresh = COALESCE((t.saved_tuple->>'policy_pending_fresh')::boolean, false),
    policy_fresh_consumed_generation = (t.saved_tuple->>'policy_fresh_consumed_generation')::bigint,
    recovery_wait_cause = t.saved_tuple->>'recovery_wait_cause',
    recovery_wait_detail = t.saved_tuple->>'recovery_wait_detail',
    recovery_wait_phase = t.saved_tuple->>'recovery_wait_phase',
    status_since = (t.saved_tuple->>'status_since')::timestamptz,
    budget_paused_seconds = (t.saved_tuple->>'budget_paused_seconds')::int,
    requeue_episode_baseline = (t.saved_tuple->>'requeue_episode_baseline')::int,
    requeue_count = (t.saved_tuple->>'requeue_count')::int,
    stale_requeue_generation = (t.saved_tuple->>'stale_requeue_generation')::bigint,
    claim_released_at = (t.saved_tuple->>'claim_released_at')::timestamptz,
    released_worker_id = (t.saved_tuple->>'released_worker_id')::uuid,
    released_worker_nonce = t.saved_tuple->>'released_worker_nonce',
    recovery_retry_not_before = (t.saved_tuple->>'recovery_retry_not_before')::timestamptz,
    worker_recovery_evidence = NULLIF(t.saved_tuple->'worker_recovery_evidence', 'null'::jsonb),
    forge_admission = NULLIF(t.saved_tuple->'forge_admission', 'null'::jsonb),
    hold_reason = t.saved_tuple->>'hold_reason',
    hold_captured_head = t.saved_tuple->>'hold_captured_head',
    health = t.saved_tuple->>'health',
    health_reason = t.saved_tuple->>'health_reason',
    health_since = (t.saved_tuple->>'health_since')::timestamptz,
    plan_cross_check_gate_reason = t.saved_tuple->>'plan_cross_check_gate_reason',
    open_question_id = t.saved_tuple->>'open_question_id',
    completion_question_at = (t.saved_tuple->>'completion_question_at')::timestamptz,
    started_at = (t.saved_tuple->>'started_at')::timestamptz,
    first_started_at = (t.saved_tuple->>'first_started_at')::timestamptz,
    iteration_count = (t.saved_tuple->>'iteration_count')::int,
    session_id = t.saved_tuple->>'session_id',
    policy_fresh_confirmed_generation = (t.saved_tuple->>'policy_fresh_confirmed_generation')::bigint,
    repo_agents = NULLIF(t.saved_tuple->'repo_agents', 'null'::jsonb),
    repo_agent_source_commit = t.saved_tuple->>'repo_agent_source_commit',
    agent_source = t.saved_tuple->>'agent_source',
    agent_exclusions = NULLIF(t.saved_tuple->'agent_exclusions', 'null'::jsonb),
    plan_agent_source = t.saved_tuple->>'plan_agent_source',
    required_capabilities = ARRAY(SELECT jsonb_array_elements_text(t.saved_tuple->'required_capabilities')),
    capability_inference_evidence = NULLIF(t.saved_tuple->'capability_inference_evidence', 'null'::jsonb),
    required_tools = ARRAY(SELECT jsonb_array_elements_text(t.saved_tuple->'required_tools')),
    size_class = t.saved_tuple->>'size_class',
    milestones_frozen = NULLIF(t.saved_tuple->'milestones_frozen', 'null'::jsonb),
    completion_contract = NULLIF(t.saved_tuple->'completion_contract', 'null'::jsonb),
    contract_revision = (t.saved_tuple->>'contract_revision')::int,
    budget_max_iterations = (t.saved_tuple->>'budget_max_iterations')::int,
    budget_wall_seconds = (t.saved_tuple->>'budget_wall_seconds')::int,
    milestones_completed = NULLIF(t.saved_tuple->'milestones_completed', 'null'::jsonb),
    milestones_in_progress = NULLIF(t.saved_tuple->'milestones_in_progress', 'null'::jsonb),
    milestones_agents = NULLIF(t.saved_tuple->'milestones_agents', 'null'::jsonb),
    worker_id = t.saved_worker_id,
    fresh_hold_version = runs.fresh_hold_version + 1,
    updated_at = now()
FROM run_fresh_resumes t
WHERE t.id = @id AND runs.id = t.run_id AND t.saved_tuple IS NOT NULL
  AND t.state = 'claimed'
  AND runs.status IN ('claimed', 'running')
  AND runs.claim_generation = t.claim_generation
  AND runs.worker_id IS NOT DISTINCT FROM t.destination_worker_id;

-- name: CloseFreshResume :one
-- Closes a pending or claimed row as refused or cancelled with a reason. Zero rows (no row) means it
-- was already closed or moved on.
UPDATE run_fresh_resumes SET
    state = @state::text,
    refusal_reason = @reason::text,
    closed_at = now()
WHERE id = @id AND state IN ('pending', 'claimed') AND @state::text IN ('refused', 'cancelled')
RETURNING *;

-- name: BumpFreshHoldVersion :one
-- Every exit from pending, and every restore, advances the version the preview token binds. The column
-- is outside the guard trigger's WHEN, so no opt-in is needed.
UPDATE runs SET fresh_hold_version = fresh_hold_version + 1 WHERE id = @id
RETURNING fresh_hold_version;

-- name: SupersedePriorSeeded :execrows
-- A chained transfer's receipt supersedes the run's earlier seeded row. Run before MarkFreshResumeSeeded:
-- at most one seeded row per run.
UPDATE run_fresh_resumes SET state = 'superseded'
WHERE run_id = @run_id AND state = 'seeded' AND id <> @id;

-- name: MarkFreshResumeSeeded :one
-- The first fenced commit: claimed at the caller's G', and the attempt id carries that generation
-- ("-g<G'>-"), parsed as the capture of the agent's attempt-id grammar (ATTEMPT_ID_RE in
-- agent/src/attempt-path.ts) and compared exactly, never by substring position. A "gx" id has no
-- generation and never matches. destination_attempt_id and seeded_generation are write-once.
UPDATE run_fresh_resumes SET
    state = 'seeded',
    destination_attempt_id = @attempt_id::text,
    seeded_generation = claim_generation,
    seeded_at = now()
WHERE id = @id AND state = 'claimed'
  AND destination_worker_id = @worker_id AND claim_generation = @claim_generation
  AND substring(@attempt_id::text from '^[0-9]{8}T[0-9]{6}Z-g([0-9]{1,18})-[0-9a-f]{16}$')::bigint = claim_generation
RETURNING *;

-- name: GetFreshResumeState :one
-- The stored state for this transfer, fenced only on run and destination (the F1 state route).
SELECT id, state, seeded_at, destination_attempt_id, seeded_generation, claim_generation
FROM run_fresh_resumes
WHERE id = @id AND run_id = @run_id AND destination_worker_id = @worker_id;

-- name: GetFreshResume :one
-- Unlocked read of one transfer row by id: the caller decides its lock order (pending: run then
-- transfer; claimed: saved worker, run, transfer) from the state it reads here, then re-reads the row
-- under the locks it takes.
SELECT * FROM run_fresh_resumes WHERE id = @id;

-- name: LockWorkerKeyShare :one
-- FOR KEY SHARE on one worker row (the saved predecessor of a claimed transfer): it keeps the row from
-- being deleted while a refusal restores the tuple, and blocks behind a worker FOR UPDATE holder. No row
-- means the worker is gone.
SELECT w.id FROM workers w WHERE w.id = @id FOR KEY SHARE;

-- name: LockWorkerKeyShareNoWait :one
-- LockWorkerKeyShare for a caller that already holds another worker lock (the claim finisher, the abandon
-- route): it never waits, a held conflicting lock is SQLSTATE 55P03 and the caller retries.
SELECT w.id FROM workers w WHERE w.id = @id FOR KEY SHARE NOWAIT;

-- name: OwnerHasOutstandingFreshResume :one
-- Whether this owner has any transfer that still reserves headroom or still has a seeded successor
-- bound to a nonterminal run. Owner Resume reads it unlocked to decide whether to take the owner lock.
SELECT EXISTS (SELECT 1 FROM run_fresh_resumes t
               JOIN runs r ON r.id = t.run_id AND r.user_id = t.user_id
               WHERE t.user_id = @user_id
                 AND t.state IN ('pending', 'claimed', 'seeded', 'superseded')
                 AND r.status NOT IN ('completed', 'failed', 'cancelled'))::boolean AS outstanding;

-- name: FreshResumeRunBaseEligible :one
-- fn_fresh_resume_run_base (00535) over one owner run. No row: not the owner's run.
SELECT fn_fresh_resume_run_base(r, @global_timeout_seconds::int)::boolean AS eligible
FROM runs r WHERE r.id = @id AND r.user_id = @user_id;

-- name: ListFreshResumeCaptureEvidence :many
-- Every capture of the run, in any state, with its hold's generation and binding. ttl_ok is the Start
-- admission window: more than 30 minutes of retention left (ExpireReadyCaptures relies on it).
SELECT c.id, c.state, c.manifest_bound, c.source_sha, c.prerequisite_shas,
       (c.expires_at IS NOT NULL AND c.expires_at > now() + interval '30 minutes')::boolean AS ttl_ok,
       h.generation AS hold_generation, h.run_id AS hold_run_id, h.repo_id AS hold_repo_id
FROM recovery_captures c
JOIN recovery_custody_holds h ON h.id = c.hold_id
WHERE c.run_id = @run_id AND c.user_id = @user_id
ORDER BY h.generation DESC, c.created_at, c.id;

-- name: CountOpenHoldsForFreshRun :one
-- How many open custody holds the run has. A checkpoint-ref candidate needs at least one: the api keeps
-- the run's checkpoint ref only while a hold is open.
SELECT count(*)::bigint AS open_holds FROM recovery_custody_holds h
WHERE h.run_id = @run_id AND h.user_id = @user_id AND h.state = 'open';

-- name: CountFreshResumeDestinations :one
-- The destination predicate: an online, persistent (non-ephemeral) hosted worker of the owner that
-- advertises fresh_resume_v1 and recovery_inventory_v1, other than the worker the run is (or was)
-- bound to and other than any holder of an open custody hold on the run. A NULL cutoff matches nothing.
SELECT count(*)::bigint AS destinations
FROM workers w
JOIN runs r ON r.id = @run_id AND r.user_id = w.user_id
WHERE w.user_id = @user_id AND w.kind = 'hosted' AND NOT w.ephemeral AND w.status = 'online'
  AND w.last_heartbeat_at >= sqlc.narg(heartbeat_cutoff)::timestamptz
  AND w.draining_since IS NULL AND NOT w.maintenance_fenced
  AND w.protocol_capabilities @> ARRAY['fresh_resume_v1', 'recovery_inventory_v1']::text[]
  AND w.id IS DISTINCT FROM r.worker_id AND w.id IS DISTINCT FROM r.released_worker_id
  AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h
                  WHERE h.run_id = r.id AND h.state = 'open'
                    AND w.id IN (h.live_worker_id, h.original_worker_id))
  -- the excluded predecessor of the run's seeded (or superseded) transfer: the guard refuses it as W2.
  AND NOT EXISTS (SELECT 1 FROM run_fresh_resumes s
                  WHERE s.run_id = r.id AND s.state IN ('seeded', 'superseded')
                    AND s.predecessor_worker_id = w.id);

-- name: LockCaptureForFreshResume :one
-- Start's capture lock: FOR SHARE, with every admission fact in the SAME statement so a concurrent expiry
-- or discard that commits while this waits is re-evaluated on the new row version (READ COMMITTED). No row
-- means the capture is no longer a usable candidate: not available, not manifest-bound, inside the 30 minute
-- retention window, bound to another hold generation, run or repo, or another source sha.
SELECT c.id, c.source_sha, c.prerequisite_shas, c.expires_at, h.generation AS hold_generation
FROM recovery_captures c
JOIN recovery_custody_holds h ON h.id = c.hold_id
WHERE c.id = @id AND c.run_id = @run_id AND c.user_id = @user_id
  AND c.state = 'available' AND c.manifest_bound
  AND c.expires_at > now() + interval '30 minutes'
  AND h.run_id = c.run_id AND h.user_id = c.user_id AND h.generation = @evidence_generation
  AND h.repo_id = (SELECT r.repo_id FROM runs r WHERE r.id = c.run_id)
  AND c.source_sha = @sha
FOR SHARE OF c;

-- name: SetFreshResumeTerminal :exec
-- Transaction-local allowance for the auto-stop failure of exactly one run (guard rule 1's
-- uzi.fresh_resume_terminal exception). Its own statement, run in the same transaction as FailRunAutoStop
-- and gone when that transaction ends.
SELECT set_config('uzi.fresh_resume_terminal', @run_id::text, true);

-- name: FreshResumeCurrentAuthority :one
-- Whether the calling worker still holds the run's live claim at @live_generation. Read under the run
-- lock by the receipt and state routes. True only when the run is claimed or running for this worker at
-- exactly that generation with the claim unreleased and no cancel or stop input still deliverable (the
-- cancel_deliverable predicate of GetFollowupFinalizeBlockers, extended to stop): unapplied, not
-- superseded and not covered by a milestone-reconciliation stop_cutoff. No row (the run is gone) is
-- false by the caller.
SELECT COALESCE((r.status IN ('claimed', 'running')
        AND r.worker_id = @worker_id::uuid
        AND r.claim_generation = @live_generation::bigint
        AND r.claim_released_at IS NULL
        AND NOT EXISTS (
            SELECT 1 FROM run_user_inputs i
            WHERE i.run_id = r.id AND i.kind IN ('cancel', 'stop')
              AND i.applied_at IS NULL AND i.disposition IS DISTINCT FROM 'superseded'
              AND NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations a
                              WHERE a.run_id = i.run_id AND i.id <= a.stop_cutoff))), false)::boolean AS current_authority
FROM runs r WHERE r.id = @run_id;

-- name: ListFreshResumeSweepCandidates :many
-- The sweep's selector: transfer rows with positive evidence that they must be refused, and the reason.
-- Candidates only; the sweep re-runs this same query narrowed to one id (only_id) under the run and
-- transfer locks and refuses only when the reason still holds. A snapshot that merely omits the run
-- inside the grace (@omitted_grace_seconds: the worker heartbeat stale window plus its interval, as the
-- missing cutoff) is NOT loss evidence. A departure stamp stops counting as evidence once the destination's
-- snapshot lists the run at G' again; the stamp column itself is never cleared.
--   pending:  gate_off (the instance gate is off) or admission_deadline (the row expired).
--   claimed (claimed_enabled): transfer_deadline; destination_deleted (the destination worker row is
--     gone); destination_stale (its heartbeat is older than @stale_cutoff); destination_reregistered (the
--     nonce it reported its first running with is no longer its register nonce); snapshot_departed
--     (StampSnapshotDepartures' stamp is present at G' under the worker's current nonce); snapshot_omitted
--     (an accepted heartbeat snapshot newer than status_since + @omitted_grace_seconds does not list the run at G', and the
--     worker is not under a pending-outcome overflow closure).
SELECT c.id, c.run_id, c.reason
FROM (
    SELECT t.id, t.run_id, t.created_at,
           (CASE
              WHEN t.state = 'pending' AND NOT @gate_on::boolean THEN 'gate_off'
              WHEN t.state = 'pending' AND t.admission_deadline <= now() THEN 'admission_deadline'
              WHEN t.state <> 'claimed' OR NOT @claimed_enabled::boolean THEN NULL
              WHEN t.transfer_deadline IS NOT NULL AND t.transfer_deadline <= now() THEN 'transfer_deadline'
              WHEN t.destination_worker_id IS NULL OR r.worker_id IS NULL OR w.id IS NULL THEN 'destination_deleted'
              WHEN COALESCE(w.last_heartbeat_at, '-infinity'::timestamptz) < @stale_cutoff::timestamptz THEN 'destination_stale'
              WHEN r.registration_evidence_generation = t.claim_generation
                   AND r.registration_evidence_nonce IS DISTINCT FROM w.snapshot_register_nonce THEN 'destination_reregistered'
              WHEN r.snapshot_departed_generation = t.claim_generation
                   AND w.snapshot_register_nonce IS NOT NULL
                   AND r.snapshot_departed_nonce = w.snapshot_register_nonce
                   AND r.snapshot_departed_epoch <= w.snapshot_epoch
                   AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                                   WHERE a.worker_id = w.id AND a.run_id = r.id
                                     AND a.claim_generation = t.claim_generation) THEN 'snapshot_departed'
              WHEN r.status IN ('claimed', 'running') AND r.claim_released_at IS NULL
                   AND w.roll_heartbeat_at IS NOT NULL
                   AND w.roll_heartbeat_at > r.status_since + make_interval(secs => @omitted_grace_seconds::double precision)
                   AND (w.pending_overflow_until IS NULL OR w.pending_overflow_until <= now())
                   AND NOT EXISTS (SELECT 1 FROM worker_active_runs a
                                   WHERE a.worker_id = w.id AND a.run_id = r.id
                                     AND a.claim_generation = t.claim_generation) THEN 'snapshot_omitted'
            END)::text AS reason
    FROM run_fresh_resumes t
    JOIN runs r ON r.id = t.run_id
    LEFT JOIN workers w ON w.id = t.destination_worker_id
    WHERE t.state IN ('pending', 'claimed')
      AND (sqlc.narg(only_id)::uuid IS NULL OR t.id = sqlc.narg(only_id)::uuid)
) c
WHERE c.reason IS NOT NULL
ORDER BY c.created_at, c.id
LIMIT 200;

-- name: CaptureHasActiveFreshResume :one
-- Whether a pending or claimed transfer references the capture: its bytes must not be discarded,
-- expired or fenced while the destination may still fetch them.
SELECT EXISTS (SELECT 1 FROM run_fresh_resumes t
               WHERE t.capture_id = @capture_id AND t.state IN ('pending', 'claimed'))::boolean AS referenced;
