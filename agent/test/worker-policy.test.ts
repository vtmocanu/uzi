import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { StatsCollector } from "../src/stats.js";
import type { ActiveSnapshot, OutboxHeartbeatEntry, PolicyHoldRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const dirs: string[] = [];
afterEach(async () => { for (const dir of dirs.splice(0)) await fs.rm(dir, { recursive: true, force: true }); });
async function setup() {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/worker-policy-")); dirs.push(root);
  const box = new Outbox({ root, log: nullLogger(), runMaxBytes: 1024, maxBytes: 2048, retentionMs: 1 }); await box.init();
  await box.journalPolicy("r", { outcome_id: "11111111-1111-4111-8111-111111111111",
    source_generation: 1, detail: "resume_rejected", head: "", published: false });
  return box;
}
function makeWorker(box: Outbox, client: WorkerClient, reg: ActiveRunRegistry) {
  const config = { workerName: "test", pollIntervalMs: 1, heartbeatIntervalMs: 5, chatPollMs: 1,
    chatSessions: 0, crossCheckSlots: 0, maxConcurrentRuns: 1, dockerWiring: {} } as unknown as Config;
  const runner = { resumePendingRecoveries: async () => {}, settlePendingPredecessors: async () => {},
    execute: async () => { assert.fail("policy evidence must never execute"); } } as unknown as RunRunner;
  return new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), () => ({ ok: true, missing: [] }), box, undefined, reg, undefined, undefined, undefined,
    undefined, () => ({ collect: () => undefined }) as unknown as StatsCollector);
}
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

it("pre-register delivery precedes registration and claims while heartbeat reports retained policy count", async () => {
  const box = await setup();
  const reg = new ActiveRunRegistry(() => box.listPendingTerminals(), () => 8, () => box.listPendingPolicies());
  const controller = new AbortController();
  const sent = deferred(), release = deferred(), claimed = deferred();
  let claims = 0, sends = 0;
  let registerSnapshot: ActiveSnapshot | undefined;
  let capabilities: string[] | undefined;
  let depths: OutboxHeartbeatEntry[] | undefined;
  const client = {
    register: async (_n: string, _t: string, _max: number, _c: string[], caps: string[], snapshot: ActiveSnapshot) => {
      capabilities = caps; registerSnapshot = snapshot; return {};
    },
    hasFeature: () => true, supportsPolicyHold: () => true,
    heartbeat: async (_stats: unknown, entries: OutboxHeartbeatEntry[]) => { depths = entries; },
    reportPolicyHold: async (_run: string, b: PolicyHoldRequest) => {
      sends++; sent.resolve(); await release.promise;
      return { disposition: "applied", outcome_id: b.outcome_id, claim_generation: b.claim_generation,
        status: "paused", hold_reason: "provider_policy_refusal" };
    },
    claimRun: async () => { claims++; claimed.resolve(); controller.abort(); return null; },
  } as unknown as WorkerClient;
  const done = makeWorker(box, client, reg).run(controller.signal);
  try {
    await sent.promise;
    assert.equal(claims, 0);
    assert.equal(registerSnapshot, undefined, "snapshot must freeze after bounded identity reconciliation");
    release.resolve();
    await claimed.promise;
  } finally { release.resolve(); controller.abort(); await done; }
  assert.equal(sends, 1);
  assert.deepEqual(registerSnapshot!.pending_all, []);
  assert.equal(registerSnapshot!.pending_overflow, false);
  assert.equal(depths![0]!.pending_policy, 1);
  assert.equal(depths![0]!.pending_terminal, 0);
  assert.equal(capabilities!.includes("provider_policy_hold_v1"), true);
  assert.equal(box.listPendingPolicies()[0]!.server_settled, true);
  assert.equal(box.hasPendingPolicy("r"), true);
});

it("unresolved pre-register evidence rides the first registration snapshot", async () => {
  const box = await setup();
  const reg = new ActiveRunRegistry(undefined, () => 8, () => box.listPendingPolicies());
  const controller = new AbortController();
  let sends = 0;
  let snapshot: ActiveSnapshot | undefined;
  const client = {
    register: async (_n: unknown, _t: unknown, _m: unknown, _c: unknown, _caps: unknown, s: ActiveSnapshot) => {
      assert.equal(sends, 1); snapshot = s; return {};
    },
    hasFeature: () => true, supportsPolicyHold: () => false,
    heartbeat: async () => {},
    reportPolicyHold: async (_id: string, b: PolicyHoldRequest) => {
      sends++;
      return { disposition: sends === 1 ? "pending" : "applied", outcome_id: b.outcome_id,
        claim_generation: b.claim_generation, status: "paused", hold_reason: "provider_policy_refusal" };
    },
    claimRun: async () => { controller.abort(); return null; },
  } as unknown as WorkerClient;
  await makeWorker(box, client, reg).run(controller.signal);
  assert.deepEqual(snapshot!.pending_all, [{ run_id: "r", claim_generation: 1 }]);
  assert.equal(snapshot!.pending_overflow, true);
  assert.equal(sends, 2);
});

for (const unknown of [false, true]) it(`pre-register exact identity reconciliation preserves finalize only with known policy evidence: unknown=${unknown}`, async () => {
  const box = await setup();
  const observed = await box.readPolicyForRetirement("r", 1); assert.ok(observed);
  assert.equal((await box.bindPolicyDelivery("r", 1, 2, observed.context.expectedIdentity!)).bound, true);
  await box.journalFinalize("r", 2);
  if (unknown) await fs.writeFile(path.join(dirs.at(-1)!, "r", "policy-unknown.json"), "{}");
  const reg = new ActiveRunRegistry(undefined, () => 8, () => box.listPendingPolicies());
  const controller = new AbortController();
  let snapshot: ActiveSnapshot | undefined;
  let sends = 0, sendsAtRegistration = -1;
  const client = {
    register: async (_n: unknown, _t: unknown, _m: unknown, _c: unknown, _caps: unknown, s: ActiveSnapshot) => {
      sendsAtRegistration = sends;
      snapshot = s;
      return {};
    },
    hasFeature: () => true, supportsPolicyHold: () => false,
    heartbeat: async () => {},
    reportPolicyHold: async (_id: string, b: PolicyHoldRequest) => {
      sends++;
      assert.equal(b.source_generation, 1);
      assert.equal(b.claim_generation, 2);
      return { disposition: "already_applied", outcome_id: b.outcome_id,
        claim_generation: 2, status: "running" };
    },
    claimRun: async () => { controller.abort(); return null; },
  } as unknown as WorkerClient;
  await makeWorker(box, client, reg).run(controller.signal);
  assert.equal(sendsAtRegistration, 1, "reconcile before freezing the register attestation");
  assert.deepEqual(snapshot!.finalize_resume, unknown ? undefined : [{ run_id: "r", claim_generation: 2 }]);
  assert.equal(snapshot!.pending_overflow, false);
  assert.equal(box.hasPendingPolicy("r"), true);
  assert.equal(await box.hasPhysicalTerminalProtection("r"), true);
});

it("heartbeat retries policy-only pending without drain and leases it while other claims remain available", async () => {
  const box = await setup();
  const controller = new AbortController();
  let sends = 0, claims = 0, ticks = 0;
  const reg = new ActiveRunRegistry(undefined, () => 8, () => box.listPendingPolicies());
  const client = {
    register: async () => ({}), hasFeature: () => true, supportsPolicyHold: () => true,
    heartbeat: async () => { ticks++; },
    reportPolicyHold: async (_id: string, b: PolicyHoldRequest) => {
      sends++;
      if (sends >= 5) controller.abort();
      return { disposition: "pending", outcome_id: b.outcome_id, claim_generation: b.claim_generation, status: "running" };
    },
    claimRun: async () => { claims++; return null; },
  } as unknown as WorkerClient;
  await makeWorker(box, client, reg).run(controller.signal);
  assert.ok(claims > 0, "a leased pending run must not block unrelated claims within the cap");
  assert.ok(ticks >= 2);
  assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
  assert.equal(box.hasPendingPolicy("r"), true);
});

it("completed message drain re-drives policy delivery independently of heartbeat sweep", async () => {
  const box = await setup();
  await box.appendSegment("r", 1, [{ seq: 1, kind: "status", payload: {} }]);
  let sends = 0;
  const reg = new ActiveRunRegistry(undefined, () => 8, () => box.listPendingPolicies());
  const client = {
    supportsPolicyHold: () => true,
    heartbeat: async () => { assert.fail("post-drain retry must not wait for a heartbeat"); },
    postMessages: async () => {},
    reportPolicyHold: async (_id: string, b: PolicyHoldRequest) => {
      sends++;
      const pending = box.hasUndrainedMessages("r");
      return { disposition: pending ? "pending" : "applied", outcome_id: b.outcome_id,
        claim_generation: b.claim_generation, status: "paused", hold_reason: "provider_policy_refusal" };
    },
  } as unknown as WorkerClient;
  const worker = makeWorker(box, client, reg) as unknown as {
    sweepPendingPolicies(signal?: AbortSignal): Promise<void>;
    drainOutbox(signal?: AbortSignal): Promise<void>;
  };
  await worker.sweepPendingPolicies();
  assert.equal(sends, 1);
  assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
  await worker.drainOutbox();
  assert.equal(sends, 2);
  assert.equal(box.hasUndrainedMessages("r"), false);
  assert.equal(box.listPendingPolicies()[0]!.server_settled, true);
});

it("sweeps skip live runner delivery ownership", async () => {
  const box = await setup();
  const controller = new AbortController();
  let sends = 0, ticks = 0, claims = 0;
  const reg = new ActiveRunRegistry(undefined, () => 8, () => box.listPendingPolicies()); reg.add("r", 1);
  const client = {
    register: async () => ({}), hasFeature: () => true, supportsPolicyHold: () => true,
    heartbeat: async () => { if (++ticks === 3) controller.abort(); },
    reportPolicyHold: async () => { sends++; assert.fail("live runner owns delivery"); },
    claimRun: async () => { claims++; return null; },
  } as unknown as WorkerClient;
  await makeWorker(box, client, reg).run(controller.signal);
  assert.equal(sends, 0);
  assert.ok(claims > 0, "live delivery ownership does not prohibit unrelated claims");
  assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
});
