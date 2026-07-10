-- +goose Up

-- Worker templates (PRD #18): the curated Dockerfile variant a worker runs.
--
-- template_declared is the choice recorded in the UI at join-token issuance (the
-- template the user says this worker will run). template_reported is what the
-- worker self-declares at register (ENV WORKER_TEMPLATE, baked into the image).
-- The server surfaces both and badges drift when they disagree — soft
-- verification only (Decision 5): the join token stays the sole authn boundary,
-- so a hostile worker lying about its template buys nothing, and a mismatch is
-- observability, never a rejection.
--
-- Deliberately NO CHECK constraint tying either column to a fixed name set: the
-- valid templates live in git under agent/templates/<name>/ and evolve by code
-- review, so a DB CHECK would force a migration per new template. `declared` is
-- validated at the API against the code-curated registry; `reported` is accepted
-- as-is (even an unknown name — that IS the drift signal). Both nullable: legacy
-- workers predate the feature (NULL declared), and an older image reports nothing
-- (NULL reported).
ALTER TABLE workers
    ADD COLUMN template_declared text,
    ADD COLUMN template_reported text;

-- +goose Down
ALTER TABLE workers
    DROP COLUMN template_reported,
    DROP COLUMN template_declared;
