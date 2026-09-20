-- +goose Up

-- PRD #1484 M2: the in-app admin-health plumbing that has no home today — the
-- fleet-independent controller-report trace, and the DANGER-episode + per-admin snooze
-- storage. Four ADDITIVE tables, one partial unique index: an N-1 api reading the
-- pre-existing surface during a rolling release is unaffected (nothing here alters a
-- worker-facing table). DRAFT number 00241 above the live head 00237; the lead renumbers
-- above the live head at landing.

-- controller_report_status: the fleet-INDEPENDENT trace of "when did the controller last
-- report". Today the arrival time of a status report is stored only per worker
-- (worker_upgrade_reports.observed_at), so a zero-worker report leaves no row and the
-- controller.report health check has nothing to read. A dedicated ONE-ROW table is right;
-- the app_settings key-value table is wrong for a value rewritten every ~10s (it carries
-- an updated_by FK and settings-cache semantics). The `id smallint CHECK (id = 1)` makes
-- the singleton STRUCTURAL: UpsertControllerReport inserts id=1 and updates it on conflict,
-- so exactly one row can ever exist. observed_at is the api's OWN clock at receipt (never
-- the wire's reported_at), the same freshness discipline worker_upgrade_reports.observed_at
-- follows.
CREATE TABLE controller_report_status (
    id          smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    observed_at timestamptz NOT NULL
);

-- health_episodes: one row per DANGER episode (PRD #1484 D14). Explicit rows, not the
-- custody precedent's implicit per-owner row, because the episode id is exposed in the API
-- and keys the per-admin banner snooze and (M6) the per-admin notice claim. opened_at and
-- closed_at bound the episode; closed_at NULL means the episode is still open. The uuid PK
-- follows the users/guardrail_override_requests convention (gen_random_uuid()).
CREATE TABLE health_episodes (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    opened_at timestamptz NOT NULL,
    closed_at timestamptz
);

-- At most ONE open episode, enforced by a partial unique index on the constant expression
-- (closed_at IS NULL) restricted to the open rows. Every open row indexes the same value
-- true, so two concurrent OpenHealthEpisode inserts cannot both win — the second trips a
-- 23505 unique violation, which is how the two-replica open resolves to exactly one winner
-- (and why OpenHealthEpisode must NOT use ON CONFLICT DO NOTHING — the violation is the
-- signal). A closed row drops out of the partial index, so the next danger episode opens
-- cleanly.
CREATE UNIQUE INDEX health_episodes_one_open
    ON health_episodes ((closed_at IS NULL)) WHERE closed_at IS NULL;

-- health_episode_notices: the per-admin, per-episode notice-claim slot (PRD #1484 M6 — the
-- TABLE lands in M2, the fan-out itself is M6). PK (episode_id, user_id) makes the INSERT
-- the atomic claim: a caller sends the notice only when its insert took. Both FKs
-- ON DELETE CASCADE — a transient dedup flag, so it may cascade with the episode it belongs
-- to or the notified user.
CREATE TABLE health_episode_notices (
    episode_id  uuid NOT NULL REFERENCES health_episodes(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notified_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (episode_id, user_id)
);

-- health_banner_snoozes: the per-admin, per-episode Danger-banner snooze (PRD #1484 D2). PK
-- (episode_id, user_id); snoozed_until is the wall clock the snooze expires at. A snooze
-- applies only to the episode it names, so a new episode shows the banner again. Both FKs
-- ON DELETE CASCADE, like the notice slot.
CREATE TABLE health_banner_snoozes (
    episode_id    uuid NOT NULL REFERENCES health_episodes(id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    snoozed_until timestamptz NOT NULL,
    PRIMARY KEY (episode_id, user_id)
);

-- +goose Down

DROP TABLE health_banner_snoozes;
DROP TABLE health_episode_notices;
DROP INDEX IF EXISTS health_episodes_one_open;
DROP TABLE health_episodes;
DROP TABLE controller_report_status;
