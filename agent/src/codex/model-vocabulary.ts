/** Canonical model ids accepted by Codex role advice and provider map slots. */
export const CODEX_MODEL_IDS = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"] as const;
export const CODEX_ROLE_MODELS: ReadonlySet<string> = new Set(CODEX_MODEL_IDS);
