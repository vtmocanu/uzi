-- GitHub Enterprise Server serves its REST API on the web host under /api/v3, not on an api.<host>
-- subdomain. The worker now derives that base (agent/src/forge.ts githubRepoParts), so a GHES repo's
-- API origin equals its Git origin and the two evidence surfaces share one cooldown row, as GitLab
-- and Forgejo already do. github.com (with or without a leading www.) keeps api.github.com, and
-- GitHub Enterprise Cloud with data residency (<sub>.ghe.com) keeps api.<host>.
-- fixtures/forge-api-origin/cases.json pins this against the TS derivation. Cooldown rows keyed on
-- the old api.<host> origin are no longer authorized; the existing pruner drops them on the next repo or connection write.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION canonical_forge_api_origin(forge_type text, web_url text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE git_origin text;
BEGIN
    git_origin := canonical_forge_origin(web_url || '.git');
    IF git_origin IS NULL OR forge_type IS DISTINCT FROM 'github' THEN RETURN git_origin; END IF;
    IF git_origin IN ('https://github.com', 'https://www.github.com') THEN RETURN 'https://api.github.com'; END IF;
    IF split_part(substr(git_origin, length('https://') + 1), ':', 1) LIKE '%.ghe.com' THEN
        RETURN canonical_forge_origin('https://api.' || regexp_replace(substr(git_origin, length('https://') + 1), '^www\.', ''));
    END IF;
    RETURN git_origin;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION canonical_forge_api_origin(forge_type text, web_url text) RETURNS text
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE git_origin text; host text;
BEGIN
    git_origin := canonical_forge_origin(web_url || '.git');
    IF git_origin IS NULL OR forge_type IS DISTINCT FROM 'github' THEN RETURN git_origin; END IF;
    host := substr(git_origin, length('https://') + 1);
    IF host = 'github.com' THEN RETURN 'https://api.github.com'; END IF;
    RETURN canonical_forge_origin('https://api.' || regexp_replace(host, '^www\.', ''));
END $$;
-- +goose StatementEnd
