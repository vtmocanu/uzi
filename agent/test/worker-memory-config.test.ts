import assert from "node:assert/strict";
import { test } from "node:test";
import { loadConfig } from "../src/config.js";

const base = { UZI_API_URL: "https://example.com", UZI_WORKER_TOKEN: "fixture" };
const values = {
  WORKER_MEMORY_GUARD_RESERVE_BYTES: "1000",
  WORKER_MEMORY_GUARD_SAMPLE_MS: "10",
  WORKER_MEMORY_GUARD_HYSTERESIS_BYTES: "100",
  WORKER_MEMORY_GUARD_REARM_MS: "20",
  WORKER_MEMORY_GUARD_RESPONSE_BUDGET_MS: "30",
  WORKER_MEMORY_GUARD_MAX_INTERVENTIONS: "4",
};
const enabled = { ...base, ...values, WORKER_MEMORY_GUARD_ENABLED: "true" };

test("loader returns disabled by default and ignores inactive numeric knobs", () => {
  for (const flag of [undefined, "", " ", "0", "false", "NO", "off"]) {
    assert.deepEqual(loadConfig({
      ...base, WORKER_MEMORY_GUARD_ENABLED: flag, WORKER_MEMORY_GUARD_SAMPLE_MS: "NaN",
    }).memoryGuard, { enabled: false });
  }
});
test("strict boolean opt-in and explicit six-field policy", () => {
  for (const flag of ["true", "1", "YES", "on"]) {
    assert.deepEqual(loadConfig({ ...enabled, WORKER_MEMORY_GUARD_ENABLED: flag }).memoryGuard, {
      enabled: true, reserveBytes: 1000, sampleMs: 10, hysteresisBytes: 100,
      rearmMs: 20, responseBudgetMs: 30, maxInterventions: 4,
    });
  }
  for (const flag of ["typo", "2", "enabled"]) {
    assert.throws(() => loadConfig({ ...enabled, WORKER_MEMORY_GUARD_ENABLED: flag }), /boolean/);
  }
  for (const key of Object.keys(values)) {
    for (const value of [undefined, "", "0", "-1", "1.5", "1e3", "NaN", "Infinity", "9007199254740992"]) {
      assert.throws(() => loadConfig({ ...enabled, [key]: value }), new RegExp(key));
    }
  }
});
test("byte, timer and intervention ceilings are inclusive", () => {
  for (const key of ["WORKER_MEMORY_GUARD_RESERVE_BYTES", "WORKER_MEMORY_GUARD_HYSTERESIS_BYTES"]) {
    assert.doesNotThrow(() => loadConfig({ ...enabled, [key]: "9007199254740991" }));
  }
  for (const key of ["WORKER_MEMORY_GUARD_SAMPLE_MS", "WORKER_MEMORY_GUARD_REARM_MS",
    "WORKER_MEMORY_GUARD_RESPONSE_BUDGET_MS"]) {
    assert.doesNotThrow(() => loadConfig({ ...enabled, [key]: "2147483647" }));
    assert.throws(() => loadConfig({ ...enabled, [key]: "2147483648" }), new RegExp(key));
  }
  for (const value of ["1", "9999"]) {
    assert.doesNotThrow(() => loadConfig({ ...enabled, WORKER_MEMORY_GUARD_MAX_INTERVENTIONS: value }));
  }
  assert.throws(() => loadConfig({ ...enabled, WORKER_MEMORY_GUARD_MAX_INTERVENTIONS: "10000" }),
    /WORKER_MEMORY_GUARD_MAX_INTERVENTIONS/);
});
