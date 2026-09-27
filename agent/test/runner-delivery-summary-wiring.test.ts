import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { RunRunner } from "../src/runner.js";
import { SummaryRunner } from "../src/summary-runner.js";
import type { DeliveryContext } from "../src/pr-description-context.js";
import type { GitCache } from "../src/git.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// PRD #1798 M5, wiring only: the RunRunner holds the PR-description editor pass for the finalize
// path (M6 calls it). No run executes here.

const ctx: DeliveryContext = {
  issue: "Title: x",
  prd: null,
  plan: null,
  claims: null,
  commits: "(no commits)",
  paths: "(no changed files)",
  diff: "(no diff)",
  truncated: { issue: false, prd: false, plan: false, claims: false, commits: false, paths: false, diff: false },
  bytes: 0,
};
const codexClaim = { run_id: "run-7", secrets: { codex: { auth_mode: "api_key", access_token: "t", capability: "c" } } };

function runRunner(opts: ConstructorParameters<typeof RunRunner>[6]): RunRunner {
  return new RunRunner({} as WorkerClient, {} as GitCache, () => ({}) as never, nullLogger(), 20, undefined, opts);
}

describe("RunRunner delivery-summary wiring (PRD #1798 M5)", () => {
  it("skipDeliverySummary disables the pass even when a SummaryRunner is injected", () => {
    const injected = new SummaryRunner(nullLogger());
    assert.equal(runRunner({ skipDeliverySummary: true, summaryRunner: injected }).deliverySummaryRunner(), null);
    assert.equal(runRunner({ summaryRunner: injected }).deliverySummaryRunner(), injected);
  });

  it("the default SummaryRunner carries the injected Codex factory and the runner's clock", async () => {
    let built: string | undefined;
    const factory = (async (p: { runId: string }) => {
      built = p.runId;
      return {
        kind: "codex" as const,
        run: async () => ({ text: JSON.stringify({ summary: "From Codex." }), end: { kind: "terminal", terminal: { outcome: "success" } } }),
      };
    }) as never;
    const pass = runRunner({ codexAdviceHarnessFactory: factory, now: () => 1_000 }).deliverySummaryRunner();
    assert.ok(pass instanceof SummaryRunner);
    const out = await pass.generateDeliverySummary({ claim: codexClaim, context: ctx, deadlineMs: 1_000 + 60_000 });
    assert.equal(built, "run-7");
    assert.equal(out?.summary, "From Codex.");
    // The deadline is judged on the runner's clock: at its "now" the deadline is spent.
    built = undefined;
    assert.equal(await pass.generateDeliverySummary({ claim: codexClaim, context: ctx, deadlineMs: 1_000 }), null);
    assert.equal(built, undefined, "no harness built for a spent deadline");
  });
});
