-- +goose Up
-- OIDC SSO identity (PRD #45). A uzi account is matched to an IdP account by the
-- (issuer, subject) pair: the subject is the stable join key — an IdP email can be
-- reassigned, a subject cannot. Both columns are NULL for password-only accounts.
ALTER TABLE users ADD COLUMN oidc_issuer  TEXT;
ALTER TABLE users ADD COLUMN oidc_subject TEXT;

-- One uzi account per IdP identity. Partial (WHERE oidc_subject IS NOT NULL) so the
-- many password-only rows are exempt, and so a recycled/reassigned IdP email can
-- never silently attach to a row already bound to a different subject (audit H1):
-- the LinkUserOIDC update carries WHERE oidc_subject IS NULL and the handler asserts
-- exactly one row, and this index guards the (issuer, subject) uniqueness itself.
CREATE UNIQUE INDEX users_oidc_identity_key ON users (oidc_issuer, oidc_subject)
    WHERE oidc_subject IS NOT NULL;

-- OIDC-created users have no password (Decision 7). Relaxing the NOT NULL lets the
-- passwordless rows exist; Login treats a NULL hash as "password login always fails,
-- constant-time" rather than an error.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

-- +goose Down
-- Re-tightening password_hash fails if any passwordless OIDC user exists; that is
-- the intended safety interlock for a down-migration on a live OIDC deployment.
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
DROP INDEX users_oidc_identity_key;
ALTER TABLE users DROP COLUMN oidc_subject;
ALTER TABLE users DROP COLUMN oidc_issuer;
