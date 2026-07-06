-- +goose Up

-- Latest-pipeline-per-watched-ref cache (PRD #6). The forge stays the source of
-- truth; this table only remembers the newest pipeline uzi last observed for each
-- watched ref (a repo's default branch, or an agent run branch) so the repos
-- list, board header, and cards can render CI badges without a per-render forge
-- call. One row per (repo, ref), upserted every poll tick; refs no longer watched
-- are evicted on the reconcile tick, mirroring the issues-cache eviction. ref
-- holds the WATCHED ref (default branch name or an agent branch name), not the
-- pipeline's own internal ref — a detached MR pipeline reports
-- refs/merge-requests/N/head, but uzi keys the cache (and verification) on the
-- run branch. status is the raw GitLab pipeline status, stored verbatim.
CREATE TABLE pipeline_statuses (
    id               bigserial PRIMARY KEY,
    repo_id          uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    ref              text NOT NULL,          -- default branch, or an agent run branch
    pipeline_id      bigint NOT NULL,
    sha              text NOT NULL,
    status           text NOT NULL,          -- raw GitLab pipeline status
    web_url          text NOT NULL,
    forge_updated_at timestamptz,
    synced_at        timestamptz NOT NULL,
    UNIQUE (repo_id, ref)
);

CREATE INDEX idx_pipeline_statuses_repo ON pipeline_statuses (repo_id);

-- +goose Down
DROP TABLE pipeline_statuses;
