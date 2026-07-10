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

describe("loadConfig chat lifecycle knobs (PRD #39)", () => {
  it("applies the documented defaults when unset", () => {
    const c = loadConfig(baseEnv());
    assert.strictEqual(c.chatMaxTurns, 50);
    assert.strictEqual(c.chatTurnTimeoutMs, 10 * 60_000);
    assert.strictEqual(c.chatIdleTimeoutMs, 60 * 60_000);
    assert.strictEqual(c.chatPollMs, 1000);
  });

  it("parses overrides (durations + integer cap)", () => {
    const c = loadConfig(
      baseEnv({
        CHAT_MAX_TURNS: "12",
        WORKER_CHAT_TURN_TIMEOUT: "90s",
        WORKER_CHAT_IDLE_TIMEOUT: "30m",
        WORKER_CHAT_POLL_MS: "250",
      }),
    );
    assert.strictEqual(c.chatMaxTurns, 12);
    assert.strictEqual(c.chatTurnTimeoutMs, 90_000);
    assert.strictEqual(c.chatIdleTimeoutMs, 30 * 60_000);
    assert.strictEqual(c.chatPollMs, 250);
  });

  it("falls back to the default on a non-positive or non-integer turn cap", () => {
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "0" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "-3" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "1.5" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "  " })).chatMaxTurns, 50);
  });
});
