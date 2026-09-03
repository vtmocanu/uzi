-- +goose Up
-- PRD #1064 M2: the current_activity "now" line selects the newest tool_use frame per
-- run via DISTINCT ON (run_id) ... ORDER BY run_id, seq DESC (LatestToolUseForRuns).
-- run_messages already carries UNIQUE (run_id, seq), but that index spans every kind,
-- so the DISTINCT ON would walk back over all the trailing non-tool_use frames (status,
-- text, tool_result) a run accumulates. This partial index materialises exactly the
-- tool_use frames in (run_id, seq DESC) order, so the per-run first row is one index
-- seek — the board polls the list every 2s, so this lands with the query, not after a
-- measurement.
CREATE INDEX idx_run_messages_tool_use_seq
    ON run_messages (run_id, seq DESC)
    WHERE kind = 'tool_use';
-- +goose Down
DROP INDEX idx_run_messages_tool_use_seq;
