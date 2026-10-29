import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { client, fx, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { WorkerClient } from "../src/client.js";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";

describe("compact hold negotiated capability", () => {
  for (const server of [false, true]) for (const worker of [false, true]) {
    it(`requires both server=${server} and registered worker=${worker}`, async () => {
      const api = new FakeApi("policy-negotiation");
      const url = await api.listen();
      const client = new WorkerClient(url, "policy-negotiation", "test", nullLogger());
      try {
        api.setRegisterProtocolFeatures(server ? ["provider_policy_hold_v1"] : []);
        await client.register("policy-test", "base", 1, [],
          worker ? ["provider_policy_hold_v1"] : [], undefined, 0);
        assert.equal(client.supportsPolicyHold, server && worker);
        // A heartbeat feature alone cannot invent an image's registered capability.
        client.protocolFeatures = ["provider_policy_hold_v1"];
        assert.equal(client.supportsPolicyHold, worker);
        await client.register("policy-test", "base", 1, [], [], undefined, 0);
        assert.equal(client.supportsPolicyHold, false, "re-registration replaces capabilities");
      } finally {
        await api.close();
      }
    });
  }
});


describe("policy admission on every production claim", () => {
  installHarness();
  const original: PolicyHoldRequest = {
    outcome_id: "22222222-2222-4222-8222-222222222222", source_generation: 1, claim_generation: 1,
    detail: "refused", continuation_mode: "same_conversation", published: false,
    tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "b".repeat(32) + "-1",
    session_id: "original-session",
  };
  async function pendingOutbox(): Promise<Outbox> {
    const outbox = new Outbox({ root: path.join(fx.dataDir, "policy-admission"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 128 * 1024 * 1024, retentionMs: 86400000 });
    await outbox.init();
    return outbox;
  }

  for (const attempt of [false, true]) it(`first claim reholds at actual ${attempt ? "attempt" : "compatible"} mode before session drop`, async () => {
    const outbox = await pendingOutbox();
    const claim = gitlabClaim(3701, { claim_generation: 3, session_id: "original-session",
      plan_approved: true, plan_md: "approved", plan_source: "seeded" });
    await outbox.journalPolicyHold(claim.run_id, original);
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 10);
    const packets: PolicyHoldRequest[] = [];
    client.getRunOwnership = async () => ({ status: "running", claim_generation: 3, claim_released: false }) as never;
    client.reportPolicyHold = async (_id, body) => {
      packets.push(body);
      if (body.claim_generation === 1)
        return { disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 };
      assert.equal(body.continuation_mode, attempt ? "fresh_from_saved_work" : "same_conversation");
      return { disposition: "applied", outcome_id: body.outcome_id, source_generation: 1,
        observed_status: "paused", observed_generation: 3, hold_reason: "codex_policy_hold" };
    };
    const { gitlab } = fakeGitlab();
    let models = 0;
    const states: unknown[] = [];
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => { states.push(args[1]); return report(...args); };
    const runner = runnerWith(() => ({ executor: { run: async () => { models++; throw new Error("unexpected model"); } } }),
      gitlab, undefined, nullLogger(), { outbox, activeRuns: registry, ...(attempt ? { dockerHost: "unix:///fixture" } : {}) });
    const own = client.getRunOwnership;
    client.getRunOwnership = async id => {
      if (packets.some(p => p.claim_generation === 3)) return { status: "paused", claim_generation: 3, claim_released: true } as never;
      return own(id);
    };
    await runner.execute(claim);
    assert.equal(models, 0);
    assert.deepEqual(packets.at(-1), {
      outcome_id: original.outcome_id, source_generation: 1, claim_generation: 3, detail: "resume_rejected",
      continuation_mode: attempt ? "fresh_from_saved_work" : "same_conversation", published: false,
    });
    assert.equal(states.length, 0, "admission latch suppresses ordinary running and session-drop writes");
    assert.deepEqual((await outbox.readTerminalJournal(claim.run_id, 1))?.body, original);
    assert.equal(outbox.listPendingTerminals()[0]?.server_settled, true);
  });

  for (const corrupt of [false, true]) it(`first and duplicate claims block unavailable original (corrupt=${corrupt}) before factory`, async () => {
    const outbox = await pendingOutbox();
    const claim = gitlabClaim(3702, { claim_generation: 3 });
    await outbox.journalPolicyHold(claim.run_id, original);
    const root = path.join(fx.dataDir, "policy-admission", claim.run_id);
    if (corrupt) {
      const names = await fs.readdir(root);
      const terminal = names.find(n => n.startsWith("terminal"));
      assert.ok(terminal);
      await fs.writeFile(path.join(root, terminal), "{}");
    } else {
      outbox.readTerminalJournal = async () => undefined;
    }
    const { gitlab } = fakeGitlab();
    let factories = 0;
    client.getRunOwnership = async () => ({ status: "running", claim_generation: 3, claim_released: false }) as never;
    client.reportPolicyHold = async (_id, body) => ({ disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 });
    const runner = runnerWith(() => { factories++; throw new Error("factory forbidden"); }, gitlab,
      undefined, nullLogger(), { outbox });
    await Promise.all([runner.execute(claim), runner.execute(claim)]);
    assert.equal(factories, 0);
    assert.equal(await outbox.hasPendingPolicyOutcome(claim.run_id), true);
  });

  it("already applied owner action settles original without a new refusal or rehold", async () => {
    const outbox = await pendingOutbox();
    const claim = gitlabClaim(3703, { claim_generation: 3 });
    await outbox.journalPolicyHold(claim.run_id, original);
    let sends = 0;
    client.reportPolicyHold = async (_id, body) => {
      sends++;
      assert.deepEqual(body, original);
      return { disposition: "already_applied", outcome_id: body.outcome_id, source_generation: 1,
        observed_status: "cancelled", observed_generation: 3 };
    };
    client.getRunOwnership = async () => ({ status: "cancelled", claim_generation: 3, claim_released: true }) as never;
    const { gitlab } = fakeGitlab();
    let models = 0;
    client.reportState = async () => ({ applied: false, status: "cancelled" });
    const runner = runnerWith(() => ({ executor: { run: async () => { models++; throw new Error("cancelled model"); } } }),
      gitlab, undefined, nullLogger(), { outbox });
    await runner.execute(claim);
    await runner.execute(claim);
    assert.equal(sends, 1);
    assert.equal(models, 0, "existing terminal running-ACK guard still refuses a stale claim");
    assert.deepEqual((await outbox.readTerminalJournal(claim.run_id, 1))?.body, original);
    assert.equal(outbox.listPendingTerminals()[0]?.server_settled, true);
  });

  it("resolved authenticated policy history alone does not constrain a later ordinary claim", async () => {
    const outbox = await pendingOutbox();
    const claim = gitlabClaim(3706, { claim_generation: 3, plan_approved: true, plan_md: "approved" });
    await outbox.journalPolicyHold(claim.run_id, original);
    const observed = await outbox.readTerminalJournalForRetirement(claim.run_id, 1);
    assert.ok(observed);
    await outbox.markTerminalServerSettled(claim.run_id, 1, observed.context);
    let probesBeforeModel = 0, models = 0, policyReports = 0;
    client.getRunOwnership = async () => {
      if (models === 0) probesBeforeModel++;
      return { status: "running", claim_generation: 3, claim_released: false };
    };
    client.reportPolicyHold = async () => { policyReports++; throw new Error("resolved policy is not resent"); };
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async ctx => { models++; return { branch: ctx.branch }; } } }),
      gitlab, undefined, nullLogger(), { outbox });
    await runner.execute(claim);
    assert.equal(models, 1);
    assert.equal(probesBeforeModel, 0, "resolved history introduces no policy admission probe");
    assert.equal(policyReports, 0);
  });

  for (const proof of [
    { status: "running", claim_generation: 3, claim_released: true },
    { status: "running", claim_generation: 3 },
    { status: "running", claim_generation: 4, claim_released: false },
    null,
  ]) it(`pending policy never converts or models without exact ownership: ${JSON.stringify(proof)}`, async () => {
    const outbox = await pendingOutbox();
    const claim = gitlabClaim(3704, { claim_generation: 3 });
    await outbox.journalPolicyHold(claim.run_id, original);
    client.getRunOwnership = async () => {
      if (proof === null) throw new Error("ownership unavailable");
      return proof as never;
    };
    const packets: PolicyHoldRequest[] = [];
    client.reportPolicyHold = async (_id, body) => {
      packets.push(body);
      return { disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 };
    };
    const { gitlab } = fakeGitlab();
    let models = 0;
    const runner = runnerWith(() => ({ executor: { run: async () => { models++; throw new Error("unexpected model"); } } }),
      gitlab, undefined, nullLogger(), { outbox });
    await runner.execute(claim);
    assert.equal(models, 0);
    assert.deepEqual(packets, [original]);
    assert.deepEqual((await outbox.readTerminalJournal(claim.run_id, 1))?.body, original);
  });

  it("all original policy sources must settle before model admission", async () => {
    const outbox = await pendingOutbox();
    const claim = gitlabClaim(3705, { claim_generation: 3 });
    const second: PolicyHoldRequest = { ...original, outcome_id: "33333333-3333-4333-8333-333333333333",
      source_generation: 2, claim_generation: 2 };
    await outbox.journalPolicyHold(claim.run_id, original);
    await outbox.journalPolicyHold(claim.run_id, second);
    client.getRunOwnership = async () => ({ status: "running", claim_generation: 3, claim_released: false }) as never;
    const packets: PolicyHoldRequest[] = [];
    client.reportPolicyHold = async (_id, body) => {
      packets.push(body);
      if (body.claim_generation === 3 && body.source_generation === 1)
        return { disposition: "applied", outcome_id: body.outcome_id, source_generation: 1,
          observed_status: "paused", observed_generation: 3, hold_reason: "codex_policy_hold" };
      return { disposition: "pending", outcome_id: body.outcome_id, source_generation: body.source_generation };
    };
    const { gitlab } = fakeGitlab();
    let models = 0;
    const runner = runnerWith(() => ({ executor: { run: async () => { models++; throw new Error("unexpected model"); } } }),
      gitlab, undefined, nullLogger(), { outbox });
    await runner.execute(claim);
    assert.equal(models, 0);
    assert.deepEqual(packets.filter(p => p.claim_generation === 3).map(p => p.source_generation), [1, 2]);
    assert.equal(await outbox.hasPendingPolicyOutcome(claim.run_id), true);
    assert.deepEqual((await outbox.readTerminalJournal(claim.run_id, 1))?.body, original);
    assert.deepEqual((await outbox.readTerminalJournal(claim.run_id, 2))?.body, second);
  });

});
