import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { RunRunner } from "../src/runner.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimConfig } from "../src/protocol.js";
import { CODEX_PROBE_EXPECTATION } from "../src/codex/codex-runtime-probe.js";

// Issue #3251: the runner builds the one catalog-default reporter of a claim (RunContext.codexCatalogReport)
// only when the claim carries a well-formed codex_catalog_context and a positive claim generation.
describe("runner Codex catalog reporter (issue #3251)", () => {
  // Assembled at runtime to keep a token-shaped literal out of source.
  const TOKEN = ["tkn", "codex", "catalog", "3251"].join("-");
  const CTX = "0123456789abcdef".repeat(4);
  let api: FakeApi;
  let fx: Fixture;
  let client: WorkerClient;
  let git: GitCache;

  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    const baseUrl = await api.listen();
    fx = makeFixture();
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  });
  afterEach(async () => {
    try {
      await api.close();
    } finally {
      fx.cleanup();
    }
  });

  const claimWith = (iid: number, config: ClaimConfig | null, generation = 5) => makeClaim({
    issue_iid: iid,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    claim_generation: generation,
    config,
  });

  const execute = (claim: ReturnType<typeof claimWith>, script: (ctx: RunContext) => Promise<void>) => {
    const executor: Executor = {
      async run(ctx) {
        await script(ctx);
        throw new Error("the executor ends here");
      },
    };
    return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, { pollMs: 5 }).execute(claim);
  };

  const settle = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 5)); };

  it("a claim with a catalog context gets a reporter whose report posts the exact body once", async () => {
    const claim = claimWith(3251, { codex_catalog_context: CTX });
    await execute(claim, async (ctx) => {
      assert.ok(ctx.codexCatalogReport, "the runner constructed a reporter");
      ctx.codexCatalogReport.report({ defaultEffort: "low", supportedEfforts: ["low"] }, "gpt-6-astra");
      await settle();
    });
    assert.deepEqual(api.codexCatalogDefaultPosts, [{
      runId: claim.run_id,
      body: {
        claim_generation: 5,
        catalog_context: CTX,
        model: "gpt-6-astra",
        runtime_version: CODEX_PROBE_EXPECTATION.version,
        default_effort: "low",
      },
    }]);
  });

  it("a claim without a catalog context gets no reporter and posts nothing", async () => {
    const claim = claimWith(3252, { default_model: "opus" });
    let seen: unknown = "unset";
    await execute(claim, async (ctx) => { seen = ctx.codexCatalogReport; await settle(); });
    assert.equal(seen, undefined);
    assert.equal(api.codexCatalogDefaultPosts.length, 0);
  });
});
