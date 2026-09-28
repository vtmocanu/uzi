-- +goose Up

-- PRD #1810: record a run's checkpoint_retentions row IN THE SAME TRANSACTION that commits its
-- terminal status. Before this, the row was inserted only after the terminal status committed
-- (the post-commit retainOrDeleteCheckpoint on three writers), or not at all by the other
-- terminal writers (the sweeper's worker-loss and cap fails, the auto-stop, the claim-assembly
-- and Codex account-wait fails, MarkRunFailedByID), which relied on the sweeper backfill. In
-- that window a new issue run's slot claim (claimCheckpointSlot) listed the branch's active
-- records, saw none, and pushed a descendant tip over the old run's retained checkpoint. With
-- the row inserted by this trigger, any reader that sees the terminal status also sees the row.
--
-- Every terminal write is an UPDATE of runs.status (no INSERT with a terminal status, no COPY),
-- so an AFTER UPDATE OF status trigger covers every writer. The body mirrors
-- InsertCheckpointRetentionIfHeld / InsertCheckpointRetentionSettling
-- (queries/checkpoint_retention.sql): a row only when the run belongs to a repo and published a
-- checkpoint; `retained` while any custody hold of the run is open, else `settling`; ON CONFLICT
-- DO NOTHING, so an existing row is never reset (a re-transition, or a row another path wrote
-- first). The branch is derived exactly as workersvc.checkpointBranch does, kind FIRST (a
-- self_improve run also carries an issue_iid): issue with an iid -> agent/issue-<iid>,
-- self_improve -> uzi/self-improve/<run id>, any other kind -> no row. The post-commit Go path
-- still runs: it finds this row, re-reads it and dispatches the settle the row owes.
-- Additive only: a new function and trigger, no column or table change.
-- +goose StatementBegin
CREATE FUNCTION checkpoint_retention_on_terminal() RETURNS trigger AS $$
DECLARE
    v_branch text;
BEGIN
    IF NEW.repo_id IS NULL OR NEW.checkpoint_tip IS NULL THEN
        RETURN NULL;
    END IF;
    IF NEW.kind = 'issue' THEN
        IF NEW.issue_iid IS NULL THEN
            RETURN NULL;
        END IF;
        v_branch := 'agent/issue-' || NEW.issue_iid::text;
    ELSIF NEW.kind = 'self_improve' THEN
        v_branch := 'uzi/self-improve/' || NEW.id::text;
    ELSE
        RETURN NULL;
    END IF;
    INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
    VALUES (
        NEW.id, NEW.user_id, NEW.repo_id, v_branch, NEW.checkpoint_tip,
        'refs/uzi-checkpoints/' || v_branch,
        CASE WHEN EXISTS (
            SELECT 1 FROM recovery_custody_holds h
            WHERE h.run_id = NEW.id AND h.state = 'open'
        ) THEN 'retained' ELSE 'settling' END
    )
    ON CONFLICT (run_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER checkpoint_retention_on_terminal_trg
    AFTER UPDATE OF status ON runs
    FOR EACH ROW
    WHEN (NEW.status IN ('completed', 'failed', 'cancelled') AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION checkpoint_retention_on_terminal();

-- +goose Down
DROP TRIGGER checkpoint_retention_on_terminal_trg ON runs;
DROP FUNCTION checkpoint_retention_on_terminal();
