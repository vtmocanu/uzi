import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { loadConfig } from "../src/config.js";

// Minimal env that satisfies loadConfig's required fields.
const baseEnv = (over: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv => ({
  UZI_API_URL: "http://api:8080",
  UZI_WORKER_TOKEN: "worker-join-token-0123456789",
  ...over,
});

describe("loadConfig workerTemplate (PRD #18)", () => {
  it("defaults to base when UZI_WORKER_TEMPLATE is unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).workerTemplate, "base");
  });

  it("reads the baked UZI_WORKER_TEMPLATE (not the WORKER_TEMPLATE build var)", () => {
    // Only the image-baked UZI_WORKER_TEMPLATE is the reported identity; a stray
    // WORKER_TEMPLATE (the compose build arg) must NOT be picked up.
    const cfg = loadConfig(baseEnv({ UZI_WORKER_TEMPLATE: "jvm", WORKER_TEMPLATE: "base" }));
    assert.strictEqual(cfg.workerTemplate, "jvm");
  });

  it("trims and falls back to base on blank", () => {
    assert.strictEqual(loadConfig(baseEnv({ UZI_WORKER_TEMPLATE: "  " })).workerTemplate, "base");
  });
});

describe("loadConfig version (PRD #113 M1)", () => {
  it("reports the build-stamped UZI_AGENT_VERSION", () => {
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "0.11.7" })).version, "0.11.7");
  });

  it("passes the +g<short-sha> build-metadata suffix through verbatim", () => {
    // CI stamps `<release>+g<short-sha>`. SemVer §10 excludes build metadata from
    // precedence (x/mod/semver: Compare("v0.11.7+g1a2b3c4","v0.11.7") == 0), so the
    // suffix costs the api's compare nothing, but the worker must report it
    // UNTOUCHED or the commit it identifies is unrecoverable from the UI. No
    // trimming, no normalization, no stripping of the `+` on this side.
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "0.11.7+g1a2b3c4" })).version, "0.11.7+g1a2b3c4");
  });

  it("is EMPTY when unstamped — never a fake SemVer", () => {
    // The retired "0.1.0-m4" default is the whole point of this milestone: an
    // unstamped image must report nothing (the api classifies it `unknown`)
    // rather than a plausible version it is not running. An unstamped image sets
    // the ENV to the empty ARG default, so set-but-empty is the SHIPPING case,
    // not a corner one — both it and unset must land on "".
    assert.strictEqual(loadConfig(baseEnv()).version, "");
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "" })).version, "");
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "   " })).version, "");
  });
});

describe("loadConfig chat lifecycle knobs (PRD #39)", () => {
  it("applies the documented defaults when unset", () => {
    const c = loadConfig(baseEnv());
    assert.strictEqual(c.chatMaxTurns, 50);
    assert.strictEqual(c.chatTurnTimeoutMs, 10 * 60_000);
    assert.strictEqual(c.chatIdleTimeoutMs, 60 * 60_000);
    assert.strictEqual(c.chatPollMs, 1000);
    assert.strictEqual(c.chatSessions, 1);
  });

  it("parses overrides (durations + integer knobs)", () => {
    const c = loadConfig(
      baseEnv({
        CHAT_MAX_TURNS: "12",
        WORKER_CHAT_TURN_TIMEOUT: "90s",
        WORKER_CHAT_IDLE_TIMEOUT: "30m",
        WORKER_CHAT_POLL_MS: "250",
        WORKER_CHAT_SESSIONS: "3",
      }),
    );
    assert.strictEqual(c.chatMaxTurns, 12);
    assert.strictEqual(c.chatTurnTimeoutMs, 90_000);
    assert.strictEqual(c.chatIdleTimeoutMs, 30 * 60_000);
    assert.strictEqual(c.chatPollMs, 250);
    assert.strictEqual(c.chatSessions, 3);
  });

  it("falls back to the default on a non-positive or non-integer turn cap", () => {
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "0" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "-3" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "1.5" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "  " })).chatMaxTurns, 50);
  });
});

describe("loadConfig WORKER_MAX_CONCURRENT_RUNS (PRD #42 Decision 3)", () => {
  it("defaults to 1 (the pre-#42 serial run lane) when unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).maxConcurrentRuns, 1);
  });

  it("parses a valid integer cap", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "4" })).maxConcurrentRuns, 4);
  });

  it("honors a value above the soft ceiling (warn, not clamp — the warn lives in main.ts)", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "20" })).maxConcurrentRuns, 20);
  });

  it("falls back to 1 on a blank, zero, negative, or fractional value", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "  " })).maxConcurrentRuns, 1);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "0" })).maxConcurrentRuns, 1);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "-2" })).maxConcurrentRuns, 1);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "2.5" })).maxConcurrentRuns, 1);
  });
});

// PRD #108 M6: UZI_HOME_RECLAIM gates a DESTRUCTIVE startup sweep and ships ON, so
// its polarity is the whole point — a default-on safety feature that a deployment
// can disable without meaning to is worse than no kill switch at all.
describe("loadConfig UZI_HOME_RECLAIM (PRD #108 M6)", () => {
  it("defaults ON when unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).homeReclaimEnabled, true);
  });

  it("treats SET-BUT-EMPTY as unset, not as off", () => {
    // The finding this test exists for. `parseBool` accepts only 1|true|yes, so an
    // empty value would read as "off" — and empty is exactly how the var arrives
    // from a compose `${UZI_HOME_RECLAIM:-}` or a Helm value defaulting to "".
    // That would silently disable the sweep on every deployment that merely
    // mentions the variable.
    assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: "" })).homeReclaimEnabled, true);
    assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: "   " })).homeReclaimEnabled, true);
  });

  it("turns OFF only on an explicit falsy value", () => {
    for (const v of ["0", "false", "no", "off", "FALSE", " Off "]) {
      assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: v })).homeReclaimEnabled, false, `value ${JSON.stringify(v)}`);
    }
  });

  it("stays ON for the affirmative spellings", () => {
    for (const v of ["1", "true", "yes", "TRUE", " On "]) {
      assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: v })).homeReclaimEnabled, true, `value ${JSON.stringify(v)}`);
    }
  });
});
