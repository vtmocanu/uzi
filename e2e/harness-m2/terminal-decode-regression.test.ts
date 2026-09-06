// M2 terminal-decode D0 regression controls (PRD #1146, #1106 M2).
//
// Two zero-behavior-change (D0) regressions a maintainer flagged in the terminal
// decode path, each a `String(...)` coercion that threw `TypeError: Cannot convert
// object to primitive value` on a malformed frame field BEFORE the intended outcome
// was produced. These pure, credential-free in-process tests pin the fixed behavior
// AND the base behavior that must stay unchanged. No SDK query, no real tokens, no
// network; the one token-shaped value is dummy data built at runtime.
//
//   1. decodeResult (sdk-messages.ts): `.map(String)` over `errors` used to run for
//      ALL outcomes, so a SUCCESS frame carrying a malformed `errors` element threw
//      before terminal accounting (usage/modelUsage/duration/cost) was emitted. Fixed
//      to gate `.map(String)` on the failed-outcome path only — errors are only
//      meaningful on failure. The failed path still String-maps (and still throws on
//      a malformed failed-result element), matching base.
//   2. neutralTerminal (model-pass.ts): `String(subtype)` used to throw on a non-string
//      subtype, pre-empting the intended default policy error. Fixed to a string-or-
//      "unknown" projection that never throws.

import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";

import { decodeResult } from "../../agent/src/sdk-messages.js";
import { runReadOnlyModelPass, type ReadOnlyModelPassOpts } from "../../agent/src/model-pass.js";
import type { SdkQueryFn } from "../../agent/src/sdk-executor.js";
import type { Logger } from "../../agent/src/log.js";

/** A no-op logger so the pass does not spray JSON lines into the reporter. */
function nullLogger(): Logger {
  const self: Logger = {
    debug() {},
    info() {},
    warn() {},
    error() {},
    addSecret() {},
    removeSecret() {},
    child() {
      return self;
    },
  };
  return self;
}

// -- Fix 1: decodeResult ----------------------------------------------------

test("decodeResult: success frame with a malformed errors element does NOT throw and preserves accounting", () => {
  // Pre-fix this threw `TypeError: Cannot convert object to primitive value` while
  // running `.map(String)` over the errors array, BEFORE any accounting was emitted —
  // a regression vs the base mapper, which emitted the terminal status with accounting.
  const decoded = decodeResult({
    type: "result",
    subtype: "success",
    errors: [{ toString: null }],
    usage: { input_tokens: 23 },
    modelUsage: { example: { inputTokens: 23 } },
    duration_ms: 5,
    total_cost_usd: 0.01,
    num_turns: 1,
  });
  assert.equal(decoded.outcome, "success");
  // errors is ignored on the success path — never String-mapped, so no throw.
  assert.deepStrictEqual(decoded.errors, []);
  // The terminal accounting the fix restores is forwarded intact.
  assert.deepStrictEqual(decoded.wire, {
    usage: { input_tokens: 23 },
    modelUsage: { example: { inputTokens: 23 } },
    num_turns: 1,
    duration_ms: 5,
    total_cost_usd: 0.01,
  });
});

test("decodeResult: failed frame with a malformed errors element STILL throws (base behavior preserved)", () => {
  // Unchanged from base: the failed path still String-maps `errors`, so a malformed
  // element still throws the same TypeError. The fix narrows the coercion to the
  // failed path only; it does not change the failed-result behavior.
  assert.throws(
    () =>
      decodeResult({
        type: "result",
        subtype: "error_max_turns",
        is_error: true,
        errors: [{ toString: null }],
      }),
    TypeError,
  );
});

test("decodeResult: success frame with normal string errors maps cleanly (sanity control)", () => {
  const decoded = decodeResult({
    type: "result",
    subtype: "success",
    is_error: false,
    errors: ["boom"],
    usage: { input_tokens: 2 },
    modelUsage: { example: { inputTokens: 2 } },
    duration_ms: 7,
    total_cost_usd: 0.02,
    num_turns: 4,
  });
  assert.equal(decoded.outcome, "success");
  // errors is [] on success even when the frame carries a well-formed errors array.
  assert.deepStrictEqual(decoded.errors, []);
});

// -- Fix 2: model-pass neutralTerminal --------------------------------------

test("runReadOnlyModelPass: a non-string subtype terminal frame surfaces the default policy error, not a TypeError", async () => {
  // The advice review lane (no onResult) drives the default neutral policy: an error
  // terminal throws `review model call returned an error result`. Pre-fix, neutralTerminal's
  // `String(subtype)` threw `TypeError: Cannot convert object to primitive value` on this
  // non-string subtype BEFORE the policy could throw its intended error. A frame with a
  // non-"success" subtype is an error result (isErrorResult), so the review lane rejects.
  const queryFn = (() =>
    (async function* () {
      yield { type: "result", subtype: { toString: null } };
    })()) as unknown as SdkQueryFn;

  const opts: ReadOnlyModelPassOpts = {
    token: "dummy-tok",
    systemPrompt: "sys",
    prompt: "hello",
    homeRoot: os.tmpdir(),
    homePrefix: "uzi-terminal-decode-regression-",
    label: "review",
    timeoutMs: 5000,
    queryFn,
    denyReason: "the reviewer is read-only and runs no tools",
    log: nullLogger(),
  };

  await assert.rejects(runReadOnlyModelPass(opts), (err: unknown) => {
    assert.ok(err instanceof Error);
    assert.ok(
      !(err instanceof TypeError),
      "must not throw the pre-fix TypeError from String(subtype)",
    );
    assert.equal((err as Error).message, "review model call returned an error result");
    return true;
  });
});
