import assert from "node:assert/strict";
import { it } from "node:test";
import { CodexOperationController } from "../src/codex-operation-controller.js";
import { CodexOperationAdmissionError } from "../src/client.js";
import type { CodexOperationDecision, CodexOperationIdentity, CodexOperationRequest } from "../src/protocol.js";
import { AdmissionClock, flushUntil } from "./operation-admission-clock.js";

const operation = (n: number): CodexOperationIdentity => ({
  kind: "root", operation_id: `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`,
});
const admit = (r: CodexOperationRequest): CodexOperationDecision => ({
  decision: "admit", operation_id: r.operation_id, claim_generation: r.claim_generation,
});
const wait = (r: CodexOperationRequest, ms: number): CodexOperationDecision => ({
  decision: "wait", operation_id: r.operation_id, claim_generation: r.claim_generation,
  class: "overload", episode_id: operation(90).operation_id, server_now: new Date(0).toISOString(),
  eligible_at: new Date(ms).toISOString(),
});
const noop = (): void => {};
function rig(read: (r: CodexOperationRequest) => Promise<CodexOperationDecision>, supported = true) {
  const clock = new AdmissionClock();
  const calls: { at: number; request: CodexOperationRequest }[] = [];
  const state = { supported, negotiation: supported ? 1 : 0, engagements: 0, waiting: 0 };
  const c = new CodexOperationController(7, {
    supported: () => state.supported, negotiation: () => state.negotiation,
    engage: () => { state.engagements++; }, now: () => clock.now, sleep: clock.sleep,
    read: r => { calls.push({ at: clock.now, request: r }); return read(r); },
    waiting: () => { state.waiting++; return () => { state.waiting--; }; },
  });
  const start = (n: number, signal = new AbortController().signal, check = noop) =>
    c.admit(operation(n), signal, check, async () => { check(); });
  return { clock, calls, state, start };
}

for (const [delayMs, expected] of [[1, 1000], [9000, 9000], [90000, 15000]]) {
  it(`completion-relative cooldown ${delayMs}, early timer wakes settle at ${expected}`, async () => {
    const r = rig(async req => {
      if (r.calls.length === 1) { r.clock.now += 37; return wait(req, delayMs!); }
      return admit(req);
    });
    let settled = false;
    const pending = r.start(1).then(() => { settled = true; });
    await flushUntil(() => r.clock.sleepers.size === 1);
    assert.equal(r.calls.length, 1);
    r.clock.early();
    await flushUntil(() => r.clock.sleepers.size === 1);
    r.clock.advance(expected! - 1);
    assert.equal(settled, false);
    r.clock.advance(1);
    await pending;
    assert.deepEqual(r.calls.map(c => c.at), [0, 37 + expected!]);
    assert.equal(r.state.waiting, 0);
  });
}

it("coalesces schedules only; churn and unrestricted siblings each require fresh serial reads", async () => {
  const r = rig(async req => r.calls.length === 1 ? wait(req, 1000) : admit(req));
  const old = new AbortController();
  const first = r.start(1, old.signal);
  const rejected = assert.rejects(first);
  await flushUntil(() => r.clock.sleepers.size === 1);
  old.abort(new Error("source replaced"));
  await rejected;
  let completed = 0;
  const siblings = [2, 3, 4].map(n => r.start(n).then(() => { completed++; }));
  await flushUntil(() => r.clock.sleepers.size === 1);
  r.clock.advance(999);
  assert.equal(r.calls.length, 1);
  r.clock.advance(1);
  await Promise.all(siblings);
  assert.equal(completed, 3);
  assert.deepEqual(r.calls.map(c => c.request.operation_id), [1, 2, 3, 4].map(n => operation(n).operation_id));
  assert.deepEqual(r.calls.map(c => c.at), [0, 1000, 1000, 1000]);
  await r.start(5);
  assert.equal(r.calls.length, 5);
});

it("late admit cannot authorize a cancelled operation or overlap a successor read", async () => {
  let reply!: (r: CodexOperationDecision) => void;
  const r = rig(req => r.calls.length === 1 ? new Promise(resolve => { reply = resolve; }) : Promise.resolve(admit(req)));
  const abort = new AbortController();
  const old = assert.rejects(r.start(1, abort.signal), /cancelled/);
  await flushUntil(() => r.calls.length === 1);
  abort.abort(new Error("cancelled"));
  await old;
  const next = r.start(2);
  await Promise.resolve();
  assert.equal(r.calls.length, 1);
  reply(admit(r.calls[0]!.request));
  await flushUntil(() => r.clock.sleepers.size === 1);
  r.clock.advance(15000);
  await next;
  assert.deepEqual(r.calls.map(c => c.at), [0, 15000]);
});

for (const reason of ["authority_lost", "stale_source", "route_uncertain", "http_error"] as const) {
  it(`strict typed refusal ${reason} follows its own route`, async () => {
    const r = rig(async req => {
      if (r.calls.length === 1) throw new CodexOperationAdmissionError(404, reason);
      return admit(req);
    });
    const pending = r.start(1);
    if (reason === "authority_lost" || reason === "stale_source") {
      await assert.rejects(pending, (e: unknown) => e instanceof CodexOperationAdmissionError && e.reason === reason);
    } else {
      await flushUntil(() => r.clock.sleepers.size === 1);
      r.clock.advance(15000);
      await pending;
      assert.deepEqual(r.calls.map(c => c.at), [0, 15000]);
    }
  });
}

it("negotiation latches through clearing and re-registration; a never-negotiated flight stays legacy", async () => {
  const r = rig(async req => admit(req), false);
  await r.start(1);
  assert.equal(r.calls.length, 0);
  // Negotiated then cleared between two operations.
  r.state.negotiation++;
  r.state.supported = false;
  const abort = new AbortController();
  const pending = r.start(2, abort.signal);
  await flushUntil(() => r.clock.sleepers.size === 1);
  assert.equal(r.calls.length, 0);
  assert.equal(r.state.engagements, 1);
  r.state.supported = true;
  r.state.negotiation++;
  r.clock.advance(15000);
  await pending;
  r.state.supported = false;
  const stopped = assert.rejects(r.start(3, abort.signal));
  await flushUntil(() => r.clock.sleepers.size === 1);
  abort.abort();
  await stopped;
  assert.equal(r.calls.length, 1);
});

for (const change of ["feature rollback", "re-registration"] as const)
  it(`M2 ${change} while HTTP is pending invalidates its admit`, async () => {
    let reply!: (r: CodexOperationDecision) => void;
    const r = rig(req => r.calls.length === 1
      ? new Promise(resolve => { reply = resolve; }) : Promise.resolve(admit(req)));
    let dispatched = false;
    const pending = r.start(1).then(() => { dispatched = true; });
    await flushUntil(() => r.calls.length === 1);
    r.clock.advance(43);
    if (change === "feature rollback") r.state.supported = false;
    else r.state.negotiation++;
    reply(admit(r.calls[0]!.request));
    await flushUntil(() => r.clock.sleepers.size === 1);
    assert.equal(dispatched, false);
    assert.equal(r.state.engagements, 1);
    r.state.supported = true;
    r.clock.advance(14999);
    assert.equal(r.calls.length, 1);
    r.clock.advance(1);
    await pending;
    assert.deepEqual(r.calls.map(c => c.at), [0, 15043]);
    assert.equal(r.calls[1]!.request.operation_id, r.calls[0]!.request.operation_id);
    assert.equal(r.state.waiting, 0);
  });

it("M2 unavailable response schedules from HTTP completion and releases coalesced waiters independently", async () => {
  let fail!: (error: Error) => void;
  const r = rig(req => r.calls.length === 1
    ? new Promise((_, reject) => { fail = reject; }) : Promise.resolve(admit(req)));
  const first = r.start(1);
  await flushUntil(() => r.calls.length === 1);
  const next = r.start(2);
  r.clock.advance(271);
  fail(new CodexOperationAdmissionError(503, "http_error"));
  await flushUntil(() => r.clock.sleepers.size === 1);
  r.clock.advance(14999);
  assert.equal(r.calls.length, 1);
  r.clock.advance(1);
  await Promise.all([first, next]);
  assert.deepEqual(r.calls.map(c => c.at), [0, 15271, 15271]);
  assert.deepEqual(new Set(r.calls.slice(1).map(c => c.request.operation_id)),
    new Set([operation(1).operation_id, operation(2).operation_id]));
});

it("source/capture invalidation during HTTP prevents proof, dispatch and stale metadata", async () => {
  let reply!: (r: CodexOperationDecision) => void;
  const r = rig(() => new Promise(resolve => { reply = resolve; }));
  let current = true;
  const pending = assert.rejects(r.start(1, undefined, () => { if (!current) throw new Error("capture changed"); }), /capture changed/);
  await flushUntil(() => r.calls.length === 1);
  current = false;
  reply(wait(r.calls[0]!.request, 1000));
  await pending;
  assert.equal(r.state.waiting, 0);
  const next = r.start(2);
  await flushUntil(() => r.clock.sleepers.size === 1);
  assert.equal(r.calls.length, 1);
  r.clock.advance(1000);
  await flushUntil(() => r.calls.length === 2);
  reply(admit(r.calls[1]!.request));
  await next;
});

for (const mode of ["rollback", "reregister"]) it("M2 negotiation changes during proof: " + mode, async () => {
  const clock = new AdmissionClock();
  let supported = true, negotiation = 1, dispatched = false, reads = 0;
  let finishProof!: () => void;
  const controller = new CodexOperationController(7, {
    supported: () => supported, negotiation: () => negotiation, engage: () => {},
    now: () => clock.now, sleep: clock.sleep, waiting: () => () => {},
    read: async r => { reads++; return { decision: "admit", ...r }; },
  });
  const abort = new AbortController();
  const pending = controller.admit({
    kind: "root", operation_id: "00000000-0000-4000-8000-000000000001",
    source: { gate_revision: 1, presentation_id: "00000000-0000-4000-8000-000000000002", revision_input_id: 1 },
  }, abort.signal, () => {}, async () => {
    await new Promise<void>(resolve => { finishProof = resolve; });
  }).then(() => { dispatched = true; });
  try {
    await flushUntil(() => !!finishProof);
    if (mode === "rollback") supported = false;
    else negotiation++;
    finishProof();
    await flushUntil(() => dispatched || clock.sleepers.size > 0);
    assert.equal(dispatched, false, "obsolete negotiation admit must not dispatch after source-proof await");
    assert.equal(reads, 1);
  } finally { abort.abort(); finishProof?.(); await pending.catch(() => {}); }
});

for (const change of ["rollback", "reregister"] as const)
  it(`M2 final permit ${change} establishes one shared unavailable deadline`, async () => {
    const r = rig(async req => admit(req));
    const permit = await r.start(1);
    assert.equal(permit(), true);
    if (change === "rollback") r.state.supported = false;
    else r.state.negotiation++;
    r.clock.advance(17);
    assert.equal(permit(), false);
    r.clock.advance(23);
    assert.equal(permit(), false);
    r.state.supported = true;
    const sibling = r.start(2);
    await flushUntil(() => r.clock.sleepers.size === 1);
    r.clock.advance(14976);
    assert.equal(r.calls.length, 1);
    r.clock.advance(1);
    assert.equal((await sibling)(), true);
    assert.deepEqual(r.calls.map(c => c.at), [0, 15017]);
    assert.equal(permit(), false, "restored features cannot revive an obsolete permit");
  });

it("M2 legacy final permit observes first engagement and requires a fresh read", async () => {
  const r = rig(async req => admit(req), false);
  const permit = await r.start(1);
  assert.equal(permit(), true);
  r.state.supported = true;
  r.state.negotiation++;
  assert.equal(permit(), false);
  assert.equal(r.state.engagements, 1);
  assert.equal((await r.start(2))(), true);
  assert.equal(r.calls.length, 1);
});

it("M2 late wait response cannot shorten an invalidated permit's shared deadline", async () => {
  let release!: (reply: CodexOperationDecision) => void;
  const r = rig(req => r.calls.length === 2
    ? new Promise(resolve => { release = resolve; })
    : Promise.resolve(admit(req)));
  const firstPermit = await r.start(1);
  r.state.negotiation++;
  const second = r.start(2);
  try {
    await flushUntil(() => r.calls.length === 2);
    assert.equal(firstPermit(), false);
    release(wait(r.calls[1]!.request, 1000));
    await flushUntil(() => r.clock.sleepers.size === 1);
    r.clock.advance(1000);
    await flushUntil(() => r.calls.length > 2 || r.clock.sleepers.size > 0);
    assert.equal(r.calls.length, 2, "a late cooldown response cannot shorten the shared unavailable deadline");
    r.clock.advance(14000);
    await second;
    assert.deepEqual(r.calls.map(call => call.at), [0, 0, 15000]);
  } finally { r.clock.advance(30000); await second; }
});
