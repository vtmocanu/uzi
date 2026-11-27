-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4693: five-minute worker capacity samples. One row per (worker, five-minute bucket),
-- written by the sweeper pass worker_capacity_samples. Nothing reads this history yet.
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- worker_id is the worker's UUID by VALUE, with no foreign key to workers: deleting an
-- ephemeral worker must not erase its samples. user_id is the worker's owner at sampling time;
-- ON DELETE CASCADE removes an owner's samples with the owner. There is no age-based expiry.
--
-- Observed values only. max_concurrent_runs and max_cross_check_slots are copied verbatim and a
-- NULL cap stays NULL (unknown, never zero). The three occupancy counts are never clamped to
-- the caps, so there is deliberately no CHECK tying counts to caps.
--
-- attribution: 'owner' means every run holding a slot on the worker belonged to the worker's
-- owner and had a lane shape the audit proved, so the counts are real. 'unavailable' means that
-- could not be shown for this sample and all three counts are NULL; caps, freshness and
-- heartbeat are still recorded.
CREATE TABLE worker_capacity_samples (
    worker_id               uuid        NOT NULL,
    bucket_start            timestamptz NOT NULL,
    user_id                 uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    observed_at             timestamptz NOT NULL,
    freshness               text        NOT NULL CHECK (freshness IN ('online', 'stale', 'offline')),
    last_heartbeat_at       timestamptz,
    worker_kind             text        NOT NULL,
    ephemeral               boolean     NOT NULL,
    isolated_lane           boolean     NOT NULL,
    max_concurrent_runs     integer,
    max_cross_check_slots   integer,
    attribution             text        NOT NULL CHECK (attribution IN ('owner', 'unavailable')),
    ordinary_active         integer     CHECK (ordinary_active >= 0),
    cross_check_lane_active integer     CHECK (cross_check_lane_active >= 0),
    chat_active             integer     CHECK (chat_active >= 0),
    PRIMARY KEY (worker_id, bucket_start),
    CONSTRAINT worker_capacity_samples_attribution_shape CHECK (
        (attribution = 'owner'
            AND ordinary_active IS NOT NULL AND cross_check_lane_active IS NOT NULL AND chat_active IS NOT NULL)
        OR (attribution = 'unavailable'
            AND ordinary_active IS NULL AND cross_check_lane_active IS NULL AND chat_active IS NULL)
    )
);

-- The owner cascade finds an owner's rows through this index.
CREATE INDEX worker_capacity_samples_user_idx ON worker_capacity_samples (user_id, bucket_start);

-- +goose Down
DROP TABLE worker_capacity_samples;
