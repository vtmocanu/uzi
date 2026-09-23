-- +goose Up

-- Per-harness worker model defaults (PRD #1551 M1 / D2). Two nullable columns beside
-- the legacy users.default_model (00031 / PRD #17), which STAYS as a compatibility
-- projection for one release (D2): a NULL in either lane means "inherit" exactly as a
-- NULL default_model did.
--   - default_claude_model: the retained Anthropic worker-model default.
--   - default_codex_model:  the retained Codex worker-model default.
-- No CHECK and no NOT NULL: the model vocabulary is validated at every write surface
-- (agenttmpl.ValidateModel plus the closed cross-vocabulary lists), so the columns stay
-- free to hold a curated alias or a validated custom id without a migration.
ALTER TABLE users
    ADD COLUMN default_claude_model text,
    ADD COLUMN default_codex_model text;

-- Conservative backfill (D2). The three curated Codex ids that #1429 (widened by #1567)
-- allowed to reach a Codex run copy into the Codex lane; EVERY other non-null legacy
-- value copies into the Claude lane. An unknown legacy value was never an effective
-- Codex default, so it must not become newly executable on Codex merely because this
-- migration ran. default_model itself is left untouched (the Up path discards no value).
UPDATE users
SET default_codex_model = default_model
WHERE default_model IN ('gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-sol');

UPDATE users
SET default_claude_model = default_model
WHERE default_model IS NOT NULL
  AND default_model NOT IN ('gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-sol');

-- +goose Down

-- Down projects the ACTIVE harness's lane back into the legacy default_model column
-- BEFORE dropping the two new columns (D2 / decision log 2026-09-23). "Active" is the
-- PRD #1106 D11 harness resolver (api/internal/workersvc/harness_resolver.go) rendered
-- in SQL: it recomputes against the user's CURRENT credentials, so a goose Down after a
-- credential change yields the current lane, not the lane stored at the last save.
--
-- Two independent lane values cannot collapse losslessly into one column, so the
-- INACTIVE lane is LOST on Down. The active preference is retained; this is the
-- accepted, documented limitation. An image-only rollback (no goose Down) instead reads
-- the legacy default_model column that grouped saves keep equal to the effective lane.
--
-- The resolver, rendered exactly:
--   claude_usable = the user holds an anthropic_token secret (UserHasAnthropicToken).
--   codex_usable  = the user's single DEFAULT codex credential is usable:
--       - a default codex_auth alias is usable ONLY when its codex_credential_state row
--         is status='linked' AND provider_account_id IS NOT NULL; a non-linked default
--         codex_auth is NOT usable and does NOT fall through to an api key;
--       - else a default openai_api_key alias is usable by existence;
--       - with no codex credential at all, Codex is not usable.
--   harness = default_harness when non-null AND that harness is usable; else the sole
--     usable harness; else claude when both usable; else claude when neither usable
--     (ResolveSettingsHarness maps "no usable credential" to claude).
--   default_model := CASE harness WHEN 'codex' THEN default_codex_model
--                                 ELSE default_claude_model END  (NULL included).
UPDATE users u
SET default_model = CASE
        WHEN res.harness = 'codex' THEN u.default_codex_model
        ELSE u.default_claude_model
    END
FROM (
    SELECT
        c.user_id,
        CASE
            WHEN c.default_harness = 'claude' AND c.claude_usable THEN 'claude'
            WHEN c.default_harness = 'codex'  AND c.codex_usable  THEN 'codex'
            WHEN c.claude_usable AND NOT c.codex_usable THEN 'claude'
            WHEN c.codex_usable AND NOT c.claude_usable THEN 'codex'
            WHEN c.claude_usable AND c.codex_usable THEN 'claude'
            ELSE 'claude'
        END AS harness
    FROM (
        SELECT
            u2.id AS user_id,
            u2.default_harness AS default_harness,
            EXISTS (
                SELECT 1 FROM user_secrets s
                WHERE s.user_id = u2.id AND s.kind = 'anthropic_token'
            ) AS claude_usable,
            CASE
                WHEN NOT EXISTS (
                    SELECT 1 FROM user_secrets s
                    WHERE s.user_id = u2.id AND s.kind IN ('openai_api_key', 'codex_auth')
                ) THEN false
                WHEN EXISTS (
                    SELECT 1 FROM user_secrets s
                    WHERE s.user_id = u2.id AND s.kind = 'codex_auth' AND s.is_default
                ) THEN EXISTS (
                    SELECT 1
                    FROM user_secrets s
                    JOIN codex_credential_state st
                        ON st.user_secret_id = s.id AND st.user_id = u2.id
                    WHERE s.user_id = u2.id AND s.kind = 'codex_auth' AND s.is_default
                        AND st.status = 'linked' AND st.provider_account_id IS NOT NULL
                )
                WHEN EXISTS (
                    SELECT 1 FROM user_secrets s
                    WHERE s.user_id = u2.id AND s.kind = 'openai_api_key' AND s.is_default
                ) THEN true
                ELSE false
            END AS codex_usable
        FROM users u2
    ) c
) res
WHERE u.id = res.user_id;

ALTER TABLE users
    DROP COLUMN default_claude_model,
    DROP COLUMN default_codex_model;
