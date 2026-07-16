-- +goose Up

-- Hosted workers (PRD #58): a worker whose container the CONTROLLER runs in the
-- cluster, as opposed to an external one the user runs by hand. A hosted worker
-- is an ordinary worker in every other respect — same join-token trust anchor,
-- same heartbeat-derived status, same runs — so this extends `workers` rather
-- than shadowing it in a parallel table.
--
--   kind              external (today's hand-run worker) | hosted.
--   hosted_size       the S/M/L preset name. Deliberately NOT value-CHECKed:
--                     sizes are code constants (Decision 7) validated server-side
--                     against the built-in registry, exactly like template_declared
--                     (PRD #18) is validated via workertmpl.Valid. A CHECK here
--                     would force a migration to add a preset.
--   hosted_generation bumped by the api whenever the desired spec changes; the
--                     controller compares it against what it observes in the
--                     cluster to detect drift (Decision 9). Monotonic per worker.
--
-- The worker TYPE is template_declared (PRD #18), not a new column: Decision 7
-- makes a hosted worker's type exactly its template, and its drift invariant
-- (template_reported == template_declared) is the one PRD #18 already ships. A
-- second hosted-only template column would duplicate that with its own drift risk.
ALTER TABLE workers
    ADD COLUMN kind              text   NOT NULL DEFAULT 'external' CHECK (kind IN ('external', 'hosted')),
    ADD COLUMN hosted_size       text,
    ADD COLUMN hosted_generation bigint NOT NULL DEFAULT 0;

-- A hosted worker always knows its type and size (the controller cannot render a
-- pod spec without both); an external one carries no size. template_declared stays
-- optional for external workers (PRD #18: NULL = no choice made).
ALTER TABLE workers ADD CONSTRAINT ck_workers_hosted_metadata CHECK (
    CASE kind
        WHEN 'hosted'   THEN hosted_size IS NOT NULL AND template_declared IS NOT NULL
        WHEN 'external' THEN hosted_size IS NULL
    END
);

-- The controller's desired-state poll scans hosted rows only; every other worker
-- query is user-scoped and untouched by this feature.
CREATE INDEX idx_workers_hosted ON workers (kind) WHERE kind = 'hosted';

-- The delivered-once join-token handoff (Decision 3). At provision the api seals
-- the plaintext here; the controller picks it up on its next poll, writes it into
-- the worker's k8s Secret, and only once it OBSERVES that Secret does it ack —
-- which is what deletes this row. So delivery is at-least-once against a durable
-- ack, never fire-and-forget: a poll response lost to a crash or a partition is
-- simply re-delivered, and no worker is ever stranded holding a token_hash whose
-- plaintext no longer exists anywhere.
--
-- Its own table, NOT a column on `workers`: every worker read is a `SELECT *`
-- (GetWorkerByTokenHash, ListWorkersByUser, RegisterWorker/HeartbeatWorker's
-- RETURNING *), so a sealed-plaintext column would ride store.Worker into every
-- handler and DTO in the codebase, one careless field away from leaking. Here it
-- is reachable only from the two queries that exist to touch it. Deleting the
-- worker cascades the pending token away.
CREATE TABLE hosted_worker_tokens (
    worker_id        uuid PRIMARY KEY REFERENCES workers ON DELETE CASCADE,
    -- secretbox (AES-256-GCM) sealed join-token plaintext, AAD-bound to worker_id
    -- so a DB-write operator cannot move a ciphertext onto another worker's row
    -- and have it still authenticate.
    token_ciphertext bytea       NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE hosted_worker_tokens;
DROP INDEX idx_workers_hosted;
ALTER TABLE workers DROP CONSTRAINT ck_workers_hosted_metadata;
ALTER TABLE workers
    DROP COLUMN hosted_generation,
    DROP COLUMN hosted_size,
    DROP COLUMN kind;
