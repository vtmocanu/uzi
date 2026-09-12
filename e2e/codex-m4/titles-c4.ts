// PRD #1287 C4 — the exact executed-test TITLE constants shared by the three places that must
// never drift: the `it(...)` name, the `recordEvidence(...)` call, and the clause row's `tests`
// list (clauses-codex.ts). A single source of truth keeps the completeness checker's
// zero-test-match rule honest — a renamed test cannot silently stop matching its clause.
//
// Pure constants (no imports, no side effects) so both the data contribution file
// (clauses-codex.ts) and the C4 test files can import it without pulling in node built-ins or
// the node:test runner. All C4 executable cases are D7 layer U (real registry/safety/advice/
// broker/config primitives driven with injected seams; no live app-server).

// ── Delegation and cleanup (lifecycle) — real ExecutionRegistry + CodexExecutionSafety ──
export const CODEX_U_LIFECYCLE_SYNC_CHILD_TITLE =
  "codex U lifecycle: a parent result boundary cannot settle while a delegated child callback is in flight (sink uncalled + poison), and settles once the child does";
export const CODEX_U_LIFECYCLE_HELD_CALLBACK_TITLE =
  "codex U lifecycle: a held callback keeps the boundary quiesce pending (sink uncalled) and drains clean once settled, but a never-drained hold poisons at the deadline";
export const CODEX_U_LIFECYCLE_LATE_CALLBACK_TITLE =
  "codex U lifecycle: after the parent turn quiesced, a late callback replay returns the cached terminal and a new callback is denied admission_closed, with no second command spawn";
export const CODEX_U_LIFECYCLE_ROOT_ORDERING_TITLE =
  "codex U lifecycle: a worker_pat boundary action stays refused until EVERY command root reaps (the git child spawns only after the last one), with a provider root also present";
export const CODEX_U_LIFECYCLE_TIMEOUT_UNCONFIRMED_TITLE =
  "codex U lifecycle: a timed-out boundary leaves the durability sink uncalled and its poison is sticky, so a later clean-seam boundary still cannot fire the sink";

// ── Advice ceiling — real CodexAdviceHarness (fake LaunchAdviceRootSeam) + real appserver-auth ──
export const CODEX_U_ADVICE_CEILING_TITLE =
  "codex U advice ceiling: renderCodexAdvice throws for tools/toolServers/cwd and the harness launches nothing for a cwd-carrying request, while a clean advice pass returns its text";
export const CODEX_U_ADVICE_DISPOSE_RACE_TITLE =
  "codex U advice cleanup: disposeOnce runs EXACTLY once when an external abort races an already-in-flight HOME disposal";
export const CODEX_U_ADVICE_APIKEY_ZERO_REFRESH_TITLE =
  "codex U advice api_key: a clean api_key advice pass returns its text with login type apiKey and ZERO refresh on the wire, while a subscription refresh bridge is the detectable positive control";
export const CODEX_U_ADVICE_APIKEY_NO_FALLBACK_TITLE =
  "codex U advice api_key: a subscription-refresh server request during an api_key advice pass is refused fail-closed with an error and no token is minted (no fallback)";

// ── Failure closure — real config builders + real broker/handler ──
export const CODEX_U_FAILURE_HOOKS_DISABLED_TITLE =
  "codex U failure closure: the production + loopback config builders disable hooks and every native execution feature (no hooks=true, no bypass-hook-trust), so an upstream hook failure can never become production permission";
export const CODEX_U_FAILURE_BROKER_CLOSURE_TITLE =
  "codex U failure closure: a throwing/unwired worker tool handler and a rejecting command spawn fail closed (handler_error/denied_tool/broker_error) with no fabricated success, while a well-behaved handler still succeeds";
