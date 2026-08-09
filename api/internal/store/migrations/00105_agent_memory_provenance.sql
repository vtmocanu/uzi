-- +goose Up

-- PRD #266: writer-declared provenance for agent memory. Each entry now carries
-- WHY it should be trusted — a `basis` ("observed" = the writing run saw it happen
-- in this repo; "inferred" = the lead reasoned it, weaker) and free-text `evidence`
-- (a pointer to what backs the claim: a file, a command's output, a run). Both are
-- ADDITIVE and NULLABLE with no backfill: legacy rows predate provenance and read
-- back as "inferred" (the conservative default) via the API read mapper, NOT a DDL
-- default, so the single Go source of truth owns the normalization. basis is stored
-- verbatim (even an unknown value) and normalized on read; evidence is omitted from
-- the DTO when empty.
ALTER TABLE agent_memory ADD COLUMN basis text;
ALTER TABLE agent_memory ADD COLUMN evidence text;

-- +goose Down
ALTER TABLE agent_memory DROP COLUMN evidence;
ALTER TABLE agent_memory DROP COLUMN basis;
