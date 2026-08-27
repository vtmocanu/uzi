-- +goose Up

-- Issue #106: the plan-revision cap (PRD #41) was not atomic, and the breach was
-- DURABLE. This migration adds the counter column that makes it atomic; the query
-- change lands with it in queries/runtime.sql.
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00092 against a live head of
-- 00091_run_limit_wait.sql; PRD #88 merged to main first and took BOTH
-- 00092_run_awaiting_input.sql and 00093_slack_question_anchor.sql, so this renumbered
-- to 00094 on the landing merge. Per CLAUDE.md that renumber is mandatory rather than
-- tidy: the boot runner is strict goose (no allow-missing, store/migrate.go), so
-- landing a version BELOW an already-applied head makes every upgraded instance refuse
-- to boot. A collision at 00092 was exactly the shape that rule exists to prevent.

-- WHY A COUNTER COLUMN AT ALL, when the count already exists as rows.
--
-- The shipped statement took the `runs` row FOR UPDATE in a leading CTE and counted
-- `run_user_inputs` — a DIFFERENT table — in the INSERT's WHERE. At READ COMMITTED a
-- statement's snapshot is taken when the statement starts; when it unblocks on a row
-- lock, EvalPlanQual re-reads only the LOCKED ROW, it does not refresh the snapshot
-- for any other table. So the second caller blocked, unblocked, counted N-1 anyway,
-- and inserted. Measured 100/100 over-cap with the interleave forced.
--
-- Moving the count onto a column OF THE LOCKED ROW is what fixes it, because EPQ does
-- re-evaluate the qual against that row. That is the whole mechanism, and it is why
-- the cap predicate must live in the UPDATE's WHERE and must reference only columns
-- of `runs`. The rule is restated on the query itself, which is where someone
-- simplifying it will be looking.

-- Matches the three counters already on this table (requeue_count, iteration_count,
-- limit_wait_count): plain int, NOT NULL, DEFAULT 0.
--
-- NO CHECK constraint, deliberately, and this is a departure from 00091's precedent
-- rather than an oversight of it. The cap is PLAN_MAX_REVISIONS, a RUNTIME env var —
-- no CHECK or unique index can reference it, so a constraint could only bound the
-- column by some unrelated constant. The UPDATE's own WHERE is the guard, and it is
-- the guard precisely BECAUSE it is evaluated against the locked row. It is the same
-- reason a partial unique index (the 00020 precedent for the analogous
-- check-then-insert race) does not transfer here: "at most 1" is a constant an index
-- predicate can express, "at most N-from-the-environment" is not.
ALTER TABLE runs
    ADD COLUMN revise_count int NOT NULL DEFAULT 0;

-- THE BACKFILL IS LOAD-BEARING, NOT COSMETIC, and it belongs in THIS migration.
--
-- goose wraps each migration in a transaction, so the column and its values commit
-- together and no boot ever observes a run whose counter reads 0 while its rows say
-- otherwise. Split into a later migration, the window between them is a window in
-- which every existing run's budget has silently reset: a run that had already spent
-- 3 of 3 revisions would get 3 more.
--
-- Only runs that have revise_plan rows are touched; every other row keeps the
-- DEFAULT 0 the column was added with.
UPDATE runs r
SET revise_count = c.n
FROM (
    SELECT run_id, count(*) AS n
    FROM run_user_inputs
    WHERE kind = 'revise_plan'
    GROUP BY run_id
) c
WHERE r.id = c.run_id;

-- THE COUNTER CANNOT SURVIVE ITS OWN ROWS, so there is no drift to reconcile.
--
-- run_user_inputs has exactly ONE foreign key: run_id REFERENCES runs ON DELETE
-- CASCADE (00020_workers_runs.sql). There is no DELETE FROM run_user_inputs anywhere
-- in the repo, so the only way a revise_plan row dies is the cascade from deleting its
-- runs row, which takes this column with it in the same statement, because the counter
-- IS a column of that row.
--
-- Deletes of whole `runs` rows DO exist and are precisely the safe case: no sqlc query
-- has one, but e2e/run-e2e.sh:4053 deletes runs directly as a PRD #98 fixture teardown,
-- and runs rows also go by cascade from users, repos, or runs.target_run_id. Every one
-- of those removes the counter along with the rows it counts. (An earlier version of
-- this paragraph claimed there was "no DELETE FROM runs either", which is false — and
-- since this paragraph's whole value is being an enumeration someone can re-derive, a
-- false line in it discredits the rest.)
--
-- If a retention/pruning DELETE FROM run_user_inputs is added later it would
-- decrement count(*) and not this counter, and THAT DIVERGENCE WOULD BE CORRECT: a
-- pruned revise still spent a revision. The cap is a lifetime budget, not a row
-- inventory — the same semantics that make CountRunReviseInputs count consumed rows.

-- 🔴 THE COUNTER UPDATE MUST NOT SET updated_at, and the deviation from house style
-- is deliberate. ListActiveRunsForHealth includes awaiting_approval and selects
-- updated_at; healthTargetFor (workersvc/health.go) times the approval_idle flag off
-- it. A revise lands while the run is awaiting_approval, so bumping updated_at would
-- silently move when a health flag fires — a user-visible behaviour change shipped
-- inside a concurrency bugfix. CreateApprovePlanInput and CreateStopVerdictInput both
-- bump and look like authority for doing so; they are not. A stop verdict drives the
-- run terminal, where healthTargetFor returns healthOK, so its bump cannot change a
-- flag outcome. A revise's can.

-- +goose Down

-- The backfill is not reversed: dropping the column discards it, and re-running the
-- Up recomputes it from the rows, which are untouched by either direction.
ALTER TABLE runs DROP COLUMN revise_count;
