-- +goose Up

-- Denied-CLI auto-dismissal provenance (issue #167). This widens the set_via CHECK on
-- recommendation_dispositions to admit a second server-side provenance, 'denied_cli', so a
-- system auto-dismissal of a denylisted-CLI recommendation (glab/gh/aws/az/…) is visibly and
-- durably distinct from both a human verdict and the Filed→Done sync's 'issue_close'.
--
-- WHY set_via, and not `set_by_user_id IS NULL`. That inference looks equivalent — the system
-- writes the setter as NULL — but it is not a robust discriminator: set_by_user_id is an FK
-- with ON DELETE SET NULL, so a HUMAN's disposition row also becomes NULL once that user is
-- deleted, and would then be mis-read as a system action. set_via is written only from a
-- server-side literal in one query and can never arrive from a request body, so it is the one
-- field that stays honest. This is the SAME reasoning 00073/00081 give for adding provenance
-- rather than leaning on the nullable setter.
--
-- The set_via column and its UNNAMED column CHECK were added in
-- 00081_judge_issue_close_sync.sql:
--     ALTER TABLE recommendation_dispositions
--         ADD COLUMN set_via text CHECK (set_via IS NULL OR set_via IN ('issue_close'));
-- Postgres auto-names an unnamed column CHECK `recommendation_dispositions_set_via_check`;
-- this migration drops that constraint by that name and recreates it with the widened domain.
-- The name was VERIFIED by applying this migration against a throwaway Postgres 17 (the live-DB
-- integration test): a wrong name would fail the DROP.
ALTER TABLE recommendation_dispositions DROP CONSTRAINT recommendation_dispositions_set_via_check;
ALTER TABLE recommendation_dispositions ADD CONSTRAINT recommendation_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close', 'denied_cli'));

-- +goose Down
-- HAZARD: this Down re-narrows the CHECK to set_via IN ('issue_close'), and Postgres
-- validates every existing row when a CHECK constraint is ADDed. So the Down FAILS (the
-- ADD CONSTRAINT raises a check_violation) if ANY recommendation_dispositions row with
-- set_via='denied_cli' exists at rollback time — i.e. once the deterministic net has
-- auto-dismissed even one recommendation. Such rows must be cleared or re-dispositioned
-- (away from 'denied_cli') BEFORE this Down can run.
ALTER TABLE recommendation_dispositions DROP CONSTRAINT recommendation_dispositions_set_via_check;
ALTER TABLE recommendation_dispositions ADD CONSTRAINT recommendation_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close'));
