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
