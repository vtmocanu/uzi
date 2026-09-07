// M2 neutral-harness-seam differential test: the single-core limit classifier.
//
// PRD #1146 (#1106 M2) milestone m4 validation. classifyLimitEvidence is the NEW
// neutral decision core; classifyLimitFailure is the FROZEN public API that now
// delegates to it. These tests are pure and credential-free: hand-built evidence
// and raw result frames, an injected `now`, no clock and no SDK.

import test from "node:test";
import assert from "node:assert/strict";

import { classifyLimitEvidence, classifyLimitFailure } from "../../agent/src/limit.js";
import type { HarnessLimitEvidence } from "../../agent/src/harness.js";

const NOW = 1_700_000_000_000; // fixed epoch ms
const FUTURE = NOW + 3_600_000;
const PAST = NOW - 3_600_000;

test("explicit exhaustion classifies; a future reset is carried, a past/absent reset is dropped", () => {
  const future: HarnessLimitEvidence = {
    explicitExhaustion: true,
    latest: { status: "allowed", resetsAtMs: FUTURE, window: "five_hour" },
  };
  assert.deepEqual(classifyLimitEvidence(future, NOW), {
    resetsAtMs: FUTURE,
    window: "five_hour",
  });

  const past: HarnessLimitEvidence = {
    explicitExhaustion: true,
    latest: { status: "allowed", resetsAtMs: PAST, window: "five_hour" },
  };
  assert.deepEqual(
    classifyLimitEvidence(past, NOW),
    { resetsAtMs: undefined, window: "five_hour" },
    "a past reset is worse than none; window is still carried",
  );

  const noReset: HarnessLimitEvidence = { explicitExhaustion: true };
  assert.deepEqual(classifyLimitEvidence(noReset, NOW), {
    resetsAtMs: undefined,
    window: undefined,
  });
});

test("rejected + future reset classifies (the corroborated (b) path)", () => {
  const ev: HarnessLimitEvidence = {
    explicitExhaustion: false,
    latest: { status: "rejected", resetsAtMs: FUTURE, window: "weekly" },
  };
  assert.deepEqual(classifyLimitEvidence(ev, NOW), { resetsAtMs: FUTURE, window: "weekly" });
});

test("rejected without a future reset does NOT classify", () => {
  const pastReset: HarnessLimitEvidence = {
    explicitExhaustion: false,
    latest: { status: "rejected", resetsAtMs: PAST, window: "weekly" },
  };
  assert.equal(classifyLimitEvidence(pastReset, NOW), undefined, "past reset ⇒ no park");

  const absentReset: HarnessLimitEvidence = {
    explicitExhaustion: false,
    latest: { status: "rejected", window: "weekly" },
  };
  assert.equal(classifyLimitEvidence(absentReset, NOW), undefined, "absent reset ⇒ no park");
});

test("an unknown status is NOT implicitly rejected", () => {
  const ev: HarnessLimitEvidence = {
    explicitExhaustion: false,
    latest: { status: "allowed_warning", resetsAtMs: FUTURE, window: "five_hour" },
  };
  assert.equal(classifyLimitEvidence(ev, NOW), undefined);
});

test("no latest / empty evidence does not classify", () => {
  assert.equal(classifyLimitEvidence({ explicitExhaustion: false }, NOW), undefined);
});

// --- delegation pin: the frozen public API shares the one decision core --------

test("classifyLimitFailure(rawFrame,...) is consistent with classifyLimitEvidence", () => {
  // Representative raw result frame that names a usage-limit death outright.
  const rawFrame = {
    type: "result",
    subtype: "error_during_execution",
    is_error: true,
    terminal_reason: "blocking_limit",
  };
  const latest = { status: "rejected", resetsAtMs: FUTURE, rateLimitType: "five_hour" };

  // The neutral core's verdict for the equivalent evidence.
  const direct = classifyLimitEvidence(
    {
      explicitExhaustion: true, // blocking_limit ⇒ explicit
      latest: { status: latest.status, resetsAtMs: latest.resetsAtMs, window: latest.rateLimitType },
    },
    NOW,
  );
  assert.ok(direct, "the representative frame is a limit death");

  // The frozen API maps `window` back to `rateLimitType` on the way out but must
  // otherwise reach the identical decision.
  const via = classifyLimitFailure(rawFrame, latest, NOW);
  assert.deepEqual(via, { resetsAtMs: direct.resetsAtMs, rateLimitType: direct.window });
});

test("classifyLimitFailure classifies a rejected+future death with no explicit reason", () => {
  const rawFrame = {
    type: "result",
    subtype: "error_max_turns",
    is_error: true,
    // no terminal_reason ⇒ not explicit; the (b) rejected+future path decides
  };
  const latest = { status: "rejected", resetsAtMs: FUTURE, rateLimitType: "weekly" };
  assert.deepEqual(classifyLimitFailure(rawFrame, latest, NOW), {
    resetsAtMs: FUTURE,
    rateLimitType: "weekly",
  });
});

test("classifyLimitFailure honours the caller-side success gate (a success never parks)", () => {
  const successFrame = {
    type: "result",
    subtype: "success",
    is_error: false,
    terminal_reason: "blocking_limit", // representable, but a success is not a failure
  };
  const latest = { status: "rejected", resetsAtMs: FUTURE, rateLimitType: "five_hour" };
  assert.equal(classifyLimitFailure(successFrame, latest, NOW), undefined);
});
