-- +goose Up

-- Admin-managed allowlist of tool packages a repo tool profile may request
-- (PRD #18 M4, Technical Design §3). Each row permits one package by exact base
-- name; pinned_version, when set, requires that exact version (name@version) —
-- NULL means any version (or none). Exact-name match for now ("pattern" can
-- extend to globs later). The allowlist is an operability control, not a sandbox
-- (nix packages run build hooks); it bounds WHAT is installed, and must never
-- include a pre-authenticated credential-bearing CLI (Decision 6).
CREATE TABLE tool_allowlist (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL UNIQUE,   -- package base name, ^[a-z0-9][a-z0-9._+-]*$
    pinned_version TEXT,                    -- NULL = any version; else the required version
    note           TEXT,                    -- optional admin note (why it is allowed)
    updated_by     UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-(user, repo) tier-1 tool profile (PRD #18 M4): a plain package list the
-- worker provisions with devbox before the SDK starts. Validated against
-- tool_allowlist at write AND at claim time (the allowlist can shrink under a
-- grandfathered package — the claim then fails, not the save). One profile per
-- (user, repo). packages is a JSON array of "name" / "name@version" strings.
CREATE TABLE repo_tool_profiles (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    repo_id    UUID NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    packages   JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, repo_id)
);

-- Seed the allowlist with the M3 hardcoded set so the feature is usable out of the
-- box and the M3 → M4 transition preserves behavior. Admin-managed thereafter: no
-- reconciler re-seeds these, so deleting one is intentional (unlike a builtin).
INSERT INTO tool_allowlist (name) VALUES
    ('kubectl'), ('terraform'), ('jq'), ('yq'), ('ripgrep'),
    ('fd'), ('go'), ('nodejs'), ('python3');

-- +goose Down
DROP TABLE repo_tool_profiles;
DROP TABLE tool_allowlist;
