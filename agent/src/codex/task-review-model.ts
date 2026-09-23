// PRD #1551 (M2, D5): the built-in Codex TASK-REVIEW model.
//
// This is a deliberately tiny LEAF module (no imports) so `agent/src/review-runner.ts`
// can read the constant without a runtime import of the heavy `codex-executor.ts`
// (which review-runner otherwise references type-only). `codex-executor.ts` re-exports
// it beside CODEX_PRODUCTION_PROVIDER for locality.
//
// Codex task review is INDEPENDENT of the saved per-user worker-model defaults on both
// harnesses: it always runs on this curated review-specific model, selected in the
// advice request's `model` field (never the shared provider default). The shared
// production provider default and the ordinary no-model Codex run fallback stay
// `gpt-6-astra`; only Codex task review uses this value. It is one of the curated
// contract models, so the advice renderer accepts it. No review-model SETTING ships in
// #1551 (configurable review-model defaults are future work, brainstorm issue #1570).

export const CODEX_TASK_REVIEW_MODEL = "gpt-6-sol";
