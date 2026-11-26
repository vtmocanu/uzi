-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4700 (admin all-users Spend, window=all): a materialized per-(run, model) fold of
-- run_usage_model_totals (00542), plus the freshness state that says whether a run's stored
-- fold may be read. Purely additive: two new tables, two functions, one trigger.
--
-- NOTE (goose numbering): drafted as 00543; renumber above the live head at landing via
-- `task migration:renumber` if another migration lands first.
--
-- FRESHNESS. run_usage_fold_state has one row per run that was ever touched by a run_usage
-- writer or the materializer:
--   * no state row  = the run is read LIVE from run_usage_model_totals (missing lane);
--   * stale = true  = the run is read LIVE (stale lane); a writer touched run_usage after
--                     the last fold, or no fold exists yet;
--   * stale = false = the stored run_usage_model_folds rows are the run's fold. Readers may
--                     use them only for a terminal run (the materializer only folds terminal,
--                     non-chat, usage_refolded runs).
-- A run with a fresh state and zero fold rows is a legitimately empty fold.
--
-- WRITE PATH. Every statement that writes run_usage calls fn_run_usage_fold_lock(run_id)
-- first (a CTE the statement references). The function takes FOR KEY SHARE on the runs row,
-- then upserts the state row to stale = true. The state row's UPDATE lock is held until the
-- writer's transaction ends, so the materializer (which takes the state row FOR UPDATE SKIP
-- LOCKED before it reads run_usage) either sees the writer's lock and skips the run, or
-- takes the row first and makes the writer wait; in both orders a fold computed before a
-- write can never be marked fresh over it.
--
-- LOCK ORDER. The runs row is the master lock: a deleting transaction holds it exclusively
-- before the AFTER DELETE trigger touches the state row, writers take it FOR KEY SHARE (or
-- stronger) before the state row, and the materializer takes it FOR KEY SHARE SKIP LOCKED
-- before inserting a state row. Below runs, the order of state/run_usage/fold rows is
-- irrelevant because no path holds two of them in opposite orders while waiting.
--
-- NO FK from run_usage_fold_state to runs: the state row must be creatable under a runs
-- FOR KEY SHARE lock without a competing FK check, and the AFTER DELETE trigger removes it
-- (cascading to the fold rows). TRUNCATE of runs is excluded by design: orphan state rows
-- are inert because every reader joins through runs.
--
-- MANUAL DML RULE. Any hand-written DML on run_usage must, in the same transaction and
-- before it changes run_usage, call SELECT fn_run_usage_fold_lock(run_id) for each affected
-- run, in run-id order. There are deliberately NO triggers on run_usage (a per-row trigger
-- would tax every live upsert); the store enforcement test pins the callers.
CREATE TABLE run_usage_fold_state (
    run_id    uuid        PRIMARY KEY,
    stale     boolean     NOT NULL DEFAULT true,
    folded_at timestamptz
);

CREATE INDEX idx_run_usage_fold_state_stale ON run_usage_fold_state (run_id) WHERE stale;

CREATE TABLE run_usage_model_folds (
    run_id                uuid    NOT NULL REFERENCES run_usage_fold_state (run_id) ON DELETE CASCADE,
    model                 text    NOT NULL,
    input_tokens          bigint  NOT NULL,
    cache_read_tokens     bigint  NOT NULL,
    cache_creation_tokens bigint  NOT NULL,
    output_tokens         bigint  NOT NULL,
    cost_usd              numeric NOT NULL,
    cost_status           text    NOT NULL CHECK (cost_status IN ('metered', 'subscription', 'unreported')),
    PRIMARY KEY (run_id, model)
);

-- +goose StatementBegin
CREATE FUNCTION fn_run_usage_fold_lock(r uuid) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    PERFORM 1 FROM runs WHERE id = r FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE foreign_key_violation
            USING MESSAGE = 'run_usage write references a run that does not exist (run_usage.run_id -> runs.id)';
    END IF;
    INSERT INTO run_usage_fold_state (run_id) VALUES (r)
    ON CONFLICT (run_id) DO UPDATE SET stale = true WHERE NOT run_usage_fold_state.stale;
    RETURN true;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION fn_run_usage_fold_state_on_run_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM run_usage_fold_state WHERE run_id = OLD.id;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER run_usage_fold_state_run_delete AFTER DELETE ON runs
FOR EACH ROW EXECUTE FUNCTION fn_run_usage_fold_state_on_run_delete();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER run_usage_fold_state_run_delete ON runs;
DROP FUNCTION fn_run_usage_fold_state_on_run_delete();
DROP FUNCTION fn_run_usage_fold_lock(uuid);
DROP TABLE run_usage_model_folds;
DROP INDEX idx_run_usage_fold_state_stale;
DROP TABLE run_usage_fold_state;
