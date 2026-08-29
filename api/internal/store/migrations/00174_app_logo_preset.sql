-- +goose Up

-- PRD #780 M5 (D7): promote the Metaminds "M" from a silent app-slot fallback to an
-- explicit, named app-logo PRESET, and transition THIS instance to it with no visible
-- change. Before this migration the M only appeared because our instance sits in
-- app_logo_mode='custom' with no uploaded 'app' asset, which the chrome used to render
-- as /brand-default.svg. M2 dropped that implicit fallback (custom+no-upload now shows
-- the stock uzi mark), so without this flip our live sidebar would lose the M. This is a
-- one-time, idempotent data flip — app_settings is a KV store with no seeded branding
-- rows (an absent row synthesizes to the compiled-in default), so this is data-only.
--
-- Statement 1 — flip mode custom -> preset ONLY for an instance currently in the exact
-- pre-M2 shape: custom mode AND no uploaded app asset. The value='custom' guard and the
-- NOT EXISTS guard together make this a no-op on any instance that has genuinely
-- uploaded a custom app logo, or is already in default/preset mode. Idempotent: a second
-- run finds value='preset' (not 'custom') and updates nothing.
UPDATE app_settings
   SET value = 'preset'
 WHERE key = 'app_logo_mode'
   AND value = 'custom'
   AND NOT EXISTS (SELECT 1 FROM branding_assets WHERE slot = 'app');

-- Statement 2 — record the chosen preset slug. app_logo_preset has no default row (its
-- compiled-in default is ""), so a plain INSERT is required; ON CONFLICT (key) DO NOTHING
-- is what makes the claimed idempotency real — a re-run (or an admin who already picked a
-- preset) is left untouched rather than violating the PK. Written unconditionally: on a
-- default-mode instance the slug is inert (default mode ignores the preset), so this is
-- harmless there and correct once such an instance later selects the preset tile.
INSERT INTO app_settings (key, value)
VALUES ('app_logo_preset', 'metaminds')
ON CONFLICT (key) DO NOTHING;

-- +goose Down

-- Reverse both statements. The mode UPDATE is SCOPED to value='preset' so a rollback
-- cannot corrupt an instance that is (or was) in default mode into custom — it only
-- undoes the preset flip this migration could have made. Note this is a best-effort
-- inverse: an admin who deliberately selected preset mode AFTER this migration would
-- also be flipped back to custom on a down-migration, which is the accepted limitation
-- of a data migration on a single self-hosted instance.
DELETE FROM app_settings WHERE key = 'app_logo_preset';

UPDATE app_settings
   SET value = 'custom'
 WHERE key = 'app_logo_mode'
   AND value = 'preset';
