-- +goose Up
-- Comment identity is (namespace, id), not a bare id (ADR-1199, issue 1199). The forge hands the
-- rework detector comments from distinct id spaces (review comments, issue comments, review
-- bodies, ...) whose numeric ids are NOT comparable, so the single scalar high_water mark that
-- 00168 introduced can neither order them nor tell "already consumed" from "new". This
-- migration supersedes 00168's KNOWN LIMITATION (one scalar mark over mixed id spaces).
--
-- The superseding invariant:
--   * identity = namespace + id;
--   * comment_marks is a jsonb object, namespace -> highest consumed id, advanced per key only
--     (never backward, GREATEST semantics, mr_rework_merge_marks);
--   * pending permission-unknown comments are stored as source-qualified keys
--     '<namespace>:<id>' in pending_unknown_keys, filtered on add against the PER-NAMESPACE mark
--     the row had BEFORE the update (mr_rework_merge_pending_keys);
--   * comment_marks IS NULL marks a LEGACY row written by a binary that predates this column.
--     There is deliberately NO DEFAULT: the service bootstraps such a row from the legacy
--     scalar state on its first complete listing (BootstrapMRReworkLedgerMarks), after which
--     the row is non-legacy for good;
--   * high_water is still advanced (GREATEST) so the column stays readable and a binary
--     rollback keeps a sane legacy mark; pending_unknown_ids is frozen after bootstrap and no
--     longer written by the upserts.
-- A binary rollback WITHOUT running this migration's Down leaves comment_marks and
-- pending_unknown_keys stored: the old binary ignores them and updates only the legacy columns,
-- so on re-upgrade an already-bootstrapped row stays non-legacy and is NOT re-bootstrapped (its
-- marks and keys may be stale relative to the legacy columns, but nothing is physically lost).
-- Running Down is different: it drops the new columns.
ALTER TABLE mr_rework_ledger
    ADD COLUMN comment_marks jsonb NULL,
    ADD COLUMN pending_unknown_keys text[] NOT NULL DEFAULT '{}';

-- Every pending key is '<namespace>:<id>': a lowercase/underscore namespace (possibly empty),
-- one colon, then a canonical non-negative decimal id (no leading zeros, at most 19 digits). The
-- digit limit does not bound the value to bigint width (19 digits exceed 9223372036854775807);
-- writers encode int64 ids and key extraction rejects larger values. A NULL element is rejected.
-- Empty array is well formed.
-- +goose StatementBegin
CREATE FUNCTION mr_rework_pending_keys_wellformed(keys text[]) RETURNS boolean
LANGUAGE sql IMMUTABLE
AS $$
  SELECT COALESCE(bool_and(k IS NOT NULL AND k ~ '^[a-z_]*:(0|[1-9][0-9]{0,18})$'), true)
  FROM unnest(keys) AS k
$$;
-- +goose StatementEnd

-- comment_marks is a jsonb object whose every value is a JSON number that is a canonical
-- non-negative integer within bigint (what mr_rework_merge_marks casts to). The regex gates the
-- numeric comparison inside one CASE so a non-numeric value never reaches the cast.
-- +goose StatementBegin
CREATE FUNCTION mr_rework_marks_wellformed(m jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE
AS $$
  SELECT jsonb_typeof(m) = 'object'
     AND NOT EXISTS (
       SELECT 1 FROM jsonb_each(m) AS e
       WHERE NOT (CASE WHEN jsonb_typeof(e.value) = 'number'
                        AND e.value::text ~ '^(0|[1-9][0-9]{0,18})$'
                       THEN e.value::text::numeric <= 9223372036854775807
                       ELSE false END))
$$;
-- +goose StatementEnd

-- Split a key at its LAST colon (split_part with a negative position counts from the end). A
-- key with no colon has no namespace (NULL).
-- +goose StatementBegin
CREATE FUNCTION mr_rework_key_ns(k text) RETURNS text
LANGUAGE sql IMMUTABLE
AS $$
  SELECT CASE WHEN strpos(k, ':') > 0
              THEN left(k, -length(split_part(k, ':', -1)) - 1) END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mr_rework_key_id(k text) RETURNS bigint
LANGUAGE sql IMMUTABLE
AS $$
  SELECT split_part(k, ':', -1)::bigint
$$;
-- +goose StatementEnd

-- Per-key maximum over the union of keys of two mark objects. NOT STRICT: any non-object input
-- (SQL NULL, JSON null, an array or a scalar) is the empty object. jsonb_object_agg over zero rows is NULL, so the result is COALESCEd to
-- '{}'.
-- +goose StatementBegin
CREATE FUNCTION mr_rework_merge_marks(existing jsonb, incoming jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE
AS $$
  SELECT COALESCE(jsonb_object_agg(m.k, to_jsonb(m.v) ORDER BY m.k), '{}'::jsonb)
  FROM (
    SELECT u.k, max(u.v) AS v
    FROM (
      SELECT e.key AS k, e.value::bigint AS v
        FROM jsonb_each_text(CASE WHEN jsonb_typeof(existing) = 'object' THEN existing ELSE '{}'::jsonb END) AS e
      UNION ALL
      SELECT i.key AS k, i.value::bigint AS v
        FROM jsonb_each_text(CASE WHEN jsonb_typeof(incoming) = 'object' THEN incoming ELSE '{}'::jsonb END) AS i
    ) u
    GROUP BY u.k
  ) m
$$;
-- +goose StatementEnd

-- The key-based port of mr_rework_merge_pending (00307), shared by the automatic upsert and the
-- on-demand atomic create. The base set is (existing UNION added keys whose id is above the
-- PER-NAMESPACE mark in prior_marks, the marks the row had BEFORE this update) EXCEPT removed
-- keys. Supersession is conditional exactly as in 00307: the parallel arrays superseded /
-- superseded_by name (older key, newer key) pairs, a pair applies only when BOTH keys are in the
-- base set, and an older key is dropped only when it is not itself the replacement of another
-- applied pair.
-- ORDER: the stored admission order is the primary order. Existing keys keep their array
-- position; a kept replacement takes the position of the earliest-positioned older key it
-- replaces (the minimum with its own position when it was already stored), so two keys can share
-- a position. Only keys that share a position (ties) and newly appended keys (placed after every
-- stored position) are ordered by (id, namespace), numerically: a deterministic tiebreak, not an
-- age claim across namespaces, whose ids are not comparable. The first 10000 in that order are
-- kept, so on overflow the oldest-admitted are kept and a later flood cannot displace an earlier
-- finding.
-- +goose StatementBegin
CREATE FUNCTION mr_rework_merge_pending_keys(
    existing text[],
    added text[],
    removed text[],
    superseded text[],
    superseded_by text[],
    prior_marks jsonb
) RETURNS text[]
LANGUAGE sql IMMUTABLE
AS $$
  WITH ex AS (
    SELECT u.k, min(u.ord) AS pos
    FROM unnest(COALESCE(existing, '{}'::text[])) WITH ORDINALITY AS u(k, ord)
    WHERE u.k IS NOT NULL
    GROUP BY u.k
  ),
  add_f AS (
    SELECT DISTINCT a.k
    FROM unnest(COALESCE(added, '{}'::text[])) AS a(k)
    WHERE a.k IS NOT NULL
      AND mr_rework_key_id(a.k) >
          COALESCE((COALESCE(prior_marks, '{}'::jsonb) ->> mr_rework_key_ns(a.k))::bigint, 0)
  ),
  base AS (
    (SELECT ex.k FROM ex
     UNION
     SELECT add_f.k FROM add_f)
    EXCEPT SELECT r FROM unnest(COALESCE(removed, '{}'::text[])) AS r
  ),
  pairs AS (
    SELECT DISTINCT p.o, p.n
    FROM unnest(COALESCE(superseded, '{}'::text[]), COALESCE(superseded_by, '{}'::text[])) AS p(o, n)
    WHERE p.o IS NOT NULL AND p.n IS NOT NULL AND p.o <> p.n
  ),
  eff AS (
    SELECT pairs.o, pairs.n FROM pairs
    JOIN base bo ON bo.k = pairs.o
    JOIN base bn ON bn.k = pairs.n
  ),
  dropped AS (SELECT eff.o AS k FROM eff EXCEPT SELECT eff.n FROM eff),
  kept AS (SELECT base.k FROM base EXCEPT SELECT dropped.k FROM dropped),
  slotted AS (
    SELECT kp.k,
           LEAST(ex.pos, s.slot) AS pos
    FROM kept kp
    LEFT JOIN ex ON ex.k = kp.k
    LEFT JOIN (
      SELECT eff.n, min(eo.pos) AS slot
      FROM eff JOIN ex eo ON eo.k = eff.o
      GROUP BY eff.n
    ) s ON s.n = kp.k
  ),
  ranked AS (
    SELECT sl.k,
           row_number() OVER (
             ORDER BY (sl.pos IS NULL), sl.pos,
                      mr_rework_key_id(sl.k), mr_rework_key_ns(sl.k), sl.k
           ) AS rn
    FROM slotted sl
  )
  SELECT COALESCE(array_agg(t.k ORDER BY t.rn), '{}'::text[])
  FROM (SELECT ranked.k, ranked.rn FROM ranked ORDER BY ranked.rn LIMIT 10000) t
$$;
-- +goose StatementEnd

ALTER TABLE mr_rework_ledger
    ADD CONSTRAINT mr_rework_ledger_comment_marks_wellformed
        CHECK (comment_marks IS NULL OR mr_rework_marks_wellformed(comment_marks)),
    ADD CONSTRAINT mr_rework_ledger_pending_unknown_keys_check
        CHECK (cardinality(pending_unknown_keys) <= 10000),
    ADD CONSTRAINT mr_rework_ledger_pending_unknown_keys_wellformed
        CHECK (mr_rework_pending_keys_wellformed(pending_unknown_keys));

-- +goose Down
ALTER TABLE mr_rework_ledger
    DROP CONSTRAINT mr_rework_ledger_pending_unknown_keys_wellformed,
    DROP CONSTRAINT mr_rework_ledger_pending_unknown_keys_check,
    DROP CONSTRAINT mr_rework_ledger_comment_marks_wellformed,
    DROP COLUMN pending_unknown_keys,
    DROP COLUMN comment_marks;
DROP FUNCTION mr_rework_merge_pending_keys(text[], text[], text[], text[], text[], jsonb);
DROP FUNCTION mr_rework_merge_marks(jsonb, jsonb);
DROP FUNCTION mr_rework_key_id(text);
DROP FUNCTION mr_rework_key_ns(text);
DROP FUNCTION mr_rework_marks_wellformed(jsonb);
DROP FUNCTION mr_rework_pending_keys_wellformed(text[]);
