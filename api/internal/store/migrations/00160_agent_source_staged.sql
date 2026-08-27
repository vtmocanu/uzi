-- +goose Up
-- PRD #602 (M3): the agent-source staging table. It holds the latest
-- SUCCESSFULLY-staged reconcile snapshot — the parsed role set and the computed
-- per-name diff — that an admin reviews and M4 applies. This table is a PREVIEW
-- only: the reconcile job never writes agent_templates, so nothing here reaches a
-- run before an admin approves it (see adr/0602-agent-source-repo-sync.md,
-- "Approve-before-apply is the primary control").
--
-- A SINGLETON: at most one row ever exists (the latest good snapshot). The
-- singleton boolean is UNIQUE and CHECK-pinned to true, so the M3 upsert's
-- ON CONFLICT (singleton) DO UPDATE keeps updating the one row rather than
-- accumulating history. jsonb/text columns are NOT NULL with defaults so a nil
-- from the writer never becomes SQL NULL.
CREATE TABLE agent_source_staged (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  singleton    boolean NOT NULL DEFAULT true UNIQUE,   -- enforces at most one row
  fetched_at   timestamptz NOT NULL DEFAULT now(),
  fetched_sha  text NOT NULL,
  source_url   text NOT NULL,
  source_ref   text NOT NULL,
  roles        jsonb NOT NULL DEFAULT '[]'::jsonb,   -- parsed roles to apply + skipped/failed ones w/ reason
  diff         jsonb NOT NULL DEFAULT '[]'::jsonb,   -- per-name classification (add/override/conflict/unchanged/remove)
  CONSTRAINT agent_source_staged_singleton_ck CHECK (singleton = true)
);

-- +goose Down
DROP TABLE agent_source_staged;
