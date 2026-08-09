-- +goose Up
-- Trusted-repo instructions opt-in (PRD #246): per-repo, default OFF. When true the
-- LEAD reads the clone's root CLAUDE.md as a nonce-fenced UNTRUSTED/ADVISORY block.
-- Sibling of repo_skills_enabled (PRD #16) and repo_devbox_opt_in (PRD #18) — same
-- trust posture, same PatchRepo toggle. settingSources stays []; the worker reads and
-- injects through its own channel.
ALTER TABLE repos ADD COLUMN repo_claudemd_enabled BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE repos DROP COLUMN repo_claudemd_enabled;
