-- Estimated usage tail of an interrupted Claude session (issue #2014, ADR-2014).
--
-- The metered total (run_usage / run_usage_totals) reads only SDK result frames, so a leg
-- interrupted before its result frame leaves its model calls unmetered. These three tables
-- carry a SIDE channel: per-message usage the worker posts to POST /api/worker/runs/{id}/usage,
-- the leg identity/coverage the incremental fold stamps from init/result frames, and the
-- per-run cap flag. Nothing here feeds run_usage_totals or any budget; the tail is read,
-- priced and shown apart from the metered figure.
--
-- Additive: three new tables, no existing row or column changes. DDL only, inline CHECKs.
--
-- run_usage_messages carries two nullable text columns beyond the ADR's column list, speed and
-- inference_geo: the price table marks a message unpriced when usage.speed is present and not
-- "standard" or usage.inference_geo is present and not "global" (internal/anthropicprice).
--
-- Cascades: legs and tail_state cascade from runs; messages cascade from their leg through the
-- composite (run_id, leg_id) key. The (run_id, leg_id, ordinal) index is deliberately NOT unique:
-- a conflicting re-post is recorded (conflict = true), never rejected.

-- +goose Up
CREATE TABLE run_usage_legs (
    run_id             uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    leg_id             uuid        NOT NULL,
    init_seq           bigint      CHECK (init_seq IS NULL OR init_seq >= 0),
    sdk_session_id     text        CHECK (sdk_session_id IS NULL OR char_length(sdk_session_id) BETWEEN 1 AND 200),
    closed_through     integer     CHECK (closed_through IS NULL OR closed_through >= 0),
    covered_through    integer     CHECK (covered_through IS NULL OR covered_through >= 0),
    covered_cumulative boolean     NOT NULL DEFAULT false,
    dropped_records    bigint      NOT NULL DEFAULT 0 CHECK (dropped_records >= 0),
    claim_generation   bigint,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, leg_id)
);

CREATE TABLE run_usage_messages (
    run_id                         uuid        NOT NULL,
    message_id                     text        NOT NULL CHECK (char_length(message_id) BETWEEN 1 AND 200),
    leg_id                         uuid        NOT NULL,
    ordinal                        integer     NOT NULL CHECK (ordinal >= 1),
    frame_session_id               text        CHECK (frame_session_id IS NULL OR char_length(frame_session_id) <= 200),
    model                          text        NOT NULL CHECK (char_length(model) BETWEEN 1 AND 200),
    subagent                       boolean     NOT NULL DEFAULT false,
    input_tokens                   bigint      NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    cache_read_input_tokens        bigint      NOT NULL DEFAULT 0 CHECK (cache_read_input_tokens >= 0),
    cache_creation_input_tokens    bigint      NOT NULL DEFAULT 0 CHECK (cache_creation_input_tokens >= 0),
    cache_creation_5m_input_tokens bigint      CHECK (cache_creation_5m_input_tokens IS NULL OR cache_creation_5m_input_tokens >= 0),
    cache_creation_1h_input_tokens bigint      CHECK (cache_creation_1h_input_tokens IS NULL OR cache_creation_1h_input_tokens >= 0),
    output_tokens                  bigint      NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    output_final                   boolean     NOT NULL DEFAULT false,
    service_tier                   text        CHECK (service_tier IS NULL OR char_length(service_tier) <= 64),
    speed                          text        CHECK (speed IS NULL OR char_length(speed) <= 64),
    inference_geo                  text        CHECK (inference_geo IS NULL OR char_length(inference_geo) <= 64),
    conflict                       boolean     NOT NULL DEFAULT false,
    claim_generation               bigint,
    created_at                     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, message_id),
    FOREIGN KEY (run_id, leg_id) REFERENCES run_usage_legs (run_id, leg_id) ON DELETE CASCADE
);

CREATE INDEX idx_run_usage_messages_leg_ordinal ON run_usage_messages (run_id, leg_id, ordinal);

CREATE TABLE run_usage_tail_state (
    run_id             uuid    PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    record_cap_reached boolean NOT NULL DEFAULT false,
    capped_records     bigint  NOT NULL DEFAULT 0 CHECK (capped_records >= 0),
    capped_legs        bigint  NOT NULL DEFAULT 0 CHECK (capped_legs >= 0)
);

-- +goose Down
DROP TABLE run_usage_tail_state;
DROP TABLE run_usage_messages;
DROP TABLE run_usage_legs;
