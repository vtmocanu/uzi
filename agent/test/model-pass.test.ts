import { describe, it } from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import { promises as fs } from "node:fs";
import { existsSync } from "node:fs";

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

// ── issue #933: HOME cleanup must not race the aborted SDK CLI on the timeout path ──
//
// runReadOnlyModelPass races the query against a wall-clock timeout that calls
// `abort.abort()`; the SDK terminates the CLI asynchronously, so cleanup must wait for the
// query to settle (bounded by a grace) before removing the ephemeral HOME — otherwise it
// can rm $HOME while the aborted CLI is still exiting and may still touch it. These
// fixtures drive an aborting/gated query and read the ephemeral HOME from
// `options.env.HOME` (buildSdkEnv pins HOME = homeDir). Assertions are timing-free (no
// Date.now()/sleep comparisons); node's --test-timeout is the only backstop for a hang.

interface FixtureOptions {
  env?: Record<string, string | undefined>;
  abortController?: AbortController;
}

/** Resolve once the query's abort signal has fired (or is already aborted). */
function whenAborted(signal: AbortSignal | undefined): Promise<void> {
  return new Promise<void>((resolve) => {
    if (!signal || signal.aborted) {
      resolve();
      return;
    }
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

describe("runReadOnlyModelPass — HOME cleanup vs the aborted CLI (issue #933)", () => {
  it("timeout path: aborts the query, rejects, and removes HOME only after the query settles", async () => {
    let capturedHome: string | undefined;
    let homeExistedAtSettle: boolean | undefined;

    // Never yields a terminal result, so the wall-clock timeout wins the race. On abort
    // (the exiting CLI) it records whether HOME still exists, then ends the iteration so
    // the consume() promise settles.
    const queryFn: SdkQueryFn = (params) => {
      const fixtureOpts = (params as { options: FixtureOptions }).options;
      capturedHome = fixtureOpts.env?.HOME;
      const signal = fixtureOpts.abortController?.signal;
      // A hung/aborting query is the fixture: it never yields a terminal result (mirrors
      // judge-runner.test's hung fixture), so the wall-clock timeout fires.
      // eslint-disable-next-line require-yield
      return (async function* () {
        await whenAborted(signal);
        homeExistedAtSettle = capturedHome ? existsSync(capturedHome) : undefined;
      })() as never;
    };

    await assert.rejects(
      runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 5, graceMs: 1000 })),
      /review model call exceeded 5ms/,
    );
    assert.equal(homeExistedAtSettle, true, "HOME must still exist when the aborted query settles");
    assert.ok(capturedHome, "the query must have received an ephemeral HOME");
    assert.equal(existsSync(capturedHome!), false, "HOME must be removed after the query settles");
  });

  it("timeout path: the pass does not settle (HOME is not cleaned) until the query settles", async () => {
    let openGate!: () => void;
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    let sawAbort!: () => void;
    const abortSeen = new Promise<void>((resolve) => {
      sawAbort = resolve;
    });

    // After abort the query does NOT settle on its own — it blocks on a test-controlled
    // gate, modelling the aborted CLI still exiting. With the fix, cleanup (and thus the
    // returned promise) is deferred until the query settles; without it, the finally
    // removes HOME and the promise rejects at the timeout, gate still closed.
    const queryFn: SdkQueryFn = (params) => {
      const signal = (params as { options: FixtureOptions }).options.abortController?.signal;
      // eslint-disable-next-line require-yield
      return (async function* () {
        await whenAborted(signal);
        sawAbort();
        await gate;
      })() as never;
    };

    // graceMs is an hour: the grace must not be what unblocks cleanup here — only the
    // query settling (via the gate) may.
    const pass = runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 5, graceMs: 3_600_000 }));
    let settled = false;
    pass.then(
      () => {
        settled = true;
      },
      () => {
        settled = true;
      },
    );

    await abortSeen;
    // Drain the event loop so that if cleanup were NOT deferred, the finally would have
    // removed HOME and settled the promise by now (no wall-clock wait — just event-loop
    // yields). With the fix the query is still gated, so the pass stays pending here no
    // matter how many turns elapse.
    for (let i = 0; i < 20; i++) await new Promise((resolve) => setImmediate(resolve));
    assert.equal(settled, false, "the pass must not settle until the query settles");

    openGate();
    await assert.rejects(pass, /review model call exceeded 5ms/);
  });

  // Grace-expiry branch, the complement of the preceding "does not settle until the query
  // settles" case: here the query stays pending forever after abort, so the bounded grace
  // (not the query settling) is what unblocks cleanup — HOME is removed once the small
  // graceMs elapses, and the authoritative timeout error still propagates.
  it("timeout path: the grace expiry (not the query settling) unblocks HOME cleanup when the query never settles", async () => {
    let capturedHome: string | undefined;

    // Never settles after abort: models an aborted CLI that hangs. The finally's
    // awaitQuerySettled must fall back to the grace timer to proceed with cleanup.
    const queryFn: SdkQueryFn = (params) => {
      const fixtureOpts = (params as { options: FixtureOptions }).options;
      capturedHome = fixtureOpts.env?.HOME;
      const signal = fixtureOpts.abortController?.signal;
      // eslint-disable-next-line require-yield
      return (async function* () {
        await whenAborted(signal);
        await new Promise<void>(() => {});
      })() as never;
    };

    await assert.rejects(
      runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 5, graceMs: 20 })),
      /review model call exceeded 5ms/,
    );
    assert.ok(capturedHome, "the query must have received an ephemeral HOME");
    assert.equal(
      existsSync(capturedHome!),
      false,
      "HOME must be removed after the grace expires even though the query never settled",
    );
  });

  it("timeout path: a query that rejects after abort is swallowed — the run still cleans HOME", async () => {
    let capturedHome: string | undefined;
    let homeExistedAtSettle: boolean | undefined;

    // On abort the query records, then rejects. The timeout error (not the query error)
    // must propagate, cleanup must still run, and the rejection must not surface as an
    // unhandled rejection (which node's test runner would fail on).
    const queryFn: SdkQueryFn = (params) => {
      const fixtureOpts = (params as { options: FixtureOptions }).options;
      capturedHome = fixtureOpts.env?.HOME;
      const signal = fixtureOpts.abortController?.signal;
      // eslint-disable-next-line require-yield
      return (async function* () {
        await whenAborted(signal);
        homeExistedAtSettle = capturedHome ? existsSync(capturedHome) : undefined;
        throw new Error("aborted query boom");
      })() as never;
    };

    await assert.rejects(
      runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 5, graceMs: 1000 })),
      /review model call exceeded 5ms/,
    );
    assert.equal(homeExistedAtSettle, true, "HOME must still exist when the aborted query settles");
    assert.ok(capturedHome);
    assert.equal(existsSync(capturedHome!), false, "HOME must be removed after the query settles");
  });

  it("success path: does not wait for the grace once the query has settled", async () => {
    // Settles immediately with text. graceMs is set to an hour: if cleanup wrongly waited
    // on the grace instead of the settled query, the test would hang past --test-timeout.
    const { queryFn, seen } = capturingQueryFn([assistantText("hello"), successResult()]);
    const out = await runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 60_000, graceMs: 3_600_000 }));
    assert.equal(out, "hello");
    const home = (seen.options as FixtureOptions | undefined)?.env?.HOME;
    assert.ok(home, "the query must have received an ephemeral HOME");
    assert.equal(existsSync(home!), false, "HOME must be cleaned without waiting for the grace");
  });
});
