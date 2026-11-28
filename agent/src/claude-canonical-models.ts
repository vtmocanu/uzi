// PRD #3252 D8 (m4): the bundled Claude CLI reports these values as `applied.model` and
// `applied.effort` from `getSettings` for a requested alias. The offline phase-switch test
// confirmed the same ids and omitted-effort defaults on SDK 0.3.330 / CLI 2.1.330.
// Fable moved from claude-fable-5-1/high at 0.3.319 to claude-fable-5-2/medium at 0.3.325.
// CI checks these tables against CLAUDE_PHASE_SDK and CLAUDE_PHASE_CLI on each bump.
// For an alias absent from the table, the settings barrier holds.

/** alias -> the canonical id the pinned CLI applies (and sends on the wire). */
export const CLAUDE_CANONICAL_MODELS: Readonly<Record<string, string>> = {
  opus: "claude-opus-5-5",
  sonnet: "claude-sonnet-5-5",
  haiku: "claude-haiku-5-5",
  fable: "claude-fable-5-2",
};

/** canonical id -> the effort the pinned CLI applies when the request names none. */
export const CLAUDE_OMITTED_EFFORT: Readonly<Record<string, string>> = {
  "claude-opus-5-5": "medium",
  "claude-sonnet-5-5": "medium",
  "claude-haiku-5-5": "medium",
  "claude-fable-5-2": "medium",
};

/** Aliases the Claude CLI resolves itself (a bare family name, optionally the 1M-context variant). */
const KNOWN_CLAUDE_ALIAS = /^(opus|sonnet|haiku|fable|best|default|opusplan)(\[1m\])?$/i;

export type ModelExpectation =
  /** No model was requested: the session keeps its model, nothing to compare. */
  | { kind: "none" }
  | { kind: "id"; id: string }
  /** A known alias this pin has not characterized. */
  | { kind: "uncharacterized" };

/** The `applied.model` the barrier must see for a requested model (already provider-mapped). */
export function expectedAppliedModel(requested: string | undefined): ModelExpectation {
  if (requested === undefined || requested === "") return { kind: "none" };
  const alias = requested.trim();
  const id = Object.hasOwn(CLAUDE_CANONICAL_MODELS, alias) ? CLAUDE_CANONICAL_MODELS[alias] : undefined;
  if (id !== undefined) return { kind: "id", id };
  if (KNOWN_CLAUDE_ALIAS.test(alias)) return { kind: "uncharacterized" };
  return { kind: "id", id: requested };
}

/** The effort the pinned CLI applies for an omitted request on a canonical model; undefined = unknown. */
export function expectedOmittedEffort(canonicalModel: string): string | undefined {
  return Object.hasOwn(CLAUDE_OMITTED_EFFORT, canonicalModel) ? CLAUDE_OMITTED_EFFORT[canonicalModel] : undefined;
}
