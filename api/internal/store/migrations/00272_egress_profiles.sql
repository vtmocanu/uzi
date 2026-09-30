-- +goose Up

-- PRD #1906 M1: egress profiles, the admin-managed, NAMED site lists a later milestone
-- binds a no-internet research run to (Decision 4). A run will reference a list by name
-- only; no request ever sends domains. Only admins create or edit lists (the cookie-only
-- admin write group).
--
-- hosts holds the NORMALIZED entries (lowercase, IDNA A-label, no trailing dot): an exact
-- host or "*.base". The api normalizes and validates every entry before writing
-- (api/internal/egressprofile), so the CHECKs below are a floor against a writer that
-- skips that code, not the rule itself: the public suffix and multi-publisher rules live
-- in Go because they need the Public Suffix List.
--
-- multi_publisher_override records the entries an admin explicitly accepted although they
-- reach a known multi-publisher host (code hosting, path-style object storage, docs
-- hosting, forums). It is always a subset of hosts.
--
-- created_by / updated_by are provenance only: ON DELETE SET NULL, so removing an admin
-- account never removes a site list.
--
-- Purely ADDITIVE: one brand-new table, no change to an existing one. The migration
-- number is a draft: it is renumbered above the live head at landing.
CREATE TABLE egress_profiles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- A lowercase slug, 1-64 characters: usable as a path segment and a CLI argument.
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    hosts text[] NOT NULL CHECK (
        cardinality(hosts) BETWEEN 1 AND 200
        AND array_position(hosts, NULL) IS NULL
    ),
    multi_publisher_override text[] NOT NULL DEFAULT '{}' CHECK (
        multi_publisher_override <@ hosts
        AND array_position(multi_publisher_override, NULL) IS NULL
    ),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE egress_profiles;
