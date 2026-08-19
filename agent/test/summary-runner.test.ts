import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";

import {
  SummaryRunner,
  buildIntentPrompt,
  buildPlanPrompt,
  type IntentSummaryInput,
  type PlanSummaryInput,
} from "../src/summary-runner.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

// A queryFn that emits one assistant text block then a terminal result (success or error).
function replyingQueryFn(text: string, error = false): SdkQueryFn {
  return async function* () {
    yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text }] } };
    yield { type: "result", subtype: error ? "error_max_turns" : "success", is_error: error };
  } as unknown as SdkQueryFn;
}

// A queryFn whose stream never yields a terminal result — it awaits a promise that never
// resolves, so the only way runModel settles is the injected wall-clock timeout.
function hangingQueryFn(): SdkQueryFn {
  return async function* () {
    await new Promise(() => {});
    yield { type: "result", subtype: "success", is_error: false };
  } as unknown as SdkQueryFn;
}

const intentInput: IntentSummaryInput = {
  token: "tok-abc",
  model: "haiku",
  issueTitle: "Add run summaries",
  issueBody: "Show plain-English summaries on the run card.",
  prdText: "PRD #362 body",
};

const planInput: PlanSummaryInput = {
  ...intentInput,
  planMd: "# Plan\n- add columns\n- add endpoint",
};

// A tiny runner: stub queryFn, a 20ms cap so the timeout path is fast, and a dedicated
// homeRoot so a test can assert the ephemeral HOME was cleaned up.
async function makeRunner(queryFn: SdkQueryFn, modelTimeoutMs = 20) {
  const homeRoot = await fs.mkdtemp(path.join(os.tmpdir(), "summary-test-"));
  const runner = new SummaryRunner(nullLogger(), { queryFn, homeRoot, modelTimeoutMs });
  return { runner, homeRoot };
}

describe("SummaryRunner.generateIntentSummary", () => {
  it("returns the trimmed text on a text-yielding stub", async () => {
    const { runner } = await makeRunner(replyingQueryFn("  This run adds run summaries.  "));
    const out = await runner.generateIntentSummary(intentInput);
    assert.equal(out, "This run adds run summaries.");
  });

  it("returns null (not throw) on a terminal error result", async () => {
    const { runner } = await makeRunner(replyingQueryFn("partial", true));
    const out = await runner.generateIntentSummary(intentInput);
    assert.equal(out, null);
  });

  it("returns null (not throw) when the model hangs, within the injected timeout", async () => {
    const { runner } = await makeRunner(hangingQueryFn(), 20);
    const started = Date.now();
    const out = await runner.generateIntentSummary(intentInput);
    assert.equal(out, null);
    assert.ok(Date.now() - started < 5000, "settled well within the wall-clock cap");
  });

  it("returns null on empty model output", async () => {
    const { runner } = await makeRunner(replyingQueryFn("   "));
    const out = await runner.generateIntentSummary(intentInput);
    assert.equal(out, null);
  });

  it("cleans up the ephemeral homeDir", async () => {
    const { runner, homeRoot } = await makeRunner(replyingQueryFn("done"));
    await runner.generateIntentSummary(intentInput);
    const entries = await fs.readdir(homeRoot);
    assert.deepEqual(entries, [], "the per-turn uzi-summary-* HOME was removed");
    await fs.rm(homeRoot, { recursive: true, force: true });
  });
});

describe("SummaryRunner.generatePlanSummary", () => {
  it("parses summary + well-formed deltas and drops malformed elements", async () => {
    const json = JSON.stringify({
      summary: "  Adds columns and an endpoint.  ",
      deltas: [
        { kind: "added", text: "  a websocket frame  " },
        { kind: "changed", text: "reuse the judge recipe" },
        { kind: "dropped", text: "" }, // blank text → dropped
        { kind: "sideways", text: "bad kind" }, // invalid kind → dropped
        "not an object", // non-object → dropped
        { kind: "added" }, // missing text → dropped
      ],
    });
    const { runner } = await makeRunner(replyingQueryFn("prose before\n```json\n" + json + "\n```"));
    const out = await runner.generatePlanSummary(planInput);
    assert.ok(out);
    assert.equal(out.summary, "Adds columns and an endpoint.");
    assert.deepEqual(out.deltas, [
      { kind: "added", text: "a websocket frame" },
      { kind: "changed", text: "reuse the judge recipe" },
    ]);
  });

  it("degrades a non-array deltas to an empty list, keeping the summary", async () => {
    const json = JSON.stringify({ summary: "Plan looks fine.", deltas: "nope" });
    const { runner } = await makeRunner(replyingQueryFn(json));
    const out = await runner.generatePlanSummary(planInput);
    assert.ok(out);
    assert.equal(out.summary, "Plan looks fine.");
    assert.deepEqual(out.deltas, []);
  });

  it("returns null when the summary is missing", async () => {
    const json = JSON.stringify({ deltas: [{ kind: "added", text: "x" }] });
    const { runner } = await makeRunner(replyingQueryFn(json));
    const out = await runner.generatePlanSummary(planInput);
    assert.equal(out, null);
  });

  it("returns null (not throw) on unparseable output", async () => {
    const { runner } = await makeRunner(replyingQueryFn("no json here at all"));
    const out = await runner.generatePlanSummary(planInput);
    assert.equal(out, null);
  });

  it("returns null (not throw) on a terminal error result", async () => {
    const { runner } = await makeRunner(replyingQueryFn("{}", true));
    const out = await runner.generatePlanSummary(planInput);
    assert.equal(out, null);
  });

  it("returns null (not throw) when the model hangs, within the injected timeout", async () => {
    const { runner } = await makeRunner(hangingQueryFn(), 20);
    const out = await runner.generatePlanSummary(planInput);
    assert.equal(out, null);
  });
});

describe("summary prompts", () => {
  it("fence the untrusted issue/PRD inputs and ask for the right shape", () => {
    const p = buildIntentPrompt(intentInput);
    assert.match(p, /UNTRUSTED DATA/);
    assert.match(p, /<untrusted_inputs_[0-9a-f]{16}>/);
    assert.match(p, /Add run summaries/);
  });

  it("plan prompt fences the plan and requests a JSON object with deltas", () => {
    const p = buildPlanPrompt(planInput);
    assert.match(p, /<untrusted_plan_[0-9a-f]{16}>/);
    assert.match(p, /"kind":"added\|changed\|dropped"/);
    assert.match(p, /add columns/);
  });
});
