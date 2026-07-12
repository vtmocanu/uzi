// The UZI_E2E_EXECUTOR=stub judge model call (PRD #46 M8/B). It mirrors the
// StubExecutor (run lane) and StubChatExecutor (chat lane) seams: a test-only
// substitute for the live Anthropic call so the e2e can drive the judge lane end to
// end with a DUMMY token and ZERO spend.
//
// It makes NO network call and yields a terminal ERROR result, so JudgeRunner
// deterministically falls back to the command-not-found review (Decision 4) — which
// is exactly the "forced model failure → deterministic fallback" the judge e2e
// asserts (a planted `command not found` in the trace surfaces as an
// install_worker_tool recommendation even though the model never ran).
import type { SdkQueryFn } from "./sdk-executor.js";

export const stubJudgeQueryFn: SdkQueryFn = (async function* () {
  yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: "[stub judge] no model call in e2e" }] } };
  yield { type: "result", subtype: "error_stub", is_error: true };
} as unknown as SdkQueryFn);
