-- +goose Up

-- Tier-2 repo devbox.json opt-in (PRD #18 M5, Decision 4): per-repo, default OFF.
-- When true, the WORKER extracts ONLY the `packages` array from the cloned repo's
-- own devbox.json and unions it with the tier-1 profile (tier-1 wins version
-- conflicts). shell.init_hook / shell.scripts / flake refs / every other key are
-- ignored and never executed. Lives on `repos` alongside repo_skills_enabled (the
-- sibling repo-borne-config opt-in, PRD #16) — same trust posture, same PatchRepo
-- toggle, one repo-settings surface.
ALTER TABLE repos ADD COLUMN repo_devbox_opt_in BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE repos DROP COLUMN repo_devbox_opt_in;
