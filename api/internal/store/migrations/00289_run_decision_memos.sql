-- Run decisions memo (issue #2083 M1): one bounded memo per run, written by the run's lead through
-- the worker protocol and read back by a later mr_rework run on the same (owner, repo, branch,
-- mr_iid) lineage. Additive: a new table, no existing row or column changes.
--
-- claim_generation is the claim generation that wrote the row. The lineage resolver only trusts a
-- row whose generation still equals the run's CURRENT claim_generation, so a memo from a flight
-- that was later re-claimed (and never re-saved) is ignored rather than served stale.

-- +goose Up
CREATE TABLE run_decision_memos (
    run_id           uuid        PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation bigint      NOT NULL,
    format_version   smallint    NOT NULL DEFAULT 1,
    body             text        NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 8192),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE run_decision_memos;
