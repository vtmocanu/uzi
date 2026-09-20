-- +goose Up

-- Validate the CHECK added NOT VALID in 00239 (PRD #1209 M1). VALIDATE CONSTRAINT scans
-- codex_provider_account to confirm existing rows satisfy the coherence CHECK but takes
-- only a SHARE UPDATE EXCLUSIVE lock (write-compatible: concurrent reads and writes
-- proceed), unlike the ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT ... CHECK
-- would hold. Split from 00239 so the add is lock-cheap and the validation non-blocking,
-- per the two-step pattern (00226/00227, 00233/00234) for a CHECK on a live table.
--
-- NOTE (goose numbering): drafted as 00240, immediately after 00239; renumber the PAIR
-- above the live head together at landing via `task migration:renumber` if another
-- migration lands first.
--
-- The backlog satisfies the CHECK by construction: 00239 added reauth_required NOT NULL
-- DEFAULT false, so every existing row has reauth_required=false and the coherence CHECK
-- (which only constrains a row with reauth_required=true) holds vacuously.
ALTER TABLE codex_provider_account VALIDATE CONSTRAINT codex_provider_account_reauth_coherence;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00240 state — the constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK body matches 00239's Up verbatim; 00239's Down then drops it with
-- the columns. Mirrors 00227's / 00234's Down exactly.
ALTER TABLE codex_provider_account DROP CONSTRAINT codex_provider_account_reauth_coherence;
ALTER TABLE codex_provider_account ADD CONSTRAINT codex_provider_account_reauth_coherence
    CHECK ((reauth_required = false) OR (reauth_generation IS NOT NULL AND reauth_credential_revision IS NOT NULL)) NOT VALID;
