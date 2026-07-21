-- +goose Up

-- Judge-lane → token binding (PRD #104 M4, D1). Which Anthropic credential this
-- user's RETROSPECTIVES spend, independent of what their runs spend. NULL means
-- "my default", which is every user's state until they choose otherwise.
--
-- Why this is a per-USER column and not a per-worker one: a judge run is claimed by
-- an ordinary worker through the same ClaimRun lane, so it would have been easy to
-- let the claiming worker's binding decide. It must not. Which credential reviews
-- your work is a property of you, not of whichever worker happened to pick the
-- retrospective up — otherwise the same retrospective bills different accounts run
-- to run, for no reason a user could see or control. Self-improve runs follow this
-- binding for the same reason.
--
-- The composite FK is D11's ownership guarantee, identical in purpose to 00078's on
-- workers: (id, judge_anthropic_secret_id) → user_secrets (user_id, id) means the
-- pair must exist on ONE user_secrets row, and users.id is the owner, so a
-- credential belonging to someone else has no matching pair and the database
-- refuses it. A plain REFERENCES user_secrets (id) would prove only that the row
-- exists, leaving a handler check as the only thing between a crafted request and
-- another user's Anthropic account.
--
-- THE COLUMN LIST ON SET NULL IS NOT OPTIONAL HERE, AND THE FAILURE IS WORSE THAN
-- 00078's. A bare ON DELETE SET NULL on a composite foreign key nulls EVERY
-- referencing column — which on this table means users.id, the PRIMARY KEY. Postgres
-- would reject the delete (id is NOT NULL), so deleting a judge-bound token would
-- fail outright rather than unbinding, and it would fail only for users who had
-- actually set a judge binding. `SET NULL (judge_anthropic_secret_id)` (Postgres
-- 15+) nulls just the binding. 00078 hit the same trap against workers.user_id; this
-- one aims at the primary key, so it is the same bug one notch more destructive.
ALTER TABLE users
    ADD COLUMN judge_anthropic_secret_id UUID,
    ADD CONSTRAINT users_judge_anthropic_secret_fk
        FOREIGN KEY (id, judge_anthropic_secret_id)
        REFERENCES user_secrets (user_id, id) ON DELETE SET NULL (judge_anthropic_secret_id);

-- Partial, like 00078's: only bound users are worth looking up, and this backs the
-- delete-confirmation query M2 needs ("does deleting this token unbind my judge?").
CREATE INDEX idx_users_judge_anthropic_secret ON users (judge_anthropic_secret_id)
    WHERE judge_anthropic_secret_id IS NOT NULL;

-- +goose Down
DROP INDEX idx_users_judge_anthropic_secret;
ALTER TABLE users DROP CONSTRAINT users_judge_anthropic_secret_fk;
ALTER TABLE users DROP COLUMN judge_anthropic_secret_id;
