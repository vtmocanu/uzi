-- Issue #4257: checkpoint-publication-withheld fact ------------------------------------------
-- A worker's content-free report that checkpoint publication is withheld by a secret-scan
-- restriction. Both statements share one fence (this worker owns the unreleased running claim at
-- the reported generation and the register nonce is the worker's current one) and one order rule
-- (the stored marker (worker, generation, nonce, report_seq) is absent, belongs to another
-- incarnation, or is strictly lower than the request's report_seq). The row is locked, the order
-- compared and the write made in ONE statement so a delayed older report can never overwrite a
-- newer one. The outcome text is the ACK: a duplicate of the stored marker answers its original
-- outcome without a write; a lower marker or a fence mismatch answers 'refused'.

-- name: RecordRunCheckpointWithheld :one
-- Set (or refresh) the fact. since is kept when a fact of the same incarnation is already present,
-- otherwise it is the worker's since clamped into [claimed_at, now()]. status_since pins the fact
-- to the running episode it was reported in.
WITH target AS (
  SELECT runs.id,
         (runs.checkpoint_withheld_report_seq IS NULL
          OR runs.checkpoint_withheld_worker_id IS DISTINCT FROM @worker_id::uuid
          OR runs.checkpoint_withheld_generation IS DISTINCT FROM @generation::bigint
          OR runs.checkpoint_withheld_nonce IS DISTINCT FROM @register_nonce::text
          OR @report_seq::bigint > runs.checkpoint_withheld_report_seq) AS ordered,
         (runs.checkpoint_withheld_worker_id IS NOT DISTINCT FROM @worker_id::uuid
          AND runs.checkpoint_withheld_generation IS NOT DISTINCT FROM @generation::bigint
          AND runs.checkpoint_withheld_nonce IS NOT DISTINCT FROM @register_nonce::text
          AND runs.checkpoint_withheld_report_seq IS NOT DISTINCT FROM @report_seq::bigint) AS same_marker,
         (runs.checkpoint_withheld_since IS NOT NULL) AS fact_present,
         (runs.checkpoint_withheld_since IS NOT NULL
          AND runs.checkpoint_withheld_worker_id IS NOT DISTINCT FROM @worker_id::uuid
          AND runs.checkpoint_withheld_generation IS NOT DISTINCT FROM @generation::bigint
          AND runs.checkpoint_withheld_nonce IS NOT DISTINCT FROM @register_nonce::text
          AND runs.checkpoint_withheld_status_since IS NOT DISTINCT FROM runs.status_since) AS same_episode,
         runs.checkpoint_withheld_since AS prior_since
  FROM runs
  WHERE runs.id = @id::uuid AND runs.worker_id = @worker_id::uuid AND runs.claim_generation = @generation::bigint
    AND runs.claim_released_at IS NULL AND runs.status = 'running'
    AND EXISTS (SELECT 1 FROM workers w WHERE w.id = @worker_id::uuid AND w.snapshot_register_nonce = @register_nonce::text)
  FOR NO KEY UPDATE OF runs
), applied AS (
  UPDATE runs SET
      checkpoint_withheld_since = CASE WHEN target.same_episode THEN target.prior_since
                                       ELSE LEAST(GREATEST(@since::timestamptz, runs.claimed_at), now()) END,
      checkpoint_withheld_status_since = runs.status_since,
      checkpoint_withheld_worker_id = @worker_id::uuid,
      checkpoint_withheld_generation = @generation::bigint,
      checkpoint_withheld_nonce = @register_nonce::text,
      checkpoint_withheld_report_seq = @report_seq::bigint
  FROM target
  WHERE runs.id = target.id AND target.ordered
  RETURNING runs.id
)
SELECT COALESCE((SELECT CASE WHEN t.ordered THEN 'recorded'
                             WHEN t.same_marker AND t.fact_present THEN 'recorded'
                             ELSE 'refused' END
                 FROM target t), 'refused')::text AS outcome;

-- name: ClearRunCheckpointWithheld :one
-- Clear the fact: since and status_since go NULL, while the marker moves to the request's so a
-- later lower-numbered withheld report is refused rather than reviving the fact.
WITH target AS (
  SELECT runs.id,
         (runs.checkpoint_withheld_report_seq IS NULL
          OR runs.checkpoint_withheld_worker_id IS DISTINCT FROM @worker_id::uuid
          OR runs.checkpoint_withheld_generation IS DISTINCT FROM @generation::bigint
          OR runs.checkpoint_withheld_nonce IS DISTINCT FROM @register_nonce::text
          OR @report_seq::bigint > runs.checkpoint_withheld_report_seq) AS ordered,
         (runs.checkpoint_withheld_worker_id IS NOT DISTINCT FROM @worker_id::uuid
          AND runs.checkpoint_withheld_generation IS NOT DISTINCT FROM @generation::bigint
          AND runs.checkpoint_withheld_nonce IS NOT DISTINCT FROM @register_nonce::text
          AND runs.checkpoint_withheld_report_seq IS NOT DISTINCT FROM @report_seq::bigint) AS same_marker,
         (runs.checkpoint_withheld_since IS NOT NULL) AS fact_present,
         (runs.checkpoint_withheld_since IS NOT NULL
          AND runs.checkpoint_withheld_worker_id IS NOT DISTINCT FROM @worker_id::uuid
          AND runs.checkpoint_withheld_generation IS NOT DISTINCT FROM @generation::bigint
          AND runs.checkpoint_withheld_nonce IS NOT DISTINCT FROM @register_nonce::text) AS same_episode,
         runs.checkpoint_withheld_since AS prior_since
  FROM runs
  WHERE runs.id = @id::uuid AND runs.worker_id = @worker_id::uuid AND runs.claim_generation = @generation::bigint
    AND runs.claim_released_at IS NULL AND runs.status = 'running'
    AND EXISTS (SELECT 1 FROM workers w WHERE w.id = @worker_id::uuid AND w.snapshot_register_nonce = @register_nonce::text)
  FOR NO KEY UPDATE OF runs
), applied AS (
  UPDATE runs SET
      checkpoint_withheld_since = NULL,
      checkpoint_withheld_status_since = NULL,
      checkpoint_withheld_worker_id = @worker_id::uuid,
      checkpoint_withheld_generation = @generation::bigint,
      checkpoint_withheld_nonce = @register_nonce::text,
      checkpoint_withheld_report_seq = @report_seq::bigint
  FROM target
  WHERE runs.id = target.id AND target.ordered
  RETURNING runs.id
)
SELECT COALESCE((SELECT CASE WHEN t.ordered THEN 'cleared'
                             WHEN t.same_marker AND NOT t.fact_present THEN 'cleared'
                             ELSE 'refused' END
                 FROM target t), 'refused')::text AS outcome;
