// Checklist: adding a new run kind.
//
// runs.kind is pinned in several decoupled places; the runkind package and its
// three tests exist to make a miss loud rather than silent. This skeleton lists
// the touch points; PRD #983 M5 expands it into the full runbook. The package
// doc comment (naming the DB runs_kind_check source of truth) lives in
// runkind.go, so the block below is intentionally a non-doc comment — separated
// from the package clause by a blank line — to keep exactly one package doc.
//
//  1. Migration: widen runs_kind_check (and runs_kind_shape, the partial
//     indexes, and any fn_run_priority*/fn_worker_can_claim kind sets) under
//     api/internal/store/migrations/.
//  2. runkind: add the const, extend All() (DB CHECK order), and any property
//     helper (JudgeEligible/Listed) the new kind participates in.
//  3. fixtures/run-kinds/registry.json: add the kind (and judge_eligible if so).
//  4. agent: add the RUN_KINDS entry and its RUN_KIND_PROFILES row.
//  5. web: add the RUN_KINDS entry and any label/eligibility handling.
//  6. sqlc: the INSERT/filters in store/queries/ that reference the literal.
//  7. a runner characterization test for the new kind's behaviour.

package runkind
