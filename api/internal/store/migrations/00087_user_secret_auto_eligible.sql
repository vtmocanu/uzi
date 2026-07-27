-- +goose Up

-- The auto-selection OPT-IN pool (PRD #111 M2, D2). A worker in `auto` mode (M3)
-- spends only credentials their owner has deliberately flagged here.
--
-- WHY THE DEFAULT IS false, AND WHY THAT IS THE WHOLE POINT. Auto-selecting over
-- every token a user holds would be actively wrong, not merely eager: the product
-- already documents holding a subscription token "for the work" and a console key
-- "for the retrospectives" (docs/anthropic-token.md), and a pool that helps itself
-- to both spends the reserved key on ordinary runs. So opting a token IN is a
-- deliberate act, and this migration adds the column without changing what any
-- existing token does.
--
-- NOT NULL DEFAULT false rather than a nullable tri-state: "the user has not
-- decided yet" and "the user has not opted in" are the same fact for a spend
-- decision, and a NULL would only invite a reader to treat them differently.
--
-- The CHECK ties the flag to the one kind it means anything for. Today it is
-- vacuous — 'anthropic_token' is user_secrets' only kind (00010's own CHECK) — and
-- that is exactly why it is cheap to add now: it costs nothing while it cannot
-- fire, and the day a second kind lands it makes "meaningful only for Anthropic
-- tokens" an enforced fact rather than a comment someone has to remember. Written
-- as `NOT auto_eligible OR kind = ...` so a false flag stays legal for every kind.
ALTER TABLE user_secrets
    ADD COLUMN auto_eligible BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT user_secrets_auto_eligible_kind_check
        CHECK (NOT auto_eligible OR kind = 'anthropic_token');

-- Deliberately NO index. The pool is read one user at a time (the ranker's
-- candidate query and the settings list are both `WHERE user_id = $1 AND kind =
-- 'anthropic_token'`), which the existing per-user access path already serves, and
-- a boolean over a handful of rows per user is not a selective index. Adding one
-- would cost a write on every token mutation to speed up nothing.

-- +goose Down
ALTER TABLE user_secrets
    DROP CONSTRAINT user_secrets_auto_eligible_kind_check,
    DROP COLUMN auto_eligible;
