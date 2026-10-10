import { followSuccessfulPush } from "./publication-fixture.js";
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import { SummaryRunner } from "../src/summary-runner.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// Issue #2686: the PR-description editor pass's spend travels as a summary_usage message that
// the REAL RunRunner emits into its batcher before the batcher closes, so the delivered batch
// carries it.

const HAIKU = "claude-haiku-4-5-20251001";
const homes: string[] = [];
after(async () => {
  await Promise.all(homes.map((d) => fs.rm(d, { recursive: true, force: true })));
});

describe("RunRunner: the PR-description pass's summary_usage (issue #2686)", () => {
  it("the editor pass's usage is in the delivered batch, once, as a pr_description message", async () => {
    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const gl = fakeGitlab();
    followSuccessfulPush(git, gl.pr, true);
    const claim = gitlabClaim(2686);
    const queryFn = (async function* () {
      yield {
        type: "assistant",
        message: {
          id: "m1",
          model: HAIKU,
          role: "assistant",
          content: [{ type: "text", text: JSON.stringify({ summary: "Adds usage reporting.", changes: ["c"], scope_notes: [], review_pointers: [] }) }],
          usage: { input_tokens: 21, output_tokens: 4 },
        },
      };
      yield { type: "result", subtype: "success", is_error: false };
    }) as unknown as SdkQueryFn;
    const homeRoot = await fs.mkdtemp(path.join(os.tmpdir(), "runner-summary-usage-"));
    homes.push(homeRoot);
    const summaryRunner = new SummaryRunner(nullLogger(), { queryFn, homeRoot });

    await runner(new StubExecutor(nullLogger()), gl.gitlab, undefined, { summaryRunner }).execute(claim);

    const usage = api.messages(claim.run_id).filter((m) => m.kind === "summary_usage");
    assert.equal(usage.length, 1, "delivered exactly once");
    assert.equal(usage[0]!.agent, "worker");
    assert.deepEqual(Object.keys(usage[0]!.payload as object).sort(), ["model_usage", "pass"]);
    assert.equal((usage[0]!.payload as { pass: string }).pass, "pr_description");
    const mu = (usage[0]!.payload as { model_usage: Record<string, { inputTokens: number }> }).model_usage;
    assert.equal(mu[HAIKU]!.inputTokens, 21);
  });
});
