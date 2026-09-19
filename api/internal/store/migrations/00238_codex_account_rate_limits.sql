-- +goose Up

-- Codex account rate-limit visibility (PRD #1209 M1): the STORE/SCHEMA + wire-contract
-- freeze the two later milestones (the poller and the read surface) build on. Ships
-- DARK — no poller writes these rows and no endpoint reads them until M2/M3. Every
-- statement is ADDITIVE and safe for an N-1 worker during a rolling release: a worker
-- that never touches the new table or columns keeps working.
--
-- NOTE (goose numbering): drafted as 00238 — the next free number above the live head
-- (00237) at drafting time — and renumbered above the live head at landing via
-- `task migration:renumber` if another migration lands first, per the CLAUDE.md
-- convention. Its sibling 00239 (VALIDATE) must stay immediately after it.

-- The per-account rate-limit snapshot, keyed on the CANONICAL account, not the alias:
-- the meters Codex reports are properties of the subscription ACCOUNT (like
-- codex_provider_account's generation/revision), so one row per account rather than one
-- per codex_auth alias that resolves to it, mirroring why 00080 repointed the anthropic
-- gauge per-token. The PK is (user_id, provider_account_id) so the composite FK below can
-- reference the owner-scoped codex_provider_account_user_id_id_key (00199), the same D11
-- ownership shape 00200 uses: a snapshot row can only ever describe an account the SAME
-- user owns, and deleting the account cascades its snapshot away.
--
-- buckets is the raw meter payload as Codex reports it (nullable — NULL until the first
-- successful poll, and preserved across a later poll FAILURE so a health-only write never
-- discards the last good reading). observed_generation / observed_credential_revision are
-- the account counters the successful reading was fenced against, so a reader can tell a
-- snapshot from a superseded account. The last_success_at / last_attempt_at pair plus
-- attempt_status / attempt_error carry the poll HEALTH separately from the reading, so a
-- failure after a success shows "stale reading, last attempt failed" rather than nothing.
CREATE TABLE codex_account_rate_limits (
    user_id                      UUID NOT NULL,
    provider_account_id          UUID NOT NULL,

    -- The raw meter payload as Codex reports it. Nullable: NULL before the first
    -- successful poll, and PRESERVED across a later failure (RecordCodexAccountPollFailure
    -- never overwrites it), so a stale-but-present reading survives a health-only write.
    buckets                      JSONB,

    -- The account counters the last SUCCESSFUL reading was fenced against (00199's
    -- generation / credential_revision). NULL until the first success; a reader compares
    -- them against the live account to decide whether the reading still describes it.
    observed_generation          BIGINT,
    observed_credential_revision BIGINT,

    -- Poll health, kept apart from the reading. last_success_at is when buckets were last
    -- written (NULL until the first success, preserved across a later failure);
    -- last_attempt_at is when the poller last TRIED (moved on every write, success or
    -- failure). attempt_status / attempt_error carry the latest attempt's outcome.
    last_success_at              TIMESTAMPTZ,
    last_attempt_at              TIMESTAMPTZ,
    attempt_status               TEXT,
    attempt_error                TEXT,

    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, provider_account_id),

    -- Owner-scoped composite FK to the account (00199's codex_provider_account_user_id_id_key).
    -- ON DELETE CASCADE, not SET NULL: a snapshot for a deleted account is meaningless, so
    -- the row goes with it (like 00080's anthropic gauge cascade), and no column list is
    -- needed because both PK columns cascade together.
    FOREIGN KEY (user_id, provider_account_id)
        REFERENCES codex_provider_account (user_id, id) ON DELETE CASCADE
);

-- The admin cross-user view and any by-user read group by user, so an index on (user_id)
-- mirrors idx_anthropic_rate_limits_user (00080). The PK's leading column is already
-- user_id, so this index is redundant for a pure equality lookup — kept anyway to match
-- the anthropic gauge's shape and to stay useful if the PK column order ever changes.
CREATE INDEX idx_codex_account_rate_limits_user ON codex_account_rate_limits (user_id);

-- codex_provider_account: the reauth-required signal (PRD #1209 M1). A poll that fails
-- because the account's login needs re-authentication sets reauth_required=true and
-- records the (generation, credential_revision) it observed, so a later verified install
-- (a refresh commit / recovery promotion / out-of-band re-login) can clear it atomically
-- in the same statement that advances the generation. NOT NULL DEFAULT false so every
-- existing row is "no reauth needed" and the ALTER rewrites no row; the two observation
-- counters are NULLABLE (populated only while reauth is required).
ALTER TABLE codex_provider_account ADD COLUMN reauth_required BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE codex_provider_account ADD COLUMN reauth_generation BIGINT;
ALTER TABLE codex_provider_account ADD COLUMN reauth_credential_revision BIGINT;

-- Coherence: when reauth is required the two observation counters MUST both be present, so
-- a reader (and the atomic-clear WHERE in RefreshCodexAccountLogin) can trust them. Added
-- NOT VALID (enforced for new/updated rows immediately; the backlog scan deferred to
-- 00239's VALIDATE CONSTRAINT), the lock-cheap two-step 00226/00227 established for a
-- CHECK on a live table — an inline validated CHECK would scan every row under the ACCESS
-- EXCLUSIVE lock the ALTER already holds. Every existing row is reauth_required=false, so
-- the backlog satisfies it vacuously.
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_reauth_coherence
    CHECK ((reauth_required = false) OR (reauth_generation IS NOT NULL AND reauth_credential_revision IS NOT NULL)) NOT VALID;

-- users: which linked Codex SUBSCRIPTION accounts the user chose to surface as rate meters
-- on the sidebar rail (PRD #1209 M1), mirroring sidebar_token_ids (00123) for the anthropic
-- side. A uuid[] column on users rather than a join table: it is a per-user UI preference,
-- not a property of the account. NULL and '{}' both read as "no explicit extras"; the
-- handler validates the ids against the caller's linked accounts and prunes stale ids
-- atomically (PruneUserSidebarCodexAccounts), so a since-unlinked id is dropped on read.
ALTER TABLE users ADD COLUMN sidebar_codex_account_ids uuid[];

-- +goose Down

-- True inverse of the additive Up (goose downs are not run in this deployment;
-- store.Migrate only ever goes up). Drop the constraint before the columns it references,
-- then the columns, then the table.
ALTER TABLE users DROP COLUMN IF EXISTS sidebar_codex_account_ids;

ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_reauth_coherence;
ALTER TABLE codex_provider_account DROP COLUMN IF EXISTS reauth_credential_revision;
ALTER TABLE codex_provider_account DROP COLUMN IF EXISTS reauth_generation;
ALTER TABLE codex_provider_account DROP COLUMN IF EXISTS reauth_required;

DROP TABLE codex_account_rate_limits;
