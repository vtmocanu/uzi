import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { MessageBatcher } from "../src/batcher.js";
import type { WorkerClient } from "../src/client.js";
import { projectInit, projectResult } from "../src/harness-messages.js";
import { oldCumulativeFromModelUsage } from "../src/old-cumulative.js";
import { nullLogger } from "./helpers.js";

// Issue #4201 m5b: the worker records the old credential's last known cumulative reading with apply.

const client = { async postMessages(): Promise<void> { /* delivery is irrelevant here */ } } as unknown as WorkerClient;
const newBatcher = (): MessageBatcher => new MessageBatcher(client, "run-old-cum", 0, 0, nullLogger());

const wire = (usage: unknown, modelUsage: unknown) =>
  ({ usage, modelUsage, num_turns: 1, duration_ms: 1, total_cost_usd: 0 });

function result(outcome: "success" | "failed", modelUsage: unknown, basis: boolean = true) {
  return projectResult({
    outcome, subtype: outcome === "success" ? "success" : "error_during_execution", errors: [],
    ...(basis ? { usageBasis: "session_cumulative" as const } : {}),
    wire: wire({ input_tokens: 999, output_tokens: 999 }, modelUsage),
  });
}

describe("Batcher.lastSessionCumulative", () => {
  it("no prior frame is undefined", async () => {
    const b = newBatcher();
    assert.equal(b.lastSessionCumulative(), undefined);
    await b.close();
  });

  it("is derived from modelUsage, not usage", async () => {
    const b = newBatcher();
    b.emit(result("success", { "claude-x": { inputTokens: 11, outputTokens: 22 } }));
    assert.deepEqual(b.lastSessionCumulative(), { "claude-x.inputTokens": 11, "claude-x.outputTokens": 22 });
    await b.close();
  });

  it("an error-shape result is captured", async () => {
    const b = newBatcher();
    const em = result("failed", { "claude-x": { cacheReadInputTokens: 5 } });
    assert.equal(em.kind, "error");
    b.emit(em);
    assert.deepEqual(b.lastSessionCumulative(), { "claude-x.cacheReadInputTokens": 5 });
    await b.close();
  });

  it("the latest frame wins, and a per-leg frame (no usage_basis) is ignored", async () => {
    const b = newBatcher();
    b.emit(result("success", { m: { inputTokens: 1 } }));
    b.emit(result("success", { m: { inputTokens: 2 } }));
    b.emit(result("success", { m: { inputTokens: 500 } }, false));
    assert.deepEqual(b.lastSessionCumulative(), { "m.inputTokens": 2 });
    await b.close();
  });

  it("a fresh-session init resets the capture; a continuing init does not", async () => {
    const b = newBatcher();
    b.emit(result("success", { m: { inputTokens: 2 } }));
    b.emit(projectInit("claude-x", false));
    assert.deepEqual(b.lastSessionCumulative(), { "m.inputTokens": 2 });
    b.emit(projectInit("claude-x", true));
    assert.equal(b.lastSessionCumulative(), undefined);
    await b.close();
  });
});

interface FixtureCase { name: string; modelUsage: unknown; expected: Record<string, number> | null }
const fixture = JSON.parse(fs.readFileSync(new URL("./fixtures/old-cumulative-boundaries.json", import.meta.url), "utf8")) as
  { cases: FixtureCase[]; violations: { name: string; raw: string }[] };

describe("oldCumulativeFromModelUsage boundaries (shared fixture)", () => {
  for (const c of fixture.cases) {
    it(c.name, () => {
      assert.deepEqual(oldCumulativeFromModelUsage(c.modelUsage), c.expected ?? undefined);
    });
  }

  it("never returns a key over 200 bytes or more than 64 entries", () => {
    for (const c of fixture.cases) {
      const out = oldCumulativeFromModelUsage(c.modelUsage) ?? {};
      assert.ok(Object.keys(out).length <= 64);
      for (const k of Object.keys(out)) assert.ok(Buffer.byteLength(k, "utf8") <= 200);
    }
  });

  it("warns about drops without values", () => {
    const warns: { msg: string; fields: unknown }[] = [];
    const log = { ...nullLogger(), warn: (msg: string, fields?: unknown) => { warns.push({ msg, fields }); } };
    oldCumulativeFromModelUsage({ m: { inputTokens: -123456, outputTokens: 4 } }, log as never);
    assert.equal(warns.length, 1);
    assert.ok(!JSON.stringify(warns[0]).includes("123456"));
  });

  it("non-object input is undefined", () => {
    for (const v of [undefined, null, 5, "x", [1]]) assert.equal(oldCumulativeFromModelUsage(v), undefined);
  });
});
