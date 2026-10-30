import { acquireCodexOperationScope } from "../src/codex-operation-scope.js";
import { ProviderPolicyRefusal, admitPolicyTurn } from "../src/provider-policy-refusal.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { api, client, fx, git, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { WorkerClient } from "../src/client.js";
import { FakeApi } from "./fake-api.js";
import { nullLogger, noProofReseed } from "./helpers.js";

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

  it("an unwired retained source reholds at its actual attempt path before recovery blockers", async () => {
    const claim = gitlabClaim(3707, { claim_generation: 3, session_id: "original-session" });
    const bare = await git.ensureClone(fx.originPath);
    const clone = await git.createOrAttachRunnerClone(bare, 3707, noProofReseed, claim.run_id);
    await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
    await fs.writeFile(path.join(clone.path, "retained.txt"), "keep this work");
    await git.reserveRecoveryIteration(bare, clone.branch, "issue-3707", { runId: claim.run_id, clonePath: clone.path }, 1);
    await git.blockRecoveryEpisode(bare, clone.branch, "issue-3707", { runId: claim.run_id, clonePath: clone.path }, "decoded_history_limit");
    const before = await git.discoverRetainedRecovery(fx.originPath, clone.branch, "issue-3707", claim.run_id);
    const outbox = await pendingOutbox();
    await outbox.journalPolicyHold(claim.run_id, original);
    client.getRunOwnership = async () => ({ status: packets.some(p => p.claim_generation === 3) ? "paused" : "running", claim_generation: 3, claim_released: false });
    const packets: PolicyHoldRequest[] = [];
    client.reportPolicyHold = async (_id, body) => {
      packets.push(body);
      return body.claim_generation === 3
        ? { disposition: "applied", outcome_id: body.outcome_id, source_generation: 1,
            observed_status: "paused", observed_generation: 3, hold_reason: "codex_policy_hold" }
        : { disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 };
    };
    let models = 0;
    await runnerWith(() => ({ executor: { run: async () => { models++; throw new Error("unexpected model"); } } }),
      fakeGitlab().gitlab, undefined, nullLogger(), { outbox }).execute(claim);
    assert.equal(models, 0);
    assert.equal(await fs.readFile(path.join(clone.path, "retained.txt"), "utf8"), "keep this work");
    assert.deepEqual(packets.at(-1), {
      outcome_id: original.outcome_id, source_generation: 1, claim_generation: 3,
      detail: "resume_rejected", continuation_mode: "fresh_from_saved_work", published: false,
    });
    assert.deepEqual((await outbox.readTerminalJournal(claim.run_id, 1))?.body, original);
    const after = await git.discoverRetainedRecovery(fx.originPath, clone.branch, "issue-3707", claim.run_id);
    assert.deepEqual(after?.journal, before?.journal, "policy reconciliation consumes no recovery reservation or source");
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

  for (const delayed of [false, true]) for (const replace of [false, true])
    it(`R1 ${delayed ? "delayed" : "immediate"} handoff removes only its own fallback (replacement=${replace})`, async t => {
      api.setRegisterProtocolFeatures(["provider_policy_hold_v1", "claim_generation_fence", "codex_account_operation_admission_v1"]);
      await client.register("policy-fixture", "base", 1, [], ["provider_policy_hold_v1"], undefined, 0);
      let admissionLeases = 0;
      const retain = client.retainCodexOperationFlight.bind(client);
      t.mock.method(client, "retainCodexOperationFlight", (...args: Parameters<typeof retain>) => {
        admissionLeases++;
        const release = retain(...args);
        return () => { admissionLeases--; release(); };
      });
      t.mock.method(client, "checkCodexOperationAdmission", async (...[_id, request]: Parameters<typeof client.checkCodexOperationAdmission>) => ({
        decision: "admit" as const, operation_id: request.operation_id, claim_generation: request.claim_generation,
      }));
      const outbox = await pendingOutbox();
      const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 10);
      const claim = gitlabClaim(3710, { claim_generation: 3, plan_approved: true, plan_md: "approved",
        secrets: { forge_pat: "fixture-pat", codex: { auth_mode: "subscription", access_token: "fixture-access" } } });
      let fail = delayed;
      const journal = outbox.journalPolicyHold.bind(outbox);
      t.mock.method(outbox, "journalPolicyHold", async (...args: Parameters<typeof journal>) => {
        if (fail) throw new Error("fixture metadata unavailable");
        return journal(...args);
      });
      let replacement: symbol | undefined;
      const mark = registry.markPolicyPendingOnly.bind(registry);
      t.mock.method(registry, "markPolicyPendingOnly", (...args: Parameters<typeof mark>) => {
        assert.ok(registry.setAccountCooldown(claim.run_id, 3,
          { operation_id: "11111111-1111-4111-8111-111111111111", kind: "root" }, args[2]!));
        const converted = mark(...args);
        if (replace && !delayed) replacement = registry.add(claim.run_id, 3);
        return converted;
      });
      let companion: ReturnType<typeof acquireCodexOperationScope> | undefined;
      let permit: (() => boolean | void) | undefined;
      client.reportPolicyHold = async (_id, body) => ({ disposition: "applied", outcome_id: body.outcome_id,
        source_generation: body.source_generation, observed_status: "paused", observed_generation: 3,
        hold_reason: "codex_policy_hold" });
      const r = runnerWith(() => ({ executor: { run: async () => {
        companion = acquireCodexOperationScope(client, claim.run_id, 3);
        permit = await companion.prepare("root", new AbortController().signal, () => {});
        assert.equal(permit(), true);
        assert.equal(admissionLeases, 1, "the negotiated operation acquired a real admission lease");
        throw new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
          provider: "codex", category: "policy_refusal", origin: "root", policy_tag: "cyberPolicy" });
      } } }), fakeGitlab().gitlab, undefined, nullLogger(), { outbox, activeRuns: registry });
      try {
        await r.execute(claim);
        assert.ok(permit, "the flight reached the provider seam");
        assert.throws(() => permit!(), /scope closed|owner closed/, "teardown closes operation authority even with a fallback");
        companion!.close();
        assert.equal(admissionLeases, 0, "a retained policy fallback keeps no admission lease after both operation users close");
        if (delayed) {
          assert.equal(registry.has(claim.run_id), false);
          assert.equal(registry.size, 1);
          assert.equal(registry.build().active.some(e => !e.terminal_pending || e.account_cooldown), false);
          assert.equal(registry.build().pending_overflow, true);
          if (replace) replacement = registry.add(claim.run_id, 3);
          fail = false;
          await r.retryPolicyHandoffs();
          // The existing sweep can now settle the durable original.
          await r.execute(claim);
        }
        assert.equal(outbox.listPendingTerminals()[0]?.server_settled, true);
        assert.equal(registry.size, replace ? 1 : 0);
        assert.equal(registry.has(claim.run_id), replace);
        if (replacement) assert.ok(registry.setAccountCooldown(claim.run_id, 3,
          { operation_id: "11111111-1111-4111-8111-111111111111", kind: "root" }, replacement));
      } finally { companion?.close(); }
    });

});
