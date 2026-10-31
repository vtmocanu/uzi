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
//
// PRD #1732 D15: "has a token" means an ENABLED one. A stored-but-disabled token can run
// nothing, so a user whose tokens are all disabled reads exactly like a user with none
// (the server's harness resolution and presence gates say the same). `enabled` absent is
// enabled: a pre-#1732 api pod omits the key.

import type { SecretMeta } from "./api";

export function hasAnthropicToken(secrets: SecretMeta[]): boolean {
  return secrets.some((s) => s.kind === "anthropic_token" && s.enabled !== false);
}

// Per-harness counters include disabled credentials for historical account display.
// The shared showsRunAccounts helper applies the list's total-count threshold.
export function anthropicTokenCount(secrets: SecretMeta[]): number {
  return secrets.filter((s) => s.kind === "anthropic_token").length;
}

export function codexCredentialCount(secrets: SecretMeta[]): number {
  return secrets.filter((s) => s.kind === "codex_auth" || s.kind === "openai_api_key").length;
}

// Personal lists show accounts once the viewer holds multiple credentials,
// including one of each harness. Admin lists always show accounts.
export function showsRunAccounts(counts: { claude: number; codex: number }): boolean {
  return counts.claude + counts.codex > 1;
}

// isCodexUsable approximates the server's Codex-availability rule from the facts the
// client holds (PRD #1429 D3, api/internal/workersvc/harness_resolver.go
// resolveUsableCodexCredential; issue #3719 for the pool). The SERVER IS AUTHORITATIVE:
// quarantine and re-login-required live only server-side, so a pooled login this reads as
// usable can still be refused. The web uses this only to hide or explain controls (D3).
//
// Two rules, picked by the user's codex_account_mode:
//   - POOL RULE: mode "auto" and at least one codex_auth login pooled (auto_eligible,
//     disabled ones included). Codex is usable iff some pooled login is enabled and linked.
//     The default is NOT consulted: with pooled logins and none eligible the server falls
//     an implicit run back to Claude and refuses an explicit Codex request.
//   - DEFAULT RULE (mode "default", mode unknown, or auto with nothing pooled): a
//     subscription (codex_auth) default is usable ONLY when linked; an api_key
//     (openai_api_key) default is usable by mere existence; a failed/missing/unlinked
//     subscription default NEVER falls through to a non-default api key — only the
//     DEFAULT row of either kind is consulted.
//
// `mode` is optional: a caller that has not loaded the user's settings passes undefined
// and gets the default rule, which is today's behaviour and the server's own answer for
// every user who never chose "auto".
export function isCodexUsable(secrets: SecretMeta[], mode?: string | null): boolean {
  if (mode === "auto") {
    const pooled = secrets.filter((s) => s.kind === "codex_auth" && s.auto_eligible);
    if (pooled.length > 0) {
      return pooled.some((s) => s.enabled !== false && s.codex_status === "linked");
    }
  }
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
export function hasUsableCredential(secrets: SecretMeta[], codexMode?: string | null): boolean {
  return hasAnthropicToken(secrets) || isCodexUsable(secrets, codexMode);
}
