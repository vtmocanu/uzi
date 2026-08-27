// Model-string validation for the agent package (PRD #37 Decision 2, as ratified
// 2026-07-10).
//
// A repo's `.claude/agents/*.md` may pin a `model`. The user's decision is to
// HONOR it — not clamp it to a short alias list — so a repo can legitimately pin a
// full model id like `claude-opus-4-8`, not only `opus`. The only bound is the
// same shape check the API's `ValidateModel` (api/internal/agenttmpl/model.go)
// applies to uzi's own templates: a single non-empty token, at most MAX_MODEL_LEN
// bytes, free of control characters, the Unicode replacement char, and interior
// whitespace. A value failing that is ignored (the agent inherits the run
// default) — the API cannot enumerate valid ids without calling Anthropic, so a
// genuine typo surfaces as a run-time SDK error, not here; this only rejects a
// string that could never be a model id.
//
// This intentionally REPLACES the earlier alias-only clamp (and its drift test
// against web/ ModelSelect.tsx): there is no longer any coupling between the
// worker and the web picker's alias list.

/** Cap on a model token, in lockstep with the API's MaxModelLen. */
const MAX_MODEL_LEN = 100;

// A model token must not contain a control character (Cc — newline, CR, ESC), the
// Unicode replacement char (U+FFFD), or any whitespace (it is a single token).
// Mirrors the rune loop in the Go ValidateModel.
const MODEL_INVALID_CHAR_RE = /[\p{Cc}�\s]/u;

/** True when `value` is a well-formed model token (already trimmed by the caller).
 *  A blank value is NOT valid here — blank means "inherit", handled before this. */
export function isValidModel(value: string): boolean {
  if (value.length === 0 || value.length > MAX_MODEL_LEN) return false;
  return !MODEL_INVALID_CHAR_RE.test(value);
}
