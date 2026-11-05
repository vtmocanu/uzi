import { describe, it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { PolicyHoldRequest, StateRequest } from "../src/protocol.js";
import { ProviderPolicyRefusal, admitPolicyTurn } from "../src/provider-policy-refusal.js";
import { api, client, fakeGitlab, fx, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";
import { pollUntil } from "./worker-sweep-rig.js";

// The production Worker registration list must advertise provider_policy_hold_v1 only while the outbox
// that journals a hold is usable and the active-run registry is wired: the same two preconditions
// RunRunner.executeClaimInAttempt requires before it reserves a policy flight.
function workerConfig(): Config {
  return {
    workerName: "policy-capability", workerTemplate: "base", pollIntervalMs: 1, heartbeatIntervalMs: 5,
    chatPollMs: 1, chatSessions: 1, maxConcurrentRuns: 1, crossCheckSlots: 0, dockerWiring: {}, dataDir: "/tmp/does-not-matter",
    gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20,
  } as unknown as Config;
}

describe("Worker advertises provider_policy_hold_v1", () => {
  installHarness();
  const refusal = () => new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
    provider: "codex", category: "policy_refusal", origin: "root", policy_tag: "cyberPolicy" });

  async function outboxFor(name: string): Promise<Outbox> {
    const outbox = new Outbox({ root: path.join(fx.dataDir, name), log: nullLogger(),
      runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 86400000 });
    await outbox.init();
    return outbox;
  }

  /** Register through the production Worker (real client, real FakeApi register) and return the runner. */
  async function registered(opts: { outbox: Outbox; activeRuns?: ActiveRunRegistry }): Promise<{
    runner: RunRunner; capabilities: () => string[] | undefined;
  }> {
    api.setRegisterProtocolFeatures(["provider_policy_hold_v1"]);
    let capabilities: string[] | undefined;
    const register = client.register.bind(client);
    client.register = (async (...args: Parameters<typeof register>) => {
      capabilities = args[4];
      return register(...args);
    }) as typeof client.register;
    const runner = runnerWith(() => ({ executor: { run: async () => { throw refusal(); } } }),
      fakeGitlab().gitlab, undefined, nullLogger(), { outbox: opts.outbox, ...(opts.activeRuns ? { activeRuns: opts.activeRuns } : {}) });
    const controller = new AbortController();
    const worker = new Worker(workerConfig(), client, runner, { execute: async () => {} } as unknown as ChatRunner,
      { execute: async () => {} } as unknown as JudgeRunner, { execute: async () => {} } as unknown as ReviewRunner,
      nullLogger(), () => ({ ok: true, missing: [] }), opts.outbox, new Map(), opts.activeRuns);
    const done = worker.run(controller.signal);
    try {
      // The register reply carries the server features; wait for it so shutdown cannot revoke the registration.
      await pollUntil(() => client.hasFeature("provider_policy_hold_v1"), 5000, "worker registration");
    } finally {
      controller.abort();
      await done;
    }
    return { runner, capabilities: () => capabilities };
  }

  it("advertises it, so a root refusal is held and reported instead of failing the run", async () => {
    const outbox = await outboxFor("policy-capability-on");
    const activeRuns = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    const { runner, capabilities } = await registered({ outbox, activeRuns });
    assert.ok(capabilities()?.includes("provider_policy_hold_v1"), "the production registration list carries the capability");
    assert.equal(client.supportsPolicyHold, true, "server feature and worker registration both present");

    const holds: PolicyHoldRequest[] = [];
    const states: StateRequest[] = [];
    client.reportPolicyHold = async (_id, body) => {
      holds.push(structuredClone(body));
      return { disposition: "applied", outcome_id: body.outcome_id, source_generation: body.source_generation,
        observed_status: "paused", observed_generation: 8, hold_reason: "codex_policy_hold" };
    };
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => { states.push(args[1]); return report(...args); };
    await runner.execute(gitlabClaim(4201, { run_id: randomUUID(), claim_generation: 8, plan_approved: true, plan_md: "approved",
      secrets: { forge_pat: "fixture-pat", codex: { auth_mode: "api_key", access_token: "fixture-access" } } }));
    assert.equal(holds.length, 1, "the refusal reached the policy-hold report");
    assert.equal(holds[0]?.detail, "refused");
    assert.equal(holds[0]?.cause, "refused");
    assert.equal(states.some(s => s.status === "failed"), false, "no ordinary terminal failure was reported");
  });

  for (const control of ["disabled outbox", "no active-run registry"] as const) {
    it(`control: ${control} is not advertised and the refusal fails the run as before`, async t => {
      const outbox = await outboxFor("policy-capability-off");
      if (control === "disabled outbox") t.mock.method(outbox, "isDisabled", () => true);
      const activeRuns = control === "no active-run registry" ? undefined
        : new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
      const { runner, capabilities } = await registered({ outbox, ...(activeRuns ? { activeRuns } : {}) });
      assert.ok(capabilities(), "registration happened");
      assert.equal(capabilities()?.includes("provider_policy_hold_v1"), false);
      assert.equal(client.supportsPolicyHold, false, "an unadvertised capability never negotiates");
      let holds = 0;
      client.reportPolicyHold = async () => { holds++; throw new Error("no hold without the capability"); };
      const states: StateRequest[] = [];
      const report = client.reportState.bind(client);
      client.reportState = async (...args) => { states.push(args[1]); return report(...args); };
      await runner.execute(gitlabClaim(4202, { run_id: randomUUID(), claim_generation: 8, plan_approved: true, plan_md: "approved",
        secrets: { forge_pat: "fixture-pat", codex: { auth_mode: "api_key", access_token: "fixture-access" } } }));
      assert.equal(holds, 0);
      assert.equal(states.some(s => s.status === "failed"), true);
    });
  }
});
