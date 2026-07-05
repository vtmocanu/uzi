-- +goose Up

-- app_settings: a generic key/value store for instance-level configuration
-- (PRD #19). The deliverable is the infra; the first two tenants are the
-- configurable forge labels (prd_label, autopilot_label). Values are read
-- through a short-TTL cache in the api process (internal/settings) with the
-- defaults compiled in, so a missing row never breaks boot. Future settings
-- (registration policy, self-improvement toggle) slot in as new keys with no
-- schema change.
--
-- updated_by is the admin who last wrote the row; NULL for the seeded defaults
-- below and for rows whose author was later deleted (ON DELETE SET NULL — a
-- setting must outlive the account that set it).
CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_by UUID REFERENCES users (id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed the two label keys at their compiled-in defaults so the admin Settings
-- page shows concrete values on first load. These literals MUST match the single
-- Go source of the defaults, settings.Default{PRDLabel,AutopilotLabel} (keys
-- settings.Key{PRDLabel,AutopilotLabel}); the accessor falls back to the same
-- values if a row is ever absent. SQL cannot reference the Go constants, so a
-- change to those defaults that should reseed needs a follow-up migration.
INSERT INTO app_settings (key, value) VALUES
    ('prd_label', 'PRD'),
    ('autopilot_label', 'autopilot');

-- +goose Down
DROP TABLE app_settings;
