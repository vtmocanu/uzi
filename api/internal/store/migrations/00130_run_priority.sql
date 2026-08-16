-- +goose Up
ALTER TABLE runs ADD COLUMN priority SMALLINT NULL;

-- +goose StatementBegin
CREATE FUNCTION fn_run_priority(
    run_kind text,
    priority smallint,
    is_stale boolean
) RETURNS smallint
LANGUAGE sql IMMUTABLE
AS $$
    -- PRD #320 D1/D2: the SINGLE claim-ordering RANK expression, called by
    -- ClaimRun's ORDER BY (between resume-affinity and FIFO by created_at).
    --
    -- `priority` is the nullable manual override on runs.priority: NULL means
    -- "no override — use the kind default". A non-null override always wins,
    -- so Expedite writes 2 (above normal) and undo writes NULL (back to the
    -- default). SMALLINT leaves headroom for a future demote value (e.g. -1)
    -- with no migration.
    --
    -- The kind default is the DEMOTION PREDICATE, shared verbatim with
    -- fn_run_priority_class below so rank and display class can never disagree:
    -- a non-stale judge/self_improve run is background (0); everything else
    -- (interactive kinds, and a demoted run past its grace window) is normal (1).
    -- `is_stale` is the caller-supplied fail-open flag (created_at past the
    -- background-grace cutoff): a stale demoted run ranks 1 so background work
    -- can never starve.
    --
    -- D5: adding a kind to the demotion set edits ONLY this migration (both
    -- functions' IN-list), never any Go.
    SELECT COALESCE(
        priority,
        CASE WHEN run_kind IN ('judge', 'self_improve') AND NOT is_stale
             THEN 0::smallint   -- background (demoted)
             ELSE 1::smallint   -- normal (interactive / stale-restored)
        END
    );
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION fn_run_priority_class(
    run_kind text,
    priority smallint,
    is_stale boolean
) RETURNS text
LANGUAGE sql IMMUTABLE
AS $$
    -- PRD #320 D1/D8: the SINGLE display CLASS expression, in
    -- {expedited, background, restored, normal}, called by the run read
    -- queries so the pill (web/CLI) and the claim rank are one SQL decision.
    --
    -- The class carries a distinction the rank cannot: a `restored` run
    -- (demoted but past grace) and a `normal` run both RANK 1, so the label is
    -- derived separately — but from the SAME demotion predicate as
    -- fn_run_priority, so the two can never drift. The KEY invariant:
    -- whenever fn_run_priority(...) = 0 this returns `background`, and a
    -- demoted-but-stale run ranks 1 (via `AND NOT is_stale` there) while its
    -- class is `restored` here.
    --
    -- A non-null override is named by its value (with headroom for a future
    -- demote value below 0): >= 2 → expedited, <= 0 → background, else normal.
    -- With no override (NULL), a judge/self_improve run is `restored` when
    -- stale (past grace) else `background`; every other kind is `normal`.
    --
    -- D5: adding a kind to the demotion set edits ONLY this migration (the
    -- IN-list here and in fn_run_priority above), never any Go.
    SELECT CASE
        WHEN priority IS NOT NULL THEN
            CASE
                WHEN priority >= 2 THEN 'expedited'
                WHEN priority <= 0 THEN 'background'
                ELSE 'normal'
            END
        WHEN run_kind IN ('judge', 'self_improve') THEN
            CASE WHEN is_stale THEN 'restored' ELSE 'background' END
        ELSE 'normal'
    END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS fn_run_priority_class(text, smallint, boolean);
DROP FUNCTION IF EXISTS fn_run_priority(text, smallint, boolean);
ALTER TABLE runs DROP COLUMN IF EXISTS priority;
