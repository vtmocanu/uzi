-- The caller holds the run lock and has fenced ownership and claim generation.
-- Any pending bound receipt suffices; queue order does not grant authority.
-- name: HasPendingSemanticRevision :one
SELECT EXISTS (
    SELECT 1 FROM run_user_inputs
    WHERE run_id = @run_id AND kind = 'revise_plan'
      -- Empty feedback is ignored by steering; Unicode whitespace is empty too.
      AND btrim(body, E' \t\n\r\f\013\u0085\u00A0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A\u2028\u2029\u202F\u205F\u3000\uFEFF') <> ''
      AND disposition IS DISTINCT FROM 'superseded'
      AND applied_at IS NULL AND consumed_at IS NOT NULL
      AND consumed_worker_id = @worker_id
      AND consumed_claim_generation = @claim_generation
      AND gate_binding = 'bound' AND gate_revision = @gate_revision
)::boolean;

-- All writes are made under the runs row lock by the semantic transaction seam.
-- No query creates semantic enrollment.
-- name: GetSemanticPlanning :one
SELECT enrollment, revision, fingerprint, candidate, review_state
FROM semantic_planning WHERE run_id = $1;

-- name: GetSemanticSnapshot :one
SELECT raw FROM semantic_snapshots WHERE run_id = $1 AND hash = $2;

-- name: GetSemanticSnapshotBytes :one
SELECT COALESCE(sum(octet_length(raw)), 0)::bigint AS bytes
FROM semantic_snapshots WHERE run_id = $1;

-- name: HasSemanticSnapshot :one
SELECT EXISTS(SELECT 1 FROM semantic_snapshots WHERE run_id = $1 AND hash = $2)::boolean;

-- name: InsertSemanticSnapshot :exec
INSERT INTO semantic_snapshots(run_id, hash, raw) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: InsertSemanticSourceSnapshot :exec
INSERT INTO semantic_source_snapshots
(run_id, identity_hash, source_id, kind, hash, provenance, completeness)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING;

-- name: InsertSemanticCandidateHistory :exec
INSERT INTO semantic_candidate_history(run_id, revision, candidate) VALUES ($1, $2, $3);

-- name: UpdateSemanticPlanning :execrows
UPDATE semantic_planning SET revision=$2, fingerprint=$3, candidate=$4
WHERE run_id=$1;

-- name: UpdateSemanticReviewState :exec
UPDATE semantic_planning SET review_state=$2 WHERE run_id=$1;

-- name: GetSemanticCandidateByDigest :one
SELECT candidate FROM semantic_candidate_history
WHERE run_id=$1 AND candidate->>'digest'=$2::text;
