-- +goose Up
-- PRD #191 M2 (Decision 2): the Slack conversational surface anchors a chat run on
-- the USER's top-level DM message (its ts becomes root_ts, which a thread reply's
-- thread_ts resolves against). But every existing consumer treats root_ts as the
-- BOT's editable status message (00044: "status edits target it"; notifier.go edits
-- it in place). A bot cannot edit another user's message, so a chat needs a SECOND
-- ts for the bot's own status line — exactly the shape gate_ts/question_ts already
-- have. status_ts is that column: the bot status message the chat state path edits.
--
-- Nullable, like every column added to this table since 00044 (00074, 00093, 00101):
-- NULL means no bot status message was posted for this anchor (a repo-ful issue run,
-- which edits root_ts, never sets it; or a chat whose status post failed). The
-- notifier's chat branch treats NULL as "no status line yet".
ALTER TABLE slack_run_messages ADD COLUMN status_ts text;

-- Decision 4: the replier's (channel_id, root_ts) reverse lookup is a :one, safe only
-- if the pair is unique. It was argued effectively-unique because "each run posts its
-- OWN root message, so its ts is distinct in the DM channel" — but a chat anchors on
-- the USER's message, so that argument no longer describes every row. The conclusion
-- still holds (a given message ts is unique within a channel and is claimed by exactly
-- one run), and this index MAKES it hold rather than leaving it to an argument that
-- now survives by accident: it also fail-closes a redelivered top-level DM into a
-- single anchor instead of two. No existing row can collide — an issue run's root_ts
-- is the bot's globally-unique message ts.
CREATE UNIQUE INDEX slack_run_messages_channel_root_uniq
    ON slack_run_messages (channel_id, root_ts);

-- +goose Down
DROP INDEX slack_run_messages_channel_root_uniq;
ALTER TABLE slack_run_messages DROP COLUMN status_ts;
