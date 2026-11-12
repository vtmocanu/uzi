import { after, describe, it } from "node:test";
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
// Each homeRoot is queued for removal when the file ends (PRD #1809 M2).
const homeRoots: string[] = [];
after(async () => {
  for (const r of homeRoots.splice(0)) await fs.rm(r, { recursive: true, force: true });
});

async function makeRunner(queryFn: SdkQueryFn, modelTimeoutMs = 20) {
  const homeRoot = await fs.mkdtemp(path.join(os.tmpdir(), "summary-test-"));
  homeRoots.push(homeRoot);
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

  it("applies the platform cleanup policy to the ephemeral homeDir", async () => {
    const { runner, homeRoot } = await makeRunner(replyingQueryFn("done"));
    await runner.generateIntentSummary(intentInput);
    const entries = await fs.readdir(homeRoot);
    if (process.platform === "linux") assert.deepEqual(entries, [], "the per-turn HOME was removed");
    else {
      assert.equal(entries.length, 1, "unsupported platforms retain the per-turn HOME");
      assert.ok(entries[0]!.startsWith("uzi-summary-"));
    }
    await fs.rm(homeRoot, { recursive: true, force: true });
  });

  // Code review PR #387, finding 3: the api rejects a summary over 4000 BYTES. Multibyte
  // text can sit under the 2000-CHAR clip yet blow the byte cap, and the whole summary is
  // then dropped. A CJK string of 1800 chars is 5400 UTF-8 bytes — under the char cap,
  // over the byte cap; the runner must return it clipped to ≤ 4000 bytes, not pass it on.
  it("clips a multibyte summary to the api BYTE cap, not just the char cap", async () => {
    const wide = "中".repeat(1800); // 1800 chars < 2000-char clip; 5400 bytes > 4000-byte cap
    const { runner } = await makeRunner(replyingQueryFn(wide));
    const out = await runner.generateIntentSummary(intentInput);
    assert.ok(out, "a wide summary is clipped, not dropped");
    assert.ok(
      Buffer.byteLength(out, "utf8") <= 4000,
      `summary must be ≤ 4000 bytes, got ${Buffer.byteLength(out, "utf8")}`,
    );
    assert.ok(out.endsWith("…"), "a clipped summary carries the ellipsis");
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

  // Code review PR #387, finding 3: the api rejects a delta text over 1000 BYTES and a
  // summary over 4000 BYTES. A CJK delta of 500 chars is 1500 bytes — under the 600-char
  // clip, over the byte cap. Both must come back clipped to their byte caps, not dropped.
  it("clips a multibyte plan summary and delta text to the api BYTE caps", async () => {
    const json = JSON.stringify({
      summary: "中".repeat(1800), // 5400 bytes > 4000
      deltas: [{ kind: "added", text: "中".repeat(500) }], // 1500 bytes > 1000
    });
    const { runner } = await makeRunner(replyingQueryFn(json));
    const out = await runner.generatePlanSummary(planInput);
    assert.ok(out);
    assert.ok(
      Buffer.byteLength(out.summary, "utf8") <= 4000,
      `plan summary must be ≤ 4000 bytes, got ${Buffer.byteLength(out.summary, "utf8")}`,
    );
    assert.equal(out.deltas.length, 1);
    assert.ok(
      Buffer.byteLength(out.deltas[0]!.text, "utf8") <= 1000,
      `delta text must be ≤ 1000 bytes, got ${Buffer.byteLength(out.deltas[0]!.text, "utf8")}`,
    );
  });

  it("skips a brace example in prose before the real JSON (scans later candidates)", async () => {
    const json = JSON.stringify({ summary: "Plan looks fine.", deltas: [{ kind: "added", text: "a change" }] });
    const { runner } = await makeRunner(
      replyingQueryFn("```json\nUse {summary, deltas} here.\n" + json + "\n```"),
    );
    const out = await runner.generatePlanSummary(planInput);
    assert.ok(out);
    assert.equal(out.summary, "Plan looks fine.");
    assert.deepEqual(out.deltas, [{ kind: "added", text: "a change" }]);
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

// PRD #229 D10: the endpoint is a credential. Two things are pinned here, because the
// delivery lane had BOTH halves wrong before: the gate rejected an endpoint-only claim,
// and deliveryHarness computed the endpoint then dropped it on the floor (its return type
// listed token/model/codex only). A test on the intent lane alone would have missed the
// delivery half entirely.
describe("SummaryRunner — bound custom endpoint (PRD #229 D10)", () => {
  it("generateIntentSummary runs on an endpoint-only instance and puts the endpoint in the SDK env", async () => {
    let seenEnv: Record<string, string | undefined> | undefined;
    const queryFn: SdkQueryFn = ((opts: { options?: { env?: Record<string, string | undefined> } }) => {
      seenEnv = opts.options?.env;
      return (async function* () {
        yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: "a summary" }] } };
        yield { type: "result", subtype: "success", is_error: false };
      })();
    }) as unknown as SdkQueryFn;
    const { runner } = await makeRunner(queryFn);

    const out = await runner.generateIntentSummary({
      ...intentInput,
      token: "",
      anthropicEndpoint: { base_url: "https://gateway.example", api_key: "sk-gateway", model: "gateway-model-1" },
    });

    assert.equal(out, "a summary", "an endpoint-only instance must still produce a summary");
    assert.equal(seenEnv?.ANTHROPIC_BASE_URL, "https://gateway.example");
    assert.equal(seenEnv?.ANTHROPIC_AUTH_TOKEN, "sk-gateway");
    assert.equal(seenEnv?.CLAUDE_CODE_OAUTH_TOKEN, undefined, "the subscription token is cleared alongside the endpoint");
  });

  // NOTE: this one is a CHARACTERIZATION test, not a regression test. I first believed
  // deliveryHarness dropped the endpoint (its Pick<> listed token/model/codex only), added
  // the field to the type, then sabotaged it back — and the test stayed green. The spread
  // `...harness` carries the runtime value regardless of the declared Pick, so the lane was
  // already correct and the type was merely under-declared. Kept because it pins the
  // behaviour end to end; do not read it as proof of a fix.
  it("generateDeliverySummary threads the claim's endpoint into the pass", async () => {
    let seenEnv: Record<string, string | undefined> | undefined;
    const queryFn: SdkQueryFn = ((opts: { options?: { env?: Record<string, string | undefined> } }) => {
      seenEnv = opts.options?.env;
      return (async function* () {
        yield {
          type: "assistant",
          message: { role: "assistant", content: [{ type: "text", text: JSON.stringify({ summary: "did the thing", diagram: null }) }] },
        };
        yield { type: "result", subtype: "success", is_error: false };
      })();
    }) as unknown as SdkQueryFn;
    const { runner } = await makeRunner(queryFn);

    await runner.generateDeliverySummary({
      claim: {
        run_id: "run-1",
        summary_model: "haiku",
        secrets: {
          anthropic_oauth_token: "",
          anthropic_endpoint: { base_url: "https://gateway.example", api_key: "sk-gateway", model: "gateway-model-1" },
        },
      },
      context: {
        issue: "Work", prd: null, plan: null, previous: null, claims: null,
        commits: "", paths: "", diff: "", bytes: 4,
        truncated: { issue: false, prd: false, plan: false, previous: false, claims: false, commits: false, paths: false, diff: false },
      },
      deadlineMs: Date.now() + 5_000,
      signal: new AbortController().signal,
    });

    // The whole point: deliveryHarness MUST carry the endpoint through to buildSdkEnv.
    // If its return type loses the field again, this is undefined and the test fails.
    assert.equal(seenEnv?.ANTHROPIC_BASE_URL, "https://gateway.example",
      "the PR-description pass dropped the claim's endpoint before the SDK env");
    assert.equal(seenEnv?.ANTHROPIC_AUTH_TOKEN, "sk-gateway");
  });
});
