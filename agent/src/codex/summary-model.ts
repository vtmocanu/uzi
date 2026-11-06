// Issue #3248: the built-in Codex model and effort for the intent/plan summary passes.
//
// A deliberately tiny LEAF module (no imports), mirroring `pr-description-model.ts`. The api resolves
// the Codex summary model per claim; this is the fallback when the claim's `model` is absent or not a
// Codex role model (an older server sends the Claude lane value). It is pinned to
// fixtures/model-settings/summary.json `builtins.codex` and to the api built-in
// agenttmpl.DefaultCodexSummaryModel; change all three together.

export const DEFAULT_CODEX_SUMMARY_MODEL = "gpt-6-luna";
export const CODEX_SUMMARY_EFFORT = "low";
