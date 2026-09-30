// The UZI_E2E_EXECUTOR=stub job model call (PRD #1908 M4). It mirrors judge-runner-stub.ts: a
// test-only substitute for the live Anthropic call so the e2e can drive the job lane end to end
// with a DUMMY token and ZERO spend. It makes NO network call and yields a terminal ERROR result,
// so JobRunner deterministically reports the job failed with a session-error reason (the stub
// never submits a result, so a stubbed job never completes).
import type { SdkQueryFn } from "./sdk-executor.js";

export const stubJobQueryFn: SdkQueryFn = (async function* () {
  yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: "[stub job] no model call in e2e" }] } };
  yield { type: "result", subtype: "error_stub", is_error: true };
} as unknown as SdkQueryFn);
