-- name: SnapshotOwnerSteering :many
-- Issue #3841: the bounded raw material of the owner-steering snapshot a plan cross-check
-- checker child receives. One statement, so one MVCC snapshot: the cut, the total, the kept
-- rows and every question context are read from the same view of the tables.
--
-- cut   the newest run_user_inputs id of the run (0 when none): the cutoff the snapshot names.
-- sel   the owner's authoritative in-run steering at or before the cut: answers to questions
--       (question_id present) and revise_plan feedback. follow_up, approve_plan, scope and
--       every other kind are not steering for the checker.
-- tot   how many rows sel holds, so the builder can report entries omitted by the cap.
-- kept  the newest @max_entries rows of sel. The caller reorders them ascending by id.
--
-- The answer and question-context shape checks are ORDERED NESTED CASE arms: each guard runs
-- before any cast or set-returning expansion it protects, because run_user_inputs.body is bare
-- text shared with every kind and a boolean AND/OR gives no evaluation-order guarantee. A body
-- or payload of the wrong shape yields a status string and no text; the statement never errors
-- and never stringifies a non-string element.
--
-- Limits mirrored from the writers: maxAnswerCount = 10 (workersvc submitAnswer), MAX_QUESTIONS
-- and MAX_OPTIONS = 10 (agent/src/signals.ts). Text is cut here only to bound the transfer, and
-- every left() keeps 1024 characters beyond the authoritative cap (revise 8192, answers 2048,
-- header 128, question 2048, option labels 256) so a secret that straddles the cap is delivered
-- whole to the Go builder, which scrubs FIRST and applies the authoritative caps second. A cut at
-- the cap itself would leave a short token prefix that no scrub pattern matches. The *_truncated
-- flags stay defined against the ORIGINAL caps, never the margin.
--
-- A completion-continue answer is NOT plan steering and is excluded in sel itself, so it takes no
-- kept slot and is not counted in tot. resumeCompletionBlocked (workersvc) writes that answer
-- against the run's open completion question and then writes the completion_decision audit row
-- carrying the SAME question_id (CreateCompletionDecisionInput); the two writes are
-- non-transactional on the default continue path, so their timestamps differ and the shared
-- question_id is the only link. sel drops an answer whose question_id has a completion_decision
-- row on the run. The link is a server-written fact, never lead-authored question text.
--
-- Every CTE that is referenced more than once or runs under a locked lead row is MATERIALIZED:
-- under pgx statement caching Postgres can pick a generic plan in which an inlined CTE is
-- re-evaluated per outer row (measured 2.5 s against 16 ms).
WITH cut AS (
    SELECT COALESCE(max(u.id), 0)::bigint AS cutoff
    FROM run_user_inputs u
    WHERE u.run_id = @run_id
), sel AS (
    SELECT i.*
    FROM run_user_inputs i, cut
    WHERE i.run_id = @run_id
      AND i.id <= cut.cutoff
      AND ((i.kind = 'answer' AND i.question_id IS NOT NULL) OR i.kind = 'revise_plan')
      AND NOT (i.kind = 'answer' AND EXISTS (
            SELECT 1 FROM run_user_inputs d
            WHERE d.run_id = @run_id AND d.kind = 'completion_decision' AND d.question_id = i.question_id))
), tot AS (
    SELECT count(*)::bigint AS total FROM sel
), kept AS (
    SELECT * FROM sel ORDER BY id DESC LIMIT @max_entries::int
), ans AS MATERIALIZED (
    SELECT k.id,
           CASE
               WHEN k.kind <> 'answer' THEN NULL
               WHEN k.body IS NULL OR NOT pg_input_is_valid(k.body, 'jsonb') THEN 'unparseable'
               ELSE CASE
                   WHEN jsonb_typeof(k.body::jsonb) <> 'object' THEN 'malformed'
                   ELSE CASE
                       WHEN jsonb_typeof(k.body::jsonb -> 'answers') IS DISTINCT FROM 'array' THEN 'malformed'
                       ELSE CASE
                           WHEN jsonb_array_length(k.body::jsonb -> 'answers') > 10 THEN 'oversized'
                           WHEN EXISTS (SELECT 1 FROM jsonb_array_elements(k.body::jsonb -> 'answers') e
                                        WHERE jsonb_typeof(e) <> 'string') THEN 'malformed'
                           ELSE 'ok'
                       END
                   END
               END
           END AS status
    FROM kept k
), qmsg AS MATERIALIZED (
    -- the run's question messages for the kept answers' question ids, read once for occ and first_occ
    SELECT m.seq, m.agent, m.payload, m.created_at, m.payload ->> 'question_id' AS qid
    FROM run_messages m
    WHERE m.run_id = @run_id AND m.kind = 'question'
      AND m.payload ->> 'question_id' IN (SELECT question_id FROM kept WHERE kind = 'answer')
), occ AS MATERIALIZED (
    SELECT k.id AS answer_id,
           count(*)::bigint AS occurrences,
           count(DISTINCT (m.payload -> 'questions', m.agent))::bigint AS variants
    FROM kept k
    JOIN qmsg m ON m.qid = k.question_id AND m.created_at <= k.created_at
    WHERE k.kind = 'answer'
    GROUP BY k.id
), first_occ AS MATERIALIZED (
    SELECT DISTINCT ON (k.id) k.id AS answer_id, m.agent, m.payload
    FROM kept k
    JOIN qmsg m ON m.qid = k.question_id AND m.created_at <= k.created_at
    WHERE k.kind = 'answer'
    ORDER BY k.id, m.seq
), qctx AS MATERIALIZED (
    SELECT f.answer_id, f.agent, f.payload,
           CASE
               WHEN jsonb_typeof(f.payload -> 'questions') IS DISTINCT FROM 'array' THEN 'malformed'
               ELSE CASE
                   WHEN jsonb_array_length(f.payload -> 'questions') > 10 THEN 'oversized'
                   WHEN EXISTS (
                       SELECT 1 FROM jsonb_array_elements(f.payload -> 'questions') q
                       WHERE jsonb_typeof(q) IS DISTINCT FROM 'object'
                          OR jsonb_typeof(q -> 'question') IS DISTINCT FROM 'string'
                          OR jsonb_typeof(q -> 'header') NOT IN ('string')
                          OR jsonb_typeof(q -> 'multiSelect') NOT IN ('boolean')
                          OR jsonb_typeof(q -> 'options') NOT IN ('array')
                          OR CASE WHEN jsonb_typeof(q -> 'options') = 'array'
                                  THEN jsonb_array_length(q -> 'options') > 10
                                  ELSE false END
                          OR CASE WHEN jsonb_typeof(q -> 'options') = 'array'
                                  THEN EXISTS (SELECT 1 FROM jsonb_array_elements(q -> 'options') o
                                               WHERE jsonb_typeof(o) IS DISTINCT FROM 'object'
                                                  OR jsonb_typeof(o -> 'label') IS DISTINCT FROM 'string')
                                  ELSE false END
                   ) THEN 'malformed'
                   ELSE 'ok'
               END
           END AS status
    FROM first_occ f
), proj AS MATERIALIZED (
    SELECT k.id AS input_id,
           (CASE WHEN k.kind = 'revise_plan' THEN left(k.body, 8192 + 1024) END)::text AS revise_body,
           (CASE WHEN k.kind = 'revise_plan' THEN char_length(k.body) > 8192 END)::boolean AS revise_truncated,
           ans.status::text AS answers_status,
           (CASE WHEN ans.status = 'ok' THEN
               (SELECT COALESCE(jsonb_agg(left(e #>> '{}', 2048 + 1024) ORDER BY ord), '[]'::jsonb)
                  FROM jsonb_array_elements(k.body::jsonb -> 'answers') WITH ORDINALITY t(e, ord))
           END)::jsonb AS answers,
           (CASE WHEN ans.status = 'ok' THEN
               EXISTS (SELECT 1 FROM jsonb_array_elements(k.body::jsonb -> 'answers') e
                        WHERE char_length(e #>> '{}') > 2048)
           END)::boolean AS answers_truncated,
           COALESCE(occ.occurrences, 0)::bigint AS occurrences,
           COALESCE(occ.variants, 0)::bigint AS variants,
           qctx.agent AS question_agent,
           qctx.status::text AS questions_status,
           (CASE WHEN qctx.status = 'ok' THEN
               (SELECT COALESCE(jsonb_agg(jsonb_build_object(
                           'header', left(COALESCE(q ->> 'header', ''), 128 + 1024),
                           'question', left(q ->> 'question', 2048 + 1024),
                           'options', COALESCE((SELECT jsonb_agg(left(o ->> 'label', 256 + 1024) ORDER BY oord)
                                                  FROM jsonb_array_elements(q -> 'options') WITH ORDINALITY ot(o, oord)), '[]'::jsonb),
                           'multi_select', COALESCE((q ->> 'multiSelect')::boolean, false)
                       ) ORDER BY qord), '[]'::jsonb)
                  FROM jsonb_array_elements(qctx.payload -> 'questions') WITH ORDINALITY qt(q, qord))
           END)::jsonb AS questions,
           (CASE WHEN qctx.status = 'ok' THEN
               EXISTS (SELECT 1 FROM jsonb_array_elements(qctx.payload -> 'questions') q
                        WHERE char_length(q ->> 'header') > 128
                           OR char_length(q ->> 'question') > 2048
                           OR EXISTS (SELECT 1 FROM jsonb_array_elements(q -> 'options') o
                                       WHERE char_length(o ->> 'label') > 256))
           END)::boolean AS questions_truncated
    FROM kept k
    LEFT JOIN ans ON ans.id = k.id
    LEFT JOIN occ ON occ.answer_id = k.id
    LEFT JOIN qctx ON qctx.answer_id = k.id
)
SELECT cut.cutoff,
       tot.total,
       k.id AS input_id,
       k.kind AS input_kind,
       k.question_id,
       k.gate_binding,
       k.gate_revision,
       k.created_at AS input_created_at,
       proj.revise_body,
       proj.revise_truncated,
       proj.answers_status,
       proj.answers,
       proj.answers_truncated,
       proj.occurrences,
       proj.variants,
       proj.question_agent,
       proj.questions_status,
       proj.questions,
       proj.questions_truncated
FROM cut
CROSS JOIN tot
LEFT JOIN kept k ON true
LEFT JOIN proj ON proj.input_id = k.id
ORDER BY k.id ASC NULLS LAST;

-- name: InsertOwnerSteering :execrows
-- Issue #3841: persist the frozen snapshot for a plan-stage cross-check round, in the same
-- transaction as the round's cross_checks row. Zero rows means the round is not a plan-stage
-- row; the writer refuses rather than create a round without its steering.
INSERT INTO cross_check_owner_steering (cross_check_id, snapshot, digest)
SELECT cc.id, @snapshot::text, @digest::bytea
FROM cross_checks cc
WHERE cc.id = @cross_check_id AND cc.stage = 'plan';

-- name: GetOwnerSteering :one
SELECT cross_check_id, snapshot, digest, created_at
FROM cross_check_owner_steering
WHERE cross_check_id = @cross_check_id;
