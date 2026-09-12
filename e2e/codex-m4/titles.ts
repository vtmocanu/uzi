// PRD #1287 C1 — the exact executed-test TITLE constants shared by three places that must
// never drift: the `it(...)` name, the `recordEvidence(...)` call, and the clause row's
// `tests` list. A single source of truth keeps the completeness checker's zero-test-match
// rule honest — a renamed test cannot silently stop matching its clause.
//
// Pure constants (no imports, no side effects) so both the data contribution files
// (clauses-*.ts) and the test files can import it without pulling in node built-ins.

/** The C1 real-binary P startup/allowed-callback smoke (startup-smoke.test.ts). */
export const CODEX_STARTUP_SMOKE_TITLE =
  "codex P smoke: real app-server admits one allowed uzi_bash callback through the real broker";
