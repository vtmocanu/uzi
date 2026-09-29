-- +goose Up
CREATE TABLE finding_group_operations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    repo_id uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
    phase text NOT NULL DEFAULT 'pre_call' CHECK (phase IN ('pre_call', 'in_flight', 'returned_uncertain', 'issue_recorded', 'released', 'settled')),
    deadline_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    issue_iid bigint,
    issue_url text NOT NULL DEFAULT '',
    CHECK ((issue_iid IS NULL AND issue_url = '') OR (issue_iid IS NOT NULL AND issue_url <> '')),
    CHECK (phase NOT IN ('issue_recorded', 'settled') OR issue_iid IS NOT NULL)
);
CREATE INDEX idx_finding_group_operations_pending ON finding_group_operations (user_id, created_at) WHERE phase NOT IN ('released', 'settled');
ALTER TABLE finding_dispositions ADD COLUMN group_operation_id uuid REFERENCES finding_group_operations(id);
CREATE INDEX idx_finding_dispositions_group_operation ON finding_dispositions (group_operation_id) WHERE group_operation_id IS NOT NULL;
CREATE TABLE finding_group_members (
    operation_id uuid NOT NULL REFERENCES finding_group_operations(id) ON DELETE CASCADE,
    disposition_id uuid NOT NULL REFERENCES finding_dispositions(id) ON DELETE CASCADE,
    finding_id uuid NOT NULL,
    PRIMARY KEY (operation_id, disposition_id)
);
-- +goose Down
DROP TABLE finding_group_members;
DROP INDEX idx_finding_dispositions_group_operation;
ALTER TABLE finding_dispositions DROP COLUMN group_operation_id;
DROP TABLE finding_group_operations;
