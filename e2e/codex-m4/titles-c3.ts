// Owner accepted 2026-11-20: native patch is inert without an execution environment; MCP helpers require
// no native MCP servers are configured. New server configuration requires fresh authority review.
// PRD #1287 C3 — the exact executed-test TITLE constants shared by the three places that must
// never drift: the `it(...)` name, the `recordEvidence(...)` call, and the clause row's `tests`
// list (clauses-codex.ts). A single source of truth keeps the completeness checker's
// zero-test-match rule honest — a renamed test cannot silently stop matching its clause.
//
// Pure constants (no imports, no side effects) so both the data contribution file
// (clauses-codex.ts) and the test files can import it without pulling in node built-ins or the
// node:test runner.

// ── P layer (real app-server) ──────────────────────────────────────────────────
const CODEX_P_NATIVE_ABSENT_TITLE =
  "codex P native-bypass: forced native shell/exec_command/unified_exec/write_stdin/local_shell and native apply_patch produce no worker callback and no native side effect on the real protocol, while the intended worker exec runs";
export const CODEX_P_ISOLATION_ENV_TITLE =
  "codex P isolation: the real app-server spawns under the sparse replaced env with no provider credential or worker token";
const CODEX_P_SHELL_POLICY_TITLE =
  "codex P shell policy: git push is denied on the real protocol with a zero command-spawn count while a harmless command reaches its effect";
const CODEX_P_FILE_POLICY_TITLE =
  "codex P file policy: an outside-worktree credential path is denied before the fileop effect on the real protocol while an in-worktree op works";

// Explicit serial conformance controls; independent of runtime model selection.
export const NATIVE_BYPASS_MODELS = ["gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra"] as const;
export const POLICY_MODELS = ["gpt-6-astra", "gpt-6.2-sol", "gpt-6.2-astra"] as const;

export const CODEX_P_NATIVE_ABSENT_TITLES: Readonly<Record<string, string>> = Object.fromEntries(
  NATIVE_BYPASS_MODELS.map(model => [model, `${CODEX_P_NATIVE_ABSENT_TITLE}: ${model}`]));
export const CODEX_P_SHELL_POLICY_TITLES: Readonly<Record<string, string>> = Object.fromEntries(
  POLICY_MODELS.map(model => [model, `${CODEX_P_SHELL_POLICY_TITLE}: ${model}`]));
export const CODEX_P_FILE_POLICY_TITLES: Readonly<Record<string, string>> = Object.fromEntries(
  POLICY_MODELS.map(model => [model, `${CODEX_P_FILE_POLICY_TITLE}: ${model}`]));

// ── U layer (real broker / registry / render / guardrails, direct) ───────────────
export const CODEX_U_NATIVE_DISPATCH_TITLE =
  "codex U native-bypass: forced native tool names (shell/exec_command/unified_exec/write_stdin/local_shell/native apply_patch) are denied by the real broker with no effect";
export const CODEX_U_SHELL_VARIANTS_TITLE =
  "codex U shell policy: plaintext git/env/proc/wrapper-depth variants reach the real screener and are denied with no command spawn or secret disclosure, while the base64 pipe is an honest OS-command-root-contained residual (not denied by the screener)";
export const CODEX_U_FILE_VARIANTS_TITLE =
  "codex U file policy: .git writes, symlink/canonicalization escapes, and malformed patches are denied before the fileop effect while an allowed write works";
export const CODEX_U_PHASE_GRANTS_TITLE =
  "codex U roles/phases: a plan-phase write is denied then a later implement-phase write is allowed by the immutable grants";
export const CODEX_U_ROLE_FAILCLOSED_TITLE =
  "codex U roles/phases: an unknown tool, a spoofed/unknown delegation role, and a disallowed skill fail closed while allocated actions run";
export const CODEX_U_SIGNAL_ORIGIN_TITLE =
  "codex U workflow signals: child and unknown-origin submit_plan/signal_done are denied with no reducer transition while a valid root signal latches once";
export const CODEX_U_SIGNAL_REPLAY_TITLE =
  "codex U workflow signals: a replayed callback identity returns the cached terminal and a changed-payload reuse poisons, with no second effect";
export const CODEX_U_TRUST_CONSTRUCTION_TITLE =
  "codex U trust: production start/resume/turn construction pins the project untrusted with project_doc_max_bytes=0 and fixed suppression settings, taking no repo AGENTS.md/.codex input";

/** One evidence title per serial model/scenario case, also consumed by strict completeness. */
export const NATIVE_PATCH_INERT_TITLES: Readonly<Record<string, string>> = Object.fromEntries(
  ["gpt-5.6-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra"].flatMap(model =>
    ["fresh", "cold", "cold-no-explicit-selection", "fresh-no-launcher-guard"].map(scenario =>
      [`${model}/${scenario}`, `codex P native patch inert: ${model}/${scenario}`])),
);

export const CODEX_P_NO_TTL_TITLES = {
  "gpt-6.2-sol": "no TTL overrides: actual pinned binary and provider request for gpt-6.2-sol",
  "gpt-6.2-astra": "no TTL overrides: actual pinned binary and provider request for gpt-6.2-astra",
} as const;
