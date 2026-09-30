-- +goose Up

-- Issue #1742: one-shot finalize-resume allowance. runs.finalize_resume_generation is the exact
-- claim generation at which Register's attested-finalize pass re-queued this run OVER its
-- RUN_MAX_REQUEUES budget. NULL = the allowance was never used; once set it never fires again for
-- the run. The migration number is a draft: it is renumbered above the live head at landing.
ALTER TABLE runs ADD COLUMN finalize_resume_generation BIGINT NULL;
COMMENT ON COLUMN runs.finalize_resume_generation IS
    'Issue #1742: the exact claim generation at which Register''s one-shot finalize-resume allowance re-queued this run over its budget. NULL = never used; once set the allowance never fires again for the run.';

-- +goose Down
ALTER TABLE runs DROP COLUMN finalize_resume_generation;
