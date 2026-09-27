-- +goose Up
-- PRD #1798 D9: the versioned, fenced PR-description artifact. Additive only.
--
-- pr_description_versions is append-only per publication attempt: a worker STAGES the
-- api-sanitized fields for one exact snapshot (base_sha, head_sha, target_branch) under its
-- live claim_generation, BINDS the version to the PR once the PR exists (mr_iid, plus the
-- sha256 of the exact region text it will write), and ACKS the forge write. state moves
-- pending -> published | abandoned; a published row is never rewritten.
CREATE TABLE pr_description_versions (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id                 uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    claim_generation       bigint NOT NULL,
    repo_id                uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    mr_iid                 bigint,
    fields                 jsonb NOT NULL,
    size                   jsonb,
    base_sha               text NOT NULL,
    head_sha               text NOT NULL,
    target_branch          text NOT NULL,
    source                 text NOT NULL
        CHECK (source IN ('generated', 'lead_only', 'deterministic_only')),
    rendered_region_sha256 text,
    state                  text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'published', 'abandoned')),
    created_at             timestamptz NOT NULL DEFAULT now(),
    published_at           timestamptz
);
CREATE INDEX idx_pr_description_versions_run ON pr_description_versions (run_id);
CREATE INDEX idx_pr_description_versions_pr_state ON pr_description_versions (repo_id, mr_iid, state);

-- pr_descriptions is one row per PR (refresh runs have different run ids, so the key is the
-- PR, not the run). published_version_id names the version whose region is on the forge
-- (NULL until the first acknowledged publish); lock_version is the compare-and-swap counter
-- every ack advances; last_outcome records the most recent write decision.
CREATE TABLE pr_descriptions (
    repo_id              uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    mr_iid               bigint NOT NULL,
    published_version_id uuid REFERENCES pr_description_versions (id) ON DELETE SET NULL,
    lock_version         bigint NOT NULL DEFAULT 0,
    last_outcome         text
        CHECK (last_outcome IN ('published', 'skipped_human_edit', 'skipped_no_region',
                                'skipped_malformed', 'skipped_snapshot_moved', 'write_failed')),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, mr_iid)
);

-- +goose Down
DROP TABLE pr_descriptions;
DROP TABLE pr_description_versions;
