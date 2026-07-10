// The curated model aliases, for the agent package (PRD #37 Decision 2).
//
// This is the third copy of one list, and the drift is deliberate rather than
// accidental: `web/src/components/ModelSelect.tsx` offers them in the template
// editor, `api/internal/agenttmpl/model.go` bounds the *shape* of a model string
// (single token, no control chars) without enumerating valid IDs, and the worker
// needs the enumeration to CLAMP an untrusted repo-declared model. A repo's
// `.claude/agents/*.md` may name any model; only an alias on this list is
// honored, anything else (custom IDs, typos) is ignored so the agent inherits the
// run default. That bounds two abuses at once: a hostile repo pinning the most
// expensive model onto the user's Anthropic quota, and a bogus id that would
// self-DoS the run with an SDK error.
//
// A test (test/repoagents.test.ts) pins this list against ModelSelect.tsx so the
// two never drift apart silently.

export const MODEL_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;

export type ModelAlias = (typeof MODEL_ALIASES)[number];

/** True when `value` is one of the curated aliases (exact, case-sensitive — the
 *  SDK's own alias matching is). Blank/absent is not an alias: it means inherit. */
export function isModelAlias(value: string | null | undefined): value is ModelAlias {
  if (!value) return false;
  return (MODEL_ALIASES as readonly string[]).includes(value);
}
