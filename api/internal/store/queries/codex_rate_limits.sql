-- Codex per-ACCOUNT rate-limit snapshot primitives (PRD #1209 M1), STORE/SCHEMA only —
-- ships DARK. The poller (M2) writes the two revision-fenced upserts; the read surface
-- (M3) drives the owner/admin reads. Companion schema: 00239 (codex_account_rate_limits +
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
-- WHERE puts every fence in front of the write: a failed fence selects no row, so nothing
-- is written on EITHER the insert or the conflict path. :execrows — 0 rows means authority
-- moved, and the caller discards the reading rather than writing it stale.
--
-- 🔴 ENABLEMENT fence (PRD #1732 D6/D13). A Codex reading describes an ACCOUNT, and an
-- account is live only while at least one ENABLED alias is linked to it, so the fence is
-- account-level: some linked alias must still be enabled, AND @enablement_sig — the
-- (alias id, enablement_rev) list of the account's linked aliases that the poll listing
-- captured when the poll started (ListLinkedCodexAccountsToPoll.enablement_sig) — must be
-- unchanged. The first half alone would let a poll that started before the last alias was
-- disabled, and finished after it was re-enabled, land its reading; the revision list
-- rejects it, because every transition bumps the alias's enablement_rev. The list covers
-- every linked alias, so a sibling's transition (or a newly linked alias) also discards the
-- in-flight reading: conservative, and the next poll writes a fresh one.
-- FOR SHARE OF us serialises the check against a transition, as the Anthropic
-- UpsertRateLimits' FOR SHARE does for its single token row: the enablement handler's FOR
-- UPDATE and SetSecretEnablement's UPDATE conflict with it, so a transition that commits
-- first is seen (the re-read rows carry the new revision and the fence fails), and one that
-- comes second waits for this write.
--
-- 🔴 LOCK ORDER (PRD #1732 D14). Unlike the Anthropic write this one share-locks SEVERAL
-- alias rows, and the transactions it serialises against lock several too (disable-default
-- then hand off to a replacement, or make-default: clear the old default then set the new
-- one, in either id order). Row locks alone would let each side hold one alias and wait for
-- the other: a deadlock (40P01). So the statement first takes the user's secret-mutation
-- advisory lock (store.LockSecretMutation's key: class SecretMutationLockClass = 1970959211,
-- objid = the uuid's first four bytes as a signed int32) in SHARED mode, in the
-- secret_mutation_lock CTE that gates the linked_aliases scan, and only then share-locks the
-- rows (in id order). Every multi-row transition takes that lock EXCLUSIVELY as its first
-- statement, so while this write holds it no transition holds any alias row, and a
-- transition that holds it makes this write wait before it locks anything. Concurrent poll
-- writes share it and do not serialise with each other. It is XACT-scoped: an autocommit
-- statement releases it when the statement ends. TestCodexFencedWriteTakesSecretMutationLockLiveDB
-- pins both the key derivation and the wait.
--
-- On conflict the reading + observed counters + success/attempt timestamps are overwritten
-- and attempt_error is cleared (a success clears the last failure's reason).
WITH secret_mutation_lock AS MATERIALIZED (
    SELECT pg_advisory_xact_lock_shared(
        1970959211,
        ('x' || substr(CAST(@user_id::uuid AS text), 1, 8))::bit(32)::int
    )
), linked_aliases AS (
    SELECT us.id, us.enablement_rev, us.disabled_at
    FROM codex_credential_state s
    JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
    WHERE EXISTS (SELECT 1 FROM secret_mutation_lock)
        AND s.user_id = @user_id AND s.provider_account_id = @provider_account_id
        AND s.status = 'linked'
    ORDER BY us.id
    FOR SHARE OF us
)
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
    SELECT 1 FROM linked_aliases la WHERE la.disabled_at IS NULL
) AND (
    SELECT COALESCE(string_agg(la.id::text || ':' || la.enablement_rev::text, ',' ORDER BY la.id), '')
    FROM linked_aliases la
) = @enablement_sig::text
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
-- WITHOUT touching the last good reading. Same fences as UpsertCodexAccountRateLimits
-- (still-current generation, an enabled linked alias, and the unchanged enablement list the
-- poll started under, PRD #1732 D13), so a failure whose account moved — or whose last
-- enabled alias was disabled mid-poll — is also discarded. The INSERT path (first-ever poll
-- is a failure) writes NULL buckets and NULL last_success_at — health with no reading. The
-- conflict path updates ONLY the attempt fields (last_attempt_at / attempt_status /
-- attempt_error) and DELIBERATELY leaves buckets, observed_generation,
-- observed_credential_revision and last_success_at intact, so a failure after a prior
-- success reads as "stale reading, last attempt failed" rather than discarding the reading.
-- :execrows — 0 rows means authority moved; the caller discards. The alias share locks are
-- taken behind the shared secret-mutation advisory lock, exactly as in
-- UpsertCodexAccountRateLimits (see its LOCK ORDER note).
WITH secret_mutation_lock AS MATERIALIZED (
    SELECT pg_advisory_xact_lock_shared(
        1970959211,
        ('x' || substr(CAST(@user_id::uuid AS text), 1, 8))::bit(32)::int
    )
), linked_aliases AS (
    SELECT us.id, us.enablement_rev, us.disabled_at
    FROM codex_credential_state s
    JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
    WHERE EXISTS (SELECT 1 FROM secret_mutation_lock)
        AND s.user_id = @user_id AND s.provider_account_id = @provider_account_id
        AND s.status = 'linked'
    ORDER BY us.id
    FOR SHARE OF us
)
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
    SELECT 1 FROM linked_aliases la WHERE la.disabled_at IS NULL
) AND (
    SELECT COALESCE(string_agg(la.id::text || ':' || la.enablement_rev::text, ',' ORDER BY la.id), '')
    FROM linked_aliases la
) = @enablement_sig::text
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
    a.reauth_reason,
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
    a.reauth_reason,
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
-- least one ENABLED linked alias, so the poller polls the ACCOUNT once regardless of how
-- many codex_auth aliases resolve to it (dedup by construction — the EXISTS collapses
-- duplicate aliases to one account row). Carries the identity + coordination fields the
-- poller needs to decide whether to poll and to fence its write: generation/credential_revision (the
-- fence the upserts require), coord_state (skip an in-flight refresh), reauth_required (skip
-- an account already flagged), and has_recovery (a populated recovery slot is a reconcile,
-- not a poll, target). Ordered by (user, account) for a deterministic tick.
--
-- PRD #1732 D6: an account is polled (and so background-refreshed, since the poll's 401
-- path is the only background refresh) only while at least one ENABLED alias is linked to
-- it; one disabled sibling does not stop an enabled one. enablement_sig is the (alias id,
-- enablement_rev) list of ALL the account's linked aliases at listing time — the value the
-- poll's writes are fenced on (D13, see UpsertCodexAccountRateLimits). It must produce the
-- same text the two writes recompute (same element format, separator and order).
SELECT
    a.user_id,
    a.id                              AS provider_account_id,
    a.generation,
    a.credential_revision,
    a.workspace_account_id,
    a.provider_user_id,
    a.coord_state,
    a.reauth_required,
    (a.recovery_sealed IS NOT NULL)::boolean AS has_recovery,
    (
        SELECT COALESCE(string_agg(us.id::text || ':' || us.enablement_rev::text, ',' ORDER BY us.id), '')
        FROM codex_credential_state s
        JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
        WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
    )::text AS enablement_sig
FROM codex_provider_account a
WHERE EXISTS (
    SELECT 1 FROM codex_credential_state s
    JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
    WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
        AND us.disabled_at IS NULL
)
ORDER BY a.user_id, a.id;

-- name: ListLinkedCodexAccountsForUser :many
-- The owner-scoped sibling of ListLinkedCodexAccountsToPoll (PRD #1209 M1): the same
-- one-row-per-canonical-account shape and enabled-linked-alias rule (PRD #1732 D6),
-- filtered to one user. The poller's poke path uses it, so a poke never polls, refreshes
-- or recovers an account with no enabled linked alias.
SELECT
    a.user_id,
    a.id                              AS provider_account_id,
    a.generation,
    a.credential_revision,
    a.workspace_account_id,
    a.provider_user_id,
    a.coord_state,
    a.reauth_required,
    (a.recovery_sealed IS NOT NULL)::boolean AS has_recovery,
    (
        SELECT COALESCE(string_agg(us.id::text || ':' || us.enablement_rev::text, ',' ORDER BY us.id), '')
        FROM codex_credential_state s
        JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
        WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
    )::text AS enablement_sig
FROM codex_provider_account a
WHERE a.user_id = @user_id
    AND EXISTS (
        SELECT 1 FROM codex_credential_state s
        JOIN user_secrets us ON us.id = s.user_secret_id AND us.user_id = s.user_id
        WHERE s.user_id = a.user_id AND s.provider_account_id = a.id AND s.status = 'linked'
            AND us.disabled_at IS NULL
    )
ORDER BY a.id;
