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
    -- THE PHASE IS THE DISCRIMINATOR, because the phase is what decides whether the
    -- controller ran the measurement at all. deriveRollHealth returns `settled` the
    -- instant the pod's Ready condition is True, BEFORE the blocking-container lookup,
    -- so a settled report carries the ZERO of every field here (no container, no
    -- reason, restart_count 0, no exit code) — it never looked. Every `rolling` or
    -- `stuck` report DID look, so whatever it carries, zeros included, is an
    -- observation and must land. Written as bare EXCLUDED.*, as these four arms used
    -- to be, the settled zeros erased the real diagnostics: a worker with 5 restarts
    -- and exit 1 persisted as pristine, at exactly the moment somebody was reading the
    -- row to debug it. `phase` and `pod_phase` above are NOT in this block because a
    -- settled report genuinely measures both.
    --
    -- "THE REPORT CARRIES NOTHING" IS THE WRONG PREDICATE, and it is the tempting one
    -- — it needs no knowledge of the controller and it fixes the headline symptom.
    -- It conflates "never looked" with "looked and found nothing to blame", and only
    -- the phase separates those. Two deriveRollHealth branches return `rolling` with
    -- no diagnostics at all (the pod-less Recreate gap, and the stale-pod-only
    -- branch); preserving there would carry the PREVIOUS roll's reason into a healthy
    -- mid-Recreate tick, and rollingDetail (upgrade.go) reads BlockingReason FIRST, so
    -- R2's upgrade_detail would read "CrashLoopBackOff" for a worker that is fine.
    -- That string is NOT gated the way the three blocking_* DTO fields are — it is the
    -- badge's own title attribute (WorkerUpgradeBadge.tsx) — so it renders.
    --
    -- THE FOUR MOVE TOGETHER, which is why this is a CASE over the block rather than a
    -- COALESCE per column. deriveRollHealth fills all four from ONE container status,
    -- or leaves all four zero. Per-column preservation would pair a fresh
    -- restart_count with a stale blocking_container and describe a row that was never
    -- observed — most visibly on the pod-less ReplicaFailure branch, which reports a
    -- reason with NO container and must therefore clear the last pod's name. The
    -- predicate is repeated verbatim in each arm and MUST STAY IDENTICAL: that
    -- identity is the atomicity, so changing one arm alone silently ends it. It is
    -- deliberately the same `IN ('rolling', 'stuck')` set the anchor arm below uses.
    --
    -- A uniform COALESCE would ALSO have been a silent no-op on restart_count, which
    -- is `integer NOT NULL DEFAULT 0` (migration 00083) and arrives as @restart_count,
    -- not sqlc.narg — so EXCLUDED.restart_count is never NULL and half the issue's own
    -- symptom ("5 restarts AND exit 1") would have survived the fix.
    --
    -- THIS IS DATA INTEGRITY, NOT A CONTROL AGAINST A LYING CONTROLLER, and the
    -- distinction belongs in the file whose header enumerates three real security
    -- properties. The wipe lever is unchanged and intentionally so: a controller that
    -- reports `stuck` or `rolling` with empty diagnostics still writes zeros here,
    -- because those phases assert that the lookup ran. What is fixed is an HONEST
    -- controller destroying its own earlier measurement.
    --
    -- NO CLEAR HERE, for the same reason upgrading_since has none below: a clear the
    -- controller can trigger by reporting hands the reset to the reporting party. The
    -- ONLY clear is the worker's own authenticated re-registration moving
    -- workers.version — RegisterWorker in runtime.sql, one statement, one round trip.
    --
    -- CHANGES NO RENDERED OUTPUT TODAY, so do not look for a UI difference to verify
    -- it — the live-DB test is the evidence. The preserved values are reachable only
    -- on a `settled` row, and workerDTOFromRow nulls the three blocking_* fields
    -- unless the status is upgrade_failed (which requires phase=stuck), while R1/R2's
    -- detail strings require phase stuck/rolling, where EXCLUDED wins anyway. Commit 2
    -- is what makes them displayable; the detail string it feeds must then not present
    -- a pinned older observation as a current one.
    blocking_container     = CASE WHEN EXCLUDED.phase IN ('rolling', 'stuck')
                                  THEN EXCLUDED.blocking_container
                                  ELSE worker_upgrade_reports.blocking_container END,
    blocking_reason        = CASE WHEN EXCLUDED.phase IN ('rolling', 'stuck')
                                  THEN EXCLUDED.blocking_reason
                                  ELSE worker_upgrade_reports.blocking_reason END,
    restart_count          = CASE WHEN EXCLUDED.phase IN ('rolling', 'stuck')
                                  THEN EXCLUDED.restart_count
                                  ELSE worker_upgrade_reports.restart_count END,
    last_exit_code         = CASE WHEN EXCLUDED.phase IN ('rolling', 'stuck')
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
