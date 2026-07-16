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
-- optional for external workers (PRD #18: NULL = no choice made). Shape follows
-- runs_kind_shape (00043) and agent_templates_user_scope_ck (00048): an explicit
-- per-kind OR, not a CASE, so an unmatched kind fails the check rather than
-- evaluating to NULL and passing it.
ALTER TABLE workers ADD CONSTRAINT ck_workers_hosted_metadata CHECK (
    (kind = 'hosted'   AND hosted_size IS NOT NULL AND template_declared IS NOT NULL)
 OR (kind = 'external' AND hosted_size IS NULL)
);

-- The controller's desired-state poll scans hosted rows only; every other worker
-- query is user-scoped and untouched by this feature.
CREATE INDEX idx_workers_hosted ON workers (kind) WHERE kind = 'hosted';

-- The delivered-once join-token handoff (Decision 3). At provision the api seals
-- the plaintext here; the controller collects it on its next poll and writes it
-- into the worker's k8s Secret; the pod boots, reads the file, and REGISTERS with
-- it — and that registration is what clears the ciphertext.
--
-- Delivery is settled by proof of possession, never by the controller's say-so.
-- RequireWorker resolves a worker by matching sha256(the presented token) against
-- workers.token_hash, so a successful register is unforgeable evidence that a pod
-- holds the CURRENT token. A controller ack could only assert "a Secret exists for
-- W" without naming the token inside it, which destroyed a freshly rotated
-- plaintext undelivered and left this row claiming a delivery that never happened.
-- It also demanded a Secret read, which Decision 1's RBAC refuses.
--
-- Until that proof arrives the plaintext stays parked, so a lost poll response, a
-- controller crash, or a partition costs nothing but a re-delivery on the next
-- poll: no worker is ever stranded holding a token_hash whose plaintext no longer
-- exists anywhere.
--
-- Its own table, NOT a column on `workers`: every worker read is a `SELECT *`
-- (GetWorkerByTokenHash, ListWorkersByUser, RegisterWorker/HeartbeatWorker's
-- RETURNING *), so a sealed-plaintext column would ride store.Worker into every
-- handler and DTO in the codebase, one careless field away from leaking. Here it
-- is reachable only from the queries that exist to touch it. Deleting the worker
-- cascades its token row away.
--
-- Two columns, never one overloaded: `token_ciphertext IS NULL` alone could not
-- distinguish "the controller has it" from "it expired unread" from "there never
-- was one", and those demand opposite responses (nothing / rotate / provision).
-- The three legal states are:
--
--   ciphertext NOT NULL, delivered_at NULL      -> pending: no pod has proved it holds
--                                                  this token yet
--   ciphertext NULL,     delivered_at NOT NULL  -> delivered: a pod registered with it,
--                                                  the buffer is destroyed (steady state)
--   ciphertext NULL,     delivered_at NULL      -> EXPIRED unread by the sweeper. Benign
--                                                  if the Secret was written (a late
--                                                  register self-heals this to delivered);
--                                                  a genuine strand if it was not, and
--                                                  then recovery must mint a NEW token
--                                                  (never resurrect the old plaintext),
--                                                  bumping hosted_generation
--
-- The fourth combination (a ciphertext still present after delivery) is the one
-- state that would mean the handoff failed to destroy what it promised to, so the
-- CHECK forbids it outright rather than trusting every future writer.
CREATE TABLE hosted_worker_tokens (
    worker_id        uuid PRIMARY KEY REFERENCES workers ON DELETE CASCADE,
    -- secretbox (AES-256-GCM) sealed join-token plaintext, AAD-bound to
    -- worker_id + kind so a DB-write operator cannot move a ciphertext onto
    -- another worker's row and have it still open. NULL once delivered or expired.
    token_ciphertext bytea,
    -- When the controller acked the delivery. NULL while pending or expired.
    delivered_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT hosted_worker_tokens_delivered_ck
        CHECK (NOT (token_ciphertext IS NOT NULL AND delivered_at IS NOT NULL))
);

-- The expiry sweep's scan: pending rows only. Partial, so the index stays empty on
-- a stack that never provisions a hosted worker — which is every compose stack.
CREATE INDEX idx_hosted_worker_tokens_pending ON hosted_worker_tokens (created_at)
    WHERE token_ciphertext IS NOT NULL;

-- +goose Down
DROP INDEX idx_hosted_worker_tokens_pending;
DROP TABLE hosted_worker_tokens;
DROP INDEX idx_workers_hosted;
ALTER TABLE workers DROP CONSTRAINT ck_workers_hosted_metadata;
ALTER TABLE workers
    DROP COLUMN hosted_generation,
    DROP COLUMN hosted_size,
    DROP COLUMN kind;
