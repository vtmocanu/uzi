-- +goose Up
ALTER TABLE runs ADD COLUMN checkpoint_coordinates jsonb;

-- Complete row input is required: an AFTER trigger's NEW may differ from the
-- statement snapshot. Frozen coordinates never depend on surviving ancestors.
-- +goose StatementBegin
CREATE FUNCTION fn_checkpoint_coordinates(subject runs) RETURNS jsonb
LANGUAGE plpgsql STABLE AS $$
DECLARE
    node runs := subject;
    visited uuid[] := '{}';
    branches text[] := '{}';
    parent_id uuid;
    work text;
    key text;
BEGIN
    IF subject.repo_id IS NULL THEN RETURN NULL; END IF;
    IF subject.checkpoint_coordinates IS NOT NULL THEN
        IF subject.checkpoint_coordinates->>'run_id' = subject.id::text
           AND subject.checkpoint_coordinates->>'user_id' = subject.user_id::text
           AND subject.checkpoint_coordinates->>'repo_id' = subject.repo_id::text
           AND subject.checkpoint_coordinates->>'kind' = subject.kind
           AND subject.checkpoint_coordinates->'issue_iid' = to_jsonb(subject)->'issue_iid'
           AND (subject.branch IS NULL OR subject.branch = subject.checkpoint_coordinates->>'work_branch')
        THEN RETURN subject.checkpoint_coordinates;
        END IF;
        RETURN NULL;
    END IF;
    IF subject.kind = 'issue' THEN
        IF subject.issue_iid IS NULL THEN RETURN NULL; END IF;
        work := 'agent/issue-' || subject.issue_iid::text;
        key := work;
        visited := ARRAY[subject.id];
    ELSIF subject.kind = 'self_improve' THEN
        work := 'uzi/self-improve/' || subject.id::text;
        key := work;
        visited := ARRAY[subject.id];
    ELSIF subject.kind = 'task' THEN
        key := 'uzi/task/' || subject.id::text;
        LOOP
            -- Count actual visited rows, including the subject, never path strings.
            IF cardinality(visited) >= 64 OR node.id = ANY(visited)
               OR node.kind <> 'task' OR node.user_id <> subject.user_id
               OR node.repo_id IS DISTINCT FROM subject.repo_id OR node.branch IS NULL
               OR (node.review_target_run_id IS NOT NULL AND node.then_fix_of_run_id IS NOT NULL)
            THEN RETURN NULL; END IF;
            visited := array_append(visited, node.id);
            branches := array_append(branches, node.branch);
            parent_id := COALESCE(node.review_target_run_id, node.then_fix_of_run_id);
            IF parent_id IS NULL THEN
                work := 'uzi/task/' || node.id::text;
                EXIT;
            END IF;
            IF cardinality(visited) = 64 THEN RETURN NULL; END IF;
            SELECT * INTO node FROM runs WHERE id = parent_id;
            IF NOT FOUND THEN RETURN NULL; END IF;
        END LOOP;
        IF EXISTS (SELECT 1 FROM unnest(branches) b WHERE b <> work) THEN RETURN NULL; END IF;
    ELSE
        RETURN NULL;
    END IF;
    RETURN jsonb_build_object('version', 1, 'run_id', subject.id,
        'user_id', subject.user_id, 'repo_id', subject.repo_id,
        'kind', subject.kind, 'issue_iid', subject.issue_iid,
        'work_branch', work, 'checkpoint_key', key, 'provenance', to_jsonb(visited));
END;
$$;
-- +goose StatementEnd

-- Enrollment runs only after a fresh claim, under its worker lock and claim
-- savepoint. NOWAIT prevents a child->parent lock cycle; all locks survive until
-- commit. The second resolver invocation uses a fresh READ COMMITTED snapshot.
-- +goose StatementBegin
CREATE FUNCTION fn_enroll_checkpoint_coordinates(run_id uuid, worker_id uuid, generation bigint)
RETURNS jsonb LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    subject runs;
    discovered jsonb;
    verified jsonb;
    ancestor uuid;
BEGIN
    SELECT * INTO subject FROM runs r WHERE r.id = run_id FOR UPDATE;
    IF NOT FOUND OR subject.worker_id IS DISTINCT FROM worker_id
       OR subject.claim_generation <> generation OR subject.status <> 'claimed'
       OR subject.claim_released_at IS NOT NULL THEN
        RAISE EXCEPTION 'checkpoint enrollment claim changed' USING ERRCODE = '40001';
    END IF;
    IF subject.kind <> 'task' THEN RETURN NULL; END IF;
    IF subject.checkpoint_coordinates IS NOT NULL THEN RETURN subject.checkpoint_coordinates; END IF;
    IF current_setting('uzi.task_checkpoint_coordinates_api', true) IS DISTINCT FROM 'v1'
       OR NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = worker_id AND w.user_id = subject.user_id
                      AND 'task_checkpoint_coordinates_v1' = ANY(w.protocol_capabilities))
    THEN RETURN NULL; END IF;
    discovered := fn_checkpoint_coordinates(subject);
    IF discovered IS NULL THEN RETURN NULL; END IF;
    FOR ancestor IN SELECT value::uuid FROM jsonb_array_elements_text(discovered->'provenance')
                    WHERE value::uuid <> subject.id ORDER BY value::uuid
    LOOP
        PERFORM 1 FROM runs r WHERE r.id = ancestor FOR SHARE NOWAIT;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'checkpoint lineage disappeared' USING ERRCODE = '40001';
        END IF;
    END LOOP;
    verified := fn_checkpoint_coordinates(subject);
    IF verified IS DISTINCT FROM discovered THEN
        RAISE EXCEPTION 'checkpoint lineage changed' USING ERRCODE = '40001';
    END IF;
    UPDATE runs r SET checkpoint_coordinates = verified WHERE r.id = subject.id;
    RETURN verified;
END;
$$;
-- +goose StatementEnd

-- Enrolled authority requires BOTH persisted capability and updated API opt-in.
-- Exceptions roll back historical custody CTEs and their surrounding savepoints.
-- +goose StatementBegin
CREATE FUNCTION guard_checkpoint_coordinates() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    authority boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.checkpoint_coordinates IS NOT NULL THEN
            RAISE EXCEPTION 'checkpoint coordinates require server enrollment' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.checkpoint_coordinates IS NOT NULL THEN
        IF NEW.checkpoint_coordinates IS DISTINCT FROM OLD.checkpoint_coordinates
           OR NEW.id <> OLD.id OR NEW.user_id <> OLD.user_id
           OR NEW.repo_id IS DISTINCT FROM OLD.repo_id OR NEW.kind <> OLD.kind
           OR NEW.issue_iid IS DISTINCT FROM OLD.issue_iid
           OR NEW.review_target_run_id IS DISTINCT FROM OLD.review_target_run_id
           OR NEW.then_fix_of_run_id IS DISTINCT FROM OLD.then_fix_of_run_id
           OR (NEW.branch IS DISTINCT FROM OLD.branch
               AND NEW.branch IS DISTINCT FROM OLD.checkpoint_coordinates->>'work_branch')
        THEN RAISE EXCEPTION 'frozen checkpoint coordinates are immutable' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.checkpoint_coordinates IS NOT NULL THEN
        IF NEW.kind <> 'task' OR NEW.checkpoint_coordinates IS DISTINCT FROM fn_checkpoint_coordinates(OLD)
           OR NEW.status <> 'claimed' OR NEW.claim_generation < 1
           OR fn_checkpoint_coordinates(NEW) IS DISTINCT FROM NEW.checkpoint_coordinates
        THEN RAISE EXCEPTION 'invalid checkpoint enrollment' USING ERRCODE = '23514';
        END IF;
    ELSE
        RETURN NEW;
    END IF;
    authority := NEW.checkpoint_coordinates IS NOT NULL AND (
        OLD.checkpoint_coordinates IS NULL
        OR (NEW.worker_id IS NOT NULL AND NEW.worker_id IS DISTINCT FROM OLD.worker_id)
        OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation
        OR (OLD.claim_released_at IS NOT NULL AND NEW.claim_released_at IS NULL)
        OR (NEW.status IS DISTINCT FROM OLD.status AND OLD.status NOT IN ('claimed','running') AND NEW.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup'))
    );
    IF authority AND (
        current_setting('uzi.task_checkpoint_coordinates_api', true) IS DISTINCT FROM 'v1'
        OR NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.user_id = NEW.user_id
                       AND 'task_checkpoint_coordinates_v1' = ANY(w.protocol_capabilities))
    ) THEN
        RAISE EXCEPTION 'checkpoint coordinates require capable worker and updated API' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_checkpoint_coordinates_trg BEFORE INSERT OR UPDATE ON runs
FOR EACH ROW EXECUTE FUNCTION guard_checkpoint_coordinates();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION checkpoint_retention_on_terminal() RETURNS trigger AS $$
DECLARE
    coordinates jsonb;
BEGIN
    IF NEW.repo_id IS NULL OR NEW.checkpoint_tip IS NULL THEN RETURN NULL; END IF;
    coordinates := fn_checkpoint_coordinates(NEW);
    IF coordinates IS NULL THEN RETURN NULL; END IF;
    INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
    VALUES (NEW.id, NEW.user_id, NEW.repo_id, coordinates->>'checkpoint_key', NEW.checkpoint_tip,
        'refs/uzi-checkpoints/' || (coordinates->>'checkpoint_key'),
        CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h
                          WHERE h.run_id = NEW.id AND h.state = 'open')
             THEN 'retained' ELSE 'settling' END)
    ON CONFLICT (run_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

ALTER TABLE run_salvage DROP CONSTRAINT run_salvage_branch_format_check;
ALTER TABLE run_salvage ADD CONSTRAINT run_salvage_branch_format_check
    CHECK (branch ~ '^(agent/issue-[0-9]+|uzi/(self-improve|task)/[0-9a-f-]{36})$') NOT VALID;

-- +goose Down
-- Exclude concurrent enrollment before checking the sticky descriptor. The next
-- statement takes a fresh READ COMMITTED snapshot after this lock is acquired.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
    -- A stricter isolation level could retain a snapshot from before the lock.
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'checkpoint schema downgrade requires READ COMMITTED';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE checkpoint_coordinates IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot downgrade with enrolled checkpoint coordinates';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE run_salvage DROP CONSTRAINT run_salvage_branch_format_check;
ALTER TABLE run_salvage ADD CONSTRAINT run_salvage_branch_format_check
    CHECK (branch ~ '^(agent/issue-[0-9]+|uzi/self-improve/[0-9a-f-]{36})$');
DROP TRIGGER guard_checkpoint_coordinates_trg ON runs;
DROP FUNCTION guard_checkpoint_coordinates();
DROP FUNCTION fn_enroll_checkpoint_coordinates(uuid, uuid, bigint);
-- Restore the legacy terminal behavior without depending on the new column.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION checkpoint_retention_on_terminal() RETURNS trigger AS $$
DECLARE v_branch text;
BEGIN
    IF NEW.repo_id IS NULL OR NEW.checkpoint_tip IS NULL THEN RETURN NULL; END IF;
    IF NEW.kind = 'issue' AND NEW.issue_iid IS NOT NULL THEN
        v_branch := 'agent/issue-' || NEW.issue_iid::text;
    ELSIF NEW.kind = 'self_improve' THEN
        v_branch := 'uzi/self-improve/' || NEW.id::text;
    ELSE RETURN NULL;
    END IF;
    INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
    VALUES (NEW.id, NEW.user_id, NEW.repo_id, v_branch, NEW.checkpoint_tip,
        'refs/uzi-checkpoints/' || v_branch,
        CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h
                          WHERE h.run_id = NEW.id AND h.state = 'open')
             THEN 'retained' ELSE 'settling' END)
    ON CONFLICT (run_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP FUNCTION fn_checkpoint_coordinates(runs);
ALTER TABLE runs DROP COLUMN checkpoint_coordinates;
