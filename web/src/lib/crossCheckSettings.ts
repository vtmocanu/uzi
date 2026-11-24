import { CODEX_CURATED_MODELS } from "./codexModels";
import type { Harness } from "./apiTypes";
import { modelFieldWarning } from "./agentTemplates";

export function normalizeCheckerValue(value: string): string {
  // strings.TrimSpace uses Unicode White_Space, which differs from JS trim (FEFF).
  return value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
}

// Closed family guards mirror agenttmpl.ValidateFamilyModel; custom IDs remain valid.
export function checkerModelWarning(model: string, harness: Harness): string {
  const value = normalizeCheckerValue(model);
  if (/[\p{Cc}\p{Cf}\uFFFD]/u.test(value)) return "Invalid model ID.";
  const syntax = modelFieldWarning(value);
  if (syntax) return syntax;
  if (new TextEncoder().encode(value).length > 100) return "Invalid model ID.";
  const otherFamily = harness === "claude"
    ? CODEX_CURATED_MODELS
    : ["opus", "sonnet", "haiku", "fable"];
  return otherFamily.includes(value) ? "Choose a model from the checker's model family." : "";
}
