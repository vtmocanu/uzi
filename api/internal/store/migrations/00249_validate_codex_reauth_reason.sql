-- +goose Up

-- Validate the CHECK added NOT VALID in 00248 (issue #1594). VALIDATE CONSTRAINT scans
-- codex_provider_account to confirm existing rows satisfy the reason CHECK but takes only
-- a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent reads and writes proceed),
-- unlike the ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT ... CHECK would
-- hold. Split from 00248 so the add is lock-cheap and the validation non-blocking, per the
-- two-step pattern (00239/00240) for a CHECK on a live table.
--
-- NOTE (goose numbering): drafted as 00249, immediately after 00248; renumber the PAIR
-- above the live head together at landing via `task migration:renumber` if another
-- migration lands first.
--
-- The backlog satisfies the CHECK by construction: 00248 added reauth_reason nullable
-- with no default, so every existing row has reauth_reason NULL and the CHECK (which only
-- constrains a non-NULL reason) holds vacuously.
ALTER TABLE codex_provider_account VALIDATE CONSTRAINT codex_provider_account_reauth_reason_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00249 state — the constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK body matches 00248's Up verbatim; 00248's Down then drops it with
-- the column. Mirrors 00240's Down.
ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_reauth_reason_check;
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_reauth_reason_check
    CHECK ((reauth_reason IS NULL) OR (reauth_required AND reauth_reason IN ('provider_rejected'))) NOT VALID;
