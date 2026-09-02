// Runbook: adding a new run kind.
//
// runs.kind is pinned in several decoupled places; the runkind package and its
// three tests exist to make a miss loud rather than silent. This is the
// authoritative touch-point list — work every step, in order. The package doc
// comment (naming the DB runs_kind_check source of truth) lives in runkind.go,
// so the block below is intentionally a non-doc comment — separated from the
// package clause by a blank line — to keep exactly one package doc.
//
// Three Go parity pins fail loudly if a step is missed:
// runkind_migration_test.go (const/All() vs the live migration CHECK),
// runkind_sql_test.go (the literals in store/queries/), and
// runkind_fixture_test.go (fixtures/run-kinds/registry.json). The agent and web
// halves are pinned by agent/test/run-kind-db-parity.test.ts and
// web/src/lib/runKindContract.test.ts.
//
//  1. Migration under api/internal/store/migrations/: widen the runs_kind_check
//     CHECK constraint AND runs_kind_shape, plus any partial indexes and any
//     fn_run_priority*/fn_worker_can_claim kind sets that enumerate kinds.
//  2. runkind (this package, runkind.go): add the const, extend All() in DB
//     CHECK order, and update any property helper the kind participates in
//     (JudgeEligible, Listed).
//  3. fixtures/run-kinds/registry.json: add the kind (and to its judge_eligible
//     list if the kind is judge-eligible).
//  4. agent: add the RUN_KINDS entry in agent/src/protocol.ts and its
//     RUN_KIND_PROFILES row in agent/src/run-kind.ts.
//  5. web: add the RUN_KINDS entry in web/src/lib/runKind.ts and any
//     label/eligibility handling there.
//  6. sqlc: update the INSERT/filters in api/internal/store/queries/ that
//     reference the kind literal, then run `sqlc generate` to regenerate the
//     mirror in api/internal/store/*.sql.go.
//  7. Add a runner characterization test for the new kind's behaviour, following
//     the runner-<kind>.test.ts shape.

package runkind
