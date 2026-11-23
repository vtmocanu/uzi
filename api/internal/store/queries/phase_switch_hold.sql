-- name: SetRunPhaseSwitchHold :one
-- PRD #3252 m4: hold a Claude run whose plan/implement phase cannot be bound to a supported model
-- configuration on this worker. running|awaiting_approval -> paused, hold_reason
-- 'phase_switch_unavailable', NON-TERMINAL. The worker stops further work when phase verification
-- fails, keeping its clone and claim (a contradictory init may follow an admitted request); the owner resumes it
-- (ResumePausedRun admits this reason) or cancels. No automatic promoter touches it.
--
-- THE FENCE mirrors SetRunBaseAttestationHold: the same worker, a live claim (claim_released_at IS NULL)
-- at exactly this claim_generation, in a pre-publication phase. A released, superseded or reassigned
-- flight matches 0 rows; the service re-reads the row so the worker learns the real status (409), and a
-- repeat for a generation already held here is answered from that re-read without touching
-- status_since or any budget column.
--
-- The harness, kind, review and egress predicates keep the hold to the run shapes whose resume path
-- is the ordinary claim; a job/review/egress-profile run can never be parked into it.
--
-- The pending-pause columns are left as they are: this is not an owner pause. THE HEALTH RESET is
-- mandatory for SetRunPaused's reason: ListActiveRunsForHealth never revisits a paused run, so a flag
-- live at hold time would freeze for the whole hold. The question pointer is cleared: an
-- awaiting_approval run carries none, and a resumed run must not inherit a stale one.
-- Bank the old approval wait before replacing status_since, as SetRunRunning does. Only the first
-- transition updates the bank: an idempotent repeat is answered by the service's held-row read.
UPDATE runs SET
    status             = 'paused',
    status_since       = now(),
    budget_paused_seconds = budget_paused_seconds
        + CASE WHEN status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int)
               ELSE 0 END,
    hold_reason        = 'phase_switch_unavailable',
    hold_captured_head = sqlc.narg('hold_captured_head'),
    open_question_id   = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status IN ('running', 'awaiting_approval')
  AND claim_released_at IS NULL
  AND claim_generation = @claim_generation::bigint
  AND harness IN ('claude', 'codex')
  AND kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'mr_rework', 'task')
  AND review_target_run_id IS NULL
  AND egress_profile_id IS NULL
RETURNING *;
