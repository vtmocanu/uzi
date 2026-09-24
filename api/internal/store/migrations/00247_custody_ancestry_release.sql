-- +goose Up

-- Issue #1582 M1: an OLDER-generation custody hold on a COMPLETED run can stay open after a
-- same-worker resume adopted its work (the completed-generation backstop in
-- ListReleasableCustodyHolds deliberately never qualifies an older generation). The api now
-- releases that exact older hold ONLY on its OWN proof, taken through the forge compare API:
-- every candidate SHA the worker names (what the predecessor pushed, its source, what the
-- successor adopted) must be an ancestor of, or equal to, the completed branch's head. The
-- worker's ancestry opinion is never accepted. This migration is the schema half:
--
--   1. recovery_custody_holds_release_evidence_check widened with a SIXTH class, 'ancestry'
--      (server-stamped only; the worker Release endpoint's allowlist stays
--      publication/forge_no_output). Drop + re-add, the 00232 template: the CHECK validates
--      against the existing rows on ADD, cheap for this small domain column.
--   2. Six nullable AUDIT columns recording the exact evidence an 'ancestry' release rested on:
--      the three candidate SHAs, the branch head the api read (release_final_head_sha), the
--      successor generation that completed, and the completed branch. Each SHA is CHECKed to a
--      40-char lowercase hex object id; the branch is length-bounded.
--   3. An all-or-nothing CHECK: an 'ancestry' release carries all six audit columns, so a
--      partial row (evidence without its proof) is refused by the database, not just the code.
--
-- Every legacy row predates the columns and stays NULL; the new CHECKs admit NULL, so nothing
-- existing is rewritten. Additive for an N-1 worker (no column dropped, renamed or retyped).
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN (
        'publication',
        'archive',
        'forge_no_output',
        'owner_discard',
        'no_adopted_source',
        'ancestry'
    ));

ALTER TABLE recovery_custody_holds
    ADD COLUMN release_pushed_sha text,
    ADD COLUMN release_source_sha text,
    ADD COLUMN release_adopted_sha text,
    ADD COLUMN release_final_head_sha text,
    ADD COLUMN release_successor_generation bigint,
    ADD COLUMN release_branch text;

ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_pushed_sha_check
    CHECK (release_pushed_sha IS NULL OR release_pushed_sha ~ '^[0-9a-f]{40}$');
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_source_sha_check
    CHECK (release_source_sha IS NULL OR release_source_sha ~ '^[0-9a-f]{40}$');
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_adopted_sha_check
    CHECK (release_adopted_sha IS NULL OR release_adopted_sha ~ '^[0-9a-f]{40}$');
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_final_head_sha_check
    CHECK (release_final_head_sha IS NULL OR release_final_head_sha ~ '^[0-9a-f]{40}$');
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_branch_check
    CHECK (release_branch IS NULL OR (length(release_branch) BETWEEN 1 AND 255));

ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_ancestry_audit_check
    CHECK (release_evidence IS DISTINCT FROM 'ancestry' OR (
        release_pushed_sha IS NOT NULL
        AND release_source_sha IS NOT NULL
        AND release_adopted_sha IS NOT NULL
        AND release_final_head_sha IS NOT NULL
        AND release_successor_generation IS NOT NULL
        AND release_branch IS NOT NULL
    ));

-- +goose Down

-- Narrow the evidence CHECK back to 00232's five classes. An 'ancestry' row written while this
-- migration was applied would violate the narrower CHECK, so clear the now-forbidden class
-- first (NULL, never DELETE: undoing this feature must not destroy hold records; the hold's
-- state and released_at are untouched).
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_ancestry_audit_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_branch_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_final_head_sha_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_adopted_sha_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_source_sha_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_pushed_sha_check;

ALTER TABLE recovery_custody_holds
    DROP COLUMN release_branch,
    DROP COLUMN release_successor_generation,
    DROP COLUMN release_final_head_sha,
    DROP COLUMN release_adopted_sha,
    DROP COLUMN release_source_sha,
    DROP COLUMN release_pushed_sha;

UPDATE recovery_custody_holds SET release_evidence = NULL WHERE release_evidence = 'ancestry';
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN (
        'publication',
        'archive',
        'forge_no_output',
        'owner_discard',
        'no_adopted_source'
    ));
