-- +goose Up

-- Admin cross-user "Mark done" provenance (PRD #1184 M2). This widens the set_via CHECK on
-- recommendation_dispositions to admit a THIRD server-side provenance, 'admin', so an admin
-- who marks a (category, target) coordinate done across every user's open member is visibly
-- and durably distinct from a human's own verdict (NULL), the Filed→Done sync ('issue_close')
-- and the denied-CLI net ('denied_cli').
--
-- WHY set_via, and why set_by_user_id is SET (unlike the two other server-side provenances).
-- The admin write stamps set_by_user_id = the acting admin, for accountability IN THE ROW — a
-- forensic "who did this" — while the user-facing chip never names the admin (it reads only
-- "Done by an admin"). set_via='admin' is the honest discriminator that survives even after the
-- admin's user row is deleted (set_by_user_id is an FK with ON DELETE SET NULL, so a NULL setter
-- cannot tell an admin action from a plain human one). Same reasoning 00073/00081/00128 give for
-- adding provenance rather than leaning on the nullable setter.
--
-- The set_via column and its UNNAMED column CHECK were added in 00081_judge_issue_close_sync.sql:
--     ALTER TABLE recommendation_dispositions
--         ADD COLUMN set_via text CHECK (set_via IS NULL OR set_via IN ('issue_close'));
-- Postgres auto-named that unnamed column CHECK `recommendation_dispositions_set_via_check`;
-- 00128 dropped and recreated it widened to IN ('issue_close', 'denied_cli'). This migration drops
-- it by that name again and recreates it with the third value. The name was VERIFIED against a
-- throwaway Postgres 17 (the live-DB integration test) — a wrong name would fail the DROP.
--
-- Added NOT VALID so the ADD skips the validating table scan (and the ACCESS EXCLUSIVE lock it
-- would otherwise hold); 00216 runs VALIDATE CONSTRAINT under a lock-cheap scan, the same two-step
-- pattern 00209/00210 use for a CHECK on a live table.
ALTER TABLE recommendation_dispositions DROP CONSTRAINT recommendation_dispositions_set_via_check;
ALTER TABLE recommendation_dispositions ADD CONSTRAINT recommendation_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close', 'denied_cli', 'admin')) NOT VALID;

-- +goose Down
-- HAZARD: this Down re-narrows the CHECK to set_via IN ('issue_close', 'denied_cli'), and the
-- re-added constraint is VALIDATED immediately, so Postgres scans every existing row. The Down
-- therefore FAILS (the ADD CONSTRAINT raises a check_violation) if ANY recommendation_dispositions
-- row with set_via='admin' exists at rollback time — i.e. once an admin has marked even one
-- coordinate done across users. Such rows must be cleared or re-dispositioned (away from 'admin')
-- BEFORE this Down can run. That is EXPECTED and irreversible-by-design, the same one-way property
-- 00128's Down records for 'denied_cli'; goose downs are not run in this deployment (store.Migrate
-- only ever goes up).
ALTER TABLE recommendation_dispositions DROP CONSTRAINT recommendation_dispositions_set_via_check;
ALTER TABLE recommendation_dispositions ADD CONSTRAINT recommendation_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close', 'denied_cli'));
