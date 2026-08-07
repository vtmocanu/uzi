-- +goose Up

-- Backfill the (category, target) COORDINATE to its canonical form (issue #232). `target`
-- is free text the judge LLM re-derives per run, so the same finding phrased with
-- different casing/whitespace/punctuation ("Worker Git-Identity Setup" vs
-- "worker  git-identity setup") formed SEPARATE backlog rows, defeating the cross-run
-- dedup that keys on exact (category, target) equality. From this migration on, ingest
-- canonicalizes at write (handler.canonicalizeTarget); this pass folds the rows written
-- BEFORE it so the historical backlog agrees with everything ingested after.
--
-- The canonical expression is lower(btrim(regexp_replace(target COLLATE "C",
-- '[[:space:][:punct:]]+', ' ', 'g'))): lowercase, collapse every run of
-- whitespace/punctuation to one space, trim the ends. It folds ONLY cosmetic drift — it
-- NEVER reorders tokens, drops stopwords, or stems — because over-merge (fusing two
-- genuinely different findings) is the unsafe failure mode for a triage backlog, strictly
-- worse than leaving a cosmetic duplicate.
--
-- The fold is ASCII-ONLY and LOCALE-INDEPENDENT, forced by COLLATE "C" on the regexp_replace
-- input: under the production UTF-8 locale (en_US.utf8) Postgres's POSIX [:space:]/[:punct:]
-- classes AND lower() are locale-aware and would fold non-ASCII (curly quotes, em-dashes,
-- NBSP, Unicode case); COLLATE "C" propagates through btrim() and the outer lower() so the
-- whole expression treats bytes as ASCII, leaving every non-ASCII byte UNCHANGED. That is
-- exactly what makes it match handler.canonicalizeTarget, which is ASCII-only by
-- construction (RE2's ASCII-only [:space:]/[:punct:] classes, an ASCII A–Z lowercase, and an
-- ASCII-space trim, with no Unicode NFC/lower/trim). Non-ASCII passes through unchanged on
-- BOTH sides — an under-merge, the safe direction — so ingest and these rows fold
-- byte-for-byte the same regardless of DB locale or Go/glibc Unicode-table differences.
--
-- THREE tables carry this coordinate and the backlog's LEFT JOINs match by EXACT target
-- equality: review_recommendations (the recs themselves, no unique constraint on the
-- coordinate), recommendation_dispositions and recommendation_filed_issues (both UNIQUE on
-- (review_id, category, target)). Canonicalizing only review_recommendations would
-- DE-LINK the other two — a disposition/filed row still holding the raw target no longer
-- joins its now-canonical recommendation, so a previously-triaged (dismissed/done/filed)
-- item would resurface as `todo`. So all three are folded together, in one transaction.
--
-- For the two UNIQUE tables, folding can COLLIDE two rows that previously differed only
-- cosmetically (same review_id + category, targets folding to the same value) onto one
-- unique key, which a bare UPDATE would 23505 on. So each is handled collision-safely:
-- FIRST delete the duplicate losers per canonical coordinate keeping one deterministic
-- winner, THEN run the UPDATE. Every statement is scoped to rows the fold actually CHANGES
-- (target <> the canonical value) so this is a near-noop on already-canonical data — which,
-- post-ingest, is every future row.

-- ── recommendation_dispositions: dedup losers, then fold ──
-- Winner per canonical coordinate: the most recently UPDATED disposition (a re-triage is the
-- freshest human intent), tie-broken by set_at then ctid so the choice is deterministic —
-- matching the ORDER BY below (updated_at DESC, set_at DESC, ctid DESC).
-- Only groups that ACTUALLY collide after folding (HAVING count(*) > 1) delete anything;
-- the grouping key uses the CANONICAL target so two rows folding together are one group.
DELETE FROM recommendation_dispositions d
USING (
    SELECT ctid,
           row_number() OVER (
               PARTITION BY review_id, category,
                            lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
               ORDER BY updated_at DESC, set_at DESC, ctid DESC
           ) AS rn,
           count(*) OVER (
               PARTITION BY review_id, category,
                            lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
           ) AS grp
    FROM recommendation_dispositions
) ranked
WHERE d.ctid = ranked.ctid
  AND ranked.grp > 1   -- HAVING count(*) > 1: only genuine collisions lose a row
  AND ranked.rn > 1;   -- keep rn = 1 (the winner), delete the rest

UPDATE recommendation_dispositions
SET target = lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
WHERE target <> lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')));

-- ── recommendation_filed_issues: dedup losers, then fold ──
-- Winner per canonical coordinate: a SETTLED filed row (filed_at NOT NULL) outranks an
-- unsettled claim — a real filed issue is the durable pointer worth keeping — then the
-- most recent (filed_at, then filing_since), tie-broken by ctid. Same collision-only
-- deletion, same canonical grouping key.
DELETE FROM recommendation_filed_issues f
USING (
    SELECT ctid,
           row_number() OVER (
               PARTITION BY review_id, category,
                            lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
               ORDER BY (filed_at IS NOT NULL) DESC, filed_at DESC NULLS LAST,
                        filing_since DESC NULLS LAST, ctid DESC
           ) AS rn,
           count(*) OVER (
               PARTITION BY review_id, category,
                            lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
           ) AS grp
    FROM recommendation_filed_issues
) ranked
WHERE f.ctid = ranked.ctid
  AND ranked.grp > 1
  AND ranked.rn > 1;

UPDATE recommendation_filed_issues
SET target = lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
WHERE target <> lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')));

-- ── review_recommendations: fold (no unique coordinate, so no dedup needed) ──
-- This table has no UNIQUE on the coordinate — the judge legitimately emits two rows with
-- the same (category, target) per review — so folding never violates a constraint here and
-- there is nothing to deduplicate. Fold last so, if a reader watches the tables mid-
-- transaction, the side tables are already consistent when the recs move.
UPDATE review_recommendations
SET target = lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))
WHERE target <> lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')));

-- +goose Down
-- IRREVERSIBLE, deliberately a documented no-op. This is a lossy data transform: the
-- pre-canonical raw target text ("Worker Git-Identity Setup") is overwritten in place and
-- its casing/whitespace/punctuation is not recoverable from the folded value, and the
-- collision-dedup DELETEs above are likewise gone. There is nothing correct to reverse to,
-- so Down does nothing rather than pretend to.
SELECT 1;
