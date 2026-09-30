-- +goose Up

-- PRD #1908 M1 (schema half of the repo-less `job` run kind). A job is a run with no repo, no
-- clone, no git, no MR and no plan gate, created through the /api/v1 surface, that returns a
-- structured result instead of a branch. This migration widens runs.kind with 'job', adds the
-- typed runs.job_type discriminator, and creates the four job tables plus the per-product
-- allow-list. Behaviour (claiming, gating, ingest) lands in later milestones.
--
-- The migration number is a draft: it is renumbered above the live head at landing.

-- 1. runs.job_type: the job's type, non-NULL exactly when kind = 'job'. The known-values CHECK
-- mirrors runkind.JobTypes() (pinned by a live-DB test); today the only type is 'research'.
ALTER TABLE runs ADD COLUMN job_type text;
ALTER TABLE runs ADD CONSTRAINT runs_job_type_check
    CHECK (job_type IS NULL OR job_type IN ('research'));
-- Both operands are NOT NULL booleans, so this CHECK is never UNKNOWN: a job without a type and
-- a typed non-job are both rejected.
ALTER TABLE runs ADD CONSTRAINT runs_job_type_iff_job
    CHECK ((kind = 'job') = (job_type IS NOT NULL));

-- 2. The kind domain and per-kind shape widen the same drop/re-add way 00167 did. A job is
-- repo-less like chat and judge (repo_id NULL, no issue_iid, no branch) and carries a job_type.
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task', 'mr_rework', 'job'));

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL)
 OR (kind = 'mr_rework'    AND repo_id IS NOT NULL AND pipeline_ref IS NOT NULL AND mr_iid IS NOT NULL AND target_run_id IS NOT NULL)
 OR (kind = 'job'          AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND job_type IS NOT NULL));

-- 3. runs_fail_origin_check: three new SERVER-DERIVED values (no_job_capable_worker and
-- ephemeral_worker_never_registered from the sweeper's unservable-ephemeral-worker pass,
-- job_no_result from the job-result ingest invariant). Drop and re-add with the twenty values
-- carried verbatim from 00269's Up plus the three new ones, twenty-three in all.
-- TestFailOriginVocabularyMatchesCheck asserts this CHECK equals workersvc.AllFailOrigins().
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full',
        'worker_residue_blocked',
        'skills_plugin_load_failed',
        'no_job_capable_worker',
        'ephemeral_worker_never_registered',
        'job_no_result'
    ));

-- 4. job_inputs: the inline text inputs of a job. name is a file-name-shaped token (letters,
-- digits, dot, underscore, hyphen; first character alphanumeric, so no leading dot) and must not
-- contain '..' so it can never traverse when a later milestone materialises inputs. The byte
-- CHECKs are the DB backstop; the service layer enforces the tighter product caps.
CREATE TABLE job_inputs (
    run_id     uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    ordinal    int  NOT NULL CHECK (ordinal >= 0),
    name       text NOT NULL
        CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$' AND position('..' in name) = 0),
    content_md text NOT NULL CHECK (octet_length(content_md) <= 1048576),
    PRIMARY KEY (run_id, ordinal),
    CONSTRAINT job_inputs_run_name_key UNIQUE (run_id, name)
);

-- 5. job_origins: who created the job. product_id / product_token_id are NULL for a uzc_ caller.
-- product_id is RESTRICT (a product with jobs cannot be hard-deleted; products are soft-deleted
-- anyway); product_token_id is SET NULL so revoking-then-deleting a token keeps the audit row.
-- requested_by_label is an UNTRUSTED end-user audit label supplied by a product: attribution
-- only, never an authorization input.
CREATE TABLE job_origins (
    run_id             uuid PRIMARY KEY REFERENCES runs ON DELETE CASCADE,
    product_id         uuid REFERENCES products ON DELETE RESTRICT,
    product_token_id   uuid REFERENCES product_tokens ON DELETE SET NULL,
    requested_by_label text CHECK (octet_length(requested_by_label) <= 200),
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Product-scoped reads and the revoke sweep look origins up by product.
CREATE INDEX idx_job_origins_product ON job_origins (product_id) WHERE product_id IS NOT NULL;

-- 6. job_results: the one structured result per job run (report_md issue-only invariant stays
-- intact; this is the job's own home for it). status is a short bounded token whose vocabulary
-- the ingest layer owns.
CREATE TABLE job_results (
    run_id     uuid PRIMARY KEY REFERENCES runs ON DELETE CASCADE,
    status     text NOT NULL CHECK (octet_length(status) BETWEEN 1 AND 64),
    report_md  text NOT NULL DEFAULT '' CHECK (octet_length(report_md) <= 1048576),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 7. job_findings: structured findings of a job result, mirroring task_review_findings. The
-- optional location is EITHER a url OR a file (+ line); a line needs a file.
CREATE TABLE job_findings (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id     uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    ordinal    int  NOT NULL CHECK (ordinal >= 0),
    severity   text NOT NULL CHECK (severity IN ('info', 'warning', 'error')),
    message_md text NOT NULL CHECK (octet_length(message_md) <= 65536),
    url        text CHECK (octet_length(url) <= 2048),
    file       text CHECK (octet_length(file) <= 1024),
    line       int  CHECK (line >= 0),
    CONSTRAINT job_findings_run_ordinal_key UNIQUE (run_id, ordinal),
    CONSTRAINT job_findings_location_check CHECK (NOT (url IS NOT NULL AND file IS NOT NULL)),
    CONSTRAINT job_findings_line_needs_file CHECK (line IS NULL OR file IS NOT NULL)
);

-- 8. products.allowed_job_types: the job types a product's uzp_ tokens may create. Empty (the
-- default) refuses every job. The known-values CHECK mirrors runkind.JobTypes().
-- `<@` treats a NULL element as unmatched, and array_position(..., NULL) spells that out
-- rather than relying on it (the 00270 phrasing).
ALTER TABLE products ADD COLUMN allowed_job_types text[] NOT NULL DEFAULT '{}';
ALTER TABLE products ADD CONSTRAINT products_allowed_job_types_check
    CHECK (allowed_job_types <@ ARRAY['research']::text[] AND array_position(allowed_job_types, NULL) IS NULL);

-- fn_worker_can_claim is deliberately UNCHANGED: a docker worker admits only its allow-listed
-- repos plus repo-less JUDGE runs, so it never claims a repo-less job (the judge arm is false for
-- kind 'job' and a NULL run_repo_id makes the allow-list arm NULL, so the function is not TRUE).
-- Non-docker gating for jobs is claim-query work in a later milestone.

-- +goose Down
-- Every job row must go before the kind and shape CHECKs narrow back. job_inputs, job_origins,
-- job_results and job_findings cascade from runs; other tables referencing runs by a
-- non-cascading key are not populated for a job.
DELETE FROM runs WHERE kind = 'job';

ALTER TABLE products DROP CONSTRAINT products_allowed_job_types_check;
ALTER TABLE products DROP COLUMN allowed_job_types;

DROP TABLE job_findings;
DROP TABLE job_results;
DROP INDEX idx_job_origins_product;
DROP TABLE job_origins;
DROP TABLE job_inputs;

-- The fail_origin values added here would violate the narrower CHECK; NULL them rather than
-- delete run records (fail_origin is nullable). Job rows are already gone, but a defensive NULL
-- keeps the down-migration total.
UPDATE runs SET fail_origin = NULL
 WHERE fail_origin IN ('no_job_capable_worker', 'ephemeral_worker_never_registered', 'job_no_result');
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full',
        'worker_residue_blocked',
        'skills_plugin_load_failed'
    ));

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL)
 OR (kind = 'mr_rework'    AND repo_id IS NOT NULL AND pipeline_ref IS NOT NULL AND mr_iid IS NOT NULL AND target_run_id IS NOT NULL));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task', 'mr_rework'));

ALTER TABLE runs DROP CONSTRAINT runs_job_type_iff_job;
ALTER TABLE runs DROP CONSTRAINT runs_job_type_check;
ALTER TABLE runs DROP COLUMN job_type;
