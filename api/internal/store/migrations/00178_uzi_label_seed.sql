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

DELETE FROM app_settings WHERE key = 'prd_label';

-- +goose Down

-- Reverse the reconcile: restore the retired prd_label seed and drop the uzi_label
-- seed. Uses the 00036 default 'PRD' for prd_label; only removes a uzi_label row that
-- still carries the seeded default (an admin-customised value is left intact).
INSERT INTO app_settings (key, value) VALUES ('prd_label', 'PRD')
    ON CONFLICT (key) DO NOTHING;

DELETE FROM app_settings WHERE key = 'uzi_label' AND value = 'uzi';
