-- +goose Up

-- The per-run Codex claim binding (PRD #1147 M2), STORE/SCHEMA only — ships DARK.
-- Mirrors 00086 (the Anthropic per-run secret binding) column-for-column in shape and
-- FK discipline; the differences are Codex-specific and documented inline.
--
-- WHY FROZEN ON THE RUN. A Codex claim spends a subscription whose material rotates
-- underneath it (the m2 refresher advances codex_provider_account.generation and the
-- alias's codex_credential_state.material_revision). A run must remember the identity
-- and revisions it was created/linked AT, so the service can later compare run-frozen
-- vs current (GetRunCodexAuthContext) and decide whether the run still holds authority
-- over the account. A join alone cannot answer "what was true when this run bound?" —
-- the current row has already moved. Hence the snapshot columns below.
--
-- All added columns are nullable (a non-codex run carries none) EXCEPT
-- codex_claim_epoch, which is NOT NULL DEFAULT 0 so every run — codex or not — has a
-- monotonic per-claim capability epoch the requeue paths can bump unconditionally.
ALTER TABLE runs
    -- Which user_secrets alias this claim spends (the codex analogue of
    -- anthropic_secret_id). NULL for a non-codex run.
    ADD COLUMN codex_secret_id         UUID,

    -- 'subscription' (a codex_auth login) or 'api_key' (a standalone openai_api_key).
    -- NULL for a non-codex run. Closed by the CHECK below.
    ADD COLUMN codex_auth_mode         TEXT,
    ADD CONSTRAINT runs_codex_auth_mode_check
        CHECK (codex_auth_mode IS NULL OR codex_auth_mode IN ('subscription', 'api_key')),

    -- A snapshot of the alias label at bind time, kept for the same reason 00086
    -- snapshots anthropic_secret_label: the FK nulls the id on delete and a rename
    -- rewrites the label in place, so only the snapshot keeps a finished run's history
    -- readable afterwards.
    ADD COLUMN codex_secret_label      TEXT,

    -- The frozen canonical identity tuple (provider_user_id, workspace_account_id),
    -- serialized as a two-element JSON array TEXT: json.Marshal([]string{provider_user_id,
    -- workspace_account_id}), e.g. ["provider-x","workspace-y"]. This is the identity the
    -- run is authoritative for; NULL until it is frozen at first link
    -- (SetRunCodexFrozenIdentity). A JSON array is used rather than a NUL-separated
    -- (E'\x00') join because Postgres TEXT cannot store a NUL byte (SQLSTATE 22021), and
    -- JSON escaping keeps the two components unambiguous while staying a plain comparable
    -- TEXT the authority check equality-tests as-is (both sides serialize the same way, so
    -- no parse is needed to compare).
    ADD COLUMN codex_account_key       TEXT,

    -- The alias's material_revision (codex_credential_state) frozen at run creation;
    -- NULL for a non-codex run. Compared against the CURRENT alias revision to detect
    -- a material change under the run.
    ADD COLUMN codex_material_revision BIGINT,

    -- The account's credential_revision (codex_provider_account) frozen at link; NULL
    -- until the run links to an account.
    ADD COLUMN codex_account_revision  BIGINT,

    -- The per-claim capability epoch. Bumped every time the capability is minted
    -- (SetRunCodexClaimCapability) or revoked (the three requeue paths), so a stale
    -- capability minted under an earlier ownership can be told apart from the current
    -- one. NOT NULL DEFAULT 0: a non-codex run simply never advances past 0.
    ADD COLUMN codex_claim_epoch       BIGINT NOT NULL DEFAULT 0,

    -- The hash of the currently-valid per-claim capability, or NULL when the run holds
    -- none (never minted, or revoked on ownership loss).
    ADD COLUMN codex_cap_hash          BYTEA,

    -- 🔴 THE COLUMN LIST ON SET NULL IS LOAD-BEARING, exactly as 00086 documents. A
    -- bare ON DELETE SET NULL on a COMPOSITE FK nulls EVERY referencing column, which
    -- here includes runs.user_id (NOT NULL, 00020), so deleting a bound secret would
    -- fail the NOT NULL constraint and the DELETE would error. SET NULL
    -- (codex_secret_id) (Postgres 15+) nulls only the binding and leaves the run's
    -- owner alone.
    ADD CONSTRAINT runs_codex_secret_fk
        FOREIGN KEY (user_id, codex_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE SET NULL (codex_secret_id);

-- Partial, like 00086's idx_runs_anthropic_secret: Postgres indexes no referencing
-- side of a FK, so without this every DELETE of a user_secrets row seq-scans `runs`.
CREATE INDEX idx_runs_codex_secret ON runs (codex_secret_id)
    WHERE codex_secret_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_runs_codex_secret;
ALTER TABLE runs
    DROP CONSTRAINT runs_codex_secret_fk,
    DROP CONSTRAINT runs_codex_auth_mode_check,
    DROP COLUMN codex_cap_hash,
    DROP COLUMN codex_claim_epoch,
    DROP COLUMN codex_account_revision,
    DROP COLUMN codex_material_revision,
    DROP COLUMN codex_account_key,
    DROP COLUMN codex_secret_label,
    DROP COLUMN codex_auth_mode,
    DROP COLUMN codex_secret_id;
