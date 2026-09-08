-- +goose Up

-- Findings Filed→Done close sync (PRD #1183 Child B, M3), mirroring the judge's edge-marker
-- design in 00081_judge_issue_close_sync.sql. When an issue filed from an incidental finding
-- (#333) is CLOSED on the forge, the coordinate auto-moves to a NEW 'done' status — exactly
-- once, on the open→closed edge, never overwriting a human's own verdict. Three additive
-- schema changes ride this one migration because they are one feature: the provenance column,
-- the edge marker, and the widened status domain that admits 'done'.

-- set_via records PROVENANCE so an automatic Done is visibly distinct from one a human set
-- (mirrors recommendation_dispositions.set_via, 00081:38-52): NULL (the default, and every
-- pre-existing row) means a person set the disposition; 'issue_close' means this sync did, and
-- the UI can label it "done via #IID". The column and its CHECK are added SEPARATELY: Postgres
-- does not accept NOT VALID on an inline column constraint (it is a syntax error), only on a
-- table-level ADD CONSTRAINT. Added NOT VALID so the add skips the validating table scan (and
-- the ACCESS EXCLUSIVE lock it would otherwise hold); 00207 runs VALIDATE CONSTRAINT under a
-- lock-cheap scan, the same two-step pattern 00204/00205 use.
ALTER TABLE finding_dispositions ADD COLUMN set_via text;
ALTER TABLE finding_dispositions ADD CONSTRAINT finding_dispositions_set_via_check
    CHECK (set_via IS NULL OR set_via IN ('issue_close')) NOT VALID;

-- close_synced_at is the EDGE MARKER (mirrors recommendation_filed_issues.close_synced_at,
-- 00081:26). The sync acts on a linked issue only when the cached state is closed AND this is
-- NULL — i.e. only on the open→closed edge — and stamps it immediately after, so the sync
-- fires EXACTLY ONCE per close. After a human Undo the edge is already consumed, so the next
-- tick does not re-apply and the Undo STICKS.
ALTER TABLE finding_dispositions ADD COLUMN close_synced_at timestamptz;

-- Widen the status CHECK to admit 'done', the sync's terminal rung. The 00129 CHECK was inline
-- and unnamed, so Postgres named it finding_dispositions_status_check: DROP by that name, then
-- ADD it back with the widened set. The first four values are copied VERBATIM from the live
-- 00129 constraint; 'done' is the only addition. Added NOT VALID; 00207 validates it.
ALTER TABLE finding_dispositions DROP CONSTRAINT finding_dispositions_status_check;
ALTER TABLE finding_dispositions ADD CONSTRAINT finding_dispositions_status_check
    CHECK (status IN ('open', 'filing', 'filed', 'dismissed', 'done')) NOT VALID;

-- The pass's working set: settled filed coordinates in one repo whose close edge is not yet
-- consumed. The poller runs the edge scan per repo per tick, so it is a hot path; the partial
-- predicate keeps the index to just the rows that can still fire. Mirrors 00081:33-35.
CREATE INDEX idx_finding_dispositions_close_pending
    ON finding_dispositions (repo_id)
    WHERE close_synced_at IS NULL AND status = 'filed' AND filed_issue_iid IS NOT NULL;

-- +goose Down

-- Drop the index, the two columns, then re-narrow the status CHECK to the original FOUR values
-- (no NOT VALID on the narrow one, matching 00204's Down style). Dropping the set_via column
-- takes its finding_dispositions_set_via_check constraint with it.
--
-- ⚠ RE-NARROWING FAILS WHILE ANY 'done' ROW EXISTS: the re-added narrow CHECK is validated
-- immediately and a 'done' row violates it, so this Down errors on any database that has ever
-- auto-resolved a finding. That is EXPECTED and irreversible-by-design — the same one-way
-- property the widening records. Drain 'done' rows first if a real down-migration is ever
-- needed (goose downs are not run in this deployment; store.Migrate only ever goes up).
DROP INDEX idx_finding_dispositions_close_pending;
ALTER TABLE finding_dispositions DROP COLUMN close_synced_at;
ALTER TABLE finding_dispositions DROP COLUMN set_via;
ALTER TABLE finding_dispositions DROP CONSTRAINT finding_dispositions_status_check;
ALTER TABLE finding_dispositions ADD CONSTRAINT finding_dispositions_status_check
    CHECK (status IN ('open', 'filing', 'filed', 'dismissed'));
