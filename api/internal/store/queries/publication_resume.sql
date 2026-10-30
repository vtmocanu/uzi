-- Write-ahead publication intent (issue #1324). The side table is guarded by
-- guard_publication_resume_row: enrollment, candidate updates and retirement require the
-- API opt-in below, set in the same transaction as the writer.

-- name: OptInPublicationResume :exec
SELECT set_config('uzi.publication_resume_api', 'v1', true);

-- name: GetActivePublicationResume :one
SELECT * FROM run_publication_resume p WHERE p.run_id = @run_id AND p.state = 'active';

-- name: GetPublicationResume :one
SELECT * FROM run_publication_resume p WHERE p.run_id = @run_id AND p.enrollment_seq = @enrollment_seq;

-- name: HasInactivePublicationResumeAtGeneration :one
-- True when a retired or completed enrollment of the run exists at or above the generation. A
-- fresh enrollment at such a generation would resurrect a closed one (no resurrection).
SELECT EXISTS (
    SELECT 1 FROM run_publication_resume p
    WHERE p.run_id = @run_id AND p.state <> 'active' AND p.claim_generation >= @claim_generation::bigint
)::boolean AS retired;

-- name: EnrollPublicationResume :one
-- The caller holds the run row lock, so MAX+1 cannot race; the partial unique index backs it.
INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state)
SELECT @run_id::uuid, COALESCE(MAX(p.enrollment_seq), 0) + 1, @claim_generation::bigint, @worker_id::uuid, @intent::jsonb, 'active'
FROM run_publication_resume p WHERE p.run_id = @run_id::uuid
RETURNING *;

-- name: UpdatePublicationCandidate :one
-- Compare-and-swap on candidate_seq; zero rows means a stale seq or a non-active enrollment.
UPDATE run_publication_resume SET intent = @intent::jsonb, candidate_seq = @new_candidate_seq::int
WHERE run_id = @run_id AND enrollment_seq = @enrollment_seq AND state = 'active'
  AND candidate_seq = @expected_candidate_seq::int
RETURNING *;

-- name: RetirePublicationResume :one
UPDATE run_publication_resume SET state = 'retired', retired_reason = @reason::text
WHERE run_id = @run_id AND state = 'active'
RETURNING *;

-- name: CompletePublicationResume :one
UPDATE run_publication_resume SET state = 'completed'
WHERE run_id = @run_id AND state = 'active'
RETURNING *;

-- name: OptInRunFences :exec
-- Both transaction-local opt-ins in one round trip: checkpoint coordinates and publication resume.
SELECT set_config('uzi.task_checkpoint_coordinates_api', 'v1', true),
       set_config('uzi.publication_resume_api', 'v1', true);
