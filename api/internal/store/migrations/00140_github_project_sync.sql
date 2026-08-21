-- +goose Up

-- PRD #364 M2: persistence for the GitHub Projects v2 Status sync. Two side
-- tables, both keyed off repos (never authoritative — the forge/project board is
-- the source of truth and the poller reconciles these against it).
--
-- github_project_links: one row per synced repo, holding the project handle
-- (node id + number), the Status field id, and the map of column-name → option-id
-- (status_options) the poller needs to translate a board column into a Projects v2
-- single-select option. owned_by_uzi records whether uzi created the project (so
-- teardown may delete it). last_synced_at/last_error are observability columns the
-- poller writes on each sync attempt.
CREATE TABLE github_project_links (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id         uuid NOT NULL UNIQUE REFERENCES repos ON DELETE CASCADE,
    project_node_id text NOT NULL,                              -- PVT_…
    project_number  bigint NOT NULL,
    status_field_id text NOT NULL,                              -- PVTSSF_…
    status_options  jsonb NOT NULL DEFAULT '{}'::jsonb,         -- column-name → option-id
    owned_by_uzi    boolean NOT NULL DEFAULT false,
    last_synced_at  timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- github_project_items: per-issue projection state. item_node_id is the Projects
-- v2 item handle (PVTI_…); last_status_option_id is the option last written for
-- this item and is the poller's diff basis (skip the mutation when the desired
-- option already matches). Composite PK keyed like the issues cache and the
-- autopilot_triggers side table, on the (repo_id, forge_issue_iid) natural key.
CREATE TABLE github_project_items (
    repo_id               uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    forge_issue_iid       bigint NOT NULL,
    item_node_id          text NOT NULL,                        -- PVTI_…
    last_status_option_id text,
    last_synced_at        timestamptz,
    PRIMARY KEY (repo_id, forge_issue_iid)
);

-- +goose Down
DROP TABLE github_project_items;
DROP TABLE github_project_links;
