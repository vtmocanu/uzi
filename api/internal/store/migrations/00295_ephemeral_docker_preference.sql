-- +goose Up
ALTER TABLE users ADD COLUMN ephemeral_docker_enabled boolean NOT NULL DEFAULT false;

-- +goose StatementBegin
CREATE FUNCTION fn_ephemeral_docker_preference_applies(
    preference boolean,
    tier_available boolean,
    run_repo_id uuid,
    run_kind text,
    run_egress_profile_id uuid,
    allowlist uuid[]
) RETURNS boolean
LANGUAGE sql IMMUTABLE
AS $$
    SELECT COALESCE(preference AND tier_available
        AND run_repo_id IS NOT NULL AND run_repo_id = ANY(allowlist)
        AND run_kind <> 'job' AND run_egress_profile_id IS NULL, false);
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION fn_ephemeral_docker_preference_applies(boolean, boolean, uuid, text, uuid, uuid[]);
ALTER TABLE users DROP COLUMN ephemeral_docker_enabled;
