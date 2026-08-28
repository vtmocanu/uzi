-- +goose Up
-- Instance branding logo bytes (PRD #685 M1, Decision D7). Config (modes, flags,
-- company text) lives in app_settings; only the LOGO BYTES live here, so the hot
-- settings cache never loads a blob (Risk R3) and the public /api/branding JSON stays
-- small (Risk R2). At most two rows: slot='app' (the app mark) and slot='brand' (the
-- POWERED BY logo). Read only by the image routes; updated_by references the admin who
-- last wrote it and nulls out if that user is deleted.
CREATE TABLE branding_assets (
    slot         TEXT PRIMARY KEY CHECK (slot IN ('app', 'brand')),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png', 'image/webp', 'image/svg+xml')),
    bytes        BYTEA NOT NULL,
    updated_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE branding_assets;
