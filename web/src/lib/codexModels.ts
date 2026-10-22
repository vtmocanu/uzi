// The web's Codex model vocabulary (PRD #3245 §1), a leaf module with no imports so
// the pickers, the admin warning and the mock can all read it without a cycle. Both
// lists are pinned to fixtures/codex-models/models.json by codexModels.test.ts, so a
// one-sided edit to either reddens the web gate.

// The curated Codex models: the worker, picker, run and judge vocabulary. Order is
// the picker's display order (the fixture is sorted; the test compares sorted).
export const CODEX_CURATED_MODELS: readonly string[] = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol"];

// The Codex role models: the curated set plus uzi's private advice model gpt-6-luna.
// No Claude lane accepts any of these (the server's Claude-lane guard).
export const CODEX_ROLE_MODELS: readonly string[] = [...CODEX_CURATED_MODELS, "gpt-6-luna"];
