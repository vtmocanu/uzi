import { describe, expect, it } from "vitest";
import { hasRecordedLifetimeUsage } from "./usageRecorded";
import type { RunUsage } from "./apiTypes";

const zero = (): RunUsage => ({
  input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0, cost_usd: 0, cost_status: "",
});

describe("hasRecordedLifetimeUsage", () => {
  const cases: [string, RunUsage, number, number, number, boolean][] = [
    ["all zero", zero(), 0, 0, 0, false],
    ["only input tokens", { ...zero(), input_tokens: 1 }, 0, 0, 0, true],
    ["only cache read tokens", { ...zero(), cache_read_tokens: 1 }, 0, 0, 0, true],
    ["only cache creation tokens", { ...zero(), cache_creation_tokens: 1 }, 0, 0, 0, true],
    ["only output tokens", { ...zero(), output_tokens: 1 }, 0, 0, 0, true],
    ["only cost", { ...zero(), cost_usd: 0.5 }, 0, 0, 0, true],
    ["only run count", zero(), 1, 0, 0, true],
    ["only subscription count", zero(), 0, 1, 0, true],
    ["only unreported count", zero(), 0, 0, 1, true],
  ];
  it.each(cases)("%s", (_name, usage, runs, sub, unreported, want) => {
    expect(hasRecordedLifetimeUsage(usage, runs, sub, unreported)).toBe(want);
  });
});
