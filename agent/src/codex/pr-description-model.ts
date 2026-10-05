// PRD #1798 (M5, D13): the built-in Codex model for the PR-description EDITOR pass.
//
// A deliberately tiny LEAF module (no imports) so `agent/src/summary-runner.ts` can read the
// constant without a runtime import of the heavy `codex-executor.ts` (which it references
// type-only), mirroring `task-review-model.ts`.
//
// A Codex run's delivery summary runs on this curated model, selected in the advice request's
// `model` field, never the claim's default_model: the editor pass is one short turn without worker callbacks (#1566 native async/UTC exceptions),
// so it takes the cheapest of the contract models the advice renderer accepts
// (CONTRACT_MODELS in codex/render.ts), by the per-token rates in codex/codex-pricing.ts. A
// Claude run uses the resolved `summary_model` instead (PRD #362). No setting ships for it.

export const CODEX_PR_DESCRIPTION_MODEL = "gpt-6-sol";
