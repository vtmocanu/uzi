-- name: SetRunBaseAttestationHold :one
-- issue #3194: hold a `--require-base` run whose worker adopted retained work that no checkpoint
-- attests was produced on the planned base. running -> paused, hold_reason
-- 'base_attestation_missing', NON-TERMINAL. The worker sends this from its preflight guard BEFORE
-- the executor runs, keeping its clone and custody; the owner resolves the hold through recovery,
-- export or a successor run. Nothing automatic leaves it: ResumePausedRun, the extension writers and
-- every promoter refuse or skip this reason.
--
-- THE FENCE is the completion hold's (SetRunCompletionHold) tightened to a mandatory generation:
-- the same worker, a live claim (claim_released_at IS NULL) at exactly this claim_generation, in
-- `running`. A released, superseded or reassigned flight matches 0 rows; the service re-reads the
-- row so the worker learns the real status (409), and a repeat for a generation already held here
-- is answered from that re-read without touching status_since or any budget column. Only a run
-- created with --require-base and a planned commit can enter it, so no other run can be parked
-- into a hold that refuses resume.
--
-- The pending-pause columns are left as they are: this is not an owner pause, and a held run is
-- never resumed into the flight such a request would steer. THE HEALTH RESET is mandatory for
-- SetRunPaused's reason: ListActiveRunsForHealth never revisits a paused run, so a flag live at
-- hold time would freeze for the whole hold.
UPDATE runs SET
    status             = 'paused',
    status_since       = now(),
    hold_reason        = 'base_attestation_missing',
    hold_captured_head = sqlc.narg('hold_captured_head'),
    open_question_id   = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running'
  AND require_base_match AND planned_base_commit IS NOT NULL
  AND claim_released_at IS NULL
  AND claim_generation = @claim_generation::bigint
RETURNING *;
