import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runnerWith, secretRecordingLogger } from "./runner-harness.js";

installHarness();

// PRD #4320 module 6/9: the runner registers a provider run's gateway key everywhere the OAuth token is
// registered. The server has no value-based scrubber for an arbitrary gateway key, so the worker's
// redactors are the only protection: a key surfacing in a tool output, a log line or a failure reason
// must be scrubbed. The key is assembled at runtime (no provider-token-shaped literal in source).
const KEY = ["gw", "key", "do-not-scan", "7777", "abcdefgh"].join("-");

function providerBinding(): ClaimProviderBinding {
  return {
    provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a", auth_style: "bearer",
    credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
  };
}

const providerSecrets = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: providerBinding() });

function factoryRunning(body: (ctx: RunContext) => Promise<ExecutorResult>, homeRoot: string): ExecutorFactory {
  return (runId) => ({ homeDir: path.join(homeRoot, runId), executor: { run: body } });
}

describe("provider-bound issue run: redaction and the gateway-limit park", () => {
  it("registers the gateway key with the logger and evicts it; scrubs it from payloads and the failure reason", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prov-redact-"));
    try {
      const { logger, added, removed } = secretRecordingLogger();
      const claim = gitlabClaim(931, { secrets: providerSecrets() });
      await runnerWith(factoryRunning(async (ctx) => {
        ctx.emit({ kind: "status", agent: "lead", payload: { text: `tool output: curl -H "Authorization: Bearer ${KEY}"` } });
        throw new Error(`gateway rejected key ${KEY}`);
      }, homeRoot), gitlab, undefined, logger).execute(claim);
      assert.ok(added.includes(KEY), "the gateway key was registered with the logger");
      assert.ok(removed.includes(KEY), "and evicted when the run ended");
      const feed = JSON.stringify(api.messages(claim.run_id));
      assert.ok(!feed.includes(KEY), "the key never reaches a run message");
      assert.ok(feed.includes("REDACTED"), "the secret was replaced, not dropped silently");
      const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
      assert.ok(failed, "the run failed");
      assert.ok(!String(failed.failure_reason).includes(KEY), "nor the failure reason");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("scrubs the escaped-slash JSON form and the percent-encoded form a gateway may echo", async () => {
    const SLASH_KEY = ["gw", "key", "do-not-scan", "9191", "ab/cd/efgh"].join("-");
    const jsonSlash = SLASH_KEY.replaceAll("/", "\\/");
    const pct = encodeURIComponent(SLASH_KEY);
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prov-enc-"));
    try {
      const { logger, added, removed } = secretRecordingLogger();
      const claim = gitlabClaim(933, { secrets: { ...providerSecrets(), provider: { ...providerBinding(), credential: SLASH_KEY } } });
      await runnerWith(factoryRunning(async (ctx) => {
        ctx.emit({ kind: "status", agent: "lead", payload: { text: `body: {"key":"${jsonSlash}"} url=?k=${pct}` } });
        throw new Error(`gateway rejected {"key":"${jsonSlash}"} k=${pct}`);
      }, homeRoot), gitlab, undefined, logger).execute(claim);
      const feed = JSON.stringify(api.messages(claim.run_id));
      assert.ok(!feed.includes(jsonSlash) && !feed.includes(pct) && !feed.includes(SLASH_KEY), "no encoding of the key reaches a run message");
      const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
      assert.ok(failed, "the run failed");
      const reason = String(failed.failure_reason);
      assert.ok(!reason.includes(jsonSlash) && !reason.includes(pct), "nor the failure reason");
      assert.ok(added.includes(jsonSlash) && added.includes(pct), "every form was registered with the logger");
      assert.deepEqual([...removed].sort(), [...added].sort(), "and each evicted symmetrically");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("a gateway limit parks as limit_wait with rate_limit_type provider_gateway_limit and NO limit_resets_at", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prov-limit-"));
    try {
      const claim = gitlabClaim(932, { wait_on_limit: true, secrets: providerSecrets() });
      await runnerWith(factoryRunning(async () => {
        throw new LimitReachedError({ rateLimitType: "provider_gateway_limit", detail: "provider gateway limit" });
      }, homeRoot), gitlab).execute(claim);
      const parked = api.states.find((s) => s.runId === claim.run_id && s.body.status === "limit_wait");
      assert.ok(parked, "the worker reported limit_wait");
      assert.equal(parked.body.rate_limit_type, "provider_gateway_limit");
      assert.equal(parked.body.limit_resets_at, undefined, "no reset time is invented for a gateway");
      const feed = api.messages(claim.run_id).find((m) => m.kind === "limit_wait");
      assert.equal(feed?.payload?.rate_limit_type, "provider_gateway_limit");
      assert.ok(!("resets_at" in (feed?.payload ?? {})));
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
