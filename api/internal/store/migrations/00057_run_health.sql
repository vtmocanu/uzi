-- +goose Up

-- Run-health detector (PRD #47): a non-terminal, self-clearing flag the sweeper's
-- detector writes to surface a run that looks slow, stalled, or looping WITHOUT
-- killing it — the existing RUN_TIMEOUT / idle / iteration caps remain the only
-- liveness backstops. Draft migration number: renumbered to the next free number
-- above the live head at merge time (convention in CLAUDE.md).
--
--   last_activity_at   finest activity marker, bumped inside AppendMessages'
--                      existing high-water-mark write (UpdateRunLastSeq) so the
--                      stalled signal needs no per-tick run_messages aggregate
--                      (Decision 2). Backfilled from updated_at. NULL until the
--                      first message on a fresh run; the stalled clock falls back
--                      to started_at while it is NULL (GREATEST semantics).
--   health             the current flag, or 'ok'. Orthogonal to status. Written
--                      ONLY by the sweeper's detector while a run is in a flaggable
--                      status (queued / running / awaiting_approval), and reset to
--                      'ok' by every query that moves a run OUT of a flaggable
--                      status (the exit contract, Decision 3) — so a terminal run
--                      never carries a stale flag.
--   health_reason      a short, server-controlled explanation for the owner (e.g.
--                      "no worker is online"). Fixed templates only: never a tool
--                      name, tool input, or repo content, and no live duration (the
--                      UI recomputes elapsed from health_since). NULL when 'ok'.
--   health_since       when the current flag was raised, for the UI's "stuck for
--                      Xm". NULL when 'ok'.
--   health_notified_at rolling last-nudge stamp owned by the Slack path (Decision
--                      7). Deliberately NOT cleared by the exit contract — it damps
--                      DM flapping across episodes and restarts.
ALTER TABLE runs
    ADD COLUMN last_activity_at   timestamptz,
    ADD COLUMN health             text NOT NULL DEFAULT 'ok'
        CHECK (health IN ('ok', 'stalled', 'looping', 'slow', 'waiting_worker', 'approval_idle')),
    ADD COLUMN health_reason      text,
    ADD COLUMN health_since       timestamptz,
    ADD COLUMN health_notified_at timestamptz;

-- Backfill last_activity_at so a run in flight at migration time has a sane stall
-- baseline instead of a NULL that would read as "never active" (Decision 2). New
-- rows leave it NULL until their first message; the detector falls back to
-- started_at meanwhile.
UPDATE runs SET last_activity_at = updated_at WHERE last_activity_at IS NULL;

-- +goose Down
ALTER TABLE runs
    DROP COLUMN last_activity_at,
    DROP COLUMN health,
    DROP COLUMN health_reason,
    DROP COLUMN health_since,
    DROP COLUMN health_notified_at;
