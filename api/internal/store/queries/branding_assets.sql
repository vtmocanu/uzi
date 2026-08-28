-- name: GetBrandingAsset :one
-- Read one branding logo by slot ('app'|'brand') for the public image route. Absent
-- row → pgx.ErrNoRows, which the handler answers with 404.
SELECT * FROM branding_assets WHERE slot = $1;

-- name: UpsertBrandingAsset :one
-- Admin upload of a logo: insert-or-replace the bytes for a slot, recording the admin
-- who wrote it. now() re-stamps updated_at on every write.
INSERT INTO branding_assets (slot, content_type, bytes, updated_by, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (slot) DO UPDATE
    SET content_type = EXCLUDED.content_type,
        bytes        = EXCLUDED.bytes,
        updated_by   = EXCLUDED.updated_by,
        updated_at   = now()
RETURNING *;

-- name: DeleteBrandingAsset :exec
-- Admin clear of a logo: revert the slot to its mode's fallback (preset or none).
DELETE FROM branding_assets WHERE slot = $1;

-- name: ListBrandingSlots :many
-- Which logo slots have an uploaded asset. Projects ONLY the slot column so the
-- public GET /api/branding presence check never loads the logo BYTES — that endpoint
-- is unauthenticated and AppShell-polled, so a per-poll blob read would defeat the
-- keep-the-JSON-small design (Risk R2). At most two short rows.
SELECT slot FROM branding_assets ORDER BY slot;
