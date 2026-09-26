-- +goose Up

-- Issue #1751 M2: a worker can settle an OLDER-generation custody hold while the resumed
-- (successor) run is still LIVE, same worker only. The api proves, through the forge alone,
-- that every candidate SHA the worker names is an ancestor of (or equal to) the PUBLISHED
-- target the successor generation pushed: its forge checkpoint ref
-- (refs/uzi-checkpoints/<derived branch>) or its run branch. The completed-run 'ancestry'
-- settle (migration 00251) is unchanged. This migration is the schema half:
--
--   1. recovery_custody_holds_release_evidence_check widened with a SEVENTH class,
--      'live_ancestry' (server-stamped only; the worker Release endpoint's allowlist stays
--      publication/forge_no_output). Drop + re-add, the 00232/00251 template: the CHECK
--      validates against the existing rows on ADD, cheap for this small domain column.
--   2. One nullable column, release_target, naming WHICH published target a 'live_ancestry'
--      release was proven against: 'checkpoint' or 'branch'. release_branch (00251) records
--      the branch name the proof resolved (the checkpoint's derived branch or the run branch).
--   3. An all-or-nothing CHECK: a 'live_ancestry' release carries 00251's six audit columns AND
--      release_target, and release_target is NULL on every other row, so a partial row or a
--      target stamped on another evidence class is refused by the database, not just the code.
--      00251's recovery_custody_holds_ancestry_audit_check is kept intact.
--
-- Every legacy row predates the column and stays NULL; the new CHECKs admit that, so nothing
-- existing is rewritten. Additive for an N-1 worker (no column dropped, renamed or retyped).
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN (
        'publication',
        'archive',
        'forge_no_output',
        'owner_discard',
        'no_adopted_source',
        'ancestry',
        'live_ancestry'
    ));

ALTER TABLE recovery_custody_holds ADD COLUMN release_target text;

ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_target_check
    CHECK (release_target IS NULL OR release_target IN ('checkpoint', 'branch'));

ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_live_ancestry_audit_check
    CHECK (
        (release_evidence IS DISTINCT FROM 'live_ancestry' OR (
            release_pushed_sha IS NOT NULL
            AND release_source_sha IS NOT NULL
            AND release_adopted_sha IS NOT NULL
            AND release_final_head_sha IS NOT NULL
            AND release_successor_generation IS NOT NULL
            AND release_branch IS NOT NULL
            AND release_target IS NOT NULL
        ))
        AND (release_target IS NULL OR release_evidence = 'live_ancestry')
    );

-- +goose Down

-- Narrow the evidence CHECK back to 00251's six classes. A 'live_ancestry' row written while
-- this migration was applied would violate the narrower CHECK, so clear the now-forbidden class
-- first (NULL, never DELETE: undoing this feature must not destroy hold records; the hold's
-- state, released_at and 00251's audit columns are untouched).
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_live_ancestry_audit_check;
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_target_check;
ALTER TABLE recovery_custody_holds DROP COLUMN release_target;

UPDATE recovery_custody_holds SET release_evidence = NULL WHERE release_evidence = 'live_ancestry';
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
