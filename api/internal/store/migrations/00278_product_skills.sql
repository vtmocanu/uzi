-- +goose Up

-- PRD #1909 M6: product skill sets. A registered product may point at a skills repo; an admin
-- syncs it (STAGED), reviews the staged set and approves it (APPLIED), and the approved skills
-- reach only that product's jobs at claim time.
--
-- The migration number is a draft: it is renumbered above the live head at landing.

-- 1. The skills scope vocabulary gains 'product'. The scope CHECK was created unnamed in
-- 00040_skills.sql (an inline column CHECK), so PostgreSQL named it <table>_<column>_check:
-- skills_scope_check (TestSkillsScopeConstraintNameLiveDB pins the name against pg_constraint).
-- It is dropped and re-added under the same name with the wider list.
ALTER TABLE skills DROP CONSTRAINT skills_scope_check;
ALTER TABLE skills ADD CONSTRAINT skills_scope_check
    CHECK (scope IN ('builtin', 'global', 'user', 'product'));

-- 2. product_id: set exactly on product-scope rows. CASCADE: a product's skills never outlive it
-- (products are soft-deleted, so in practice this only matters for a hard delete).
ALTER TABLE skills ADD COLUMN product_id uuid REFERENCES products ON DELETE CASCADE;
ALTER TABLE skills ADD CONSTRAINT skills_product_scope_check
    CHECK ((scope = 'product') = (product_id IS NOT NULL));

-- 3. Uniqueness. The shared index covered every scope but 'user' (scope <> 'user'); with a fourth
-- scope that predicate would make a product skill collide with a same-named builtin or global one,
-- so it is rebuilt over the two shared scopes by explicit list. A product's names are unique per
-- product only.
DROP INDEX uq_skills_shared_name;
CREATE UNIQUE INDEX uq_skills_shared_name ON skills (name) WHERE scope IN ('builtin', 'global');
CREATE UNIQUE INDEX uq_skills_product_name ON skills (product_id, name) WHERE scope = 'product';

-- 4. The product's skills source and its approval record. skills_token_sealed is the read-only
-- clone token sealed with secretbox (AAD 'product_skills_token|<product id>'); it is write-only
-- at the API and is never projected into a DTO. An empty repo URL means no source configured.
ALTER TABLE products
    ADD COLUMN skills_repo_url     text NOT NULL DEFAULT '' CHECK (octet_length(skills_repo_url) <= 2048),
    ADD COLUMN skills_ref          text NOT NULL DEFAULT '' CHECK (octet_length(skills_ref) <= 256),
    ADD COLUMN skills_token_sealed bytea,
    ADD COLUMN skills_applied_sha  text NOT NULL DEFAULT '',
    ADD COLUMN skills_applied_by   uuid REFERENCES users ON DELETE SET NULL,
    ADD COLUMN skills_applied_at   timestamptz;

-- 5. product_skill_staged: the ONE pending snapshot of a product's skills repo, awaiting admin
-- approval. A single row per product (a new sync replaces it) so an approval is atomic and a repo
-- with no skills is still a stageable, approvable state. skills is the validated set
-- [{name, description, body}], dropped the per-skill notes [{name, reason}]. It is never read at
-- claim time: only apply moves it into skills.
CREATE TABLE product_skill_staged (
    product_id uuid PRIMARY KEY REFERENCES products ON DELETE CASCADE,
    source_sha text NOT NULL CHECK (source_sha ~ '^[0-9a-f]{40}$'),
    staged_by  uuid REFERENCES users ON DELETE SET NULL,
    staged_at  timestamptz NOT NULL DEFAULT now(),
    skills     jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(skills) = 'array'),
    dropped    jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(dropped) = 'array')
);

-- +goose Down
-- Product-scope rows must go before the scope CHECK narrows back.
DELETE FROM skills WHERE scope = 'product';
DROP TABLE product_skill_staged;
ALTER TABLE products
    DROP COLUMN skills_applied_at,
    DROP COLUMN skills_applied_by,
    DROP COLUMN skills_applied_sha,
    DROP COLUMN skills_token_sealed,
    DROP COLUMN skills_ref,
    DROP COLUMN skills_repo_url;
DROP INDEX uq_skills_product_name;
DROP INDEX uq_skills_shared_name;
CREATE UNIQUE INDEX uq_skills_shared_name ON skills (name) WHERE scope <> 'user';
ALTER TABLE skills DROP CONSTRAINT skills_product_scope_check;
ALTER TABLE skills DROP COLUMN product_id;
ALTER TABLE skills DROP CONSTRAINT skills_scope_check;
ALTER TABLE skills ADD CONSTRAINT skills_scope_check CHECK (scope IN ('builtin', 'global', 'user'));
