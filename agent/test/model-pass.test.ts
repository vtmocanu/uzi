import { describe, it } from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import { promises as fs } from "node:fs";

import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";

import { runReadOnlyModelPass, type ReadOnlyModelPassOpts } from "../src/model-pass.js";
import { classifyLimitFailure, LimitReachedError, type RateLimitObservation } from "../src/limit.js";
import { mapSdkMessage } from "../src/sdk-messages.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

// A queryFn that records the `options` object the helper built, then yields a scripted
// stream. The recorded options are what the isolation-shape pin below asserts against.
function capturingQueryFn(frames: unknown[]): { queryFn: SdkQueryFn; seen: { options?: SdkOptions } } {
  const seen: { options?: SdkOptions } = {};
  const queryFn = ((params: { options: SdkOptions }) => {
    seen.options = params.options;
    return (async function* () {
      for (const f of frames) yield f;
    })();
  }) as unknown as SdkQueryFn;
  return { queryFn, seen };
}

// A minimal, hermetic set of base opts under os.tmpdir() with a tiny timeout. The helper
// mkdtemps under homeRoot and cleans it up in its finally, so no fixture teardown needed.
function baseOpts(overrides: Partial<ReadOnlyModelPassOpts> = {}): ReadOnlyModelPassOpts {
  return {
    token: "tok",
    systemPrompt: "sys",
    prompt: "hello",
    homeRoot: os.tmpdir(),
    homePrefix: "uzi-modelpass-test-",
    label: "review",
    timeoutMs: 5000,
    queryFn: capturingQueryFn([]).queryFn,
    denyReason: "the reviewer is read-only and runs no tools",
    log: nullLogger(),
    ...overrides,
  };
}

const assistantText = (text: string) => ({
  type: "assistant",
  message: { role: "assistant", content: [{ type: "text", text }] },
});
const successResult = () => ({ type: "result", subtype: "success", is_error: false });
const errorResult = () => ({ type: "result", subtype: "error_during_execution", is_error: true });
const rejectedRateLimit = (resetsAtMs: number) => ({
  type: "rate_limit_event",
  rate_limit_info: { status: "rejected", resetsAt: resetsAtMs, rateLimitType: "five_hour" },
  uuid: "u",
  session_id: "s",
});

describe("runReadOnlyModelPass — isolation-shape characterization pin", () => {
  it("builds the exact read-only isolation options and returns the accumulated text", async () => {
    const { queryFn, seen } = capturingQueryFn([assistantText("hi "), assistantText("there"), successResult()]);
    const text = await runReadOnlyModelPass(baseOpts({ queryFn }));

    const options = seen.options!;
    // The load-bearing isolation shape. `settingSources: []` is the single point of
    // truth; the deny-all hook makes the pass tool-less even under bypassPermissions.
    assert.deepEqual(options.settingSources, [], "settingSources must be the empty array literal");
    assert.equal(options.permissionMode, "bypassPermissions");
    assert.equal(options.allowDangerouslySkipPermissions, true);
    assert.equal(options.includePartialMessages, false);

    const hook = options.hooks?.PreToolUse?.[0]?.hooks?.[0];
    assert.equal(typeof hook, "function", "a PreToolUse hook function is installed");
    const out = await hook!({} as never, undefined, {} as never);
    assert.equal(
      (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
      "deny",
      "the installed hook denies every tool call",
    );

    assert.equal(text, "hi there", "the helper returns the accumulated assistant text");
  });

  it("sets options.model only when a non-empty model is passed", async () => {
    const withModel = capturingQueryFn([successResult()]);
    await runReadOnlyModelPass(baseOpts({ queryFn: withModel.queryFn, model: "claude-haiku" }));
    assert.equal(withModel.seen.options!.model, "claude-haiku");

    const noModel = capturingQueryFn([successResult()]);
    await runReadOnlyModelPass(baseOpts({ queryFn: noModel.queryFn, model: "" }));
    assert.ok(!("model" in noModel.seen.options!), "an empty model is not set on the options");
  });
});

describe("runReadOnlyModelPass — onResult contract (differential pin)", () => {
  // A rejected rate-limit event followed by a terminal ERROR result. A future reset is
  // required for classifyLimitFailure to classify the `rejected` observation.
  const streamFrames = () => [rejectedRateLimit(Date.now() + 5 * 60 * 60 * 1000), errorResult()];

  it("WITHOUT onResult throws a generic labeled Error (the review/summary path)", async () => {
    const { queryFn } = capturingQueryFn(streamFrames());
    await assert.rejects(
      runReadOnlyModelPass(baseOpts({ queryFn, label: "review" })),
      (err: unknown) => {
        assert.ok(err instanceof Error);
        assert.ok(!(err instanceof LimitReachedError), "the default path does NOT gain LimitReachedError behavior");
        assert.equal((err as Error).message, "review model call returned an error result");
        return true;
      },
    );
  });

  it("WITH the judge's onResult closure throws LimitReachedError on a usage-limit death", async () => {
    const { queryFn } = capturingQueryFn(streamFrames());
    // The judge's exact onResult body (see judge-runner.ts runModel).
    const onResult: ReadOnlyModelPassOpts["onResult"] = (msg, { isError, latest }) => {
      if (isError) {
        const limit = classifyLimitFailure(msg, latest as RateLimitObservation | undefined, Date.now());
        if (limit) throw new LimitReachedError(limit);
        throw new Error("judge model call returned an error result");
      }
      // success path: capture the terminal frame (unused in this error-path test).
      void mapSdkMessage(msg)[0];
    };
    await assert.rejects(
      runReadOnlyModelPass(baseOpts({ queryFn, label: "judge", onResult })),
      (err: unknown) => {
        assert.ok(err instanceof LimitReachedError, "the judge closure escalates a usage-limit death");
        return true;
      },
    );
  });
});

describe("runReadOnlyModelPass — HOME lifecycle", () => {
  it("removes the ephemeral HOME dir after the pass", async () => {
    // The pass mkdtemps under homeRoot with the given prefix, so after it returns no dir
    // with that (unique) prefix should remain — the finally cleanup ran.
    const { queryFn } = capturingQueryFn([successResult()]);
    await runReadOnlyModelPass(baseOpts({ queryFn, homePrefix: "uzi-modelpass-cleanup-" }));
    const after = await fs.readdir(os.tmpdir());
    const stranded = after.find((d) => d.startsWith("uzi-modelpass-cleanup-"));
    assert.equal(stranded, undefined, "the ephemeral HOME dir is cleaned up");
  });
});
