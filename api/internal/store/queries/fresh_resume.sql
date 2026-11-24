-- Fresh resume (issue #4628): a run held at an owner-held worker-exhaustion recovery_wait is started on
-- a SEPARATE persistent hosted worker (W2) from durable evidence. run_fresh_resumes is the transfer
-- row; a0_guard_fresh_resume_trg (00523) refuses every runs status / worker_id / claim_generation
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
-- Unlocked read of the owner's pending, unexpired rows, oldest first. Candidates only: every
-- condition is rechecked under the run and transfer locks before a claim.
SELECT t.* FROM run_fresh_resumes t
JOIN runs r ON r.id = t.run_id AND r.user_id = t.user_id
WHERE t.user_id = @user_id AND t.state = 'pending' AND t.admission_deadline > now()
  AND r.status = 'recovery_wait'
ORDER BY t.created_at, t.id
LIMIT @row_limit::int;

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

-- name: CaptureHasActiveFreshResume :one
-- Whether a pending or claimed transfer references the capture: its bytes must not be discarded,
-- expired or fenced while the destination may still fetch them.
SELECT EXISTS (SELECT 1 FROM run_fresh_resumes t
               WHERE t.capture_id = @capture_id AND t.state IN ('pending', 'claimed'))::boolean AS referenced;
