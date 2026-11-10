-- +goose Up
-- Issue #4116: a prompt run's checkpoint key is uzi/prompt-<run-id>, the branch the worker
-- already clones it onto (agent/src/run-kind.ts deriveCloneKey). The Go mirror is
-- checkpointBranch (workersvc/checkpoint_branch.go); fixtures/checkpoint-branch pins both.
-- Only the prompt arm is new; the rest of the body is copied verbatim from 00352.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_checkpoint_coordinates(subject runs) RETURNS jsonb
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
    ELSIF subject.kind = 'prompt' THEN
        work := 'uzi/prompt-' || subject.id::text;
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

-- A failed prompt run is now salvage-eligible, so its key must satisfy the salvage format.
-- Validated separately (00423) so the scan never extends this migration's lock.
ALTER TABLE run_salvage DROP CONSTRAINT run_salvage_branch_format_check;
ALTER TABLE run_salvage ADD CONSTRAINT run_salvage_branch_format_check
    CHECK (branch ~ '^(agent/issue-[0-9]+|uzi/(self-improve|task)/[0-9a-f-]{36}|uzi/prompt-[0-9a-f-]{36})$') NOT VALID;

-- +goose Down
-- Refuse while any prompt checkpoint state exists: restoring the old resolver would orphan a
-- paused prompt run's published checkpoint (the terminal trigger has not recorded it yet),
-- an unresolved publication, or a retention/salvage row the old CHECK rejects. Lock the
-- writers first; the checks below take a fresh READ COMMITTED snapshot after the locks.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
LOCK TABLE checkpoint_retentions, run_salvage, checkpoint_publish_attempts IN SHARE MODE;
-- +goose StatementBegin
DO $$ BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'prompt checkpoint downgrade requires READ COMMITTED';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE kind = 'prompt' AND checkpoint_tip IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot downgrade with a published prompt checkpoint';
    END IF;
    IF EXISTS (
        SELECT 1 FROM checkpoint_publish_attempts a
        WHERE a.branch LIKE 'uzi/prompt-%'
           OR a.ref LIKE 'refs/uzi-checkpoints/uzi/prompt-%'
           OR EXISTS (SELECT 1 FROM runs r WHERE r.id = a.run_id AND r.kind = 'prompt')
    ) THEN
        RAISE EXCEPTION 'cannot downgrade with an unresolved prompt checkpoint publication';
    END IF;
    IF EXISTS (SELECT 1 FROM checkpoint_retentions WHERE branch LIKE 'uzi/prompt-%') THEN
        RAISE EXCEPTION 'cannot downgrade with a prompt checkpoint retention';
    END IF;
    IF EXISTS (SELECT 1 FROM run_salvage WHERE branch LIKE 'uzi/prompt-%') THEN
        RAISE EXCEPTION 'cannot downgrade with a prompt salvage record';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE run_salvage DROP CONSTRAINT run_salvage_branch_format_check;
ALTER TABLE run_salvage ADD CONSTRAINT run_salvage_branch_format_check
    CHECK (branch ~ '^(agent/issue-[0-9]+|uzi/(self-improve|task)/[0-9a-f-]{36})$');
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_checkpoint_coordinates(subject runs) RETURNS jsonb
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
