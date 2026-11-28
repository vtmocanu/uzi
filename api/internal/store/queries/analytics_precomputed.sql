-- name: GetAnalyticsPrecomputedDoc :one
-- One stored document by (section, variant_key): a single-statement read, so it sees one
-- complete published generation (issue #4771).
SELECT document, generated_at
FROM analytics_precomputed_docs
WHERE section = $1 AND variant_key = $2;

-- name: ListAnalyticsPrecomputedDocs :many
-- The whole active set of a section in ONE statement, so it is one complete generation.
SELECT variant_key, document, generated_at, invalidation
FROM analytics_precomputed_docs
WHERE section = $1
ORDER BY variant_key;

-- name: AnalyticsPrecomputeStatus :one
-- Present-key count and oldest generated_at among the requested keys of a section, plus the
-- failure status from the state row, in one statement.
SELECT
    (SELECT count(*) FROM analytics_precomputed_docs d
      WHERE d.section = sqlc.arg('section')::text AND d.variant_key = ANY(sqlc.arg('keys')::text[]))::bigint AS present,
    (SELECT min(d.generated_at) FROM analytics_precomputed_docs d
      WHERE d.section = sqlc.arg('section')::text AND d.variant_key = ANY(sqlc.arg('keys')::text[]))::timestamptz AS oldest_generated_at,
    s.failure_count,
    s.last_failure_at
FROM analytics_precomputed_state s
WHERE s.id;

-- name: ReadAnalyticsPrecomputeCounter :one
SELECT invalidation FROM analytics_precomputed_state WHERE id;

-- name: GuardAnalyticsPrecomputePublish :execrows
-- The publish guard: succeeds only while no users or repos delete committed since the counter
-- was read. The UPDATE takes the state row lock a deleting commit also needs.
UPDATE analytics_precomputed_state
SET publications = publications + 1, failure_count = 0, last_failure_at = NULL
WHERE id AND invalidation = $1;

-- name: DeleteAnalyticsPrecomputedSection :exec
DELETE FROM analytics_precomputed_docs WHERE section = $1;

-- name: InsertAnalyticsPrecomputedDoc :exec
INSERT INTO analytics_precomputed_docs (section, variant_key, generated_at, invalidation, document)
VALUES ($1, $2, $3, $4, $5);

-- name: RecordAnalyticsPrecomputeFailure :execrows
UPDATE analytics_precomputed_state
SET failure_count = failure_count + 1, last_failure_at = $1
WHERE id;
