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

export function codexCredentialCount(secrets: SecretMeta[]): number {
  return secrets.filter((s) => s.kind === "codex_auth" || s.kind === "openai_api_key").length;
}

// isCodexUsable mirrors the server's D11 Codex-availability rule EXACTLY (PRD #1429
// D3, api/internal/workersvc/harness_resolver.go resolveUsableCodexCredential): a
// subscription (codex_auth) default is usable ONLY when linked (codex_status ===
// "linked"); an api_key (openai_api_key) default is usable by mere existence; a
// failed/missing/unlinked subscription default NEVER falls through to a non-default
// api key — only the DEFAULT row of either kind is consulted. The web only uses this
// to hide/explain controls (D3): the server is authoritative regardless.
export function isCodexUsable(secrets: SecretMeta[]): boolean {
  const sub = secrets.find((s) => s.kind === "codex_auth" && s.is_default);
  if (sub) return sub.codex_status === "linked";
  return secrets.some((s) => s.kind === "openai_api_key" && s.is_default);
}

// hasAnyCodexCredential is broader than isCodexUsable: any Codex-kind secret at all,
// usable or not (e.g. an unlinked subscription still pending its provider auth). Used
// only to pick harness-appropriate copy when NEITHER harness is currently usable (PRD
// #1429 D3/M4a) — a user who has started down the Codex path should not be told to
// "Add your Anthropic token" instead.
export function hasAnyCodexCredential(secrets: SecretMeta[]): boolean {
  return secrets.some((s) => s.kind === "codex_auth" || s.kind === "openai_api_key");
}

// hasUsableCredential is "a usable credential for AT LEAST ONE harness" — the onboarding
// checklist's DONE condition (PRD #1429 M4a, Dashboard): a Codex-only user has already
// completed the "connect a credential" step even with zero Anthropic tokens.
export function hasUsableCredential(secrets: SecretMeta[]): boolean {
  return hasAnthropicToken(secrets) || isCodexUsable(secrets);
}
