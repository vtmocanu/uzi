import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { resolvePendingPolicy } from "../src/policy-resolve.js";
import { PolicyHoldUnsupported } from "../src/client.js";
import type { PolicyHoldAck, PolicyHoldRequest } from "../src/protocol.js";
import { nullLogger, recordingLogger } from "./helpers.js";

const dirs: string[] = [];
afterEach(async () => { for (const dir of dirs.splice(0)) await fs.rm(dir, { recursive: true, force: true }); });
async function setup() {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/policy-resolver-")); dirs.push(root);
  const options = { root, log: nullLogger(), runMaxBytes: 1024, maxBytes: 2048, retentionMs: 1 };
  const box = new Outbox(options); await box.init();
  await box.journalPolicy("r", { outcome_id: "11111111-1111-4111-8111-111111111111",
    source_generation: 1, detail: "resume_rejected", head: "", published: false });
  return { box, restart: async () => { const next = new Outbox(options); await next.init(); return next; } };
}
const args = { runId: "r", sourceGeneration: 1 };
function client(send: (body: PolicyHoldRequest) => Promise<PolicyHoldAck>) {
  return { supportsPolicyHold: () => true, reportPolicyHold: async (_id: string, body: PolicyHoldRequest) => send(body) };
}
const ack = (body: PolicyHoldRequest, disposition: PolicyHoldAck["disposition"] = "applied"): PolicyHoldAck =>
  ({ disposition, outcome_id: body.outcome_id, claim_generation: body.claim_generation,
    status: disposition === "terminal" ? "cancelled" : "paused", hold_reason: "provider_policy_refusal" });

it("exact authenticated ACK settles inventory only, retains source, and restart restores pending", async () => {
  for (const disposition of ["applied", "already_applied", "resolved_newer_policy", "terminal", "recorded"] as const) {
    const { box, restart } = await setup();
    const result = await resolvePendingPolicy({ outbox: box, client: client(async b => ack(b, disposition)), log: nullLogger() }, args);
    assert.equal(result.kind, "observed");
    if (result.kind !== "observed") return;
    assert.equal(result.observation.request.detail, "resume_rejected");
    assert.equal(result.ack.disposition, disposition);
    assert.equal(box.hasPendingPolicy("r"), true);
    const reg = new ActiveRunRegistry(undefined, () => 1, () => box.listPendingPolicies());
    assert.equal(reg.build().active.length, 0);
    const next = await restart();
    assert.equal(new ActiveRunRegistry(undefined, () => 1, () => next.listPendingPolicies()).build().active.length, 1);
  }
});

it("unknown/unsupported/pending remain evidence and do not settle or send terminal state", async () => {
  for (const mode of ["pending", "unauthenticated_historical_outcome", "unknown", "unsupported", "error"] as const) {
    const { box } = await setup();
    const { logger, lines } = recordingLogger();
    const result = await resolvePendingPolicy({ outbox: box, log: logger, client: client(async b => {
      if (mode === "unsupported") throw new PolicyHoldUnsupported();
      if (mode === "error") throw new Error("sensitive");
      return ack(b, mode === "pending" || mode === "unauthenticated_historical_outcome" ? mode as PolicyHoldAck["disposition"] : mode === "unknown" ? "superseded" as "pending" : "applied");
    }) }, args);
    assert.equal(result.kind, mode === "unsupported" ? "unsupported" : mode === "pending" || mode === "unauthenticated_historical_outcome" ? "observed" : "pending");
    assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
    assert.equal(JSON.stringify(lines).includes("sensitive"), false);
    assert.equal(box.hasPendingTerminal("r", 1), false);
  }
  const { box } = await setup();
  const result = await resolvePendingPolicy({ outbox: box, log: nullLogger(),
    client: client(async b => ({ ...ack(b), claim_generation: 99 })) }, args);
  assert.equal(result.kind, "pending");
  assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
});

it("singleflight skips duplicate sends and delayed ACK cannot settle a rebound delivery identity", async () => {
  const { box } = await setup();
  let started!: () => void;
  const sending = new Promise<void>(r => { started = r; });
  let release!: () => void;
  const wait = new Promise<void>(r => { release = r; });
  let calls = 0;
  const deps = { outbox: box, log: nullLogger(), client: client(async b => { calls++; started(); await wait; return ack(b); }) };
  const first = resolvePendingPolicy(deps, args); await sending;
  assert.equal((await resolvePendingPolicy(deps, args)).kind, "busy");
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  assert.equal((await box.bindPolicyDelivery("r", 1, 2, observation.context.expectedIdentity!)).bound, true);
  release(); await first;
  assert.equal(calls, 1);
  assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
  let request: PolicyHoldRequest | undefined;
  await resolvePendingPolicy({ ...deps, client: client(async b => { request = b; return ack(b); }) }, args);
  assert.equal(request!.source_generation, 1);
  assert.equal(request!.claim_generation, 2);
});

it("a durable intent replays after feature-cache loss without admitting a new refusal", async () => {
  const { box } = await setup();
  let sends = 0;
  const sender = client(async b => { sends++; return ack(b); });
  sender.supportsPolicyHold = () => false;
  const result = await resolvePendingPolicy({ outbox: box, log: nullLogger(), client: sender }, args);
  assert.equal(result.kind, "observed");
  assert.equal(sends, 1);
  assert.equal(box.listPendingPolicies()[0]!.server_settled, true);
});

it("the awaited boot gate joins a heartbeat delivery already in flight", async () => {
  const { box } = await setup();
  let begin!: () => void, release!: () => void;
  const begun = new Promise<void>(r => { begin = r; });
  const held = new Promise<void>(r => { release = r; });
  let sends = 0, reads = 0;
  let readSecond!: () => void;
  const secondRead = new Promise<void>(r => { readSecond = r; });
  const read = box.readPolicyForRetirement.bind(box);
  box.readPolicyForRetirement = async (...parameters) => {
    const observation = await read(...parameters);
    if (++reads === 2) readSecond();
    return observation;
  };
  const deps = { outbox: box, log: nullLogger(), client: client(async b => {
    sends++; begin(); await held; return ack(b);
  }) };
  const first = resolvePendingPolicy(deps, args);
  await begun;
  let returned = false;
  const boot = resolvePendingPolicy(deps, { ...args, waitForInFlight: true })
    .then(r => { returned = true; return r; });
  await secondRead;
  await new Promise<void>(r => setImmediate(r));
  try { assert.equal(returned, false, "boot must not skip an outstanding delivery"); }
  finally { release(); }
  assert.equal((await first).kind, "observed");
  assert.equal((await boot).kind, "observed");
  assert.equal(sends, 1);
});

it("cancel is propagated without settlement", async () => {
  const { box } = await setup();
  const controller = new AbortController();
  await assert.rejects(resolvePendingPolicy({ outbox: box, log: nullLogger(), client: client(async () => {
    controller.abort(new Error("cancel")); throw new Error("transport");
  }) }, { ...args, signal: controller.signal }), { message: "cancel" });
  assert.equal(box.listPendingPolicies()[0]!.server_settled, undefined);
});
