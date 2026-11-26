-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4691: durable PR merge/close history, written atomically with runs.mr_state by the one
-- SetRunMRState statement. Additive only: two new tables, no change to an existing table.
-- Goose numbers are draft until merge; renumber above the live head on landing.
--
-- pr_history_heads: one row per (repo, PR). The current state plus an observation watermark
--   (observed_at: the newest observation time that reached the head). Observations older than or
--   tied with the watermark never change a head or write an event.
-- pr_history_events: append-only transitions (and the bootstrap first observation of a PR).
--
-- PG16 has no RETURNING OLD, so the statement carries the latest transition out of the head upsert
-- through the transport-only last_* columns of the head, which the event insert then reads.

CREATE TABLE pr_history_heads (
    repo_id             uuid        NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    mr_iid              bigint      NOT NULL,
    state               text        NOT NULL CHECK (state IN ('opened', 'closed', 'merged', 'locked')),
    -- Watermark: the newest observation time that reached this head.
    observed_at         timestamptz NOT NULL,
    -- Newest provider-reported merge/close time seen; never decreases.
    last_provider_at    timestamptz NULL,
    first_merged_at     timestamptz NULL,
    first_merged_source text        NULL CHECK (first_merged_source IN ('provider', 'observation_fallback')),
    -- TRANSPORT-ONLY columns (not history): they carry the latest transition through RETURNING so the
    -- event insert can read it in the same statement. Never read them as history and never index them;
    -- pr_history_events is the history.
    last_changed            boolean     NOT NULL DEFAULT false,
    last_from_state         text        NULL,
    last_to_state           text        NOT NULL,
    last_event_provider_at  timestamptz NULL,
    last_time_source        text        NOT NULL DEFAULT 'observation_fallback',
    last_bootstrap          boolean     NOT NULL DEFAULT false,
    last_first_merge        boolean     NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, mr_iid),
    CHECK ((first_merged_at IS NULL) = (first_merged_source IS NULL))
);

CREATE TABLE pr_history_events (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id       uuid        NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    mr_iid        bigint      NOT NULL,
    from_state    text        NULL,
    to_state      text        NOT NULL CHECK (to_state IN ('opened', 'closed', 'merged', 'locked')),
    provider_at   timestamptz NULL,
    time_source   text        NOT NULL CHECK (time_source IN ('provider', 'observation_fallback')),
    observed_at   timestamptz NOT NULL,
    bootstrap     boolean     NOT NULL,
    first_merge   boolean     NOT NULL,
    CHECK ((time_source = 'provider') = (provider_at IS NOT NULL)),
    CHECK (provider_at IS NULL OR provider_at <= observed_at),
    CHECK (bootstrap OR from_state IS DISTINCT FROM to_state)
);

-- At most one first-merge event per PR, however observations interleave.
CREATE UNIQUE INDEX pr_history_events_first_merge_uq ON pr_history_events (repo_id, mr_iid) WHERE first_merge;
-- Serves the repo ON DELETE CASCADE and per-PR reads.
CREATE INDEX pr_history_events_pr_idx ON pr_history_events (repo_id, mr_iid, id);
-- There is no retention and no age-based expiry: only the repo cascade deletes these rows.

-- +goose Down
-- This destroys PR history that cannot be recovered from the forge (the transition times and
-- observation provenance are only recorded here).
DROP TABLE pr_history_events;
DROP TABLE pr_history_heads;
