-- +goose Up

-- codex_provider_account: WHY a reauth flag was raised (issue #1594). reauth_required
-- (00239) says the login needs re-authentication; reauth_reason is the closed-set,
-- owner-facing reason it was raised for, so the read surface can say more than "reauth
-- required". The one member today is 'provider_rejected': the provider rejected the
-- refresh material with an allowlisted OAuth error code, and the rejection primitive
-- (QuarantineRejectedCodexRefresh) quarantined the account and raised the flag in one
-- transaction. NULL means "no reason recorded" (the flag is down, or it was raised by the
-- rate-limit poll path, MarkCodexReauthRequired, which records none). Nullable with no
-- default, so the ALTER rewrites no row and every existing row reads NULL.
--
-- NOTE (goose numbering): drafted as 00248 — the next free number above the live head
-- (00246) at drafting time — and renumbered above the live head at landing via
-- `task migration:renumber` if another migration lands first (a sibling PRD also drafts
-- 00248), per the CLAUDE.md convention. Its sibling 00249 (VALIDATE) must stay
-- immediately after it.
ALTER TABLE codex_provider_account ADD COLUMN reauth_reason TEXT;

-- A reason may only describe a RAISED flag, and only from the closed set: every path that
-- clears reauth_required clears reauth_reason in the same statement, so a reason never
-- outlives its flag. Added NOT VALID (enforced for new/updated rows immediately; the
-- backlog scan deferred to 00249's VALIDATE CONSTRAINT), the lock-cheap two-step
-- 00239/00240 used for the reauth coherence CHECK. Every existing row has
-- reauth_reason NULL, so the backlog satisfies it vacuously.
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_reauth_reason_check
    CHECK ((reauth_reason IS NULL) OR (reauth_required AND reauth_reason IN ('provider_rejected'))) NOT VALID;

-- +goose Down

-- True inverse of the additive Up (goose downs are not run in this deployment;
-- store.Migrate only ever goes up). Drop the constraint before the column it references.
ALTER TABLE codex_provider_account DROP CONSTRAINT IF EXISTS codex_provider_account_reauth_reason_check;
ALTER TABLE codex_provider_account DROP COLUMN IF EXISTS reauth_reason;
