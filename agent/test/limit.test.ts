import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  classifyLimitFailure,
  describeLimit,
  LimitReachedError,
  normalizeResetsAt,
  RateLimitObserver,
} from "../src/limit.js";

// Frames are hand-written to the shapes in the pinned SDK typings (0.3.219),
// because no observed production frame exists yet — which is exactly why the
// classifier is pure and why these tests pin the SHAPES it reads. If a real
// session ever proves a shape wrong, the fix lands here first.

const NOW = Date.UTC(2026, 6, 27, 12, 0, 0);
const IN_5H = NOW + 5 * 60 * 60 * 1000;
const AN_HOUR_AGO = NOW - 60 * 60 * 1000;

function rateLimitEvent(info: Record<string, unknown>): unknown {
  return { type: "rate_limit_event", rate_limit_info: info, uuid: "u", session_id: "s" };
}

function errorResult(over: Record<string, unknown> = {}): unknown {
  return { type: "result", subtype: "error_during_execution", is_error: true, ...over };
}

function successResult(over: Record<string, unknown> = {}): unknown {
  return { type: "result", subtype: "success", is_error: false, ...over };
}

describe("normalizeResetsAt", () => {
  it("scales a seconds value to milliseconds and leaves a millisecond value alone", () => {
    // The SDK declares a bare `number` with no unit, and the two readings differ by
    // 1000x — one lands in 1970, the other past the year 50000.
    assert.equal(normalizeResetsAt(IN_5H / 1000), IN_5H);
    assert.equal(normalizeResetsAt(IN_5H), IN_5H);
  });

  it("rejects values that cannot be a timestamp", () => {
    for (const bad of [undefined, null, "1700000000", NaN, Infinity, 0, -1, {}]) {
      assert.equal(normalizeResetsAt(bad), undefined, `expected undefined for ${String(bad)}`);
    }
  });

  it("does not judge whether the reset is in the past — that needs a clock the caller owns", () => {
    assert.equal(normalizeResetsAt(AN_HOUR_AGO), AN_HOUR_AGO);
  });
});

describe("RateLimitObserver", () => {
  it("ignores every frame that is not a rate_limit_event", () => {
    const obs = new RateLimitObserver();
    for (const m of [successResult(), { type: "assistant" }, { type: "system", subtype: "init" }, null, "x"]) {
      obs.observe(m);
    }
    assert.equal(obs.latest, undefined);
  });

  it("captures status, normalized reset and type", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H / 1000, rateLimitType: "five_hour" }));
    assert.deepEqual(obs.latest, { status: "rejected", resetsAtMs: IN_5H, rateLimitType: "five_hour" });
  });

  it("keeps the LATEST observation, so a limit that cleared mid-turn does not park", () => {
    // Latest-wins is the whole point: the newest frame describes the state the turn
    // actually died in. First-wins would park a run whose limit lifted.
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H / 1000, rateLimitType: "five_hour" }));
    obs.observe(rateLimitEvent({ status: "allowed" }));
    assert.equal(obs.latest?.status, "allowed");
    assert.equal(classifyLimitFailure(errorResult(), obs.latest, NOW), undefined);
  });

  it("does not let a malformed frame erase a good observation", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H / 1000, rateLimitType: "five_hour" }));
    obs.observe(rateLimitEvent({ notStatus: true }));
    obs.observe({ type: "rate_limit_event" });
    assert.equal(obs.latest?.status, "rejected");
  });

  it("passes an unknown rateLimitType through unvalidated — the server owns the allowlist", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H, rateLimitType: "some_future_window" }));
    assert.equal(obs.latest?.rateLimitType, "some_future_window");
  });
});

describe("classifyLimitFailure — terminal_reason is primary", () => {
  for (const reason of ["blocking_limit", "rapid_refill_breaker"]) {
    it(`classifies ${reason} with no rate_limit_event at all`, () => {
      // No corroboration required: the SDK named the cause. The reset is simply
      // unknown, and the server falls back to its exponential schedule.
      const got = classifyLimitFailure(errorResult({ terminal_reason: reason }), undefined, NOW);
      assert.deepEqual(got, { resetsAtMs: undefined, rateLimitType: undefined });
    });
  }

  it("carries the reset and type from the observation when present", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H / 1000, rateLimitType: "seven_day" }));
    const got = classifyLimitFailure(errorResult({ terminal_reason: "blocking_limit" }), obs.latest, NOW);
    assert.deepEqual(got, { resetsAtMs: IN_5H, rateLimitType: "seven_day" });
  });

  it("drops a reset that has already passed rather than passing it on", () => {
    // A past reset is worse than none: the server would stamp a retry_not_before
    // already elapsed and promote the run straight back into the same dead window.
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: AN_HOUR_AGO, rateLimitType: "five_hour" }));
    const got = classifyLimitFailure(errorResult({ terminal_reason: "blocking_limit" }), obs.latest, NOW);
    assert.deepEqual(got, { resetsAtMs: undefined, rateLimitType: "five_hour" });
  });

  it("ignores the 17 terminal_reason members that are not limits", () => {
    for (const reason of ["max_turns", "api_error", "prompt_too_long", "budget_exhausted", "completed"]) {
      assert.equal(classifyLimitFailure(errorResult({ terminal_reason: reason }), undefined, NOW), undefined, reason);
    }
  });
});

describe("classifyLimitFailure — a rejected event is secondary and needs corroboration", () => {
  it("classifies a rejected event corroborated by a future reset", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H / 1000, rateLimitType: "five_hour" }));
    const got = classifyLimitFailure(errorResult(), obs.latest, NOW);
    assert.deepEqual(got, { resetsAtMs: IN_5H, rateLimitType: "five_hour" });
  });

  // THE DISCRIMINATOR IS THE ELAPSED RESET, NOT THE UNRELATED DEATH. The two tests
  // below differ ONLY in the reset and disagree in outcome, which is what pins that.
  //
  // This pair replaced a single test named "does NOT classify a stale rejected event
  // followed by an unrelated death". That name attributed the non-classification to
  // the death, and a reader trusting it would conclude an unrelated death is enough
  // to prevent a park. It is not, as the second test shows.
  it("does NOT classify when the rejected event's reset has already elapsed", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: AN_HOUR_AGO, rateLimitType: "five_hour" }));
    const got = classifyLimitFailure(errorResult({ subtype: "error_max_turns" }), obs.latest, NOW);
    assert.equal(got, undefined);
  });

  // ⚠ PINS THE ACCEPTED RESIDUAL, deliberately, so it cannot change silently.
  //
  // Same unrelated death, reset still in the future ⇒ the run PARKS. The subtype is
  // never consulted. This is not a corner case: a five_hour window's reset is in the
  // future for most of five hours, so any unrelated death observed after a `rejected`
  // inside that window lands here.
  //
  // It was raised with the lead and the ruling was to keep Decision 1 as written —
  // narrowing it to a subtype allowlist would be policy the spec does not state.
  // Recorded as a passing test rather than a comment so that a future "tidy" is a
  // failing test, which is this module's whole design principle.
  it("DOES classify an unrelated death when the rejected event's reset is still in the future", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H, rateLimitType: "five_hour" }));
    const got = classifyLimitFailure(errorResult({ subtype: "error_max_turns" }), obs.latest, NOW);
    assert.deepEqual(got, { resetsAtMs: IN_5H, rateLimitType: "five_hour" });
  });

  it("does not classify allowed or allowed_warning, however much headroom talk they carry", () => {
    for (const status of ["allowed", "allowed_warning"]) {
      const obs = new RateLimitObserver();
      obs.observe(rateLimitEvent({ status, resetsAt: IN_5H, rateLimitType: "five_hour" }));
      assert.equal(classifyLimitFailure(errorResult(), obs.latest, NOW), undefined, status);
    }
  });

  it("does not classify a rejected event with no reset at all", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", rateLimitType: "five_hour" }));
    assert.equal(classifyLimitFailure(errorResult(), obs.latest, NOW), undefined);
  });
});

describe("classifyLimitFailure — what must never park", () => {
  // 🔴 PRD Decision 1's open question, decided and pinned. `terminal_reason` is
  // declared on SDKResultSuccess as well as SDKResultError, so this combination is
  // representable. A turn that PRODUCED a result is not a failure, and parking it
  // would throw away completed work to sit out a window. If that reading ever
  // changes, this test fails rather than the behaviour changing silently.
  it("never parks a clean success result, even one carrying blocking_limit", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H, rateLimitType: "five_hour" }));
    assert.equal(classifyLimitFailure(successResult({ terminal_reason: "blocking_limit" }), obs.latest, NOW), undefined);
  });

  // The asymmetry that makes the test above narrow rather than sloppy: a success
  // FLAGGED as an error is already routed down the executor's failure path by
  // isErrorResult (errorSubtype reads literally "success"), so the classifier must
  // agree with it and treat the frame as a failure.
  it("does classify a success-subtype frame that is flagged is_error, matching isErrorResult", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H, rateLimitType: "five_hour" }));
    const got = classifyLimitFailure(successResult({ is_error: true, terminal_reason: "blocking_limit" }), obs.latest, NOW);
    assert.deepEqual(got, { resetsAtMs: IN_5H, rateLimitType: "five_hour" });
  });

  it("never classifies a non-result frame", () => {
    const obs = new RateLimitObserver();
    obs.observe(rateLimitEvent({ status: "rejected", resetsAt: IN_5H, rateLimitType: "five_hour" }));
    for (const m of [{ type: "assistant" }, { type: "rate_limit_event" }, undefined, null, "result"]) {
      assert.equal(classifyLimitFailure(m, obs.latest, NOW), undefined, String(m));
    }
  });

  // A transient 429 is absorbed inside the SDK's own retry budget and the turn
  // never terminates on it. Nothing this module sees should react to one.
  it("does not classify an api_retry frame — transient retries stay invisible", () => {
    const obs = new RateLimitObserver();
    obs.observe({ type: "api_retry", attempt: 2, retry_after_ms: 5000 });
    assert.equal(obs.latest, undefined);
    assert.equal(classifyLimitFailure(errorResult(), obs.latest, NOW), undefined);
  });
});

describe("LimitReachedError + describeLimit", () => {
  it("carries the normalized facts rather than encoding them in the message", () => {
    const err = new LimitReachedError({ resetsAtMs: IN_5H, rateLimitType: "five_hour" });
    assert.ok(err instanceof Error);
    assert.equal(err.name, "LimitReachedError");
    assert.equal(err.resetsAtMs, IN_5H);
    assert.equal(err.rateLimitType, "five_hour");
  });

  it("describes a known and an unknown reset differently", () => {
    assert.equal(describeLimit({ rateLimitType: "five_hour", resetsAtMs: IN_5H }), "five_hour; resets at 2026-07-27T17:00:00.000Z");
    assert.equal(describeLimit({ rateLimitType: "five_hour" }), "five_hour; reset time unknown");
    assert.equal(describeLimit({}), "unknown; reset time unknown");
  });
});
