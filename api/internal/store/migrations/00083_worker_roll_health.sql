-- +goose Up

-- Per-worker roll health, reported by the controller (PRD #113 M4).
--
-- DISPLAY-ONLY (Decision 10). Nothing on the claim, heartbeat, register or scheduling
-- path reads this table, and that is enforced structurally rather than by convention:
-- the table is separate, its only foreign key is worker_id, and the controller's write
-- path cannot reach `workers` at all except to check that a row exists.
--
-- THERE IS DELIBERATELY NO user_id COLUMN, and it is a security control rather than
-- normalization. The controller reports the whole fleet with no notion of ownership,
-- so a summary written naively over this table would count every user's failing
-- workers into every other user's nav badge, on every page. With no user_id here, the
-- ONLY way to read a row is to join through `workers` — which carries the owner — so
-- per-user scoping is unavoidable rather than remembered. Do not add one "to make the
-- summary query simpler": simplifying that query is precisely the cross-tenant leak.
CREATE TABLE worker_upgrade_reports (
    -- One row per worker, replaced on each report. PK on worker_id (not a synthetic
    -- id) is what makes the upsert an upsert.
    worker_id uuid PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,

    -- Closed enum, validated server-side before it ever gets here: rolling|stuck|settled.
    -- A value outside the set means the api does not model that phase, and the entry is
    -- dropped rather than persisted — never persist-and-render a free string.
    phase text NOT NULL,

    -- ⚠ phase_since is the CONTROLLER's timestamp and it does NOT mean "since when has
    -- this been in this phase" for a stuck pod. It is the POD'S CREATION TIME. A
    -- stateless controller has no memory in which to record the transition, so the
    -- moment a pod became stuck is not knowable to it. DISPLAY ONLY. Any arithmetic
    -- that treats this as "how long has this been broken" measures something else, and
    -- errs toward looking worse for longer. Use observed_at / upgrading_since below,
    -- which this api stamps and therefore controls.
    phase_since timestamptz,

    target_image text,
    pod_phase text,
    blocking_container text,
    blocking_reason text,
    -- 0 is a real observation, so this is NOT NULL with a default; last_exit_code is
    -- nullable because "exited 0" and "never terminated" are different facts.
    restart_count integer NOT NULL DEFAULT 0,
    last_exit_code integer,

    -- ==================== THE THREE TIMESTAMPS ====================
    -- Each pair-collapse is a distinct, named hole. They look redundant. They are not.

    -- 1. The controller's own clock at send. DISPLAY ONLY — never an input to any
    --    decision. Collapsing observed_at into this column sources freshness from the
    --    untrusted party: clock skew alone would make a signal that never goes stale,
    --    and a malicious controller would make it permanent by simply sending a future
    --    timestamp. Freshness that the reporting party can extend is not freshness.
    controller_reported_at timestamptz NOT NULL,

    -- 2. The api's own now() at receipt. Drives the Decision 7 freshness TTL. This is
    --    the one the code reads.
    observed_at timestamptz NOT NULL,

    -- 3. When the CURRENT roll started, api-stamped, SET-IF-NULL, and cleared ONLY by
    --    the worker's own authenticated re-registration moving workers.version.
    --    Drives the INV-5 ceiling: classification may return `upgrading` only while
    --    now() - upgrading_since < MaxUpgradingWindow.
    --
    --    Collapsing this into observed_at DELETES THE CEILING — it would refresh on
    --    every report, and the resulting code looks tidier, which is what makes it
    --    dangerous. The TTL bounds a controller that STOPS talking; only this column
    --    bounds one that KEEPS talking, which is the compromised/wedged case where
    --    every `outdated` and `upgrade_failed` alert in the fleet is suppressed
    --    indefinitely.
    --
    --    The set-if-NULL lives in SQL (a COALESCE inside ON CONFLICT DO UPDATE), not
    --    in Go, so a later refactor of the service layer cannot quietly lose it.
    upgrading_since timestamptz,

    -- The controller's cadence, so the api can size its staleness window from the
    -- real interval rather than assuming the default. Display/tuning input only.
    poll_interval_seconds integer,
    -- The tag the controller actually rolls to (Decision 9's hosted target).
    worker_image_tag text,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Per-user, per-worker, per-release mute (PRD #113 M5's Mute action).
--
-- The (user_id, worker_id) pair in the PRIMARY KEY IS the authorization check, the
-- same shape notifications uses: a row belonging to another user simply matches zero
-- rows, which a handler surfaces as 404 exactly like an unknown id. Scoped per
-- release so muting a stuck worker on 0.11.7 does not also silence it on 0.11.8 —
-- a mute that outlives the thing muted is how an alert stops being trusted.
CREATE TABLE worker_upgrade_mutes (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    release text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, worker_id, release)
);

-- +goose Down
DROP TABLE worker_upgrade_mutes;
DROP TABLE worker_upgrade_reports;
