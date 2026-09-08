-- +goose Up

-- The per-user APPEARANCE model (PRD #1167 "Lights on" M1). Four nullable columns
-- layered beside the legacy users.theme (00xxx / PRD #21), which STAYS: a NULL in any
-- of these means "inherit the instance default", exactly as a NULL theme did.
--   - appearance_mode: 'system' | 'light' | 'dark' (which polarity slot renders).
--   - light_theme / dark_theme: the theme id for each polarity slot.
--   - typeface: 'system' | 'plex'.
-- No CHECK constraints and no NOT NULL: the closed value sets live in the theme
-- registry (api/internal/theme) and are enforced at every write surface, so the
-- column stays free to gain a theme id without a migration (PRD #21 SC5). NULL is
-- the inherit sentinel the resolver (theme.ResolveAppearance) treats as absent.
ALTER TABLE users
    ADD COLUMN appearance_mode text,
    ADD COLUMN light_theme text,
    ADD COLUMN dark_theme text,
    ADD COLUMN typeface text;

-- Backfill so nobody's screen changes on upgrade: an explicit past theme override
-- becomes the dark slot pinned to dark mode (PRD #1167 decision log 2026-09-07).
UPDATE users SET dark_theme = theme, appearance_mode = 'dark' WHERE theme IS NOT NULL;

-- +goose Down
ALTER TABLE users
    DROP COLUMN appearance_mode,
    DROP COLUMN light_theme,
    DROP COLUMN dark_theme,
    DROP COLUMN typeface;
