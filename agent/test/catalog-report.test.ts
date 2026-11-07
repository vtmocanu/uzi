// Issue #3251: the per-claim Codex catalog default reporter and its client route.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";

import { WorkerClient, type LeadConfigPostOutcome } from "../src/client.js";
import {
  CodexCatalogReporter,
  codexCatalogReportActive,
  type CodexCatalogReportClient,
} from "../src/codex/catalog-report.js";
import { CODEX_PROBE_EXPECTATION } from "../src/codex/codex-runtime-probe.js";
import type { CodexCatalogDefaultBody } from "../src/protocol.js";
import { FakeApi } from "./fake-api.js";
import { nullLogger, recordingLogger } from "./helpers.js";

const CTX = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
const TOKEN = "worker-join-token-0123456789";

class StubClient implements CodexCatalogReportClient {
  readonly posts: CodexCatalogDefaultBody[] = [];
  outcome: LeadConfigPostOutcome | "throw" = { kind: "ok" };
  async recordCodexCatalogDefault(_runId: string, body: CodexCatalogDefaultBody): Promise<LeadConfigPostOutcome> {
    this.posts.push(body);
    if (this.outcome === "throw") throw new Error("client exploded");
    return this.outcome;
  }
}

function reporter(client: CodexCatalogReportClient, logger = nullLogger()): CodexCatalogReporter {
  return new CodexCatalogReporter({ client, runId: "run-1", claimGeneration: 7, catalogContext: CTX, logger });
}

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r));
}

describe("codexCatalogReportActive", () => {
  it("needs a 64-hex context and a positive claim generation", () => {
    assert.equal(codexCatalogReportActive(CTX, 1), true);
    assert.equal(codexCatalogReportActive(undefined, 1), false);
    assert.equal(codexCatalogReportActive(CTX, 0), false);
    assert.equal(codexCatalogReportActive(CTX, undefined), false);
    assert.equal(codexCatalogReportActive(CTX.toUpperCase(), 1), false);
    assert.equal(codexCatalogReportActive(CTX.slice(1), 1), false);
    assert.equal(codexCatalogReportActive("", 1), false);
  });
});

describe("CodexCatalogReporter", () => {
  it("posts the exact wire body once per claim", async () => {
    const client = new StubClient();
    const r = reporter(client);
    r.report({ defaultEffort: "low", supportedEfforts: ["low"] }, "gpt-6-astra");
    r.report({ defaultEffort: "high", supportedEfforts: ["high"] }, "gpt-6-astra");
    await settle();
    assert.deepEqual(client.posts, [{
      claim_generation: 7,
      catalog_context: CTX,
      model: "gpt-6-astra",
      runtime_version: CODEX_PROBE_EXPECTATION.version,
      default_effort: "low",
    }]);
  });

  for (const effort of ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]) {
    it(`maps ${effort} into the stored set`, async () => {
      const client = new StubClient();
      reporter(client).report({ defaultEffort: effort, supportedEfforts: [] }, "m");
      await settle();
      assert.equal(client.posts[0]?.default_effort, effort);
    });
  }

  it("posts nothing for persistent, a custom string, an absent effort or no entry, and spends no attempt", async () => {
    const client = new StubClient();
    const r = reporter(client);
    r.report({ defaultEffort: "persistent", supportedEfforts: [] }, "m");
    r.report({ defaultEffort: "custom-x", supportedEfforts: [] }, "m");
    r.report({ supportedEfforts: ["low"] }, "m");
    r.report(undefined, "m");
    await settle();
    assert.deepEqual(client.posts, []);
    r.report({ defaultEffort: "medium", supportedEfforts: [] }, "m");
    await settle();
    assert.equal(client.posts.length, 1);
  });

  for (const [name, outcome] of [
    ["404 from an older API", { kind: "http", status: 404 }],
    ["409 stale context", { kind: "http", status: 409 }],
    ["a 5xx", { kind: "http", status: 503 }],
    ["a transport error", { kind: "transport", error: new Error("econnreset") }],
    ["a throwing client", "throw"],
  ] as const) {
    it(`swallows ${name} with one log line and no retry`, async () => {
      const client = new StubClient();
      client.outcome = outcome;
      const { logger, lines } = recordingLogger();
      reporter(client, logger).report({ defaultEffort: "low", supportedEfforts: [] }, "m");
      await settle();
      assert.equal(client.posts.length, 1);
      assert.equal(lines.length, 1);
    });
  }
});

describe("WorkerClient.recordCodexCatalogDefault", () => {
  let api: FakeApi;
  let baseUrl: string;
  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    baseUrl = await api.listen();
  });
  afterEach(async () => { await api.close(); });

  const body: CodexCatalogDefaultBody = {
    claim_generation: 3, catalog_context: CTX, model: "m", runtime_version: "0.164.0", default_effort: "low",
  };

  it("posts the body to the run's route and answers ok on 204", async () => {
    const client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger());
    assert.deepEqual(await client.recordCodexCatalogDefault("run-9", body, 2000), { kind: "ok" });
    assert.deepEqual(api.codexCatalogDefaultPosts, [{ runId: "run-9", body }]);
  });

  it("answers http for a refusal and transport for an unreachable server, never throwing", async () => {
    const client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger());
    api.codexCatalogDefault.status = 409;
    assert.deepEqual(await client.recordCodexCatalogDefault("run-9", body, 2000), { kind: "http", status: 409 });
    api.codexCatalogDefault.status = 404;
    assert.deepEqual(await client.recordCodexCatalogDefault("run-9", body, 2000), { kind: "http", status: 404 });
    const gone = new FakeApi(TOKEN);
    const goneUrl = await gone.listen();
    await gone.close();
    const out = await new WorkerClient(goneUrl, TOKEN, "0.1.0-test", nullLogger()).recordCodexCatalogDefault("run-9", body, 500);
    assert.equal(out.kind, "transport");
  });
});
