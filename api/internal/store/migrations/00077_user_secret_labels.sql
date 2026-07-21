-- +goose Up

-- Named per-user secrets (PRD #104 M1). Until now a user could hold exactly one
-- Anthropic credential, because UNIQUE (user_id, kind) said so (00010). That made
-- "the user's token" and "the row" the same thing, and every read path resolved it
-- by (user_id, kind). This migration breaks that identity: a user may hold several
-- secrets of one kind, each with a label they chose, and the identity of a
-- credential becomes user_secrets.id.
--
-- Why an explicit is_default flag rather than "oldest wins" (D2): the fallback has
-- to be stable under deletion. "Oldest wins" silently re-points every unbound
-- worker to a different account at the exact moment a user deletes a token, which
-- is when they are least likely to notice that their spend moved.
--
-- Why lower(label) and why an INDEX (D7): labels are user-facing text, so `Console`
-- and `console` must not be two tokens. Postgres cannot put an expression in a
-- UNIQUE *constraint*, so this is necessarily CREATE UNIQUE INDEX rather than a
-- table constraint. Labels are not secret — they appear in the UI, the CLI, admin
-- views and logs; the token value continues to appear in none of those.
--
-- What the partial index does and does NOT guarantee (D12): it enforces AT MOST one
-- default per (user_id, kind). "Exactly one, while any token exists" is NOT
-- schema-enforceable and is deliberately left as an application invariant that M2
-- enforces transactionally (SELECT ... FOR UPDATE over the user's rows, covering
-- concurrent first-creates, the delete-the-default check, and the two-statement
-- set-default swap). Do not read this index as that stronger guarantee: four
-- existing "does this user have a token?" gates (judge_enqueue, judge_read,
-- autopilot, UserHasAnthropicToken) rely on the invariant, not on the index.
--
-- Why UNIQUE (user_id, id) when id is already the primary key (D11): it is
-- redundant as a uniqueness statement and load-bearing as an FK target. It is what
-- lets M3/M4 declare FOREIGN KEY (user_id, anthropic_secret_id) REFERENCES
-- user_secrets (user_id, id), which makes binding a worker to ANOTHER user's
-- credential impossible in the schema rather than only in a handler check.
--
-- Residual, accepted, and worth stating here because this migration is what
-- narrows it (R10): DEK-sealed ciphertexts are bound by AAD to user_id||kind
-- (vault.secretAAD). While kind identified the row, that was a per-row binding and
-- the threat model could claim a DB-write operator cannot move a ciphertext onto a
-- different credential. With N tokens of one kind the AAD is identical across all
-- of them, so such an operator can swap token A's ciphertext onto the row labelled
-- `console-key` and it authenticates cleanly. Not fixed here: the adversary is
-- DB-write (strictly stronger than the passive-read adversary the vault targets),
-- and putting id in the AAD needs a versioned AAD scheme plus a rewrap of every
-- stored ciphertext. Documented instead.
--
-- Numbering: drafted as 00104 in the PRD and landed as 00077, the next free number
-- above the live head (00074) once the unmerged prd-98 (00075) and prd-99 (00076)
-- branches are accounted for — per the convention recorded in 00065. The boot
-- runner is strict goose, so a number below an applied head bricks upgrades.

-- label carries a DEFAULT only so existing rows can be backfilled by the ALTER
-- itself; it is dropped below so every future INSERT must name its label rather
-- than silently minting a second 'default'.
ALTER TABLE user_secrets
    ADD COLUMN label TEXT NOT NULL DEFAULT 'default'
        CHECK (char_length(label) BETWEEN 1 AND 64 AND label = btrim(label)),
    ADD COLUMN is_default BOOLEAN NOT NULL DEFAULT false;

-- Backfill: every pre-existing row IS the user's only token of its kind (the
-- constraint dropped below guaranteed it), so it is the default. Runs before the
-- DROP CONSTRAINT so the at-most-one-per-(user,kind) property still holds while
-- this blanket UPDATE sets the flag.
UPDATE user_secrets SET is_default = true;

ALTER TABLE user_secrets ALTER COLUMN label DROP DEFAULT;

ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_user_id_kind_key;

CREATE UNIQUE INDEX user_secrets_user_kind_label_key
    ON user_secrets (user_id, kind, lower(label));

-- NOT ONLY D2's "at most one default". This index is also the control standing
-- between the UNLOCKED D14 compatibility alias (UpsertDefaultUserSecret, which takes
-- no advisory lock) and a two-default state: the alias's sole path to a second
-- default is its ON CONFLICT arbiter missing an existing row, and this refuses that
-- before commit. Read as a data-integrity constraint alone it looks droppable in
-- favour of an application check; it is not. PRD #104 D2 records both mechanisms
-- (this index, plus the alias's write-no-false statement shape, which NO index
-- enforces) and the verification behind them.
CREATE UNIQUE INDEX user_secrets_one_default_key
    ON user_secrets (user_id, kind) WHERE is_default;

ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_user_id_id_key UNIQUE (user_id, id);

-- +goose Down

-- Restoring UNIQUE (user_id, kind) fails if any user has taken more than one token
-- of a kind, which is the honest outcome: this Down cannot decide which of a user's
-- credentials to keep, and picking one would destroy the others. Delete the extra
-- tokens first if you really mean to roll back.
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_user_id_id_key;
DROP INDEX user_secrets_one_default_key;
DROP INDEX user_secrets_user_kind_label_key;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_user_id_kind_key UNIQUE (user_id, kind);
ALTER TABLE user_secrets DROP COLUMN is_default;
ALTER TABLE user_secrets DROP COLUMN label;
