-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4208 M1: the identity-bound volume inventory taken while a hosted worker is preparing to
-- hibernate. One row per worker (the current hibernation op replaces the previous one). Nothing
-- is deleted by this milestone; the row only records what the controller observed and what the
-- worker echoed, so a later dry-run report can say "would retire" or "blocked by X".
--
-- New empty table, so every CHECK is inline (no NOT VALID staging). The only busy table touched is
-- workers, through the FK. Every write runs in a transaction that already holds the workers row
-- lock; lock order is workers, then inventory.
CREATE TABLE worker_hibernation_inventories (
    worker_id uuid PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,
    op_id uuid NOT NULL,
    -- Controller-observed identity, put into the worker's snapshot and echoed back.
    observed_at timestamptz NOT NULL,
    deployment_uid text NOT NULL CHECK (char_length(deployment_uid) <= 256),
    deployment_resource_version text NOT NULL CHECK (char_length(deployment_resource_version) <= 256),
    pod_uid uuid NOT NULL,
    -- Array of {role,name,uid,storage_class,class_reclaim_policy}.
    pvcs jsonb NOT NULL CHECK (jsonb_typeof(pvcs) = 'array' AND octet_length(pvcs::text) <= 16384),
    register_nonce text CHECK (char_length(register_nonce) <= 256),
    received_at timestamptz,
    status text NOT NULL CHECK (status IN ('awaiting','accepted','unsupported','exclusion_failed',
        'busy','timed_out','no_capability','closed_by_scale_down','identity_changed')),
    status_reason text CHECK (char_length(status_reason) <= 512),
    complete boolean,
    -- The 1 MiB bound is on jsonb's PRINTED form (entries::text), which is not the stored form: the
    -- api must bound the stored form itself and map a 23514 here to a client error.
    entries jsonb CHECK (entries IS NULL
        OR (jsonb_typeof(entries) = 'array' AND octet_length(entries::text) <= 1048576)),
    receipt_keys jsonb CHECK (receipt_keys IS NULL
        OR (jsonb_typeof(receipt_keys) = 'array' AND octet_length(receipt_keys::text) <= 1048576)),
    digest text CHECK (char_length(digest) <= 128),
    fingerprint text CHECK (char_length(fingerprint) <= 128),
    lane text CHECK (char_length(lane) <= 64),
    mechanism text CHECK (char_length(mechanism) <= 128),
    -- The exclusion's uncovered list as the worker reported it. A non-empty list, or a lane other
    -- than dind, forces complete = false; the list is kept so the evidence is not discarded.
    uncovered jsonb CHECK (uncovered IS NULL
        OR (jsonb_typeof(uncovered) = 'array' AND jsonb_array_length(uncovered) <= 64
            AND octet_length(uncovered::text) <= 16384)),
    invalidated_reason text CHECK (invalidated_reason IN ('other_pod','pod_set_unproven',
        'deployment_changed','observation_gap','process_restarted','exclusion_lost',
        'written_after_inventory','identity_changed','scale_down_pod_unproven')),
    invalidated_at timestamptz,
    scaling_down_pod_uid uuid,
    fence_generation bigint,
    scaled_down_at timestamptz,
    last_held_at timestamptz,
    exclusion_final_at timestamptz,
    exclusion_final_digest text CHECK (char_length(exclusion_final_digest) <= 128),
    exclusion_final_fingerprint text CHECK (char_length(exclusion_final_fingerprint) <= 128),
    CONSTRAINT worker_hibernation_inventories_accepted_coherent CHECK (
        status <> 'accepted'
        OR (entries IS NOT NULL AND digest IS NOT NULL AND fingerprint IS NOT NULL
            AND complete IS NOT NULL AND received_at IS NOT NULL AND register_nonce IS NOT NULL)),
    CONSTRAINT worker_hibernation_inventories_invalidation_coherent CHECK (
        (invalidated_reason IS NULL) = (invalidated_at IS NULL))
);

-- The settle pass lists stale awaiting rows.
CREATE INDEX worker_hibernation_inventories_awaiting_idx
    ON worker_hibernation_inventories (observed_at, worker_id) WHERE status = 'awaiting';

-- +goose Down
SET LOCAL lock_timeout = '5s';

DROP TABLE worker_hibernation_inventories;
