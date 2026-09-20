-- Codex per-ACCOUNT rate-limit snapshot primitives (PRD #1209 M1), STORE/SCHEMA only —
-- ships DARK. The poller (M2) writes the two revision-fenced upserts; the read surface
-- (M3) drives the owner/admin reads. Companion schema: 00238 (codex_account_rate_limits +
-- codex_provider_account.reauth_*), 00199/00200 (codex_provider_account /
-- codex_credential_state).

-- name: UpsertCodexAccountRateLimits :execrows
-- The revision-fenced SUCCESS write (PRD #1209 M1): install a fresh reading on an account,
-- but ONLY IF authority still holds — the account is still at the (generation,
-- credential_revision) the poll observed AND still has at least one linked alias. Both are
-- required: the generation fence rejects a reading whose account rotated its login under
-- the poll (the reading describes a superseded generation), and the linked-alias fence
-- rejects a reading for an account whose last codex_auth alias was unlinked/deleted between
-- observation and write (there is no longer a subscription to meter). INSERT ... SELECT ...
-- WHERE puts BOTH fences in front of the write: a failed fence selects no row, so nothing
-- is written on EITHER the insert or the conflict path. :execrows — 0 rows means authority
-- moved, and the caller discards the reading rather than writing it stale.
--
-- On conflict the reading + observed counters + success/attempt timestamps are overwritten
-- and attempt_error is cleared (a success clears the last failure's reason).
INSERT INTO codex_account_rate_limits (
    user_id, provider_account_id, buckets,
    observed_generation, observed_credential_revision,
    last_success_at, last_attempt_at, attempt_status, attempt_error
)
SELECT @user_id, @provider_account_id, @buckets::jsonb,
       @observed_generation::bigint, @observed_credential_revision::bigint,
       now(), now(), @attempt_status::text, NULL::text
WHERE EXISTS (
    SELECT 1 FROM codex_provider_account a
    WHERE a.user_id = @user_id AND a.id = @provider_account_id
        AND a.generation = @observed_generation::bigint
        AND a.credential_revision = @observed_credential_revision::bigint
) AND EXISTS (
    SELECT 1 FROM codex_credential_state s
    WHERE s.user_id = @user_id AND s.provider_account_id = @provider_account_id
        AND s.status = 'linked'
)
ON CONFLICT (user_id, provider_account_id) DO UPDATE SET
    buckets                      = EXCLUDED.buckets,
    observed_generation          = EXCLUDED.observed_generation,
    observed_credential_revision = EXCLUDED.observed_credential_revision,
    last_success_at              = now(),
    last_attempt_at              = now(),
    attempt_status               = EXCLUDED.attempt_status,
    attempt_error                = NULL,
    updated_at                   = now();

-- name: RecordCodexAccountPollFailure :execrows
-- The health-only FAILURE write (PRD #1209 M1): record that the last poll ATTEMPT failed,
-- WITHOUT touching the last good reading. Same double fence as UpsertCodexAccountRateLimits
-- (still-current generation AND a linked alias), so a failure whose account moved is also
-- discarded. The INSERT path (first-ever poll is a failure) writes NULL buckets and NULL
-- last_success_at — health with no reading. The conflict path updates ONLY the attempt
-- fields (last_attempt_at / attempt_status / attempt_error) and DELIBERATELY leaves
-- buckets, observed_generation, observed_credential_revision and last_success_at intact, so
-- a failure after a prior success reads as "stale reading, last attempt failed" rather than
-- discarding the reading. :execrows — 0 rows means authority moved; the caller discards.
INSERT INTO codex_account_rate_limits (
    user_id, provider_account_id, buckets,
    observed_generation, observed_credential_revision,
    last_success_at, last_attempt_at, attempt_status, attempt_error
)
SELECT @user_id, @provider_account_id, NULL::jsonb,
       NULL::bigint, NULL::bigint,
       NULL::timestamptz, now(), @attempt_status::text, @attempt_error::text
WHERE EXISTS (
    SELECT 1 FROM codex_provider_account a
    WHERE a.user_id = @user_id AND a.id = @provider_account_id
        AND a.generation = @observed_generation::bigint
        AND a.credential_revision = @observed_credential_revision::bigint
) AND EXISTS (
    SELECT 1 FROM codex_credential_state s
    WHERE s.user_id = @user_id AND s.provider_account_id = @provider_account_id
        AND s.status = 'linked'
)
ON CONFLICT (user_id, provider_account_id) DO UPDATE SET
    last_attempt_at = now(),
    attempt_status  = EXCLUDED.attempt_status,
    attempt_error   = EXCLUDED.attempt_error,
    updated_at      = now();

-- name: GetCodexAccountRateLimitsForUser :many
-- The OWNER read (PRD #1209 M1): one row per canonical LINKED subscription account this
-- user owns, LEFT JOINed to the snapshot so an account with no reading yet still appears
-- (all snapshot fields NULL) rather than vanishing — the anthropic ListRateLimitsForUser
-- shape. The lateral rolls up the account's linked-alias labels (default-first, then by
-- label) and whether any of them is the user's default codex credential, so the DTO can
-- name WHICH account each meter describes without a second query. Owner-scoped; carries
-- only labels + flags + the reading, never a token/login/raw-provider-id. Ordered by
-- account id for a deterministic list.
SELECT
    a.id                         AS provider_account_id,
    COALESCE(al.labels, '{}')::text[] AS aliases,
    COALESCE(al.is_default, false)    AS is_default,
    a.reauth_required,
    rl.buckets,
    rl.observed_generation,
    rl.observed_credential_revision,
    rl.last_success_at,
    rl.last_attempt_at,
    rl.attempt_status,
    rl.attempt_error
FROM codex_provider_account a
LEFT JOIN codex_account_rate_limits rl
    ON rl.user_id = a.user_id AND rl.provider_account_id = a.id
LEFT JOIN LATERAL (
    SELECT array_agg(us.label ORDER BY us.is_default DESC, lower(us.label)) AS labels,
           bool_or(us.is_default)                                           AS is_default
    FROM codex_credential_state s
    JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
    WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
) al ON true
WHERE a.user_id = @user_id
    AND EXISTS (
        SELECT 1 FROM codex_credential_state s
        WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
    )
ORDER BY a.id;

-- name: ListCodexAccountRateLimits :many
-- The ADMIN cross-user read (PRD #1209 M1): GetCodexAccountRateLimitsForUser's shape across
-- ALL users, joined to users(id, email, display_name) so the admin view names the owner.
-- Driven from codex_provider_account (one row per canonical linked account), so a user with
-- no linked codex account simply does not appear — the per-account view, not a per-user
-- one. vault_locked is DELIBERATELY not selected: like ListRateLimits it is computed
-- in-memory from the live vault, never stored, so the handler folds it in. Ordered by email
-- then account id for a stable admin list.
SELECT
    u.id                         AS user_id,
    u.email                      AS email,
    u.display_name               AS display_name,
    a.id                         AS provider_account_id,
    COALESCE(al.labels, '{}')::text[] AS aliases,
    COALESCE(al.is_default, false)    AS is_default,
    a.reauth_required,
    rl.buckets,
    rl.observed_generation,
    rl.observed_credential_revision,
    rl.last_success_at,
    rl.last_attempt_at,
    rl.attempt_status,
    rl.attempt_error
FROM codex_provider_account a
JOIN users u ON u.id = a.user_id
LEFT JOIN codex_account_rate_limits rl
    ON rl.user_id = a.user_id AND rl.provider_account_id = a.id
LEFT JOIN LATERAL (
    SELECT array_agg(us.label ORDER BY us.is_default DESC, lower(us.label)) AS labels,
           bool_or(us.is_default)                                           AS is_default
    FROM codex_credential_state s
    JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
    WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
) al ON true
WHERE EXISTS (
    SELECT 1 FROM codex_credential_state s
    WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
)
ORDER BY u.email ASC, a.id ASC;

-- name: ListLinkedCodexAccountsToPoll :many
-- The factory-wide poll listing (PRD #1209 M1): one row per canonical account that has at
-- least one linked alias, so the poller polls the ACCOUNT once regardless of how many
-- codex_auth aliases resolve to it (dedup by construction — the EXISTS collapses duplicate
-- aliases to one account row). Carries the identity + coordination fields the poller needs
-- to decide whether to poll and to fence its write: generation/credential_revision (the
-- fence the upserts require), coord_state (skip an in-flight refresh), reauth_required (skip
-- an account already flagged), and has_recovery (a populated recovery slot is a reconcile,
-- not a poll, target). Ordered by (user, account) for a deterministic tick.
SELECT
    a.user_id,
    a.id                              AS provider_account_id,
    a.generation,
    a.credential_revision,
    a.workspace_account_id,
    a.provider_user_id,
    a.coord_state,
    a.reauth_required,
    (a.recovery_sealed IS NOT NULL)::boolean AS has_recovery
FROM codex_provider_account a
WHERE EXISTS (
    SELECT 1 FROM codex_credential_state s
    WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
)
ORDER BY a.user_id, a.id;

-- name: ListLinkedCodexAccountsForUser :many
-- The owner-scoped sibling of ListLinkedCodexAccountsToPoll (PRD #1209 M1): the same
-- one-row-per-canonical-linked-account shape, filtered to one user. The settings handler
-- can use it to resolve the caller's linked accounts in one read.
SELECT
    a.user_id,
    a.id                              AS provider_account_id,
    a.generation,
    a.credential_revision,
    a.workspace_account_id,
    a.provider_user_id,
    a.coord_state,
    a.reauth_required,
    (a.recovery_sealed IS NOT NULL)::boolean AS has_recovery
FROM codex_provider_account a
WHERE a.user_id = @user_id
    AND EXISTS (
        SELECT 1 FROM codex_credential_state s
        WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
    )
ORDER BY a.id;
