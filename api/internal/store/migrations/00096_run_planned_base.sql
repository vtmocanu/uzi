-- +goose Up

-- Staleness guard (PRD #209 M4): the commit a SEEDED plan was written against, plus
-- whether a divergence should FAIL the run rather than warn.
--
--   planned_base_commit  the commit the external planner (a local Claude Code session)
--                        planned against, forwarded on `uzi run create --planned-commit`.
--                        Nullable: only a seeded run with --planned-commit sets it; every
--                        other run (ordinary issue run, or a seeded run that gave no
--                        commit) leaves it NULL, and the worker then proceeds silently.
--                        Stored verbatim as the user gave it (a full or abbreviated SHA);
--                        the worker compares prefix-tolerantly against the clone's
--                        resolved base, so a short SHA still matches its full form.
--   require_base_match   whether a base-commit divergence should FAIL the run instead of
--                        warning into the feed (`uzi run create --require-base`). Default
--                        false: the run warns and implements anyway (Open Question 3, the
--                        lead's decision). Only meaningful alongside planned_base_commit;
--                        without a planned commit to compare against the flag can never
--                        fire, which the CLI and the API reject at create time.
--
-- NOT NULL DEFAULT false on require_base_match is deliberate: like auto_approve/wait_on_limit
-- it reads as a plain Go bool rather than a pgtype.Bool unwrap, and the default backfills
-- every existing row to the warn-only behaviour (Success Criterion 2 — a run created with no
-- seeded plan behaves byte-identically to a pre-#209 run). planned_base_commit stays nullable
-- because "no commit was supplied" and "an empty commit" are different, and only the former
-- must exist.
ALTER TABLE runs
    ADD COLUMN planned_base_commit text,
    ADD COLUMN require_base_match  boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE runs
    DROP COLUMN planned_base_commit,
    DROP COLUMN require_base_match;
