import { describe, it, expect } from "vitest";
import { mockDoneMessages, mockFailedMessages } from "./data";
import { PLAN_RESULT_FRAME, RUN_RESULT_FRAME } from "./engine";

// PRD #311 / issue #195 realism guard for the RESULT frames the cost/usage
// surfaces read (run view usage strip, per-phase table, finish lines — all of
// which fold `modelUsage`, NOT the frame's top-level `usage`; see
// web/src/lib/runUsage.ts). A real result frame carries MULTIPLE model keys and
// the per-model `modelUsage` sums ABOVE the frame's top-level `usage`: the
// top-level UNDER-reads (measured 2.5x–229x low on run 84b6a933 seq 173,
// runUsage.ts:19-27), which is precisely why every surface reads `modelUsage`
// and never top-level. This test pins that shape so a fixture cannot silently
// drift back to a single-key `modelUsage` whose numbers equal the top-level
// `usage` — which is what made the mock unrealistic and hid the very divergence
// issue #195 fixed.
//
// SCOPE, deliberately narrow: this guards ONLY the five named divergence-consuming
// fixtures below. It does NOT assert ">1 model key" as a universal invariant of
// every result frame — legitimate single-model runs exist and are correct. It also
// does NOT cover `mockLiveTokensMessages` (the PRD #237 live-tokens trap fixture),
// which has its own guard in mockApi.lanes.test.ts.
//
// It asserts the INVARIANT (keys ≥ 2, and the per-model sum EXCEEDS the top-level
// usage in the documented direction), NOT the exact token numbers — so a future
// realistic renumber still passes, while both a regression to single-key/identical
// usage AND a renumber that flips the divergence to the wrong direction fail.

interface ModelUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens: number;
  cacheCreationInputTokens: number;
  costUSD: number;
}
interface TopUsage {
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
}
interface ResultFrame {
  event: string;
  usage: TopUsage;
  modelUsage: Record<string, ModelUsage>;
}

// exceedsTopLevel returns true iff the per-column sum over models of `modelUsage`
// is >= the top-level `usage` on EVERY token column AND strictly greater on at
// least one — i.e. the frame diverges in the documented real direction (modelUsage
// carries more than the under-reading top-level; runUsage.ts:19-27). This rejects
// three regressions at once: a single-key split equal to `usage` (no column
// exceeds), identical-usage (nothing strictly greater), and a flip to the wrong
// direction (a column where the sum falls below top-level). The all-zero
// cache_creation column is 0 >= 0 and carries the "strictly greater" via the three
// value columns.
function exceedsTopLevel(usage: TopUsage, modelUsage: Record<string, ModelUsage>): boolean {
  const sum = Object.values(modelUsage).reduce(
    (acc, m) => ({
      input: acc.input + m.inputTokens,
      output: acc.output + m.outputTokens,
      cacheRead: acc.cacheRead + m.cacheReadInputTokens,
      cacheCreation: acc.cacheCreation + m.cacheCreationInputTokens,
    }),
    { input: 0, output: 0, cacheRead: 0, cacheCreation: 0 },
  );
  const everyColumnAtLeast =
    sum.input >= usage.input_tokens &&
    sum.output >= usage.output_tokens &&
    sum.cacheRead >= usage.cache_read_input_tokens &&
    sum.cacheCreation >= usage.cache_creation_input_tokens;
  const someColumnStrictlyGreater =
    sum.input > usage.input_tokens ||
    sum.output > usage.output_tokens ||
    sum.cacheRead > usage.cache_read_input_tokens ||
    sum.cacheCreation > usage.cache_creation_input_tokens;
  return everyColumnAtLeast && someColumnStrictlyGreater;
}

// The exact set of 5 named result frames the cost/usage surfaces read: 2 from the
// Done stream, 1 from the Failed stream, plus the two engine consts.
const doneResults = mockDoneMessages
  .filter((m) => (m.payload as { event?: string }).event === "result")
  .map((m) => m.payload as ResultFrame);
const failedResults = mockFailedMessages
  .filter((m) => (m.payload as { event?: string }).event === "result")
  .map((m) => m.payload as ResultFrame);

const frames: Array<{ name: string; frame: ResultFrame }> = [
  ...doneResults.map((frame, i) => ({ name: `mockDoneMessages result #${i + 1}`, frame })),
  ...failedResults.map((frame, i) => ({ name: `mockFailedMessages result #${i + 1}`, frame })),
  { name: "PLAN_RESULT_FRAME", frame: PLAN_RESULT_FRAME as unknown as ResultFrame },
  { name: "RUN_RESULT_FRAME", frame: RUN_RESULT_FRAME as unknown as ResultFrame },
];

describe("PRD #311 result-frame realism guard (issue #195)", () => {
  it("collects exactly the 5 named divergence-consuming result frames", () => {
    expect(doneResults.length).toBe(2);
    expect(failedResults.length).toBe(1);
    expect(frames.length).toBe(5);
  });

  for (const { name, frame } of frames) {
    it(`${name}: spans ≥2 models and modelUsage exceeds top-level usage`, () => {
      // 1. The frame spans more than one model.
      expect(Object.keys(frame.modelUsage).length).toBeGreaterThanOrEqual(2);
      // 2. The per-model `modelUsage` sum exceeds the under-reading top-level `usage`
      //    (the documented issue #195 direction).
      expect(exceedsTopLevel(frame.usage, frame.modelUsage)).toBe(true);
    });
  }
});
