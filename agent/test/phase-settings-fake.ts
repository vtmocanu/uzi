import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";
import { CLAUDE_CANONICAL_MODELS, CLAUDE_OMITTED_EFFORT } from "../src/claude-canonical-models.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";

/** What a runtime that honoured the request would report from `getSettings` for these options. */
export function honouredSettings(options: SdkOptions): { model?: string; effort?: string } {
  const requested = options.model;
  const model = requested === undefined ? undefined : (CLAUDE_CANONICAL_MODELS[requested] ?? requested);
  const effort = options.effort ?? (model !== undefined ? CLAUDE_OMITTED_EFFORT[model] : undefined);
  return { ...(model !== undefined ? { model } : {}), ...(effort !== undefined ? { effort } : {}) };
}

/**
 * Give every query a `getSettings()` control method, as the pinned SDK has. `answer` decides what the
 * runtime reports for a turn's options (default: it honoured them); returning "reject" makes the call
 * throw, "none" leaves the method off. `calls` counts the invocations.
 */
export function withAppliedSettings(
  queryFn: SdkQueryFn,
  answer: (options: SdkOptions, call: number) => { model?: string; effort?: string } | "reject" | "none" = honouredSettings,
  calls: { count: number } = { count: 0 },
): SdkQueryFn {
  return (params) => {
    const q = queryFn(params);
    const verdict = answer(params.options, calls.count);
    if (verdict === "none") return q;
    (q as unknown as Record<string, unknown>)["getSettings"] = async () => {
      calls.count++;
      if (verdict === "reject") throw new Error("settings unavailable");
      return { applied: verdict };
    };
    return q;
  };
}
