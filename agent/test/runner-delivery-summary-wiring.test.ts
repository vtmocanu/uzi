import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { RunRunner } from "../src/runner.js";
import { SummaryRunner } from "../src/summary-runner.js";
import type { GitCache } from "../src/git.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// PRD #1798 M5, wiring only: the RunRunner holds the PR-description editor pass for the finalize
// path (M6 calls it). No run executes here.

function runRunner(opts: ConstructorParameters<typeof RunRunner>[6]): RunRunner {
  return new RunRunner({} as WorkerClient, {} as GitCache, () => ({}) as never, nullLogger(), 20, undefined, opts);
}

describe("RunRunner delivery-summary wiring (PRD #1798 M5)", () => {
  it("has no editor pass unless one is injected: no live-SDK default", () => {
    assert.equal(runRunner({}).deliverySummaryRunner(), null);
    assert.equal(runRunner(undefined).deliverySummaryRunner(), null);
  });

  it("returns exactly the injected SummaryRunner", () => {
    const injected = new SummaryRunner(nullLogger());
    assert.equal(runRunner({ summaryRunner: injected }).deliverySummaryRunner(), injected);
  });

  it("skipDeliverySummary disables the pass even when a SummaryRunner is injected", () => {
    const injected = new SummaryRunner(nullLogger());
    assert.equal(runRunner({ skipDeliverySummary: true, summaryRunner: injected }).deliverySummaryRunner(), null);
  });
});
