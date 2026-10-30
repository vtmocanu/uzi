-- +goose Up
SET LOCAL lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION merged_predecessor_discard_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    b jsonb;
    field text;
    verified_at timestamptz;
    mutable_keys text[] := ARRAY['state', 'live_worker_id', 'live_run_id', 'release_evidence',
        'merged_predecessor_discard_binding', 'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.merged_predecessor_discard_binding IS NOT NULL
            OR NEW.release_evidence = 'merged_predecessor_discard' THEN
            RAISE EXCEPTION 'merged predecessor discard cannot be inserted';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF OLD.merged_predecessor_discard_binding IS NOT NULL THEN
            RAISE EXCEPTION 'merged predecessor audit cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.merged_predecessor_discard_binding IS NOT NULL THEN
        IF (to_jsonb(NEW) - 'updated_at') IS DISTINCT FROM (to_jsonb(OLD) - 'updated_at') THEN
            RAISE EXCEPTION 'merged predecessor audit is immutable';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.merged_predecessor_discard_binding IS NULL
        AND NEW.release_evidence IS DISTINCT FROM 'merged_predecessor_discard' THEN RETURN NEW; END IF;

    b := NEW.merged_predecessor_discard_binding;
    IF OLD.state <> 'open' OR NEW.state <> 'discarded'
        OR NEW.release_evidence IS DISTINCT FROM 'merged_predecessor_discard'
        OR NEW.live_worker_id IS NOT NULL OR NEW.live_run_id IS NOT NULL
        OR jsonb_typeof(b) IS DISTINCT FROM 'object'
        OR jsonb_typeof(b->'verified_at') IS DISTINCT FROM 'string'
        OR (to_jsonb(NEW) - mutable_keys) IS DISTINCT FROM (to_jsonb(OLD) - mutable_keys) THEN
        RAISE EXCEPTION 'invalid merged predecessor transition';
    END IF;
    -- Cast errors reject malformed values; NULL, infinity, relative dates and future values
    -- are refused. The server emits an absolute RFC3339 timestamp.
    IF (b->>'verified_at') !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' THEN
        RAISE EXCEPTION 'invalid merged predecessor verification timestamp';
    END IF;
    verified_at := (b->>'verified_at')::timestamptz;
    IF verified_at IS NULL OR NOT isfinite(verified_at) OR verified_at > clock_timestamp() THEN
        RAISE EXCEPTION 'invalid merged predecessor verification timestamp';
    END IF;
    IF b->>'basis' = 'same_issue_witness' THEN
        -- Fixed-size field lists validate JSON types and lexical integers separately
        -- from jsonb equality, which considers 1.0 equal to 1.
        FOREACH field IN ARRAY ARRAY['hold_id', 'target_run_id', 'witness_run_id',
            'owner_id', 'repo_id', 'connection_id'] LOOP
            IF jsonb_typeof(b->field) IS DISTINCT FROM 'string'
                OR (b->>field) !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                OR b->>field = '00000000-0000-0000-0000-000000000000' THEN
                RAISE EXCEPTION 'invalid same issue witness UUID';
            END IF;
        END LOOP;
        FOREACH field IN ARRAY ARRAY['generation', 'target_terminal_generation',
            'witness_completed_generation', 'issue_iid', 'project_id', 'witness_mr_iid'] LOOP
            IF jsonb_typeof(b->field) IS DISTINCT FROM 'number'
                OR (b->>field) !~ '^[1-9][0-9]*$' THEN
                RAISE EXCEPTION 'invalid same issue witness integer';
            END IF;
            IF (b->>field)::numeric > 9223372036854775807 THEN
                RAISE EXCEPTION 'invalid same issue witness integer';
            END IF;
        END LOOP;
        FOREACH field IN ARRAY ARRAY['target_finished_at', 'witness_created_at', 'verified_at'] LOOP
            IF jsonb_typeof(b->field) IS DISTINCT FROM 'string'
                OR (b->>field) !~ '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,6})?Z$' THEN
                RAISE EXCEPTION 'invalid same issue witness timestamp';
            END IF;
            IF NOT isfinite((b->>field)::timestamptz) THEN
                RAISE EXCEPTION 'invalid same issue witness timestamp';
            END IF;
        END LOOP;
        -- Ordinary reads only: the service owns ordered run/repo/hold locks.
        -- A consumer inserted after this recheck is an accepted policy race.
        IF NOT EXISTS (
            SELECT 1 FROM runs a
            JOIN runs w ON w.id = (b->>'witness_run_id')::uuid
            JOIN repos p ON p.id = a.repo_id
            JOIN forge_connections c ON c.id = p.connection_id
            WHERE a.id = OLD.run_id AND a.user_id = OLD.user_id AND a.repo_id = OLD.repo_id
                AND a.kind = 'issue' AND a.issue_iid > 0
                AND a.status IN ('failed', 'cancelled') AND a.mr_iid IS NULL
                AND a.fail_origin IS DISTINCT FROM 'run_timeout'
                AND a.claim_generation >= OLD.generation AND OLD.generation > 0
                AND isfinite(a.finished_at) AND isfinite(w.created_at)
                AND a.finished_at < w.created_at
                AND a.finished_at = (b->>'target_finished_at')::timestamptz
                AND w.created_at = (b->>'witness_created_at')::timestamptz
                AND w.id <> a.id AND w.user_id = a.user_id AND w.repo_id = a.repo_id
                AND w.kind = 'issue' AND w.issue_iid = a.issue_iid
                AND w.status = 'completed' AND w.claim_generation > 0 AND w.mr_iid > 0
                AND c.user_id = OLD.user_id AND p.forge_project_id > 0
                AND c.forge_type IN ('github', 'gitlab', 'forgejo')
                AND NOT EXISTS (SELECT 1 FROM worker_active_runs ar WHERE ar.run_id = a.id)
                AND NOT EXISTS (SELECT 1 FROM runs live
                    WHERE live.user_id = a.user_id AND live.repo_id = a.repo_id
                        AND live.status NOT IN ('completed', 'failed', 'cancelled')
                        AND (live.issue_iid = a.issue_iid OR live.continued_issue_iid = a.issue_iid))
                AND NOT EXISTS (SELECT 1 FROM run_held_publications pub
                    WHERE pub.state IN ('prepared', 'invoked', 'create_unknown')
                        AND (pub.run_id = a.id OR pub.hold_id IN (
                            SELECT h.id FROM recovery_custody_holds h WHERE h.run_id = a.id)))
                AND b = jsonb_build_object(
                    'basis', 'same_issue_witness', 'hold_id', OLD.id, 'generation', OLD.generation,
                    'target_run_id', a.id, 'target_terminal_generation', a.claim_generation,
                    'target_terminal_status', a.status,
                    'witness_run_id', w.id, 'witness_completed_generation', w.claim_generation,
                    'owner_id', OLD.user_id, 'repo_id', OLD.repo_id, 'issue_iid', a.issue_iid,
                    'target_finished_at', b->'target_finished_at',
                    'witness_created_at', b->'witness_created_at',
                    'connection_id', c.id, 'forge_type', c.forge_type, 'base_url', c.base_url,
                    'project_id', p.forge_project_id, 'witness_mr_iid', w.mr_iid,
                    'verified_at', b->'verified_at')
        ) THEN
            RAISE EXCEPTION 'same issue witness binding mismatch';
        END IF;
    ELSE
    IF NOT EXISTS (
        SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id
        JOIN forge_connections c ON c.id = p.connection_id
        WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id AND r.repo_id = OLD.repo_id
            AND c.user_id = OLD.user_id AND r.status = 'completed'
            AND r.claim_generation > OLD.generation AND r.mr_iid > 0
            AND b = jsonb_build_object(
                'hold_id', OLD.id, 'generation', OLD.generation, 'run_id', OLD.run_id,
                'owner_id', OLD.user_id, 'repo_id', OLD.repo_id, 'connection_id', c.id,
                'forge_type', c.forge_type, 'base_url', c.base_url, 'project_id', p.forge_project_id,
                'mr_iid', r.mr_iid, 'completed_generation', r.claim_generation,
                'verified_at', b->'verified_at')
    ) THEN
        RAISE EXCEPTION 'merged predecessor binding mismatch';
    END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE recovery_custody_holds IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM recovery_custody_holds
        WHERE merged_predecessor_discard_binding->>'basis' = 'same_issue_witness') THEN
        RAISE EXCEPTION 'same issue witness audit exists; refusing destructive rollback';
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION merged_predecessor_discard_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    b jsonb;
    verified_at timestamptz;
    mutable_keys text[] := ARRAY['state', 'live_worker_id', 'live_run_id', 'release_evidence',
        'merged_predecessor_discard_binding', 'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.merged_predecessor_discard_binding IS NOT NULL
            OR NEW.release_evidence = 'merged_predecessor_discard' THEN
            RAISE EXCEPTION 'merged predecessor discard cannot be inserted';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF OLD.merged_predecessor_discard_binding IS NOT NULL THEN
            RAISE EXCEPTION 'merged predecessor audit cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.merged_predecessor_discard_binding IS NOT NULL THEN
        IF (to_jsonb(NEW) - 'updated_at') IS DISTINCT FROM (to_jsonb(OLD) - 'updated_at') THEN
            RAISE EXCEPTION 'merged predecessor audit is immutable';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.merged_predecessor_discard_binding IS NULL
        AND NEW.release_evidence IS DISTINCT FROM 'merged_predecessor_discard' THEN RETURN NEW; END IF;

    b := NEW.merged_predecessor_discard_binding;
    IF OLD.state <> 'open' OR NEW.state <> 'discarded'
        OR NEW.release_evidence IS DISTINCT FROM 'merged_predecessor_discard'
        OR NEW.live_worker_id IS NOT NULL OR NEW.live_run_id IS NOT NULL
        OR jsonb_typeof(b) IS DISTINCT FROM 'object'
        OR jsonb_typeof(b->'verified_at') IS DISTINCT FROM 'string'
        OR (to_jsonb(NEW) - mutable_keys) IS DISTINCT FROM (to_jsonb(OLD) - mutable_keys) THEN
        RAISE EXCEPTION 'invalid merged predecessor transition';
    END IF;
    -- Cast errors reject malformed values; NULL, infinity, relative dates and future values
    -- are refused. The server emits an absolute RFC3339 timestamp.
    IF (b->>'verified_at') !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' THEN
        RAISE EXCEPTION 'invalid merged predecessor verification timestamp';
    END IF;
    verified_at := (b->>'verified_at')::timestamptz;
    IF verified_at IS NULL OR NOT isfinite(verified_at) OR verified_at > clock_timestamp() THEN
        RAISE EXCEPTION 'invalid merged predecessor verification timestamp';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id
        JOIN forge_connections c ON c.id = p.connection_id
        WHERE r.id = OLD.run_id AND r.user_id = OLD.user_id AND r.repo_id = OLD.repo_id
            AND c.user_id = OLD.user_id AND r.status = 'completed'
            AND r.claim_generation > OLD.generation AND r.mr_iid > 0
            AND b = jsonb_build_object(
                'hold_id', OLD.id, 'generation', OLD.generation, 'run_id', OLD.run_id,
                'owner_id', OLD.user_id, 'repo_id', OLD.repo_id, 'connection_id', c.id,
                'forge_type', c.forge_type, 'base_url', c.base_url, 'project_id', p.forge_project_id,
                'mr_iid', r.mr_iid, 'completed_generation', r.claim_generation,
                'verified_at', b->'verified_at')
    ) THEN
        RAISE EXCEPTION 'merged predecessor binding mismatch';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
