-- +goose Up

-- Worker → token binding (PRD #104 M3, D1). A worker may name which of its
-- owner's Anthropic credentials its RUN-lane claims spend. NULL means "whatever
-- my owner's default is", which is what every existing worker keeps being, so
-- this column changes nothing until someone sets it.
--
-- Why the FK is composite, and why a plain REFERENCES user_secrets (id) is not
-- acceptable (D11): a single-column FK guarantees the secret ROW EXISTS, not that
-- the worker's owner owns it. PATCH /api/workers/{id} takes a secret id from the
-- caller, so under a single-column FK a crafted request would bind a worker to
-- ANOTHER user's credential and quietly spend their Anthropic account — the
-- handler check would be the only thing standing in the way, and a handler check
-- is one refactor away from being bypassed. Referencing (user_id, id) instead
-- makes the database itself reject it: the pair must exist on ONE user_secrets
-- row, and workers.user_id is the worker's owner, so a foreign secret has no
-- matching pair. This is why 00077 added UNIQUE (user_id, id) — a redundant
-- uniqueness statement that exists solely to be a legal FK target.
--
-- ON DELETE SET NULL, not CASCADE (D5): deleting a token must not delete the
-- workers bound to it. They fall back to the owner's default, which is a quiet
-- behavior change — acceptable as behavior, NOT acceptable as UX, so M2's delete
-- confirmation names the affected workers before it happens.
--
-- The COLUMN LIST on SET NULL is load-bearing, not decoration. A bare
-- `ON DELETE SET NULL` on a COMPOSITE foreign key nulls EVERY referencing column,
-- which here means workers.user_id as well as the binding. user_id is NOT NULL, so
-- deleting a token that any worker is bound to would fail the NOT NULL constraint
-- and the DELETE would error out — D5's "deleting a bound token unbinds its
-- workers" would be impossible rather than automatic, and the failure would only
-- show up the first time a user deleted a token that happened to be bound.
-- `SET NULL (anthropic_secret_id)` (Postgres 15+) nulls only the binding and
-- leaves the worker's owner alone. M4's users.judge_anthropic_secret_id needs the
-- same treatment, where nulling users.id would be worse still.
--
-- The FK is deliberately NOT declared ON UPDATE: user_secrets.id and user_id are
-- both immutable in every write path (D10's rotate replaces the ciphertext in
-- place, it never re-keys the row), so there is no update to cascade.
--
-- A NULL binding satisfies the FK under the default MATCH SIMPLE (any NULL column
-- means the constraint is not checked), which is exactly what "no binding, use my
-- default" needs.
ALTER TABLE workers
    ADD COLUMN anthropic_secret_id UUID,
    ADD CONSTRAINT workers_anthropic_secret_fk
        FOREIGN KEY (user_id, anthropic_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE SET NULL (anthropic_secret_id);

-- Partial: only bound workers are interesting to look up, and the overwhelming
-- majority of rows are NULL. It backs the delete-confirmation query M2 needs
-- ("which of my workers point at this token?"), which would otherwise scan.
CREATE INDEX idx_workers_anthropic_secret ON workers (anthropic_secret_id)
    WHERE anthropic_secret_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_workers_anthropic_secret;
ALTER TABLE workers DROP CONSTRAINT workers_anthropic_secret_fk;
ALTER TABLE workers DROP COLUMN anthropic_secret_id;
