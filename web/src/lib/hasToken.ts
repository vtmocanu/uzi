// The one "does this user have an Anthropic token?" gate, shared by Dashboard,
// Board and IssueView (PRD #104 M6).
//
// It lives here rather than inlined at the three call sites for a reason the
// review made concrete: `hasToken.test.ts` used to declare its OWN copy of the
// predicate, so it asserted a copy and not the thing that shipped — narrowing any
// of the three inlined expressions to `is_default` would have left the test green
// while the board gated on the wrong thing. One exported function is what makes
// that test hold a real line.
//
// ANY-ROW semantics are the contract: a user whose tokens are all non-default
// still has a token. D6 makes that state unreachable through the UI today, which
// is exactly why the rule has to be pinned by a test rather than by the runtime.

import type { SecretMeta } from "./api";

export function hasAnthropicToken(secrets: SecretMeta[]): boolean {
  return secrets.some((s) => s.kind === "anthropic_token");
}
