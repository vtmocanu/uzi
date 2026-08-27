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

// anthropicTokenCount is the ">1 token" gate for the Runs-list credential badge
// (PRD #295): a single-token user's every run billed the one token, so the badge
// says nothing and is hidden. It lives here beside `hasAnthropicToken` for the same
// reason that predicate does — one exported, tested function keeps the threshold in
// a single place rather than inlining `.filter(...).length > 1` at the call site,
// where narrowing the kind check would leave the caller's test green while the gate
// counted the wrong thing.
export function anthropicTokenCount(secrets: SecretMeta[]): number {
  return secrets.filter((s) => s.kind === "anthropic_token").length;
}
