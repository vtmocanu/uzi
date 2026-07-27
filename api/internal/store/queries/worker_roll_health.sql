-- Roll-health persistence (PRD #113 M4). Display-only: no query here is reachable
-- from claim, heartbeat, register or any scheduling path.

-- name: UpsertWorkerRollHealth :execrows
-- Record one worker's roll health from a controller report.
--
-- INSERT ... SELECT FROM workers, not INSERT ... VALUES, and that is the security
-- control rather than a style choice. Three properties fall out of it, none of which
-- the handler has to remember:
--
--   1. An UNKNOWN worker id inserts nothing (the SELECT finds no row) — so a report
--      can never create worker rows, and a hostile controller cannot grow this table
--      without bound by inventing uuids.
--   2. An EXTERNAL worker is untouched (kind = 'hosted' in the WHERE) — the
--      controller has no jurisdiction over a worker its owner runs by hand, and
--      without this it could assert `upgrade_failed` with attacker-authored text
--      against one.
--   3. Nothing in `workers` is written. Liveness (status, last_heartbeat_at) stays
--      heartbeat-only, so a lying controller cannot hold a dead worker "online",
--      which is run-scheduling state and not a badge.
--
-- :execrows so the caller can count what landed and log the difference between rows
-- reported and rows accepted, rather than assuming.
INSERT INTO worker_upgrade_reports (
    worker_id, phase, phase_since, target_image, pod_phase,
    blocking_container, blocking_reason, restart_count, last_exit_code,
    controller_reported_at, observed_at, upgrading_since,
    poll_interval_seconds, worker_image_tag
)
SELECT
    w.id, @phase, sqlc.narg('phase_since'), sqlc.narg('target_image'), sqlc.narg('pod_phase'),
    sqlc.narg('blocking_container'), sqlc.narg('blocking_reason'), @restart_count, sqlc.narg('last_exit_code'),
    @controller_reported_at, @observed_at,
    -- First report of a non-terminal roll stamps the ceiling's anchor. A `settled`
    -- report stamps nothing: settled is not a roll in progress.
    --
    -- The ::timestamptz cast is REQUIRED, not decoration: @observed_at appears twice in
    -- this statement, and in this arm its sibling branch is a bare NULL, so Postgres
    -- cannot deduce one type for the parameter and rejects the whole statement with
    -- "inconsistent types deduced for parameter" (SQLSTATE 42P08). Note sqlc's static
    -- analysis accepts it uncast — this failure appears only against a real server,
    -- which is why the live-DB test is what proves the query runs at all.
    CASE WHEN @phase::text IN ('rolling', 'stuck') THEN @observed_at::timestamptz ELSE NULL END,
    sqlc.narg('poll_interval_seconds'), sqlc.narg('worker_image_tag')
FROM workers w
WHERE w.id = @worker_id AND w.kind = 'hosted'
ON CONFLICT (worker_id) DO UPDATE SET
    phase                  = EXCLUDED.phase,
    phase_since            = EXCLUDED.phase_since,
    target_image           = EXCLUDED.target_image,
    pod_phase              = EXCLUDED.pod_phase,
    -- ================= THE DIAGNOSTIC BLOCK: ALL FOUR, OR NONE =================
    -- A REPORT MUST NOT BLANK FIELDS IT DID NOT MEASURE (issue #145).
    --
    -- deriveRollHealth returns `settled` the instant the pod's Ready condition is
    -- True, BEFORE the blocking-container lookup — so a settled report carries the
    -- ZERO of every field here (no container, no reason, restart_count 0, no exit
    -- code) rather than an observation that they are absent. Written as bare
    -- EXCLUDED.*, as these four arms used to be, that erased the real diagnostics: a
    -- worker with 5 restarts and exit 1 persisted as pristine, at exactly the moment
    -- somebody was reading the row to debug it. `phase` and `pod_phase` above are NOT
    -- in this block because a settled report genuinely measures both.
    --
    -- THE FOUR MOVE TOGETHER, and that is why this is a CASE over the block rather
    -- than a COALESCE per column. deriveRollHealth fills all four from ONE container
    -- status, or leaves all four zero. Per-column preservation would pair a fresh
    -- restart_count with a stale blocking_container and describe a row that was never
    -- observed — most visibly on the pod-less ReplicaFailure branch, which reports a
    -- reason with no container at all and must therefore clear the last pod's name.
    -- The predicate is repeated verbatim in each arm and MUST STAY IDENTICAL: that
    -- identity is the atomicity, so changing one arm alone silently ends it.
    --
    -- NO CLEAR HERE, for the same reason upgrading_since has none below: a clear the
    -- controller can trigger by reporting hands the reset to the reporting party. The
    -- ONLY clear is the worker's own authenticated re-registration moving
    -- workers.version — RegisterWorker in runtime.sql, one statement, one round trip.
    --
    -- Preserving these under `settled` changes no CLASSIFICATION: they reach
    -- ClassifyUpgrade only through stuckDetail (R1, phase=stuck) and rollingDetail
    -- (R2, phase=rolling), and workerDTOFromRow surfaces them only when the status is
    -- upgrade_failed. This is a data-integrity fix on a display-only table.
    blocking_container     = CASE WHEN EXCLUDED.blocking_container IS NOT NULL
                                    OR EXCLUDED.blocking_reason IS NOT NULL
                                    OR EXCLUDED.restart_count <> 0
                                    OR EXCLUDED.last_exit_code IS NOT NULL
                                  THEN EXCLUDED.blocking_container
                                  ELSE worker_upgrade_reports.blocking_container END,
    blocking_reason        = CASE WHEN EXCLUDED.blocking_container IS NOT NULL
                                    OR EXCLUDED.blocking_reason IS NOT NULL
                                    OR EXCLUDED.restart_count <> 0
                                    OR EXCLUDED.last_exit_code IS NOT NULL
                                  THEN EXCLUDED.blocking_reason
                                  ELSE worker_upgrade_reports.blocking_reason END,
    restart_count          = CASE WHEN EXCLUDED.blocking_container IS NOT NULL
                                    OR EXCLUDED.blocking_reason IS NOT NULL
                                    OR EXCLUDED.restart_count <> 0
                                    OR EXCLUDED.last_exit_code IS NOT NULL
                                  THEN EXCLUDED.restart_count
                                  ELSE worker_upgrade_reports.restart_count END,
    last_exit_code         = CASE WHEN EXCLUDED.blocking_container IS NOT NULL
                                    OR EXCLUDED.blocking_reason IS NOT NULL
                                    OR EXCLUDED.restart_count <> 0
                                    OR EXCLUDED.last_exit_code IS NOT NULL
                                  THEN EXCLUDED.last_exit_code
                                  ELSE worker_upgrade_reports.last_exit_code END,
    controller_reported_at = EXCLUDED.controller_reported_at,
    observed_at            = EXCLUDED.observed_at,
    poll_interval_seconds  = EXCLUDED.poll_interval_seconds,
    worker_image_tag       = EXCLUDED.worker_image_tag,
    updated_at             = now(),
    -- ===================== THE INV-5 CEILING ANCHOR =====================
    -- SET-IF-NULL, expressed HERE in SQL rather than in Go so that a later refactor
    -- of the service layer cannot lose it. Read the three arms carefully:
    --
    --   * non-terminal phase, anchor already set -> KEEP the existing value. This is
    --     the whole ceiling. Writing EXCLUDED.upgrading_since here instead would
    --     refresh the anchor on every report, which does not weaken the ceiling, it
    --     DELETES it: a controller posting `upgrading` every 10s would satisfy any
    --     window forever. The tidier-looking line is the broken one.
    --   * non-terminal phase, no anchor yet -> stamp it (the COALESCE).
    --   * terminal phase (`settled`) -> KEEP whatever is there. Deliberately NOT a
    --     clear. A clear here would hand the reset back to the controller — report
    --     `settled` once, then resume lying, and the clock restarts — which is
    --     exactly the forgeability the anchor exists to prevent. The ONLY clear is
    --     the worker's own authenticated re-registration moving workers.version, in
    --     RegisterWorker.
    upgrading_since = CASE
        WHEN EXCLUDED.phase IN ('rolling', 'stuck')
            THEN COALESCE(worker_upgrade_reports.upgrading_since, EXCLUDED.upgrading_since)
        ELSE worker_upgrade_reports.upgrading_since
    END;

-- name: GetWorkerUpgradeSummaryForUser :many
-- Roll health for ONE user's workers. Named for the scoping so a caller cannot
-- reach for it thinking it is fleet-wide.
--
-- The join through `workers` is the tenancy boundary and it is unavoidable by
-- construction: worker_upgrade_reports has no user_id, so there is no way to read a
-- row without going through the table that knows who owns it. A LEFT JOIN because a
-- worker with no report is the normal case (external workers, and any hosted worker
-- the controller has not reached yet) and must still appear.
SELECT
    w.id AS worker_id,
    w.kind,
    w.version,
    r.phase,
    r.phase_since,
    r.target_image,
    r.pod_phase,
    r.blocking_container,
    r.blocking_reason,
    r.restart_count,
    r.last_exit_code,
    r.observed_at,
    r.upgrading_since,
    r.worker_image_tag,
    -- ::boolean, not a bare expression: sqlc types an uncast boolean expression as
    -- interface{}, so the Go field cannot be used as a bool without a type assertion.
    -- Same family as the LEFT-JOIN nullability trap CLAUDE.md records — sqlc's inference
    -- on expressions is weaker than on columns, and the cast is what makes it a bool.
    (m.user_id IS NOT NULL)::boolean AS muted
FROM workers w
LEFT JOIN worker_upgrade_reports r ON r.worker_id = w.id
-- The mute's release key is the controller's rolled tag for a HOSTED worker and the
-- worker's OWN reported version for anything else. That is not a fallback, it is what
-- "release" means for each population (PRD #113 M5, B-2):
--
--   * hosted: the tag being rolled TO. A stuck roll on 0.11.7 is muted; 0.11.8 alerts
--     again, because the next roll is a different event.
--   * external: the version the worker IS on. Nothing upgrades an external worker, so
--     it sits `outdated` indefinitely and the mute means "I know this one runs 0.11.0".
--     It expires when the WORKER moves, which is the only event that changes the fact
--     being muted — a new control-plane release does not.
--
-- COALESCE(r.worker_image_tag, '') alone was dead for external workers and they are
-- exactly who the mute is for: the roll-health upsert is confined to kind='hosted', so
-- an external worker has no report, its key was always '' and a mute stored against
-- any real release never matched. Keying on '' instead would have made an external
-- mute PERMANENT, which contradicts the whole point of scoping a mute to a release.
LEFT JOIN worker_upgrade_mutes m
       ON m.worker_id = w.id AND m.user_id = w.user_id
      AND m.release = COALESCE(r.worker_image_tag, w.version, '')
WHERE w.user_id = @user_id
ORDER BY w.created_at;

-- name: MuteWorkerUpgrade :exec
-- Mute one worker for one release, for the calling user only.
--
-- (user_id, worker_id) is the authorization: the INSERT ... SELECT means a worker
-- belonging to another user matches zero rows and writes nothing, exactly as an
-- unknown worker id does — the same shape notifications uses, where the (id, user_id)
-- match IS the ownership check. Idempotent: re-muting keeps the original timestamp.
INSERT INTO worker_upgrade_mutes (user_id, worker_id, release)
SELECT w.user_id, w.id, @release
FROM workers w
WHERE w.id = @worker_id AND w.user_id = @user_id
ON CONFLICT (user_id, worker_id, release) DO NOTHING;

-- name: UnmuteWorkerUpgrade :exec
-- Drop a mute. The (user_id, worker_id) match is again the authorization check.
DELETE FROM worker_upgrade_mutes
WHERE user_id = @user_id AND worker_id = @worker_id AND release = @release;
