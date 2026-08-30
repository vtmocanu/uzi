-- +goose Up

-- Reconcile the app_settings label seed with PRD #764, which renamed the
-- run-eligibility key prd_label -> uzi_label (settings.KeyUziLabel) but shipped no
-- data migration. Two consequences this closes (issue #831):
--
--   1. Migration 00036 seeded prd_label, which is now an UNKNOWN key: settings.Effective
--      ignores any row whose key is not in settings.Defaults, so the row is dead weight.
--   2. A fresh DB has NO uzi_label row. The settings-PUT cross-key check reads rows
--      SELECT ... FOR UPDATE, which locks only rows that EXIST; with no uzi_label row a
--      concurrent PUT that inserts it is invisible to the loser's READ COMMITTED
--      snapshot, so two PUTs could commit uzi_label == autopilot_label. The advisory
--      lock added alongside this migration (store.SettingsMutationLockKey) is the
--      authoritative fix; seeding the row additionally restores the pre-#764 shape and,
--      like 00036's original intent, makes the admin Settings page show a concrete value.
--
-- Behaviour-neutral: the accessor already falls back to DefaultUziLabel ("uzi"), so
-- seeding that value changes no effective setting. A prd_label an admin had customised
-- pre-#764 is already inert after #764 and is not carried over (that was #764's decision,
-- not this migration's to reverse). This literal MUST match settings.DefaultUziLabel.
INSERT INTO app_settings (key, value) VALUES ('uzi_label', 'uzi')
    ON CONFLICT (key) DO NOTHING;

-- Remove ONLY the untouched 00036 seed row, never an admin-customised prd_label. The
-- value guard spares a customisation to a non-default string even if its author was
-- later deleted (updated_by is ON DELETE SET NULL, so it cannot alone mean "pristine");
-- updated_by IS NULL additionally spares a deliberate 'PRD' set by an existing admin. A
-- deliberate 'PRD' by a since-deleted admin is indistinguishable from the seed and is
-- the one unavoidable case.
DELETE FROM app_settings WHERE key = 'prd_label' AND value = 'PRD' AND updated_by IS NULL;

-- +goose Down

-- Reverse the reconcile: restore the retired prd_label seed and drop the uzi_label seed.
-- Symmetric with the Up delete: only a uzi_label row that still looks like the untouched
-- seed is removed, so an admin-set uzi_label survives a rollback.
INSERT INTO app_settings (key, value) VALUES ('prd_label', 'PRD')
    ON CONFLICT (key) DO NOTHING;

DELETE FROM app_settings WHERE key = 'uzi_label' AND value = 'uzi' AND updated_by IS NULL;
