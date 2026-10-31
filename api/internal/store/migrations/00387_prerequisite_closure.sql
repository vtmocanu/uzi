-- +goose Up
SET LOCAL lock_timeout = '5s';

CREATE TABLE recovery_prerequisite_closures (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    capture_id uuid NOT NULL UNIQUE REFERENCES recovery_captures(id) ON DELETE RESTRICT,
    hold_id uuid NOT NULL REFERENCES recovery_custody_holds(id) ON DELETE RESTRICT,
    run_id uuid NOT NULL,
    user_id uuid NOT NULL,
    repo_id uuid NOT NULL,
    live_run_id uuid CONSTRAINT recovery_prerequisite_closures_live_run_id_fkey REFERENCES runs(id) ON DELETE RESTRICT,
    checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    prerequisite_shas text[] NOT NULL CHECK (
        cardinality(prerequisite_shas) BETWEEN 1 AND 64
        AND array_ndims(prerequisite_shas) = 1 AND array_lower(prerequisite_shas,1) = 1
        AND array_position(prerequisite_shas,NULL) IS NULL),
    pin_ref text NOT NULL CHECK (pin_ref = 'refs/uzi-prereq/' || capture_id::text),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN
        ('pending','invoked','create_unknown','pinned','refused','foreign','lost','deleted')),
    reason text NOT NULL DEFAULT '' CHECK (reason IN ('','closure_unsupported',
        'prerequisite_not_on_default','prerequisite_ancestry_unknown',
        'prerequisite_prepare_budget_exhausted','pin_absence_unconfirmed','pin_refused',
        'pin_unknown','pin_foreign','archive_unavailable','pin_lost','pin_cleanup_exhausted')),
    last_error text NOT NULL DEFAULT '' CHECK (length(last_error) <= 512),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 8),
    reconciliation_attempts integer NOT NULL DEFAULT 0 CHECK (reconciliation_attempts BETWEEN 0 AND 7),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempt_deadline timestamptz,
    invoked_at timestamptz,
    invoked_deadline timestamptz,
    pin_tip text,
    connection_id uuid,
    forge_kind text,
    normalized_base_url text,
    project_id bigint,
    default_branch text,
    cleanup_attempts integer NOT NULL DEFAULT 0 CHECK (cleanup_attempts BETWEEN 0 AND 8),
    cleanup_deadline timestamptz,
    cleanup_exhausted_at timestamptz,
    absent_since timestamptz,
    absence_pass_id uuid,
    audit_next_at timestamptz NOT NULL DEFAULT now(),
    last_audit_pass_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (live_run_id IS NULL OR live_run_id = run_id),
    CHECK ((state IN ('invoked','create_unknown','pinned')) = (live_run_id IS NOT NULL)),
    CHECK ((invoked_at IS NULL AND invoked_deadline IS NULL AND pin_tip IS NULL
        AND connection_id IS NULL AND forge_kind IS NULL AND normalized_base_url IS NULL
        AND project_id IS NULL AND default_branch IS NULL)
      OR (invoked_at IS NOT NULL AND invoked_deadline IS NOT NULL AND invoked_deadline > invoked_at
        AND pin_tip IS NOT NULL AND pin_tip ~ '^[0-9a-f]{40}$' AND pin_tip <> repeat('0',40)
        AND connection_id IS NOT NULL AND forge_kind IS NOT NULL AND forge_kind IN ('github','gitlab','forgejo')
        AND normalized_base_url IS NOT NULL AND length(normalized_base_url) > 0
        AND project_id IS NOT NULL AND default_branch IS NOT NULL AND length(default_branch) > 0)),
    CHECK ((state = 'pending' AND invoked_at IS NULL)
        OR (state = 'refused') OR (state <> 'pending' AND invoked_at IS NOT NULL)),
    CHECK ((absent_since IS NULL AND absence_pass_id IS NULL)
        OR (state = 'pinned' AND absent_since IS NOT NULL AND absence_pass_id IS NOT NULL)),
    CHECK (cleanup_exhausted_at IS NULL OR (state = 'pinned' AND cleanup_attempts = 8)),
    CHECK (reason <> 'prerequisite_prepare_budget_exhausted' OR (state = 'refused' AND attempts = 8))
);
CREATE INDEX prerequisite_closure_pending_due ON recovery_prerequisite_closures(next_attempt_at,id) WHERE state = 'pending';
CREATE INDEX prerequisite_closure_invoked_due ON recovery_prerequisite_closures(invoked_deadline,id) WHERE state = 'invoked';
CREATE INDEX prerequisite_closure_unknown_due ON recovery_prerequisite_closures(next_attempt_at,id) WHERE state = 'create_unknown';
CREATE INDEX prerequisite_closure_cleanup_due ON recovery_prerequisite_closures(next_attempt_at,id) WHERE state = 'pinned';
CREATE INDEX prerequisite_closure_audit_due ON recovery_prerequisite_closures(audit_next_at,id) WHERE state = 'pinned';
CREATE INDEX prerequisite_closure_live_run ON recovery_prerequisite_closures(live_run_id) WHERE live_run_id IS NOT NULL;
CREATE INDEX prerequisite_closure_live_repo ON recovery_prerequisite_closures(repo_id,user_id) WHERE live_run_id IS NOT NULL;
CREATE INDEX prerequisite_closure_hold ON recovery_prerequisite_closures(hold_id);

-- +goose StatementBegin
CREATE FUNCTION prerequisite_closure_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'prerequisite closure audit tombstones cannot be deleted' USING ERRCODE='55000';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'pending' OR NEW.live_run_id IS NOT NULL OR NEW.invoked_at IS NOT NULL
            OR NEW.reconciliation_attempts <> 0 OR NEW.attempts <> 0 OR NEW.attempt_deadline IS NOT NULL OR NEW.cleanup_attempts <> 0
            OR NEW.cleanup_deadline IS NOT NULL OR NEW.cleanup_exhausted_at IS NOT NULL
            OR NEW.absent_since IS NOT NULL OR NEW.last_audit_pass_id IS NOT NULL OR NEW.reason <> '' THEN
            RAISE EXCEPTION 'closure insertion must be pristine pending' USING ERRCODE='55000';
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM recovery_captures c JOIN recovery_custody_holds h ON h.id=c.hold_id
            WHERE c.id=NEW.capture_id AND c.hold_id=NEW.hold_id AND c.run_id=NEW.run_id
                AND c.user_id=NEW.user_id AND h.run_id=NEW.run_id AND h.user_id=NEW.user_id
                AND h.repo_id=NEW.repo_id AND c.original_worker_id IS NOT DISTINCT FROM h.original_worker_id
                AND c.original_worker_identity=h.original_worker_identity AND h.inventory_guarded AND h.state='open'
                AND c.manifest_bound AND c.state='available'
                AND c.checksum=NEW.checksum AND c.prerequisite_shas=NEW.prerequisite_shas
        ) THEN
            RAISE EXCEPTION 'closure manifest or provenance mismatch' USING ERRCODE='55000';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.id,NEW.capture_id,NEW.hold_id,NEW.run_id,NEW.user_id,NEW.repo_id,
        NEW.checksum,NEW.prerequisite_shas,NEW.pin_ref,NEW.created_at)
        IS DISTINCT FROM (OLD.id,OLD.capture_id,OLD.hold_id,OLD.run_id,OLD.user_id,OLD.repo_id,
        OLD.checksum,OLD.prerequisite_shas,OLD.pin_ref,OLD.created_at) THEN
        RAISE EXCEPTION 'closure identity is immutable' USING ERRCODE='55000';
    END IF;
    IF OLD.invoked_at IS NULL AND NEW.invoked_at IS NOT NULL
        AND NOT (OLD.state='pending' AND NEW.state='invoked') THEN
        RAISE EXCEPTION 'only the invocation marker freezes binding' USING ERRCODE='55000';
    END IF;
    IF OLD.invoked_at IS NOT NULL AND
        (NEW.invoked_at,NEW.invoked_deadline,NEW.pin_tip,NEW.connection_id,NEW.forge_kind,
         NEW.normalized_base_url,NEW.project_id,NEW.default_branch)
        IS DISTINCT FROM
        (OLD.invoked_at,OLD.invoked_deadline,OLD.pin_tip,OLD.connection_id,OLD.forge_kind,
         OLD.normalized_base_url,OLD.project_id,OLD.default_branch) THEN
        RAISE EXCEPTION 'closure invocation is immutable' USING ERRCODE='55000';
    END IF;
    IF OLD.state IN ('refused','foreign','lost','deleted') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'closure terminal tombstone is immutable' USING ERRCODE='55000';
    END IF;
    IF NEW.state <> OLD.state AND NOT (
        (OLD.state='pending' AND NEW.state IN ('invoked','refused'))
        OR (OLD.state='invoked' AND NEW.state IN ('pinned','refused','create_unknown','foreign'))
        OR (OLD.state='create_unknown' AND NEW.state IN ('pinned','foreign'))
        OR (OLD.state='pinned' AND NEW.state IN ('foreign','lost','deleted'))) THEN
        RAISE EXCEPTION 'invalid closure transition' USING ERRCODE='55000';
    END IF;
    IF NEW.attempts=OLD.attempts AND NEW.attempt_deadline IS DISTINCT FROM OLD.attempt_deadline
        AND NEW.attempt_deadline IS NOT NULL THEN
        RAISE EXCEPTION 'preparation lease requires a new generation' USING ERRCODE='55000';
    END IF;
    IF NEW.cleanup_attempts=OLD.cleanup_attempts AND NEW.cleanup_deadline IS DISTINCT FROM OLD.cleanup_deadline
        AND NEW.cleanup_deadline IS NOT NULL THEN
        RAISE EXCEPTION 'cleanup lease requires a new generation' USING ERRCODE='55000';
    END IF;
    IF NEW.attempts <> OLD.attempts AND (OLD.state='pending' AND NEW.state='pending'
        AND NEW.attempts=OLD.attempts+1 AND OLD.attempts<8 AND OLD.next_attempt_at<=now()
        AND (OLD.attempt_deadline IS NULL OR OLD.attempt_deadline<=now())
        AND NEW.attempt_deadline>now() AND NEW.next_attempt_at>NEW.attempt_deadline) IS NOT TRUE THEN
        RAISE EXCEPTION 'invalid preparation reservation' USING ERRCODE='55000';
    END IF;
    IF NEW.cleanup_attempts <> OLD.cleanup_attempts AND (OLD.state='pinned' AND NEW.state='pinned'
        AND NEW.cleanup_attempts=OLD.cleanup_attempts+1 AND OLD.cleanup_attempts<8
        AND OLD.cleanup_exhausted_at IS NULL AND OLD.next_attempt_at<=now()
        AND (OLD.cleanup_deadline IS NULL OR OLD.cleanup_deadline<=now())
        AND NEW.cleanup_deadline>now() AND NEW.next_attempt_at>NEW.cleanup_deadline) IS NOT TRUE THEN
        RAISE EXCEPTION 'invalid cleanup reservation' USING ERRCODE='55000';
    END IF;
    IF OLD.state='pending' AND NEW.state='invoked' THEN
        IF OLD.attempts=0 OR OLD.attempt_deadline IS NULL OR OLD.attempt_deadline<=now()
            OR NOT EXISTS (
                SELECT 1 FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id
                JOIN recovery_captures c ON c.hold_id=h.id
                JOIN repos p ON p.id=r.repo_id JOIN forge_connections f ON f.id=p.connection_id
                WHERE r.id=NEW.run_id AND r.user_id=NEW.user_id AND p.id=NEW.repo_id
                    AND f.user_id=NEW.user_id AND h.id=NEW.hold_id AND h.state='open' AND h.inventory_guarded
                    AND c.id=NEW.capture_id AND c.state='available' AND c.manifest_bound
                    AND c.checksum=NEW.checksum AND c.prerequisite_shas=NEW.prerequisite_shas
                    AND f.id=NEW.connection_id AND f.forge_type=NEW.forge_kind
                    AND rtrim(f.base_url,'/')=NEW.normalized_base_url
                    AND p.forge_project_id=NEW.project_id AND p.default_branch=NEW.default_branch
            ) THEN
            RAISE EXCEPTION 'closure marker unavailable or binding changed' USING ERRCODE='55000';
        END IF;
    END IF;
    IF NEW.state='deleted' AND OLD.state<>'deleted' AND NOT EXISTS (
        SELECT 1 FROM recovery_captures c WHERE c.id=NEW.capture_id AND c.state IN ('expired','discarded')
    ) THEN
        RAISE EXCEPTION 'closure deletion requires an expired or discarded capture' USING ERRCODE='55000';
    END IF;
    IF OLD.absent_since IS NOT NULL AND NEW.absent_since IS NOT NULL
        AND (NEW.absent_since,NEW.absence_pass_id) IS DISTINCT FROM (OLD.absent_since,OLD.absence_pass_id) THEN
        RAISE EXCEPTION 'absence candidate must be cleared before replacement' USING ERRCODE='55000';
    END IF;
    IF NEW.state='lost' AND (OLD.absent_since IS NULL OR OLD.absent_since+interval '10 minutes'>now()
        OR NEW.last_audit_pass_id IS NULL OR NEW.last_audit_pass_id=OLD.absence_pass_id) THEN
        RAISE EXCEPTION 'absence requires a later confirming pass after ten minutes' USING ERRCODE='55000';
    END IF;
    IF OLD.absent_since IS NOT NULL AND NEW.state='pinned' AND NEW.absent_since IS NULL
        AND (NEW.last_audit_pass_id IS NULL OR NEW.last_audit_pass_id=OLD.absence_pass_id) THEN
        RAISE EXCEPTION 'absence clear requires a later pass' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER prerequisite_closure_guard BEFORE INSERT OR UPDATE OR DELETE ON recovery_prerequisite_closures
    FOR EACH ROW EXECUTE FUNCTION prerequisite_closure_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Goose runs this migration in one transaction. Block inserts before checking ANY evidence.
LOCK TABLE recovery_prerequisite_closures IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM recovery_prerequisite_closures) THEN
        RAISE EXCEPTION 'prerequisite closure evidence exists; rollback refused' USING ERRCODE='55000';
    END IF;
END $$;
-- +goose StatementEnd
-- No CASCADE: PostgreSQL also refuses dependent schema evidence.
DROP TABLE recovery_prerequisite_closures;
DROP FUNCTION prerequisite_closure_guard();
