-- +goose Up

-- PRD #914: make users.ci_autofix_enabled a nullable tri-state (NULL = inherit the
-- admin global default, which is ON). Mirrors mr_rework (00165). The column was added
-- NOT NULL DEFAULT false (00115), so history cannot distinguish "opted out" from
-- "never chose" — both are false. We deliberately TURN EVERYONE ON: fold existing
-- false -> NULL (inherit = on), preserve existing true. New rows get NULL (the insert
-- paths never set the column and the default is dropped) = inherit = on. This
-- RE-ENABLES prior explicit opt-outs — an accepted one-time trade (best practice is
-- preserved by the tri-state going forward). The gate becomes `IS NOT FALSE`.
ALTER TABLE users ALTER COLUMN ci_autofix_enabled DROP NOT NULL, ALTER COLUMN ci_autofix_enabled DROP DEFAULT;
UPDATE users SET ci_autofix_enabled = NULL WHERE ci_autofix_enabled = false;

-- +goose Down

-- LOSSY: rollback collapses every inherit-on (NULL) row to off; it CANNOT restore the
-- pre-Up values (the fold above is irreversible). This only restores the column shape.
UPDATE users SET ci_autofix_enabled = false WHERE ci_autofix_enabled IS NULL;
ALTER TABLE users ALTER COLUMN ci_autofix_enabled SET DEFAULT false;
ALTER TABLE users ALTER COLUMN ci_autofix_enabled SET NOT NULL;
