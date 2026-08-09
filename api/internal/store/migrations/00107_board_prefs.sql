-- +goose Up

-- Per-(user, repo) board view preferences (PRD #196 M3): the server-side home for
-- what today is per-browser localStorage. Two fields:
--   extra_labels — the user's board membership extras override. NULLABLE, and the
--     nullability is the sentinel: NULL means "not customized" (the client falls
--     back to the admin default board_extra_labels), while a JSON array — INCLUDING
--     the empty array [] — is the user's ABSOLUTE set (Decision 9). So this column
--     must NOT carry a NOT NULL default; the "unset vs empty" distinction depends on
--     NULL being expressible.
--   show_all — the old per-browser "show all other issues" boolean, now per-account.
-- One row per (user, repo). This governs VISIBILITY only; it never touches run
-- eligibility or the run gate (that is M4).
CREATE TABLE board_prefs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    repo_id      UUID NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    extra_labels JSONB,                              -- NULL = not customized; JSON array = absolute set
    show_all     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, repo_id)
);

-- +goose Down
DROP TABLE board_prefs;
