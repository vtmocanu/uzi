-- +goose Up

-- Forgejo support (PRD #65), schema half. This migration is INERT: it widens a
-- domain and adds a nullable column, and nothing can reach either yet.
--
-- `forgejo` stays unreachable until PRD #65's M6b flips the handler gate
-- (handler/forge.go:125 advertises the type, :158 rejects everything else), and
-- forge.New has no forgejo arm until M2. Widening the CHECK ahead of the gate is
-- safe precisely BECAUSE the handler still refuses the type: no forgejo row can
-- be written while that refusal stands, so this migration changes no observable
-- behaviour on any existing instance. That ordering is the whole point of M6's
-- split (PRD #65, "Why M6 is split") — the migration is inert and wants to be
-- early; the gate flip is the go-live and must be last.

-- Widen the forge_type domain. The constraint is postgres-AUTO-named: 00002
-- declared it inline on the column (`forge_type text NOT NULL CHECK (forge_type
-- IN ('gitlab'))`), so there is no `ADD CONSTRAINT` to grep for and the name
-- below cannot be found in the migration history. It was read from the live
-- catalog (pg_constraint on a fully migrated database), not assumed.
--
-- Postgres normalizes a single-element IN to a plain equality, so 00002's
-- constraint is stored as `CHECK ((forge_type = 'gitlab'::text))`. That is what
-- the Down below restores — definitionally identical to the original, not merely
-- equivalent.
ALTER TABLE forge_connections DROP CONSTRAINT forge_connections_forge_type_check;
ALTER TABLE forge_connections ADD CONSTRAINT forge_connections_forge_type_check
    CHECK (forge_type IN ('gitlab', 'forgejo'));

-- The merge request's web URL as the FORGE reported it, captured at MR creation
-- (PRD #65 D8). NULL for every run that predates this column and for every run
-- that has not opened an MR.
--
-- It exists because uzi already had this URL and threw it away: forge.MergeRequest
-- carries WebURL, the driver populates it (gitlab.go:468), and the only consumer
-- (forgesvc/mr_watch.go) reads .State and never .WebURL. So the web rebuilds
-- `/-/merge_requests/N` from the issue URL by string surgery
-- (web/src/lib/forgeUrls.ts) — guessing a URL the forge already told us. Forgejo
-- spells the same thing `/{owner}/{repo}/pulls/{n}`, and teaching that file a
-- second URL grammar to guess with is the wrong fix.
--
-- WORKER-OWNED, written once at completion (SetRunCompleted below): the worker is
-- what opens the MR, so it is the first and only place that holds the URL at the
-- moment it comes into existence. Rejected: populating it from mr_watch.go, which
-- also has the URL but would surface the link only after the first watch tick and
-- only for MRs parked in Human Review.
--
-- Deliberately NO backfill, and no attempt to reconstruct one. Old rows keep
-- rendering through forgeUrls.ts, which survives as the GitLab-only legacy path —
-- a fallback that is exactly as correct as it was before this column existed. A
-- backfill would have to guess the very grammar this column exists to stop
-- guessing.
--
-- It is forge-supplied text rendered as an anchor, so the web MUST route it
-- through isHttpsUrl before it becomes an href (D8; M8 owns that). Not a new
-- class of exposure — issues.web_url, repos.web_url and pipeline_web_url are all
-- already forge-supplied and rendered behind the same guard — but the guard is
-- not optional here just because the column is new.
ALTER TABLE runs ADD COLUMN mr_web_url text;

-- +goose Down

ALTER TABLE runs DROP COLUMN mr_web_url;

-- Narrow the domain back to 00002's. This FAILS, by design, if any forgejo row
-- exists: postgres validates existing rows when adding a CHECK, so a database
-- carrying real Forgejo connections refuses to go down rather than silently
-- keeping rows the restored constraint forbids.
--
-- That refusal is the correct outcome and the reason there is no DELETE here. The
-- alternative — deleting forgejo connections to make the constraint fit — would
-- cascade through repos → board_columns/issues (00002) and destroy a user's board
-- to satisfy a schema rollback. An operator who genuinely wants to roll back past
-- Forgejo support must remove those connections deliberately, as an act with its
-- own blast radius, not as a side effect of `goose down`. Same stance as 00058,
-- whose down narrows runs_kind_check and fails the same way on a judge row.
ALTER TABLE forge_connections DROP CONSTRAINT forge_connections_forge_type_check;
ALTER TABLE forge_connections ADD CONSTRAINT forge_connections_forge_type_check
    CHECK (forge_type IN ('gitlab'));
