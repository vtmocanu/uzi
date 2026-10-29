-- name: InsertRunLeadConfigFenced :one
-- Lead-config record write (PRD #3244 M1), statement 1 of store.RecordRunLeadConfig. ONE statement
-- locks the run row FOR NO KEY UPDATE and evaluates the claim fence under that lock, then inserts
-- the row with harness copied from the run. fenced = the fence admitted; inserted = a new row
-- landed. fenced AND NOT inserted means a row with this key already exists (see RunLeadConfigSameBody).
-- The fence mirrors leadcfg.LeadLane (kind list, no review target, no egress profile) plus the
-- claim predicates (worker, generation, unreleased claim, live status). FOR NO KEY UPDATE, not
-- FOR UPDATE: same conflict set as ClaimRun and SetRunCompleted, but it does not block the KEY SHARE
-- lock a child-table foreign-key insert takes on the run row.
-- The ON CONFLICT target is named so a later unique index cannot be absorbed as a duplicate.
WITH r AS (
    SELECT id, harness FROM runs
     WHERE id = @run_id::uuid
       AND worker_id = @worker_id::uuid
       AND claim_generation = @claim_generation::bigint
       AND claim_released_at IS NULL
       AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
       AND kind IN ('issue', 'ci_fix', 'prompt', 'self_improve', 'mr_rework', 'task')
       AND review_target_run_id IS NULL
       AND egress_profile_id IS NULL
       FOR NO KEY UPDATE
), ins AS (
    INSERT INTO run_lead_configs (
        run_id, claim_generation, phase, phase_entry, seq, harness,
        requested_model, requested_model_source, observed_model, observed_model_evidence,
        requested_effort, requested_effort_mode, requested_effort_source,
        observed_effort, observed_effort_evidence, truncated
    )
    SELECT r.id, @claim_generation::bigint, @phase::text, @phase_entry::smallint, @seq::smallint, r.harness,
           sqlc.narg('requested_model')::text, @requested_model_source::text,
           sqlc.narg('observed_model')::text, sqlc.narg('observed_model_evidence')::text,
           sqlc.narg('requested_effort')::text, @requested_effort_mode::text, @requested_effort_source::text,
           sqlc.narg('observed_effort')::text, sqlc.narg('observed_effort_evidence')::text,
           @truncated::boolean
      FROM r
    ON CONFLICT (run_id, claim_generation, phase, seq) DO NOTHING
    RETURNING 1 AS one
)
SELECT EXISTS (SELECT 1 FROM r)::boolean AS fenced,
       EXISTS (SELECT 1 FROM ins)::boolean AS inserted;

-- name: RunLeadConfigSameBody :one
-- Statement 2 of store.RecordRunLeadConfig, run only when the fence admitted and the insert hit an
-- existing key. It runs as a second statement of the same READ COMMITTED transaction, so it sees a
-- row committed by the writer statement 1 waited on, while the run row stays locked. The nullable
-- columns use IS NOT DISTINCT FROM (a plain = would answer false for every identical retry of a
-- requested-only row). reported_at (server time) and harness (copied from the run) are excluded.
-- No stored row for the key yields no rows (sql.ErrNoRows), which the caller treats as an invariant breach.
SELECT COALESCE(
           c.phase_entry = @phase_entry::smallint
       AND c.truncated = @truncated::boolean
       AND c.requested_model_source = @requested_model_source::text
       AND c.requested_effort_mode = @requested_effort_mode::text
       AND c.requested_effort_source = @requested_effort_source::text
       AND c.requested_model IS NOT DISTINCT FROM sqlc.narg('requested_model')::text
       AND c.requested_effort IS NOT DISTINCT FROM sqlc.narg('requested_effort')::text
       AND c.observed_model IS NOT DISTINCT FROM sqlc.narg('observed_model')::text
       AND c.observed_model_evidence IS NOT DISTINCT FROM sqlc.narg('observed_model_evidence')::text
       AND c.observed_effort IS NOT DISTINCT FROM sqlc.narg('observed_effort')::text
       AND c.observed_effort_evidence IS NOT DISTINCT FROM sqlc.narg('observed_effort_evidence')::text,
           false)::boolean AS same_body
  FROM run_lead_configs c
 WHERE c.run_id = @run_id::uuid
   AND c.claim_generation = @claim_generation::bigint
   AND c.phase = @phase::text
   AND c.seq = @seq::smallint;

-- name: ListLatestRunLeadConfig :many
-- The highest-seq row per phase within the highest claim generation that has rows (PRD #3244 section 5),
-- plan first. A plain ORDER BY phase would put implement before plan, hence the outer CASE ordering.
SELECT latest.run_id, latest.claim_generation, latest.phase, latest.phase_entry, latest.seq, latest.harness,
       latest.requested_model, latest.requested_model_source, latest.observed_model, latest.observed_model_evidence,
       latest.requested_effort, latest.requested_effort_mode, latest.requested_effort_source,
       latest.observed_effort, latest.observed_effort_evidence, latest.truncated, latest.reported_at
  FROM (
    SELECT DISTINCT ON (c.phase) c.*
      FROM run_lead_configs c
     WHERE c.run_id = @run_id::uuid
       AND c.claim_generation = (SELECT max(m.claim_generation) FROM run_lead_configs m WHERE m.run_id = @run_id::uuid)
     ORDER BY c.phase, c.seq DESC
  ) latest
 ORDER BY CASE latest.phase WHEN 'plan' THEN 0 ELSE 1 END;
