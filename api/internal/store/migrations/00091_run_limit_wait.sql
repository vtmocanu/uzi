-- +goose Up

-- PRD #35 M-SEAM: the schema half of "park a run until the Anthropic usage limit
-- resets, then resume it". This migration adds storage and widens the status
-- domain; NOTHING reads or writes any of it yet. The worker-side detection (M1)
-- and the server park machinery (M2) land on top.
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00090 against a live head of
-- 00089_run_select_reason_check.sql; PRD #102 merged to main first and took
-- 00090_issue_board_position.sql, so this renumbered to 00091 on the landing
-- rebase. Per CLAUDE.md that renumber is mandatory rather than tidy: the boot
-- runner is strict goose (no allow-missing, store/migrate.go), so landing a
-- version BELOW an already-applied head makes every upgraded instance refuse
-- to boot.

-- runs ---------------------------------------------------------------------
--
-- wait_on_limit is the per-run opt-in (Decision 7), stamped at creation from the
-- owner's users.wait_on_limit default or an explicit request override. DEFAULT
-- false makes every existing run, and every creation path not yet taught to stamp
-- it, opt OUT — the safe direction, since an un-opted run keeps today's behaviour
-- exactly (it fails, now with a better reason). M3 teaches all four creation paths.
--
-- limit_resets_at is when the exhausted window reopens, as the worker read it off
-- the SDK frame. It is REPORTED DATA, kept for display and for the Slack line; it
-- is deliberately NOT the promotion gate, because a compromised worker must not be
-- able to park a run for years.
--
-- retry_not_before IS the promotion gate, and it is computed server-side (M2):
-- cross-checked against this user's own anthropic_rate_limits gauge, made
-- pool-aware (Decision 6e), jittered to avoid a stampede across one user's parked
-- runs, and clamped to RUN_LIMIT_MAX_PARK. Nullable because every pre-feature row
-- has no park.
--
-- limit_wait_count counts parks for THIS run and is capped by RUN_LIMIT_MAX_WAITS.
-- It is deliberately distinct from requeue_count rather than folded into it
-- (Decision 5, mirroring the vault lock-race precedent in specs/ai.md §139): the
-- two count different failures — requeue_count is worker deaths, limit_wait_count
-- is limit parks — and a shared counter would let one exhaust the other's budget.
--
-- rate_limit_type is the SDK's rateLimitType for the window that rejected us,
-- after the server has allowlisted it against the SDK union (anything unrecognised
-- is coerced to 'unknown' before it reaches this column). It lives on the ROW, not
-- only in the feed message, because GetSlackRunContext (slack.sql) is an explicit
-- column list selected FROM runs and cannot reach a run_messages payload — the
-- "paused: usage limit (five_hour)" line is unbuildable without it.
--
ALTER TABLE runs
    ADD COLUMN wait_on_limit    boolean     NOT NULL DEFAULT false,
    ADD COLUMN limit_resets_at  timestamptz,
    ADD COLUMN retry_not_before timestamptz,
    ADD COLUMN limit_wait_count int         NOT NULL DEFAULT 0,
    ADD COLUMN rate_limit_type  text;

-- rate_limit_type IS closed, in the SAME migration that adds the column, and that
-- is a deliberate departure from 00086's precedent rather than an oversight of it.
--
-- 00086 left anthropic_select_reason open and said why: "M4 adds five values inside
-- this same PRD, so a CHECK written then would have been rewritten before it ever
-- guarded anything". The discriminating fact there was that the VOCABULARY WAS
-- STILL MOVING while the PRD ran; 00089 closed it the moment it stopped. This
-- column's six members are closed at authoring — they are the whole
-- SDKRateLimitInfo.rateLimitType union at the pinned @anthropic-ai/claude-agent-sdk
-- (sdk.d.ts) — plus the server's own 'unknown' coercion. Nothing inside PRD #35 adds
-- a seventh. The only mover is an SDK pin bump, which is external and rare.
--
-- WHY A CHECK AT ALL, given the column is display-only — and it is the same
-- argument 00089 makes, which applies here MORE strongly than it did there. Nothing
-- branches on this value: it is not read by the state machine, the claim path, any
-- sweep gate, or the promotion predicate. So a wrong value has no failing consumer
-- anywhere. It renders as itself in the run view, means nothing to the user, means
-- nothing to a support query, and now also composes an M5 Slack line. The database
-- is the only reader positioned to notice.
--
-- The CHECK is strictly WEAKER than the Go allowlist the park path applies, so on
-- the shipped path it can never fire — which is the point. It exists for the writers
-- that bypass that path: a backfill, an admin tool, a second writer added later, or
-- a refactor that moves the coercion. An auditor demonstrated the exposure by
-- storing 5009 bytes containing a control character straight into this column.
--
-- THE TWO DRIFT DIRECTIONS ARE NOT SYMMETRIC, AND ONLY ONE IS PINNED. Naming which
-- is which is the whole justification for the CHECK, so read this before "closing
-- the gap" in the unpinned direction.
--
--   Go taught, SQL forgotten  — FAILS HARD. A park carrying the new member raises
--     23514 at runtime, turning a display nicety into a failed run on a user's work.
--     PINNED: TestRateLimitTypeVocabularyMatchesCheck parses the list below out of
--     this file and compares it to the Go allowlist, so this reddens at `go test`,
--     where a developer is, instead of at park time, where a user is. 00089 accepted
--     the identical risk and mitigated it with the identical instrument.
--
--   SDK bumped, Go forgotten  — FAILS SOFT, BY CONSTRUCTION. The new member arrives
--     as an unrecognised string, CoerceRateLimitType maps it to 'unknown', it stores
--     cleanly, and the only cost is display fidelity on one field. DELIBERATELY
--     UNPINNED, and it is not merely an oversight to be corrected later: the SDK
--     typings live in agent/node_modules/@anthropic-ai/claude-agent-sdk/sdk.d.ts,
--     which is outside the api Go module and is not committed. A Go test reading them
--     would be CACHE-INVISIBLE under CLAUDE.md's cross-module rule and would fail
--     outright in the api CI job, where agent/node_modules does not exist. Measured:
--     'seven_day_overage_included' appears in exactly ONE non-node_modules file in
--     the tree, workersvc/limitwait.go. Nothing in this repo observes the SDK.
--
-- (An earlier draft of this paragraph promised "a failing test" on an SDK bump. That
-- was false and is the exact shape of wrong doc this file is otherwise careful about:
-- a promise of a guard that does not exist, sitting in the paragraph whose job is to
-- justify the mitigation.)
--
-- NULL stays legal and is not an eighth value: every run predating this migration
-- has one, as does every run that never parks, so a CHECK rejecting NULL would fail
-- against the existing table at migration time.
ALTER TABLE runs
    ADD CONSTRAINT runs_rate_limit_type_check
        CHECK (rate_limit_type IS NULL OR rate_limit_type IN (
            'five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet',
            'seven_day_overage_included', 'overage', 'unknown'
        ));

-- users --------------------------------------------------------------------
--
-- The per-user default a new run inherits (Decision 7), an exact clone of the
-- users.autopilot_enabled precedent (00037_autopilot_mapping.sql): per-user consent
-- set from the user's own Settings page, default false. app_settings is NOT used —
-- it is admin/instance-wide only ("there is no user_settings table",
-- 00044_slack.sql) — and the caps are env config like the other run clocks.
ALTER TABLE users
    ADD COLUMN wait_on_limit boolean NOT NULL DEFAULT false;

-- status domain ------------------------------------------------------------
--
-- 'limit_wait' is a NON-TERMINAL status: a parked run still holds its issue (the
-- uq_runs_one_active_per_issue partial index is a negative guard and therefore
-- already counts it as active), still cancels server-side (CancelRunServerSide is
-- likewise a negative guard), and is invisible to every sweeper filter and to the
-- PRD #47 health detector because each of those is a POSITIVE allowlist. None of
-- them needed editing; see PRD #35 Decision 3 for the site-by-site argument.
--
-- The constraint is unnamed in 00020_workers_runs.sql (declared inline on the
-- column), so Postgres auto-generated its name. Verified against a live database
-- rather than assumed, since a wrong DROP CONSTRAINT fails at boot:
--   SELECT conname FROM pg_constraint WHERE conrelid='runs'::regclass AND contype='c';
--   -> runs_status_check
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval',
                      'limit_wait', 'completed', 'failed', 'cancelled'));

-- The promotion pass (M2) sweeps `status = 'limit_wait' AND retry_not_before <= now()`
-- on every tick. Partial on the status so the index holds only parked runs — a set
-- that is empty on a healthy instance — rather than one entry per run ever created.
-- Cheap to add now and awkward to retrofit once the table is large.
CREATE INDEX idx_runs_limit_wait_retry
    ON runs (retry_not_before)
    WHERE status = 'limit_wait';

-- +goose Down

-- The status CHECK narrows on the way down, so this Down FAILS LOUDLY if any run is
-- currently parked. That is correct and is not worth "fixing" with a pre-emptive
-- UPDATE: silently rewriting a user's parked run to some other status on a rollback
-- would lose work that the up-migration promised to resume. Cancel or let the parked
-- runs drain first.
--
-- 🔴 THAT FAIL-LOUD PROPERTY IS ENTIRELY LOAD-BEARING ON GOOSE'S TRANSACTION
-- WRAPPER, SO DO NOT ADD THE "NO TRANSACTION" ANNOTATION TO THIS FILE.
--
-- (Named without its leading plus-goose token on purpose. goose's parser fires on
--   HasPrefix(TrimSpace(line), "--") && Contains(line, "<plus>goose")
-- so the trigger is THE TOKEN ITSELF ON ANY COMMENT LINE — not the words "NO
-- TRANSACTION", and not annotation-shaped prose. `-- see the plus-goose docs` bricks
-- this file just as thoroughly. Writing the phrase in prose, as above, is safe BY
-- CONSTRUCTION rather than by care, which is why it is the mitigation chosen here;
-- but someone who only learns "avoid that phrase" still walks into it.
--
-- The failure is at API BOOT: "not supported: invalid annotation", with this and
-- EVERY LATER migration unapplied. Measured here, on this very comment, by the live
-- sweep — and nothing cheaper sees it. Note precisely why, because the obvious
-- premise is wrong: sqlc DOES read this directory (sqlc.yaml's `schema:` points at
-- it, and a deliberate syntax error here exits 1 with a line and column). It parses
-- the SQL and is BLIND TO THE ANNOTATIONS, so a file with three plus-goose
-- occurrences including the bricking one gives sqlc exit 0. sqlc validates the SQL;
-- goose validates the annotations; neither validates the other's half.
--
-- CI catches a reintroduction — test:api-store-it runs a real postgres and any
-- *LiveDB test calls store.Migrate. The blind spot is LOCAL: the ordinary pre-push
-- gate runs with UZI_TEST_DATABASE_URL unset, so every live-DB test self-skips and
-- cannot see a bricked migration. The window is one push.)
-- Run in autocommit, the sequence below does not stop at the failing statement: the
-- DROP INDEX commits, the narrowing ADD CONSTRAINT raises 23514 and is skipped, and
-- every DROP COLUMN after it commits anyway. The instance is then left with `runs`
-- carrying NO status CHECK AT ALL — status becomes free text — and the parked run's
-- five columns gone, which is precisely the data loss the loud failure exists to
-- prevent. No migration in this repo carries that annotation today, so this is
-- unreachable as shipped; it is written here because the natural reason to reach for
-- it is CREATE INDEX CONCURRENTLY, and this is the migration that creates an index
-- on `runs`, the table that grows without bound. An operator who wants a concurrent
-- index must split it into its own migration and leave this one transactional.
DROP INDEX idx_runs_limit_wait_retry;

-- Bare DROP CONSTRAINT, with NO `IF EXISTS`, and the omission is deliberate — it
-- deviates from 00074's precedent on purpose, so please do not "fix" it.
-- IF EXISTS turns the one failure mode that is loud and immediate into one that is
-- silent and deferred: against a database whose status constraint carries a
-- different auto-generated name, the drop would be skipped, the ADD below would
-- succeed under the new name, the OLD narrow constraint would survive alongside it,
-- and then every park would raise 23514 at runtime on a healthy-looking instance.
-- Failing at boot, on the operator's terminal, naming the constraint, is strictly
-- better than failing months later on a user's run. The same reasoning applies to
-- the Up section's drop.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval',
                      'completed', 'failed', 'cancelled'));

-- runs_rate_limit_type_check needs no explicit drop: DROP COLUMN rate_limit_type
-- below takes its own constraints with it. Spelled out because its absence here
-- otherwise reads as the omission it is not.

ALTER TABLE users DROP COLUMN wait_on_limit;

ALTER TABLE runs
    DROP COLUMN rate_limit_type,
    DROP COLUMN limit_wait_count,
    DROP COLUMN retry_not_before,
    DROP COLUMN limit_resets_at,
    DROP COLUMN wait_on_limit;
