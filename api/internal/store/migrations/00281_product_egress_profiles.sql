-- +goose Up

-- PRD #1976 M1: the per-product site-list allowance. A product token may create a job bound to an
-- egress profile (site list) only when a row here pairs its product with that profile; a user
-- (uzc_) token is not limited by this table. Admin writes only (the cookie-only admin write group).
--
-- Both sides CASCADE: a hard-deleted product or a deleted profile drops its allowances (profiles
-- referenced by a run are protected separately by runs.egress_profile_id's RESTRICT). A product
-- whose allowance rows are all gone is refused every site list: fail closed.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
CREATE TABLE product_egress_profiles (
    product_id        uuid        NOT NULL REFERENCES products ON DELETE CASCADE,
    egress_profile_id uuid        NOT NULL REFERENCES egress_profiles ON DELETE CASCADE,
    created_by        uuid        REFERENCES users ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (product_id, egress_profile_id)
);

-- The profile-side lookup (what a profile delete cascades over, and any future "who may use this
-- list" read) is not served by the (product_id, ...) primary key.
CREATE INDEX idx_product_egress_profiles_profile ON product_egress_profiles (egress_profile_id);

-- +goose Down
DROP TABLE product_egress_profiles;
