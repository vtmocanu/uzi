-- Estimated usage tail (issue #2014, ADR-2014). The service layer is
-- workersvc/usage_tail.go (writes) and workersvc/usage_tail_read.go (the read). Nothing here
-- touches run_usage or run_usage_totals: the tail is a separate, estimated figure.

-- name: LockRunUsage :exec
-- Serializes every writer of ONE run's usage-tail rows: the /usage route's transaction and the
-- incremental fold's leg upsert. It is the FIRST statement of both, before any row write, so the
-- per-run caps (counted under this lock) can never be passed by two concurrent transactions and
-- the two paths cannot deadlock (they only ever take this one lock before touching rows).
--
-- Two-int advisory lock: class 1970959731 = 0x757A7573 ("uzus"), the value of
-- store.RunUsageLockClass in migrate.go; TestRunUsageLockClassMatchesSQL pins this literal to
-- it. XACT-scoped: released on commit or rollback, so it must run on a transaction-bound
-- Queries (on a bare pool it would release immediately).
SELECT pg_advisory_xact_lock(
    1970959731,
    hashtext(sqlc.arg(run_id)::uuid::text)
);

-- name: RunUsageFenceLive :one
-- The /usage route's claim fence: the SAME predicate InsertRunMessage's generation_live reads
-- (unreleased claim, and the claim generation equal to the stamped one when one is stamped).
SELECT EXISTS (SELECT 1 FROM runs r
               WHERE r.id = sqlc.arg(run_id)
                 AND r.claim_released_at IS NULL
                 AND (sqlc.narg('claim_generation')::bigint IS NULL
                      OR r.claim_generation = sqlc.narg('claim_generation')::bigint)) AS live;

-- name: RunUsageFenceLiveLocked :one
-- RecordRunUsage's recheck, the LAST statement before Commit. Same predicate as RunUsageFenceLive
-- plus the claimant's worker_id, and it row-locks the runs row with FOR SHARE. READ COMMITTED and
-- the release/reclaim UPDATEs do not take the usage advisory lock, so a per-statement fence is
-- insufficient: a release that committed before this statement is seen here (re-checked on the new
-- row version), and one that starts after it waits for our commit. An FK leg insert may already hold
-- FOR KEY SHARE on the row; this upgrades it. Precedent: judge.sql's `live` CTE.
SELECT EXISTS (SELECT 1 FROM runs r
               WHERE r.id = sqlc.arg(run_id)
                 AND r.worker_id = sqlc.arg(worker_id)
                 AND r.claim_released_at IS NULL
                 AND (sqlc.narg('claim_generation')::bigint IS NULL
                      OR r.claim_generation = sqlc.narg('claim_generation')::bigint)
               FOR SHARE) AS live;

-- name: CountRunUsageLegs :one
SELECT count(*) FROM run_usage_legs WHERE run_id = $1;

-- name: CountRunUsageMessages :one
SELECT count(*) FROM run_usage_messages WHERE run_id = $1;

-- name: ListExistingRunUsageLegIDs :many
SELECT leg_id FROM run_usage_legs
WHERE run_id = sqlc.arg(run_id) AND leg_id = ANY(sqlc.arg(leg_ids)::uuid[]);

-- name: ListExistingRunUsageMessageIDs :many
SELECT message_id FROM run_usage_messages
WHERE run_id = sqlc.arg(run_id) AND message_id = ANY(sqlc.arg(message_ids)::text[]);

-- name: UpsertRunUsageLegMarker :exec
-- A leg's close marker, or just its existence (both narg NULL / 0). Monotone: closed_through and
-- dropped_records merge with GREATEST (which ignores a NULL side); identity and coverage columns
-- are not touched. claim_generation is provenance: COALESCE keeps an established value.
INSERT INTO run_usage_legs (run_id, leg_id, closed_through, dropped_records, claim_generation)
VALUES (sqlc.arg(run_id), sqlc.arg(leg_id), sqlc.narg('closed_through')::int, sqlc.arg(dropped_records)::bigint, sqlc.narg('claim_generation')::bigint)
ON CONFLICT (run_id, leg_id) DO UPDATE SET
    closed_through   = GREATEST(run_usage_legs.closed_through, EXCLUDED.closed_through),
    dropped_records  = GREATEST(run_usage_legs.dropped_records, EXCLUDED.dropped_records),
    claim_generation = COALESCE(run_usage_legs.claim_generation, EXCLUDED.claim_generation);

-- name: UpsertRunUsageLegInit :exec
-- The fold's leg identity from a persisted init frame: init_seq = the frame's seq and
-- sdk_session_id = the stamped SDK session, each COALESCE(existing, new) so a re-delivery or a
-- later stamp never moves an established value. Touches no other column.
INSERT INTO run_usage_legs (run_id, leg_id, init_seq, sdk_session_id, claim_generation)
VALUES (sqlc.arg(run_id), sqlc.arg(leg_id), sqlc.arg(init_seq)::bigint, sqlc.arg(sdk_session_id)::text, sqlc.narg('claim_generation')::bigint)
ON CONFLICT (run_id, leg_id) DO UPDATE SET
    init_seq         = COALESCE(run_usage_legs.init_seq, EXCLUDED.init_seq),
    sdk_session_id   = COALESCE(run_usage_legs.sdk_session_id, EXCLUDED.sdk_session_id),
    claim_generation = COALESCE(run_usage_legs.claim_generation, EXCLUDED.claim_generation);

-- name: UpsertRunUsageLegCoverage :exec
-- The fold's coverage from a result frame: covered_through = GREATEST(existing, usage_through),
-- covered_cumulative = existing OR (basis = session_cumulative). Touches no other column.
INSERT INTO run_usage_legs (run_id, leg_id, covered_through, covered_cumulative, claim_generation)
VALUES (sqlc.arg(run_id), sqlc.arg(leg_id), sqlc.arg(covered_through)::int, sqlc.arg(covered_cumulative)::boolean, sqlc.narg('claim_generation')::bigint)
ON CONFLICT (run_id, leg_id) DO UPDATE SET
    covered_through    = GREATEST(run_usage_legs.covered_through, EXCLUDED.covered_through),
    covered_cumulative = run_usage_legs.covered_cumulative OR EXCLUDED.covered_cumulative,
    claim_generation   = COALESCE(run_usage_legs.claim_generation, EXCLUDED.claim_generation);

-- name: UpsertRunUsageMessage :exec
-- One per-message usage record, keyed (run_id, message_id). Monotone: every token column and the
-- cache split merge with GREATEST (consecutive assistant frames share one message id and carry
-- partial snapshots), output_final ORs, identity text is COALESCE(existing, new). A re-post that
-- names a DIFFERENT leg or ordinal than the stored row sets conflict = true (and keeps the stored
-- leg/ordinal): the read treats a conflicting row as unresolved and never counts it.
INSERT INTO run_usage_messages (
    run_id, message_id, leg_id, ordinal, frame_session_id, model, subagent,
    input_tokens, cache_read_input_tokens, cache_creation_input_tokens,
    cache_creation_5m_input_tokens, cache_creation_1h_input_tokens, output_tokens,
    output_final, service_tier, speed, inference_geo, claim_generation)
VALUES (
    sqlc.arg(run_id), sqlc.arg(message_id), sqlc.arg(leg_id), sqlc.arg(ordinal)::int,
    sqlc.narg('frame_session_id')::text, sqlc.arg(model), sqlc.arg(subagent)::boolean,
    sqlc.arg(input_tokens)::bigint, sqlc.arg(cache_read_input_tokens)::bigint, sqlc.arg(cache_creation_input_tokens)::bigint,
    sqlc.narg('cache_creation_5m_input_tokens')::bigint, sqlc.narg('cache_creation_1h_input_tokens')::bigint, sqlc.arg(output_tokens)::bigint,
    sqlc.arg(output_final)::boolean, sqlc.narg('service_tier')::text, sqlc.narg('speed')::text, sqlc.narg('inference_geo')::text,
    sqlc.narg('claim_generation')::bigint)
ON CONFLICT (run_id, message_id) DO UPDATE SET
    input_tokens                   = GREATEST(run_usage_messages.input_tokens, EXCLUDED.input_tokens),
    cache_read_input_tokens        = GREATEST(run_usage_messages.cache_read_input_tokens, EXCLUDED.cache_read_input_tokens),
    cache_creation_input_tokens    = GREATEST(run_usage_messages.cache_creation_input_tokens, EXCLUDED.cache_creation_input_tokens),
    cache_creation_5m_input_tokens = GREATEST(run_usage_messages.cache_creation_5m_input_tokens, EXCLUDED.cache_creation_5m_input_tokens),
    cache_creation_1h_input_tokens = GREATEST(run_usage_messages.cache_creation_1h_input_tokens, EXCLUDED.cache_creation_1h_input_tokens),
    output_tokens                  = GREATEST(run_usage_messages.output_tokens, EXCLUDED.output_tokens),
    output_final                   = run_usage_messages.output_final OR EXCLUDED.output_final,
    frame_session_id               = COALESCE(run_usage_messages.frame_session_id, EXCLUDED.frame_session_id),
    service_tier                   = COALESCE(run_usage_messages.service_tier, EXCLUDED.service_tier),
    speed                          = COALESCE(run_usage_messages.speed, EXCLUDED.speed),
    inference_geo                  = COALESCE(run_usage_messages.inference_geo, EXCLUDED.inference_geo),
    claim_generation               = COALESCE(run_usage_messages.claim_generation, EXCLUDED.claim_generation),
    conflict                       = run_usage_messages.conflict
                                     OR run_usage_messages.leg_id <> EXCLUDED.leg_id
                                     OR run_usage_messages.ordinal <> EXCLUDED.ordinal;

-- name: FlagRunUsageOrdinalConflicts :exec
-- Two DIFFERENT message ids claiming the same (leg, ordinal) are an ordinal conflict: flag every
-- row sharing such an ordinal in the named legs. Run once per post over the legs it touched, under
-- the run's usage lock. Never un-flags.
UPDATE run_usage_messages m
SET conflict = true
WHERE m.run_id = sqlc.arg(run_id)
  AND m.leg_id = ANY(sqlc.arg(leg_ids)::uuid[])
  AND NOT m.conflict
  AND EXISTS (SELECT 1 FROM run_usage_messages o
              WHERE o.run_id = m.run_id AND o.leg_id = m.leg_id
                AND o.ordinal = m.ordinal AND o.message_id <> m.message_id);

-- name: UpsertRunUsageTailState :exec
-- Record that the api dropped records or legs past a per-run cap. Monotone: the flag only
-- sets, the counters only add.
INSERT INTO run_usage_tail_state (run_id, record_cap_reached, capped_records, capped_legs)
VALUES (sqlc.arg(run_id), true, sqlc.arg(capped_records)::bigint, sqlc.arg(capped_legs)::bigint)
ON CONFLICT (run_id) DO UPDATE SET
    record_cap_reached = true,
    capped_records     = run_usage_tail_state.capped_records + EXCLUDED.capped_records,
    capped_legs        = run_usage_tail_state.capped_legs + EXCLUDED.capped_legs;

-- name: GetRunUsageTailState :one
SELECT run_id, record_cap_reached, capped_records, capped_legs
FROM run_usage_tail_state WHERE run_id = $1;

-- name: ListRunUsageLegsForTail :many
-- Every leg of the run with the per-leg aggregates the coverage rule needs, so the read never
-- loads the covered messages. n_uncovered_own counts non-conflicting messages the leg's OWN
-- result (arm a) did not cover.
SELECT l.leg_id, l.init_seq, l.sdk_session_id, l.closed_through, l.covered_through,
       l.covered_cumulative, l.dropped_records,
       COALESCE(a.distinct_ordinals, 0)::bigint AS distinct_ordinals,
       COALESCE(a.max_ordinal, 0)::int AS max_ordinal,
       COALESCE(a.any_not_final, false)::boolean AS any_not_final,
       COALESCE(a.n_conflict, 0)::bigint AS n_conflict,
       COALESCE(a.n_uncovered_own, 0)::bigint AS n_uncovered_own
FROM run_usage_legs l
LEFT JOIN LATERAL (
    SELECT count(DISTINCT m.ordinal) AS distinct_ordinals,
           max(m.ordinal) AS max_ordinal,
           bool_or(NOT m.output_final) AS any_not_final,
           count(*) FILTER (WHERE m.conflict) AS n_conflict,
           count(*) FILTER (WHERE NOT m.conflict
                              AND (l.covered_through IS NULL OR m.ordinal > l.covered_through)) AS n_uncovered_own
    FROM run_usage_messages m
    WHERE m.run_id = l.run_id AND m.leg_id = l.leg_id
) a ON true
WHERE l.run_id = $1
ORDER BY l.init_seq NULLS LAST, l.leg_id;

-- name: ListRunUsageTailMessages :many
-- The tail: messages whose leg has a known init_seq and sdk_session_id, that are not conflicting,
-- and that NO leg's result covered. Coverage predicate (ADR-2014 D4), NULL-safe (a NULL on either
-- side of a comparison is not a match):
--   arm (a) the message's own leg's result covered its ordinal, or
--   arm (b) a LATER leg of the SAME SDK session reported a session-cumulative total.
-- Arm (b) is evaluated once per leg (lg), not once per message.
WITH lg AS (
    SELECT l.leg_id, l.covered_through
    FROM run_usage_legs l
    WHERE l.run_id = $1
      AND l.init_seq IS NOT NULL
      AND l.sdk_session_id IS NOT NULL
      AND NOT EXISTS (
          SELECT 1 FROM run_usage_legs c
          WHERE c.run_id = l.run_id
            AND c.covered_through IS NOT NULL
            AND c.covered_cumulative
            AND c.sdk_session_id = l.sdk_session_id
            AND c.init_seq > l.init_seq)
)
SELECT m.message_id, m.leg_id, m.ordinal, m.model, m.subagent,
       m.input_tokens, m.cache_read_input_tokens, m.cache_creation_input_tokens,
       m.cache_creation_5m_input_tokens, m.cache_creation_1h_input_tokens, m.output_tokens,
       m.output_final, m.service_tier, m.speed, m.inference_geo
FROM run_usage_messages m
JOIN lg ON lg.leg_id = m.leg_id
WHERE m.run_id = $1
  AND NOT m.conflict
  AND (lg.covered_through IS NULL OR m.ordinal > lg.covered_through)
ORDER BY m.leg_id, m.ordinal, m.message_id;
